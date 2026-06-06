package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shindakun/goclaw/internal/credstore"
	"github.com/shindakun/goclaw/internal/db"
	"github.com/shindakun/goclaw/internal/plugin"
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

// gmailSpec is a representative Google-shaped oauth block for the URL/exchange tests.
func gmailSpec() plugin.OAuthSpec {
	return plugin.OAuthSpec{
		Provider:   "google",
		AuthURL:    "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:   "https://oauth2.googleapis.com/token",
		TargetHost: "gmail.googleapis.com",
		Scopes:     []string{"https://www.googleapis.com/auth/gmail.modify"},
		AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"},
	}
}

// writePluginWithOAuth writes a minimal plugin.yml (with an oauth block) under
// $GOCLAW_DATA_DIR/plugins/<name>/ and sets GOCLAW_DATA_DIR so --plugin <name> resolves.
// oauthYAML is the YAML body of the oauth: block (without the "oauth:" key), or "" for none.
func writePluginWithOAuth(t *testing.T, name, oauthYAML string) {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("GOCLAW_DATA_DIR", dataDir)
	dir := filepath.Join(dataDir, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: " + name + "\nkind: tool\nexec: " + name + "\n"
	if oauthYAML != "" {
		body += "oauth:\n" + oauthYAML
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// The binary need not be runnable for add-oauth (it only reads the manifest), but
	// LoadManifest requires exec to be present as a field, which it is.
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

// buildAuthCodeURL is driven by the plugin's oauth block: Google's auth_params appear,
// scopes join with the (default space) separator, and the auth_url is the provider's.
func TestBuildAuthCodeURL_Google(t *testing.T) {
	spec := gmailSpec()
	spec.Scopes = []string{"scopeA", "scopeB"}
	u := buildAuthCodeURL(spec, "cid-123", "http://127.0.0.1:5555/")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, spec.AuthURL+"?") {
		t.Errorf("auth URL base = %q, want %q", u, spec.AuthURL)
	}
	q := parsed.Query()
	// Provider auth_params (force a refresh token) come from the manifest, not hardcoded.
	if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Errorf("auth_params not applied: access_type=%q prompt=%q", q.Get("access_type"), q.Get("prompt"))
	}
	if q.Get("response_type") != "code" || q.Get("client_id") != "cid-123" {
		t.Errorf("response_type=%q client_id=%q", q.Get("response_type"), q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "http://127.0.0.1:5555/" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("scope") != "scopeA scopeB" {
		t.Errorf("scope = %q, want space-joined", q.Get("scope"))
	}
}

// A provider with NO auth_params and a comma scope separator (proving none of the Google
// shape is hardcoded): the URL has no access_type/prompt and joins scopes with commas.
func TestBuildAuthCodeURL_GenericProvider(t *testing.T) {
	spec := plugin.OAuthSpec{
		AuthURL:        "https://example.test/authorize",
		TokenURL:       "https://example.test/token",
		TargetHost:     "api.example.test",
		Scopes:         []string{"read", "write"},
		ScopeSeparator: ",",
	}
	u := buildAuthCodeURL(spec, "cid", "http://127.0.0.1:1/")
	q, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if q.Get("access_type") != "" || q.Get("prompt") != "" {
		t.Errorf("no auth_params declared, but URL carried Google params: %q", u)
	}
	if q.Get("scope") != "read,write" {
		t.Errorf("scope = %q, want comma-joined", q.Get("scope"))
	}
}

func TestExchangeCode_Body(t *testing.T) {
	var gotGrant, gotCode, gotRedirect, gotCID, gotAuthHdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant, gotCode, gotRedirect = r.Form.Get("grant_type"), r.Form.Get("code"), r.Form.Get("redirect_uri")
		gotCID = r.Form.Get("client_id")
		gotAuthHdr = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-xyz", "refresh_token": "rt-fromcode"})
	}))
	defer srv.Close()
	old := authHTTPClient
	authHTTPClient = srv.Client()
	defer func() { authHTTPClient = old }()

	spec := gmailSpec()
	spec.TokenURL = srv.URL // body client-auth (default)
	rt, err := exchangeCode(context.Background(), spec, "cid", "csec", "the-code", "http://127.0.0.1:9/")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if rt != "rt-fromcode" {
		t.Fatalf("refresh token = %q", rt)
	}
	if gotGrant != "authorization_code" || gotCode != "the-code" || gotRedirect != "http://127.0.0.1:9/" {
		t.Fatalf("endpoint saw grant=%q code=%q redirect=%q", gotGrant, gotCode, gotRedirect)
	}
	// body mode: client_id in the form, no Authorization header.
	if gotCID != "cid" || gotAuthHdr != "" {
		t.Fatalf("body client-auth wrong: form client_id=%q authHdr=%q", gotCID, gotAuthHdr)
	}
}

