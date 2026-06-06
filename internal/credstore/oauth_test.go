package credstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTokenEndpoint is a stand-in OAuth2 token endpoint. It counts refresh calls, can be
// told to rotate the refresh token, and records the refresh_token it last received (so a
// test can assert rotation persisted).
type fakeTokenEndpoint struct {
	srv         *httptest.Server
	calls       atomic.Int64
	mu          sync.Mutex
	lastRefresh string // the refresh_token the client last sent
	rotateTo    string // if set, return this as a new refresh_token
	expiresIn   int64
}

func newFakeTokenEndpoint(t *testing.T) *fakeTokenEndpoint {
	t.Helper()
	f := &fakeTokenEndpoint{expiresIn: 3600}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		_ = r.ParseForm()
		f.mu.Lock()
		f.lastRefresh = r.Form.Get("refresh_token")
		rotate := f.rotateTo
		exp := f.expiresIn
		f.mu.Unlock()

		resp := map[string]any{
			"access_token": "access-" + itoa(f.calls.Load()),
			"token_type":   "Bearer",
			"expires_in":   exp,
		}
		if rotate != "" {
			resp["refresh_token"] = rotate
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func itoa(n int64) string {
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

// withFakeOAuthClient points the package's oauth HTTP client at the fake endpoint and
// restores it after the test.
func withFakeOAuthClient(t *testing.T, f *fakeTokenEndpoint) {
	t.Helper()
	old := oauthHTTPClient
	oauthHTTPClient = f.srv.Client()
	t.Cleanup(func() { oauthHTTPClient = old })
}

func addTestOAuth(t *testing.T, s *Store, tokenURL string) {
	t.Helper()
	_, err := s.AddOAuth2(OAuth2Params{
		Name:         "gmail",
		TargetURL:    "https://gmail.googleapis.com",
		RefreshToken: "rt-original",
		ClientID:     "cid",
		ClientSecret: "csec",
		TokenURL:     tokenURL,
	})
	if err != nil {
		t.Fatalf("AddOAuth2: %v", err)
	}
}

func TestAccessToken_MintsAndCaches(t *testing.T) {
	s := testStore(t, testKey(t))
	f := newFakeTokenEndpoint(t)
	withFakeOAuthClient(t, f)

	addTestOAuth(t, s, f.srv.URL)
	ctx := context.Background()

	// First call: no cached token -> one refresh.
	tok, ok, err := s.AccessToken(ctx, "gmail.googleapis.com")
	if err != nil || !ok {
		t.Fatalf("AccessToken: ok=%v err=%v", ok, err)
	}
	if tok != "access-1" {
		t.Fatalf("first token = %q, want access-1", tok)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", f.calls.Load())
	}

	// Second call: cached, not expired -> NO new refresh.
	tok2, _, _ := s.AccessToken(ctx, "gmail.googleapis.com")
	if tok2 != "access-1" {
		t.Fatalf("cached token = %q, want access-1 (no refresh)", tok2)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("refresh calls = %d after cached read, want still 1", f.calls.Load())
	}
}

func TestAccessToken_RefreshesWhenExpired(t *testing.T) {
	s := testStore(t, testKey(t))
	f := newFakeTokenEndpoint(t)
	withFakeOAuthClient(t, f)
	addTestOAuth(t, s, f.srv.URL)
	ctx := context.Background()

	// Pin a clock we can advance. Mint once.
	base := time.Now()
	old := oauthNow
	oauthNow = func() time.Time { return base }
	t.Cleanup(func() { oauthNow = old })

	if _, _, err := s.AccessToken(ctx, "gmail.googleapis.com"); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", f.calls.Load())
	}

	// Advance past expiry (3600s + skew). Next call must refresh again.
	oauthNow = func() time.Time { return base.Add(2 * time.Hour) }
	tok, _, err := s.AccessToken(ctx, "gmail.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "access-2" || f.calls.Load() != 2 {
		t.Fatalf("after expiry: token=%q calls=%d, want access-2 / 2", tok, f.calls.Load())
	}
}

func TestAccessToken_SingleFlight(t *testing.T) {
	s := testStore(t, testKey(t))
	f := newFakeTokenEndpoint(t)
	withFakeOAuthClient(t, f)
	addTestOAuth(t, s, f.srv.URL)
	ctx := context.Background()

	// 20 concurrent first-calls (all see no cached token) must collapse to ONE refresh.
	const n = 20
	var wg sync.WaitGroup
	toks := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, _, err := s.AccessToken(ctx, "gmail.googleapis.com")
			if err == nil {
				toks[i] = tok
			}
		}(i)
	}
	wg.Wait()

	if got := f.calls.Load(); got != 1 {
		t.Fatalf("concurrent refresh stampede: %d refreshes, want 1 (single-flight)", got)
	}
	for i, tok := range toks {
		if tok != "access-1" {
			t.Fatalf("caller %d got %q, want all access-1", i, tok)
		}
	}
}

func TestAccessToken_PersistsRotatedRefreshToken(t *testing.T) {
	s := testStore(t, testKey(t))
	f := newFakeTokenEndpoint(t)
	withFakeOAuthClient(t, f)
	addTestOAuth(t, s, f.srv.URL)
	ctx := context.Background()

	base := time.Now()
	oldNow := oauthNow
	oauthNow = func() time.Time { return base }
	t.Cleanup(func() { oauthNow = oldNow })

	// First refresh ROTATES the refresh token.
	f.mu.Lock()
	f.rotateTo = "rt-rotated"
	f.mu.Unlock()

	if _, _, err := s.AccessToken(ctx, "gmail.googleapis.com"); err != nil {
		t.Fatal(err)
	}
	// The endpoint received the ORIGINAL refresh token on the first call.
	f.mu.Lock()
	if f.lastRefresh != "rt-original" {
		f.mu.Unlock()
		t.Fatalf("first refresh used %q, want rt-original", f.lastRefresh)
	}
	f.rotateTo = "" // stop rotating
	f.mu.Unlock()

	// Force a second refresh (advance past expiry). It must send the ROTATED token,
	// proving the rotation was persisted (not the stale original).
	oauthNow = func() time.Time { return base.Add(2 * time.Hour) }
	if _, _, err := s.AccessToken(ctx, "gmail.googleapis.com"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.lastRefresh
	f.mu.Unlock()
	if got != "rt-rotated" {
		t.Fatalf("second refresh used %q, want rt-rotated (rotation not persisted)", got)
	}
}

// A static credential still resolves via ResolveByHost; AccessToken on a static host errors
// (wrong kind), and AccessToken on an absent host reports ok=false.
func TestAccessToken_KindBoundaries(t *testing.T) {
	s := testStore(t, testKey(t))
	if _, err := s.Add("anthropic", "https://api.anthropic.com", "sk-ant-xyz"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, _, err := s.AccessToken(ctx, "api.anthropic.com"); err == nil {
		t.Fatal("AccessToken on a static credential should error (wrong kind)")
	}
	if _, ok, err := s.AccessToken(ctx, "nope.example.com"); ok || err != nil {
		t.Fatalf("AccessToken on absent host: ok=%v err=%v, want false/nil", ok, err)
	}
	// And the static one still resolves the old way.
	tok, _, ok, err := s.ResolveByHost("api.anthropic.com")
	if err != nil || !ok || tok != "sk-ant-xyz" {
		t.Fatalf("static ResolveByHost broken: %q ok=%v err=%v", tok, ok, err)
	}
}
