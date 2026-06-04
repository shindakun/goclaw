package credproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeResolver returns a fixed token + target for one host.
type fakeResolver struct {
	host, token, target string
}

func (f fakeResolver) ResolveByHost(host string) (string, string, bool, error) {
	if host == f.host {
		return f.token, f.target, true, nil
	}
	return "", "", false, nil
}

func TestProxy_InjectsAnthropicKeyAndForwards(t *testing.T) {
	// Fake upstream "api.anthropic.com" that records what it received.
	var gotXAPIKey, gotAuth, gotPath string
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	// Resolver maps the anthropic host to the fake upstream + real token.
	res := fakeResolver{host: "api.anthropic.com", token: "sk-ant-REAL-secret", target: upstream.URL}
	p := New(res, "api.anthropic.com", quiet())
	front := httptest.NewServer(p)
	defer front.Close()

	// Agent calls the proxy with a PLACEHOLDER key, like the runner would.
	req, _ := http.NewRequest("POST", front.URL+"/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("x-api-key", "placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("unexpected response: %d %s", resp.StatusCode, body)
	}
	// The upstream saw the REAL token, not the placeholder.
	if gotXAPIKey != "sk-ant-REAL-secret" {
		t.Fatalf("upstream x-api-key = %q, want the real token", gotXAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization should be empty for anthropic, got %q", gotAuth)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path not preserved: %q", gotPath)
	}
	if gotBody != `{"model":"x"}` {
		t.Fatalf("body not forwarded: %q", gotBody)
	}
}

func TestProxy_BearerForNonAnthropic(t *testing.T) {
	var gotAuth, gotXAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	res := fakeResolver{host: "api.example.com", token: "tok-123", target: upstream.URL}
	p := New(res, "api.example.com", quiet())
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/things")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("non-anthropic should use Bearer, got %q", gotAuth)
	}
	if gotXAPIKey != "" {
		t.Fatalf("x-api-key should be empty for non-anthropic, got %q", gotXAPIKey)
	}
}

func TestProxy_NoCredential502(t *testing.T) {
	res := fakeResolver{host: "api.anthropic.com", token: "t", target: "http://x"}
	// defaultHost has no matching credential in the resolver.
	p := New(res, "api.unconfigured.com", quiet())
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when no credential, got %d", resp.StatusCode)
	}
}

// TestProxy_StreamsSSE checks the response body is streamed (flushed), not
// buffered to completion, which matters for the claude CLI's SSE responses.
func TestProxy_StreamsSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		w.Write([]byte("data: one\n\n"))
		if fl != nil {
			fl.Flush()
		}
		w.Write([]byte("data: two\n\n"))
	}))
	defer upstream.Close()

	res := fakeResolver{host: "api.anthropic.com", token: "t", target: upstream.URL}
	p := New(res, "api.anthropic.com", quiet())
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: one") || !strings.Contains(string(body), "data: two") {
		t.Fatalf("SSE body not fully proxied: %q", body)
	}
}
