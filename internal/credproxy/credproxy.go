// Package credproxy is the bundled credential-injecting proxy (brief §8). The
// host runs it; runner containers are pointed at it via base-URL overrides (e.g.
// ANTHROPIC_BASE_URL=http://host.docker.internal:<port>). On each request the
// proxy looks up the stored credential for the upstream, swaps in the real token
// (api.anthropic.com -> x-api-key, otherwise Authorization: Bearer), and forwards
// to the real API. The raw token lives only in host memory and never enters the
// container.
//
// This covers services that honor a base-URL override (the claude CLI does for
// ANTHROPIC_BASE_URL). It does NOT cover tools that hit a fixed HTTPS host with
// no redirect (git/gh) - those would need a TLS-intercepting proxy, out of scope.
package credproxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Resolver returns the token + upstream URL for a given upstream host. credstore
// implements it. ok is false when no credential matches.
type Resolver interface {
	ResolveByHost(host string) (token, targetURL string, ok bool, err error)
}

// Proxy is the credential-injecting reverse proxy.
type Proxy struct {
	resolver Resolver
	// route maps the path prefix the agent is pointed at to the upstream host
	// whose credential should be injected. For the base-URL case there is a
	// single default upstream (e.g. api.anthropic.com), so an empty prefix maps
	// to it; richer routing can add more entries.
	defaultHost string
	log         *slog.Logger
	client      *http.Transport
}

// New builds a Proxy. defaultHost is the upstream a base-URL-redirected request
// is treated as destined for (e.g. "api.anthropic.com"); its credential is what
// gets injected. resolver looks the token + target up at request time, so a
// credential rotated via `goclaw auth` is picked up without a restart.
func New(resolver Resolver, defaultHost string, log *slog.Logger) *Proxy {
	return &Proxy{
		resolver:    resolver,
		defaultHost: defaultHost,
		log:         log,
		client: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// ServeHTTP handles one proxied request: resolve the credential for the upstream,
// rewrite the request to the real API with the injected auth header, and stream
// the response back (SSE-safe; httputil.ReverseProxy flushes streaming bodies).
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := p.defaultHost
	token, targetURL, ok, err := p.resolver.ResolveByHost(host)
	if err != nil {
		p.log.Error("credproxy resolve", "host", host, "err", err)
		http.Error(w, "credential proxy error", http.StatusBadGateway)
		return
	}
	if !ok {
		p.log.Warn("credproxy no credential for upstream", "host", host)
		http.Error(w, "no credential configured for "+host, http.StatusBadGateway)
		return
	}
	upstream, err := url.Parse(targetURL)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusBadGateway)
		return
	}

	rp := &httputil.ReverseProxy{
		Transport: p.client,
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			// Strip any inbound proxy/placeholder auth and inject the real token.
			// Use the LOGICAL host (what the credential is for, e.g.
			// api.anthropic.com) to pick the header, not the rewritten upstream
			// host (which may be an IP in tests or a private endpoint).
			injectAuth(req, host, token)
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, e error) {
			p.log.Error("credproxy upstream", "host", upstream.Host, "err", e)
			http.Error(rw, "upstream error", http.StatusBadGateway)
		},
		FlushInterval: -1, // flush immediately - required for SSE streaming
	}
	rp.ServeHTTP(w, r)
}

// injectAuth removes any inbound auth headers and sets the correct one for the
// upstream: api.anthropic.com uses x-api-key (raw value); everything else uses
// Authorization: Bearer (inferred-from-host rule).
func injectAuth(req *http.Request, host, token string) {
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("X-Api-Key")
	if isAnthropic(host) {
		req.Header.Set("x-api-key", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func isAnthropic(host string) bool {
	return host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com")
}

// Serve runs the proxy on addr until ctx is cancelled, then shuts down cleanly.
func (p *Proxy) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: p,
	}
	errc := make(chan error, 1)
	go func() {
		p.log.Info("credential proxy listening", "addr", addr, "upstream", p.defaultHost)
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