// client_auth: basic puts the client creds in an HTTP Basic header, NOT the body (proving
// the client-auth style is plugin data: this is the Spotify/Reddit shape).
func TestExchangeCode_Basic(t *testing.T) {
	var gotCID, gotAuthHdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotCID = r.Form.Get("client_id")
		gotAuthHdr = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "refresh_token": "rt-basic"})
	}))
	defer srv.Close()
	old := authHTTPClient
	authHTTPClient = srv.Client()
	defer func() { authHTTPClient = old }()

	spec := plugin.OAuthSpec{TokenURL: srv.URL, ClientAuth: "basic", Scopes: []string{"s"}}
	rt, err := exchangeCode(context.Background(), spec, "cid", "csec", "code", "http://127.0.0.1:9/")
	if err != nil || rt != "rt-basic" {
		t.Fatalf("exchangeCode basic: rt=%q err=%v", rt, err)
	}
	wantHdr := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:csec"))
	if gotAuthHdr != wantHdr {
		t.Fatalf("Authorization = %q, want %q", gotAuthHdr, wantHdr)
	}
	if gotCID != "" {
		t.Fatalf("basic mode must NOT put client_id in the body, got %q", gotCID)
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

	spec := gmailSpec()
	spec.TokenURL = srv.URL
	_, err := exchangeCode(context.Background(), spec, "cid", "csec", "code", "http://127.0.0.1:9/")
	if err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("want a missing-refresh_token error, got %v", err)
	}
}

// The --plugin + --refresh-token path: reads the plugin's oauth block for the provider data
// (target host, token url, scopes), stores an oauth2-bearer credential, no consent flow.
func TestAuthAddOAuth_PluginRefreshTokenPath(t *testing.T) {
	writePluginWithOAuth(t, "gmail", `  provider: google
  auth_url: https://accounts.google.com/o/oauth2/v2/auth
  token_url: https://oauth2.googleapis.com/token
  target_host: gmail.googleapis.com
  scopes:
    - https://www.googleapis.com/auth/gmail.modify
  auth_params:
    access_type: offline
    prompt: consent
`)
	store := oauthTestStore(t)
	err := authAddOAuth(store, []string{
		"--plugin", "gmail",
		"--refresh-token", "rt-direct",
		"--client-id", "cid",
		"--client-secret", "csec",
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
		t.Errorf("target host = %q (should come from the plugin's oauth block)", c.TargetHost)
	}
	if c.Name != "gmail" {
		t.Errorf("name = %q, want the plugin name default", c.Name)
	}
	if strings.Contains(c.Preview, "rt-direct") {
		t.Errorf("preview leaked the refresh token: %q", c.Preview)
	}
}

// --plugin is required; without it, fail closed and store nothing.
func TestAuthAddOAuth_RequiresPlugin(t *testing.T) {
	store := oauthTestStore(t)
	if err := authAddOAuth(store, []string{"--refresh-token", "rt"}); err == nil {
		t.Fatal("want an error when --plugin is missing")
	}
	if creds, _ := store.List(); len(creds) != 0 {
		t.Fatalf("nothing should have been stored, got %d", len(creds))
	}
}

// A plugin with no oauth block is rejected with a clear message.
func TestAuthAddOAuth_PluginWithoutOAuthBlock(t *testing.T) {
	writePluginWithOAuth(t, "roll", "") // no oauth block
	store := oauthTestStore(t)
	err := authAddOAuth(store, []string{"--plugin", "roll", "--refresh-token", "rt"})
	if err == nil || !strings.Contains(err.Error(), "no oauth block") {
		t.Fatalf("want a 'no oauth block' error, got %v", err)
	}
}

// Without a refresh token AND without client creds, the consent flow cannot run; fail closed
// (but only AFTER the confirm, which --yes skips here).
func TestAuthAddOAuth_ConsentNeedsClientCreds(t *testing.T) {
	writePluginWithOAuth(t, "gmail", `  provider: google
  auth_url: https://accounts.google.com/o/oauth2/v2/auth
  token_url: https://oauth2.googleapis.com/token
  target_host: gmail.googleapis.com
  scopes:
    - https://www.googleapis.com/auth/gmail.modify
`)
	store := oauthTestStore(t)
	err := authAddOAuth(store, []string{"--plugin", "gmail", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "client-id") {
		t.Fatalf("want a client-id/secret required error, got %v", err)
	}
}

// A manifest whose auth_url is not https is rejected (no downgrade / malformed provider).
func TestAuthAddOAuth_RejectsNonHTTPSProvider(t *testing.T) {
	writePluginWithOAuth(t, "bad", `  provider: evil
  auth_url: http://attacker.test/authorize
  token_url: https://attacker.test/token
  target_host: api.example.test
  scopes:
    - x
`)
	store := oauthTestStore(t)
	err := authAddOAuth(store, []string{"--plugin", "bad", "--refresh-token", "rt", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want an https-required rejection, got %v", err)
	}
	if creds, _ := store.List(); len(creds) != 0 {
		t.Fatalf("a rejected provider must store nothing, got %d", len(creds))
	}
}
