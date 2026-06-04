package credproxy

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

// quiet is the shared silent logger for credproxy tests.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestInjectAuth_PerHostScheme(t *testing.T) {
	basicTok := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	cases := []struct {
		host     string
		wantXAPI string
		wantAuth string
	}{
		{"api.anthropic.com", "tok", ""},      // x-api-key
		{"foo.anthropic.com", "tok", ""},      // x-api-key
		{"api.github.com", "", "Bearer tok"},  // GitHub API: Bearer
		{"github.com", "", basicTok},          // git smart-HTTP: Basic
		{"codeload.github.com", "", basicTok}, // git archive/pack: Basic
		{"api.openai.com", "", "Bearer tok"},  // default: Bearer
	}
	for _, c := range cases {
		req, _ := http.NewRequest("GET", "https://"+c.host+"/x", nil)
		// Seed inbound auth that must be stripped.
		req.Header.Set("x-api-key", "placeholder")
		req.Header.Set("Authorization", "Bearer placeholder")
		injectAuth(req, c.host, "tok")
		if got := req.Header.Get("x-api-key"); got != c.wantXAPI {
			t.Errorf("%s: x-api-key = %q, want %q", c.host, got, c.wantXAPI)
		}
		if got := req.Header.Get("Authorization"); got != c.wantAuth {
			t.Errorf("%s: Authorization = %q, want %q", c.host, got, c.wantAuth)
		}
	}
}

func TestIsAnthropic(t *testing.T) {
	for host, want := range map[string]bool{
		"api.anthropic.com": true,
		"x.anthropic.com":   true,
		"anthropic.com":     false, // bare apex is not the API host
		"github.com":        false,
	} {
		if got := isAnthropic(host); got != want {
			t.Errorf("isAnthropic(%q) = %v, want %v", host, got, want)
		}
	}
}
