package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shindakun/goclaw/internal/credstore"
	"github.com/shindakun/goclaw/internal/db"
)

func oauthTestStore(t *testing.T) *credstore.Store {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(filepath.Join(t.TempDir(), "central.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return credstore.New(d.DB, base64.StdEncoding.EncodeToString(k))
}

func TestSplitScopes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a b c", []string{"a", "b", "c"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b   c ", []string{"a", "b", "c"}},
		{"https://www.googleapis.com/auth/gmail.readonly", []string{"https://www.googleapis.com/auth/gmail.readonly"}},
	}
	for _, c := range cases {
		got := splitScopes(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitScopes(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBuildAuthCodeURL(t *testing.T) {
	u := buildAuthCodeURL("cid-123", "http://127.0.0.1:5555/", []string{"scopeA", "scopeB"})
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	// access_type=offline + prompt=consent are REQUIRED to be issued a refresh token.
	if q.Get("access_type") != "offline" {
		t.Errorf("access_type = %q, want offline", q.Get("access_type"))
	}
	if q.Get("prompt") != "consent" {
		t.Errorf("prompt = %q, want consent", q.Get("prompt"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	if q.Get("client_id") != "cid-123" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "http://127.0.0.1:5555/" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("scope") != "scopeA scopeB" {
		t.Errorf("scope = %q, want space-joined", q.Get("scope"))
	}
}

func TestExchangeCode(t *testing.T) {
	// Fake token endpoint that returns a refresh token and echoes what it received.
	var gotGrant, gotCode, gotRedirect string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotCode = r.Form.Get("code")
		gotRedirect = r.Form.Get("redirect_uri")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-xyz",
			"refresh_token": "rt-fromcode",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	old := authHTTPClient
	authHTTPClient = srv.Client()
	defer func() { authHTTPClient = old }()

	rt, err := exchangeCode(context.Background(), "cid", "csec", srv.URL, "the-code", "http://127.0.0.1:9/")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if rt != "rt-fromcode" {
		t.Fatalf("refresh token = %q, want rt-fromcode", rt)
	}
	if gotGrant != "authorization_code" || gotCode != "the-code" || gotRedirect != "http://127.0.0.1:9/" {
		t.Fatalf("endpoint saw grant=%q code=%q redirect=%q", gotGrant, gotCode, gotRedirect)
	}
}

func TestExchangeCode_NoRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-only"})
	}))
	defer srv.Close()
	old := authHTTPClient
	authHTTPClient = srv.Client()
	defer func() { authHTTPClient = old }()

	_, err := exchangeCode(context.Background(), "cid", "csec", srv.URL, "code", "http://127.0.0.1:9/")
	if err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("want a missing-refresh_token error, got %v", err)
	}
}

// The --refresh-token path stores an oauth2-bearer credential without any consent flow.
func TestAuthAddOAuth_RefreshTokenPath(t *testing.T) {
	store := oauthTestStore(t)
	err := authAddOAuth(store, []string{
		"--name", "gmail",
		"--target-api-url", "https://gmail.googleapis.com",
		"--refresh-token", "rt-direct",
		"--client-id", "cid",
		"--client-secret", "csec",
		"--scopes", "https://www.googleapis.com/auth/gmail.readonly",
	})
	if err != nil {
		t.Fatalf("authAddOAuth: %v", err)
	}
	creds, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("want 1 credential, got %d", len(creds))
	}
	c := creds[0]
	if c.Kind != credstore.KindOAuth2Bearer {
		t.Errorf("kind = %q, want oauth2-bearer", c.Kind)
	}
	if c.TargetHost != "gmail.googleapis.com" {
		t.Errorf("target host = %q", c.TargetHost)
	}
	// The listing must NOT leak the refresh token; oauth2 rows preview as <oauth2>.
	if strings.Contains(c.Preview, "rt-direct") {
		t.Errorf("preview leaked the refresh token: %q", c.Preview)
	}
}

// Missing required flags fail closed with a clear error, not a panic or a partial store.
func TestAuthAddOAuth_RequiresNameAndTarget(t *testing.T) {
	store := oauthTestStore(t)
	if err := authAddOAuth(store, []string{"--refresh-token", "rt"}); err == nil {
		t.Fatal("want an error when --name and --target-api-url are missing")
	}
	if creds, _ := store.List(); len(creds) != 0 {
		t.Fatalf("nothing should have been stored, got %d", len(creds))
	}
}

// Without a refresh token AND without client creds, the consent flow cannot run; fail closed.
func TestAuthAddOAuth_ConsentNeedsClientCreds(t *testing.T) {
	store := oauthTestStore(t)
	err := authAddOAuth(store, []string{
		"--name", "gmail",
		"--target-api-url", "https://gmail.googleapis.com",
	})
	if err == nil || !strings.Contains(err.Error(), "client-id") {
		t.Fatalf("want a client-id/secret required error, got %v", err)
	}
}
