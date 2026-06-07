package credproxy

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCA_GeneratePersistReload(t *testing.T) {
	dir := t.TempDir()
	ca1, _, err := LoadOrGenerateCA(dir, "", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Files persisted, key at 0600.
	keyPath := filepath.Join(dir, "ca.key")
	if fi, err := os.Stat(keyPath); err != nil {
		t.Fatalf("ca.key missing: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key perms = %v, want 0600", fi.Mode().Perm())
	}
	if !fileExists(filepath.Join(dir, "ca.pem")) {
		t.Fatal("ca.pem missing")
	}
	// Reload from disk yields the same cert.
	ca2, _, err := LoadOrGenerateCA(dir, "", "")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(ca1.CertPEM()) != string(ca2.CertPEM()) {
		t.Fatal("reloaded CA cert differs from the persisted one")
	}
}

// TestCA_StableIdentityWhenCertLost is the regression guard for the "tls: bad certificate"
// flood: if ca.pem is lost (deleted/corrupt) but ca.key SURVIVES, LoadOrGenerateCA must
// RE-DERIVE the cert from the existing key (same identity, generated=false), NOT mint a new
// CA. A new CA would invalidate the cert every running container trusts and break all
// intercepted TLS.
func TestCA_StableIdentityWhenCertLost(t *testing.T) {
	dir := t.TempDir()
	orig, gen1, err := LoadOrGenerateCA(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !gen1 {
		t.Fatal("first call should report generated=true (no prior CA)")
	}

	// A container is mounted the ORIGINAL cert; capture it as the trust root.
	origCertPEM := append([]byte(nil), orig.CertPEM()...)

	// Lose the cert but keep the key (the real failure: a partial data/proxy state).
	if err := os.Remove(filepath.Join(dir, "ca.pem")); err != nil {
		t.Fatal(err)
	}

	rederived, gen2, err := LoadOrGenerateCA(dir, "", "")
	if err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	if gen2 {
		t.Fatal("re-deriving from an existing key must NOT report a new identity (generated=false)")
	}

	// The decisive check: a leaf minted by the RE-DERIVED CA must validate against a client
	// that trusts the ORIGINAL cert. Same key -> same identity -> the old mounted cert still
	// works, which is exactly what keeps running containers happy.
	leaf, err := rederived.leafFor("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(origCertPEM) {
		t.Fatal("could not load original cert into pool")
	}
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{
		DNSName: "api.anthropic.com",
		Roots:   roots,
	}); err != nil {
		t.Fatalf("leaf from re-derived CA does NOT validate against the original cert: %v "+
			"(identity changed; this is the bad-certificate flood)", err)
	}

	// And ca.pem was rewritten so a fresh mount also gets a working cert.
	if !fileExists(filepath.Join(dir, "ca.pem")) {
		t.Fatal("ca.pem not re-persisted after re-derive")
	}
}

func TestCA_FromEnvPEM(t *testing.T) {
	// Generate + persist a CA, then reload it purely from its PEM (the env path).
	dir := t.TempDir()
	gen, _, err := LoadOrGenerateCA(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := os.ReadFile(filepath.Join(dir, "ca.key"))
	cb, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	fromEnv, _, err := LoadOrGenerateCA(t.TempDir(), string(kb), string(cb))
	if err != nil {
		t.Fatalf("from env PEM: %v", err)
	}
	if string(fromEnv.CertPEM()) != string(gen.CertPEM()) {
		t.Fatal("env-loaded CA cert differs from the source")
	}
}

// TestCA_LeafChainsAndValidates is the load-bearing test: a leaf minted for a
// host must verify against the CA for that exact host, and fail for others.
func TestCA_LeafChainsAndValidates(t *testing.T) {
	ca, _, err := LoadOrGenerateCA(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ca.LeafConfig("github.com")
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}

	// Build a roots pool trusting only our CA.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("could not add CA to pool")
	}

	leafDER := cfg.Certificates[0].Certificate[0]
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	// Verifies for the right host.
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "github.com", Roots: roots}); err != nil {
		t.Fatalf("leaf should verify for github.com: %v", err)
	}
	// Fails for a different host (SAN mismatch).
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "evil.com", Roots: roots}); err == nil {
		t.Fatal("leaf must NOT verify for a different host")
	}
	// Fails without our CA in the roots.
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "github.com", Roots: x509.NewCertPool()}); err == nil {
		t.Fatal("leaf must NOT verify without the CA root")
	}
}

func TestCA_LeafCacheAndRefresh(t *testing.T) {
	ca, _, err := LoadOrGenerateCA(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	ca.clock = func() time.Time { return base }

	c1, err := ca.leafFor("api.github.com")
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := ca.leafFor("api.github.com")
	if len(c1.Certificate) == 0 || string(c1.Certificate[0]) != string(c2.Certificate[0]) {
		t.Fatal("expected the cached leaf to be reused")
	}
	// Advance past refresh window -> a fresh leaf.
	ca.clock = func() time.Time { return base.Add(leafValidity - 30*time.Minute) }
	c3, _ := ca.leafFor("api.github.com")
	if string(c3.Certificate[0]) == string(c1.Certificate[0]) {
		t.Fatal("expected a regenerated leaf after the refresh window")
	}
}

func TestCA_LeafForIP(t *testing.T) {
	ca, _, err := LoadOrGenerateCA(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ca.LeafConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ip leaf: %v", err)
	}
	leaf, _ := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "127.0.0.1" {
		t.Fatalf("IP host should land in IPAddresses SAN, got DNS=%v IP=%v", leaf.DNSNames, leaf.IPAddresses)
	}
}

// TestCA_ServesRealTLS spins up a real TLS server using a minted leaf and
// connects with a client that trusts only our CA, end to end.
func TestCA_ServesRealTLS(t *testing.T) {
	ca, _, err := LoadOrGenerateCA(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	leafCfg, err := ca.LeafConfig("example.test")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())

	// A TLS handshake using the leaf as server cert, verified by a client that
	// trusts our CA, asserting the server name.
	clientCfg := &tls.Config{RootCAs: roots, ServerName: "example.test"}
	serverConn, clientConn := tlsPipe(t, leafCfg, clientCfg)
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()
	if err := clientConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake against minted leaf failed: %v", err)
	}
}

// tlsPipe wires an in-memory TLS client+server over a net.Pipe.
func tlsPipe(t *testing.T, serverCfg, clientCfg *tls.Config) (*tls.Conn, *tls.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	server := tls.Server(c1, serverCfg)
	client := tls.Client(c2, clientCfg)
	go func() { _ = server.Handshake() }() // the client.Handshake in the test drives it
	return server, client
}
