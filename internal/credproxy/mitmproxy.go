package credproxy

// MITMProxy is the TLS-intercepting credential proxy (docs/tls-proxy-scope.md).
// Containers point HTTPS_PROXY at it. For each CONNECT:
//   - if a credential is stored for the host, it INTERCEPTS: terminates the
//     client's TLS with a leaf cert signed by our CA (which the container
//     trusts), injects the real token into each request, and forwards to the
//     real upstream over a fresh TLS connection.
//   - otherwise it BLIND-TUNNELS: pipes bytes opaquely so that traffic stays
//     end-to-end encrypted to its real destination and we never decrypt it.
//
// This covers tools that hit a fixed HTTPS host with no base-URL override
// (git/gh), which the base-URL proxy in credproxy.go cannot reach.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// HostResolver answers, for a destination host, whether a credential is stored
// and (if so) the token + upstream URL to forward to. credstore satisfies it via
// Hosts()+ResolveByHost; here we only need the per-host lookup plus a membership
// check, both expressed through ResolveByHost.
type HostResolver interface {
	ResolveByHost(host string) (token, targetURL string, ok bool, err error)
}

// leafProvider supplies a per-host tls.Config (the CA implements it).
type leafProvider interface {
	LeafConfig(host string) (*tls.Config, error)
	CertPEM() []byte
}

// MITMProxy intercepts HTTPS via CONNECT and injects stored credentials.
type MITMProxy struct {
	resolver HostResolver
	ca       leafProvider
	log      *slog.Logger
	// testUpstreamRoots, when set, is added to the forward transport's trusted
	// roots so tests can point at a self-signed fake upstream. nil in production
	// (real upstreams use the system roots).
	testUpstreamRoots *x509.Certificate
}

// NewMITM builds a TLS-intercepting proxy.
func NewMITM(resolver HostResolver, ca leafProvider, log *slog.Logger) *MITMProxy {
	return &MITMProxy{resolver: resolver, ca: ca, log: log}
}

// CACertPEM exposes the CA cert to mount into the container trust store.
func (m *MITMProxy) CACertPEM() []byte { return m.ca.CertPEM() }

// Serve runs the proxy on addr until ctx is cancelled, then shuts down cleanly.
func (m *MITMProxy) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: m}
	errc := make(chan error, 1)
	go func() {
		m.log.Info("credential proxy listening (TLS-intercepting)", "addr", addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return fmt.Errorf("credproxy: %w", err)
	}
}

// ServeHTTP handles CONNECT (the only method a forward HTTPS proxy receives for
// TLS). Non-CONNECT requests are plain-HTTP forward-proxy requests; we reject
// them for now (the agent's HTTPS all arrives as CONNECT).
func (m *MITMProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "this proxy only handles CONNECT (HTTPS)", http.StatusMethodNotAllowed)
		return
	}
	m.handleConnect(w, r)
}

// handleConnect hijacks the connection and either intercepts or blind-tunnels.
func (m *MITMProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	hostPort := r.Host // "host:port"
	host := hostOnly(hostPort)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		m.log.Error("mitm hijack", "err", err)
		return
	}
	defer clientConn.Close()

	// Tell the client the tunnel is established.
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	token, targetURL, haveCred, err := m.resolver.ResolveByHost(host)
	if err != nil {
		m.log.Error("mitm resolve", "host", host, "err", err)
		return
	}
	if !haveCred {
		m.blindTunnel(clientConn, hostPort)
		return
	}
	m.intercept(clientConn, host, hostPort, token, targetURL)
}

// blindTunnel pipes bytes both directions without decrypting (no credential for
// this host), so the client's TLS stays end to end with the real upstream.
func (m *MITMProxy) blindTunnel(client net.Conn, hostPort string) {
	upstream, err := net.DialTimeout("tcp", hostPort, 15*time.Second)
	if err != nil {
		m.log.Warn("mitm blind dial", "host", hostPort, "err", err)
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done // first half to finish tears the tunnel down
}

// intercept terminates the client's TLS with a leaf for host, reads each HTTP
// request, injects the token, and forwards over a fresh TLS connection to the
// real upstream. Handles HTTP keep-alive by looping over requests on the conn.
func (m *MITMProxy) intercept(client net.Conn, host, hostPort, token, targetURL string) {
	leafCfg, err := m.ca.LeafConfig(host)
	if err != nil {
		m.log.Error("mitm leaf", "host", host, "err", err)
		return
	}
	tlsConn := tls.Server(client, leafCfg)
	if err := tlsConn.Handshake(); err != nil {
		m.log.Warn("mitm client handshake", "host", host, "err", err)
		return
	}
	defer tlsConn.Close()

	// Dial the real upstream once and reuse via a ReverseProxy per request.
	upstream, err := url.Parse(normalizeUpstream(targetURL, hostPort))
	if err != nil {
		m.log.Error("mitm upstream url", "err", err)
		return
	}
	transport := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	if m.testUpstreamRoots != nil {
		pool := x509.NewCertPool()
		pool.AddCert(m.testUpstreamRoots)
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	rp := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // SSE-safe
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			injectAuth(req, host, token) // reuse credproxy.go's host-based header rule
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, e error) {
			m.log.Error("mitm forward", "host", host, "err", e)
			rw.WriteHeader(http.StatusBadGateway)
		},
	}

	// Serve requests off the decrypted client conn until it closes.
	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return // client closed or bad request: end the tunnel
		}
		req.RemoteAddr = client.RemoteAddr().String()
		rw := &connResponseWriter{conn: tlsConn, header: http.Header{}}
		rp.ServeHTTP(rw, req)
		rw.flush()
		if req.Close || rw.closeAfter {
			return
		}
	}
}

// connResponseWriter adapts an http.ResponseWriter onto the raw decrypted
// connection so ReverseProxy can write the upstream response straight back to
// the client over the terminated TLS conn. It streams: WriteHeader emits the
// status line + headers, Write streams the body, Flush is a no-op (the conn is
// unbuffered at this layer).
type connResponseWriter struct {
	conn        net.Conn
	header      http.Header
	wroteHeader bool
	closeAfter  bool
}

func (w *connResponseWriter) Header() http.Header { return w.header }

func (w *connResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	// Force connection close after this response to keep the loop simple and
	// correct (no need to track response framing for keep-alive reuse).
	w.header.Set("Connection", "close")
	w.closeAfter = true
	w.header.Write(w.conn)
	io.WriteString(w.conn, "\r\n")
}

func (w *connResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(b)
}

// Flush satisfies http.Flusher so ReverseProxy streams (SSE) rather than buffers.
func (w *connResponseWriter) Flush() {}

func (w *connResponseWriter) flush() {}

func hostOnly(hostPort string) string {
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		return h
	}
	return hostPort
}

// normalizeUpstream turns a stored target URL into an absolute https URL to the
// real host. If the stored target lacks a scheme/host, fall back to https to the
// CONNECT host:port.
func normalizeUpstream(targetURL, hostPort string) string {
	if u, err := url.Parse(targetURL); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	host := hostOnly(hostPort)
	return "https://" + host
}
