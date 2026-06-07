package credproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA is the certificate authority the TLS-intercepting proxy uses to terminate
// (man-in-the-middle) client TLS connections. It signs a short-lived leaf
// certificate per upstream host on demand, signed by a root the container
// trusts. The root is generated once and persisted (or supplied via env); the
// container only trusts it for traffic the proxy intercepts, and the proxy is on
// the host, so the trust boundary is the sandbox.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey

	mu    sync.Mutex
	leaf  map[string]*leafEntry // per-host leaf cert cache
	clock func() time.Time      // injectable for tests
}

type leafEntry struct {
	cert      tls.Certificate
	expiresAt time.Time
}

const (
	caValidity      = 10 * 365 * 24 * time.Hour // long-lived root
	leafValidity    = 24 * time.Hour            // short-lived leaves
	leafRefreshHead = time.Hour                 // regenerate when under this much remains
	caCommonName    = "goclaw credential proxy CA"
)

// LoadOrGenerateCA returns the proxy CA and whether it MINTED A NEW IDENTITY (a fresh key).
// Resolution order, designed so the CA identity is STABLE for the life of `ca.key`, because
// a CA identity change invalidates the cert mounted into every running container and breaks
// all intercepted TLS until those containers see the new cert:
//
//  1. keyPEM + certPEM (env GOCLAW_PROXY_CA_KEY / _CERT) when both non-empty;
//  2. both files exist and parse -> load them (no change, no rewrite);
//  3. ca.key exists but ca.pem is MISSING or unparseable -> RE-DERIVE the cert from the
//     existing key (SAME identity) and persist just the cert. We do NOT throw away a good
//     key and mint a new CA, that was the bug that silently changed the CA out from under
//     live containers;
//  4. no usable key -> generate a brand-new CA (new key + cert). This is the ONLY path that
//     changes identity; it returns generated=true so the caller can log it LOUDLY (it means
//     every running container now trusts a stale CA until it sees the new one).
//
// dir is created if needed. The env path stays first so an operator can pin the CA outside
// the data dir.
func LoadOrGenerateCA(dir, keyPEM, certPEM string) (ca *CA, generated bool, err error) {
	if keyPEM != "" && certPEM != "" {
		c, e := caFromPEM([]byte(keyPEM), []byte(certPEM))
		return c, false, e
	}
	keyPath := filepath.Join(dir, "ca.key")
	certPath := filepath.Join(dir, "ca.pem")

	// Both present and parseable: load as-is, no rewrite (a gratuitous rewrite can change
	// the file inode under a live single-file bind mount).
	if fileExists(keyPath) && fileExists(certPath) {
		kb, e := os.ReadFile(keyPath)
		if e != nil {
			return nil, false, fmt.Errorf("ca: read key: %w", e)
		}
		cb, e := os.ReadFile(certPath)
		if e != nil {
			return nil, false, fmt.Errorf("ca: read cert: %w", e)
		}
		if c, e := caFromPEM(kb, cb); e == nil {
			return c, false, nil
		}
		// Cert unparseable but key present: fall through to re-derive from the key below
		// rather than mint a new identity.
	}

	// Key present (cert missing or unparseable): re-derive the cert from the EXISTING key,
	// preserving the CA identity. No new key is generated.
	if fileExists(keyPath) {
		kb, e := os.ReadFile(keyPath)
		if e != nil {
			return nil, false, fmt.Errorf("ca: read key: %w", e)
		}
		c, e := caFromExistingKey(kb, certPath)
		if e != nil {
			return nil, false, e
		}
		return c, false, nil
	}

	// Nothing usable: this is the only identity-minting path.
	c, e := generateAndPersistCA(dir, keyPath, certPath)
	return c, true, e
}

// CertPEM returns the CA certificate in PEM form, to be mounted into the
// container and pointed at by the tools' trust env vars.
func (c *CA) CertPEM() []byte { return c.certPEM }

// LeafConfig returns a *tls.Config that serves a leaf certificate for host,
// signed by this CA. Leaves are cached per host and regenerated before expiry.
func (c *CA) LeafConfig(host string) (*tls.Config, error) {
	leaf, err := c.leafFor(host)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{leaf},
		MinVersion:   tls.VersionTLS12,
		// Advertise ONLY http/1.1. The intercept loop speaks HTTP/1.1
		// (http.ReadRequest + a 1.1 response); advertising h2 here would make
		// clients negotiate HTTP/2 and then stall on the 1.1 response. Clients
		// (curl, git, gh, node) fall back to 1.1 cleanly.
		NextProtos: []string{"http/1.1"},
	}, nil
}

