package credproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// recordingResolver answers for one host. token may be a fixed string; if tokenFn is set
// it is called per resolve (so a test can prove the proxy re-resolves per REQUEST, the way
// an oauth2 token refresh would hand back a new value mid-tunnel).
type recordingResolver struct {
	host, token, target string
	tokenFn             func() string
}

func (r recordingResolver) BearerForHost(_ context.Context, host string) (string, string, bool, error) {
	if host != r.host {
		return "", "", false, nil
	}
	tok := r.token
	if r.tokenFn != nil {
		tok = r.tokenFn()
	}
	return tok, r.target, true, nil
}

func (r recordingResolver) UpstreamForHost(host string) (string, bool, error) {
	if host != r.host {
		return "", false, nil
	}
	return r.target, true, nil
}

// startMITM runs the proxy on a random port and returns its addr + a cleanup.
func startMITM(t *testing.T, p *MITMProxy) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: p}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}

// proxyClient builds an http.Client that CONNECTs through proxyAddr and trusts
// caPEM for the intercepted leaf certs.
func proxyClient(proxyAddr string, caPEM []byte) *http.Client {
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: proxyAddr}),
			TLSClientConfig: &tls.Config{RootCAs: roots},
		},
	}
}

func TestMITM_InterceptInjectsAndForwards(t *testing.T) {
	// Fake upstream HTTPS server (its own self-signed cert via httptest).
	var gotAuth, gotPath string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()
	upHost := upstream.Listener.Addr().String() // 127.0.0.1:port

	ca, err := LoadOrGenerateCA(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	// The proxy must trust the fake upstream's cert: rebuild its forward
	// transport to trust httptest's CA. We do this by giving the resolver a
	// target that points at the upstream and relaxing upstream verification via
	// a custom proxy whose transport trusts the test server.
	res := recordingResolver{host: "git.example.test", token: "ghtok-123", target: "https://" + upHost}
	p := NewMITM(res, ca, quiet())
	// Trust the upstream's self-signed cert inside the proxy's forwarding.
	p.testUpstreamRoots = upstream.Certificate()

	addr, stop := startMITM(t, p)
	defer stop()

	client := proxyClient(addr, ca.CertPEM())
	// The client thinks it is talking to https://git.example.test; the proxy
	// intercepts (credential present) and forwards to the fake upstream.
	resp, err := client.Get("https://git.example.test/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("request through MITM: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "upstream-ok" {
		t.Fatalf("body = %q", body)
	}
	if gotAuth != "Bearer ghtok-123" {
		t.Fatalf("upstream Authorization = %q, want injected Bearer", gotAuth)
	}
	if !strings.HasPrefix(gotPath, "/info/refs") {
		t.Fatalf("path not preserved: %q", gotPath)
	}
}

// TestMITM_ReResolvesTokenPerRequest proves the proxy injects a CURRENT token on EACH
// request over one keep-alive tunnel, not a single token captured at CONNECT time. This is
// what lets an oauth2 access token refresh mid-tunnel: the resolver hands back a new token
// on the second resolve and the upstream must see it.
func TestMITM_ReResolvesTokenPerRequest(t *testing.T) {
	var gotAuth []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upHost := upstream.Listener.Addr().String()

	ca, _ := LoadOrGenerateCA(t.TempDir(), "", "")
	var n atomic.Int64
	res := recordingResolver{
		host:   "gmail.googleapis.com",
		target: "https://" + upHost,
		tokenFn: func() string {
			// "access-1" on the first resolve, "access-2" on the next, like a refresh.
			return "access-" + itoaTest(n.Add(1))
		},
	}
	p := NewMITM(res, ca, quiet())
	p.testUpstreamRoots = upstream.Certificate()
	addr, stop := startMITM(t, p)
	defer stop()

	client := proxyClient(addr, ca.CertPEM())
	// Two requests; the default transport reuses the tunnel (keep-alive), so both go over
	// the SAME intercepted conn, exercising the per-request Director re-resolve.
	for i := 0; i < 2; i++ {
		resp, err := client.Get("https://gmail.googleapis.com/gmail/v1/users/me/messages")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if len(gotAuth) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(gotAuth))
	}
	if gotAuth[0] != "Bearer access-1" || gotAuth[1] != "Bearer access-2" {
		t.Fatalf("per-request tokens = %v, want [Bearer access-1, Bearer access-2]", gotAuth)
	}
}

func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestMITM_StreamsSSEThroughIntercept(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: one\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	defer upstream.Close()
	upHost := upstream.Listener.Addr().String()

	ca, _ := LoadOrGenerateCA(t.TempDir(), "", "")
	res := recordingResolver{host: "api.anthropic.com", token: "sk-ant-x", target: "https://" + upHost}
	p := NewMITM(res, ca, quiet())
	p.testUpstreamRoots = upstream.Certificate()
	addr, stop := startMITM(t, p)
	defer stop()

	client := proxyClient(addr, ca.CertPEM())
	resp, err := client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatalf("sse request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: one") || !strings.Contains(string(body), "data: two") {
		t.Fatalf("SSE not fully proxied through MITM: %q", body)
	}
}

func TestMITM_BlindTunnelNoCredential(t *testing.T) {
	// Upstream the proxy has NO credential for: must blind-tunnel, so the
	// client's own TLS to the upstream succeeds end to end (the proxy never
	// presents a forged cert).
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "direct-tls")
	}))
	defer upstream.Close()
	upHost := upstream.Listener.Addr().String()

	ca, _ := LoadOrGenerateCA(t.TempDir(), "", "")
	res := recordingResolver{host: "has-no-cred.test"} // resolver only knows that host
	p := NewMITM(res, ca, quiet())
	addr, stop := startMITM(t, p)
	defer stop()

	// Client trusts the UPSTREAM's real cert (not our CA), routes through the
	// proxy. Because there is no credential for upHost, the proxy blind-tunnels
	// and the client's TLS terminates at the real upstream.
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: addr}),
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"},
	}}
	// httptest server cert is for example.com / 127.0.0.1; dial by IP host.
	resp, err := client.Get("https://" + upHost + "/")
	if err != nil {
		t.Fatalf("blind-tunnel request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "direct-tls" {
		t.Fatalf("blind tunnel body = %q", body)
	}
}