func (c *CA) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// leafFor returns a cached or freshly minted leaf cert for host.
func (c *CA) leafFor(host string) (tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.leaf[host]; ok && c.now().Before(e.expiresAt.Add(-leafRefreshHead)) {
		return e.cert, nil
	}
	leaf, exp, err := c.mintLeaf(host)
	if err != nil {
		return tls.Certificate{}, err
	}
	c.leaf[host] = &leafEntry{cert: leaf, expiresAt: exp}
	return leaf, nil
}

// mintLeaf signs a new leaf cert for host with the CA. Returns the tls cert and
// its expiry. The leaf's SAN carries the exact host (DNS or IP) so strict
// hostname validation in git/gh/curl passes.
func (c *CA) mintLeaf(host string) (tls.Certificate, time.Time, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, time.Time{}, err
	}
	serial, err := randSerial()
	if err != nil {
		return tls.Certificate{}, time.Time{}, err
	}
	now := c.now()
	notAfter := now.Add(leafValidity)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &leafKey.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, time.Time{}, fmt.Errorf("ca: sign leaf for %q: %w", host, err)
	}
	// The leaf's chain includes the CA cert so clients can build the path.
	leaf := tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  leafKey,
		Leaf:        nil,
	}
	return leaf, notAfter, nil
}

// --- construction helpers ---

func newCA(cert *x509.Certificate, certPEM []byte, key *ecdsa.PrivateKey) *CA {
	return &CA{cert: cert, certPEM: certPEM, key: key, leaf: map[string]*leafEntry{}}
}

func caFromPEM(keyPEM, certPEM []byte) (*CA, error) {
	kBlock, _ := pem.Decode(keyPEM)
	if kBlock == nil {
		return nil, fmt.Errorf("ca: no PEM block in CA key")
	}
	key, err := x509.ParseECPrivateKey(kBlock.Bytes)
	if err != nil {
		// tolerate PKCS#8
		if k8, e8 := x509.ParsePKCS8PrivateKey(kBlock.Bytes); e8 == nil {
			if ek, ok := k8.(*ecdsa.PrivateKey); ok {
				key = ek
			} else {
				return nil, fmt.Errorf("ca: CA key is not ECDSA")
			}
		} else {
			return nil, fmt.Errorf("ca: parse CA key: %w", err)
		}
	}
	cBlock, _ := pem.Decode(certPEM)
	if cBlock == nil {
		return nil, fmt.Errorf("ca: no PEM block in CA cert")
	}
	cert, err := x509.ParseCertificate(cBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca: parse CA cert: %w", err)
	}
	return newCA(cert, certPEM, key), nil
}

func generateAndPersistCA(dir, keyPath, certPath string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ca: create dir %q: %w", dir, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// Persist the key first (0600, sensitive): the key IS the CA identity. If cert minting
	// or its write fails after this, a later start re-derives the cert from this key rather
	// than minting a new identity (see LoadOrGenerateCA).
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("ca: write key: %w", err)
	}
	return mintAndPersistCert(key, certPath)
}

// caFromExistingKey parses an existing CA key and re-derives a self-signed CA cert from it,
// persisting the cert to certPath. Because the cert is signed by (and carries the public key
// of) the SAME key, the CA identity is unchanged: leaves the proxy already minted, and any
// container trusting a prior cert from this key, still validate. Used when the key survives
// but the cert is gone/corrupt, so we never mint a new identity just because the cert file
// was lost.
func caFromExistingKey(keyPEM []byte, certPath string) (*CA, error) {
	kBlock, _ := pem.Decode(keyPEM)
	if kBlock == nil {
		return nil, fmt.Errorf("ca: no PEM block in CA key")
	}
	key, err := x509.ParseECPrivateKey(kBlock.Bytes)
	if err != nil {
		if k8, e8 := x509.ParsePKCS8PrivateKey(kBlock.Bytes); e8 == nil {
			if ek, ok := k8.(*ecdsa.PrivateKey); ok {
				key = ek
			} else {
				return nil, fmt.Errorf("ca: CA key is not ECDSA")
			}
		} else {
			return nil, fmt.Errorf("ca: parse CA key: %w", err)
		}
	}
	return mintAndPersistCert(key, certPath)
}

// mintAndPersistCert self-signs a CA cert with key, writes it to certPath, and returns the
// CA. Shared by generate (new key) and re-derive (existing key).
func mintAndPersistCert(key *ecdsa.PrivateKey, certPath string) (*CA, error) {
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName, Organization: []string{"goclaw"}},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("ca: self-sign: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("ca: write cert: %w", err)
	}
	return newCA(cert, certPEM, key), nil
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
