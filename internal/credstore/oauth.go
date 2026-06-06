package credstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

// oauth2Bundle is the PLAINTEXT shape stored (encrypted) in token_ciphertext for a
// kind=oauth2-bearer credential. credstore treats the column as opaque bytes; this is
// what the bytes decode to. It is read-WRITE: the cached access token and (on rotation)
// the refresh token change as the credential is used, and the mutated bundle is
// re-encrypted and written back. See docs/oauth-credentials.md.
type oauth2Bundle struct {
	RefreshToken string   `json:"refresh_token"`          // the long-lived secret
	ClientID     string   `json:"client_id"`              //
	ClientSecret string   `json:"client_secret"`          //
	TokenURL     string   `json:"token_url"`              // e.g. https://oauth2.googleapis.com/token
	Scopes       []string `json:"scopes,omitempty"`       //
	AccessToken  string   `json:"access_token,omitempty"` // cached, minted from refresh
	ExpiresAt    int64    `json:"expires_at,omitempty"`   // unix seconds; 0 = none cached
}

// OAuth2Params is what `goclaw auth add-oauth` collects to store an oauth2-bearer
// credential. The refresh token comes from a consent flow (or is provided directly).
type OAuth2Params struct {
	Name         string
	TargetURL    string // the API base whose HOST this credential authenticates (match key)
	RefreshToken string
	ClientID     string
	ClientSecret string
	TokenURL     string
	Scopes       []string
}

// refreshSkew is how long before expiry we proactively refresh, so a token never expires
// mid-request.
const refreshSkew = 60 * time.Second

// httpClient and now are package-overridable for tests (a fake token endpoint, a pinned
// clock). Production uses a real client with a timeout and time.Now.
var (
	oauthHTTPClient = &http.Client{Timeout: 15 * time.Second}
	oauthNow        = time.Now
)

// refreshGroup single-flights refresh per credential host: concurrent AccessToken callers
// that find the token expired trigger ONE refresh and share its result, so we never
// stampede the token endpoint (and never burn a rotating refresh token N times at once).
var refreshGroup singleflight.Group

// AddOAuth2 stores an oauth2-bearer credential (host parsed from TargetURL, the match key,
// one credential per host as for static). The bundle is encrypted; no access token is
// cached yet (the first AccessToken call mints one). Returns the assigned UUID.
func (s *Store) AddOAuth2(p OAuth2Params) (string, error) {
	if !s.HasKey() {
		return "", ErrNoKey
	}
	host, err := hostOf(p.TargetURL)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Name) == "" {
		return "", fmt.Errorf("credstore: name is required")
	}
	if strings.TrimSpace(p.RefreshToken) == "" || strings.TrimSpace(p.TokenURL) == "" {
		return "", fmt.Errorf("credstore: oauth2 needs a refresh_token and token_url")
	}
	if _, err := url.Parse(p.TokenURL); err != nil {
		return "", fmt.Errorf("credstore: bad token_url %q: %w", p.TokenURL, err)
	}
	bundle := oauth2Bundle{
		RefreshToken: p.RefreshToken,
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		TokenURL:     p.TokenURL,
		Scopes:       p.Scopes,
	}
	ct, err := s.encryptBundle(bundle)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	_, err = s.db.Exec(`
		INSERT INTO credentials (id, name, target_url, target_host, token_ciphertext, kind)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(target_host) DO UPDATE SET
			id = excluded.id, name = excluded.name,
			target_url = excluded.target_url, token_ciphertext = excluded.token_ciphertext,
			kind = excluded.kind, created_at = datetime('now')`,
		id, p.Name, p.TargetURL, host, ct, KindOAuth2Bearer)
	if err != nil {
		return "", fmt.Errorf("credstore: add oauth2: %w", err)
	}
	return id, nil
}

// AccessToken returns a CURRENT bearer access token for the oauth2-bearer credential
// matching host, refreshing from the stored refresh token if the cached token is missing
// or near expiry. Refresh is single-flight per host. ok is false if there is no
// credential for the host; an error is returned only for a credential that exists but
// could not be resolved/refreshed.
//
// For a STATIC credential, callers use ResolveByHost; this is the oauth path. (A future
// BearerForHost could dispatch on kind, but keeping them separate is clearer for now.)
func (s *Store) AccessToken(ctx context.Context, host string) (token string, ok bool, err error) {
	if !s.HasKey() {
		return "", false, ErrNoKey
	}
	bundle, found, err := s.loadOAuth2(host)
	if err != nil || !found {
		return "", found, err
	}
	// Fast path: a cached token with comfortable life left needs no refresh and no lock.
	if bundle.AccessToken != "" && bundle.ExpiresAt > 0 &&
		oauthNow().Add(refreshSkew).Unix() < bundle.ExpiresAt {
		return bundle.AccessToken, true, nil
	}
	// Slow path: refresh, single-flight per host so concurrent callers share one refresh.
	v, ferr, _ := refreshGroup.Do(host, func() (any, error) {
		// Re-load inside the flight: another caller may have just refreshed.
		b, ok2, lerr := s.loadOAuth2(host)
		if lerr != nil || !ok2 {
			return "", lerr
		}
		if b.AccessToken != "" && b.ExpiresAt > 0 && oauthNow().Add(refreshSkew).Unix() < b.ExpiresAt {
			return b.AccessToken, nil
		}
		return s.refreshOAuth2(ctx, host, b)
	})
	if ferr != nil {
		return "", true, ferr
	}
	return v.(string), true, nil
}

// refreshOAuth2 POSTs the refresh grant, persists the new access token (+ rotated refresh
// token, if the provider returned one), and returns the access token. Caller holds the
// single-flight slot for host.
func (s *Store) refreshOAuth2(ctx context.Context, host string, b oauth2Bundle) (string, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {b.RefreshToken},
	}
	if b.ClientID != "" {
		form.Set("client_id", b.ClientID)
	}
	if b.ClientSecret != "" {
		form.Set("client_secret", b.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("credstore: oauth2 refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("credstore: oauth2 refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("credstore: oauth2 refresh: token endpoint returned %s", resp.Status)
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int64  `json:"expires_in"`
		RefreshToken string `json:"refresh_token"` // rotation: present iff the provider rotated it
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("credstore: oauth2 refresh: decode response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("credstore: oauth2 refresh: response had no access_token")
	}

	b.AccessToken = tr.AccessToken
	if tr.ExpiresIn > 0 {
		b.ExpiresAt = oauthNow().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	} else {
		b.ExpiresAt = 0 // unknown expiry: treat as always-needs-refresh next time
	}
	// ROTATION: if the provider returned a new refresh token, the old one may now be
	// invalid, so persist the new one or the next refresh fails.
	if tr.RefreshToken != "" {
		b.RefreshToken = tr.RefreshToken
	}
	if err := s.storeOAuth2(host, b); err != nil {
		// We have a usable access token but failed to persist; return the token so this
		// request succeeds, but the next refresh will redo work (or, if rotation was
		// dropped, fail). Surface as an error so it is visible.
		return b.AccessToken, fmt.Errorf("credstore: oauth2 refresh: persist updated bundle: %w", err)
	}
	return b.AccessToken, nil
}

// loadOAuth2 reads and decrypts the oauth2 bundle for host. found=false when there is no
// credential for the host; an error if a row exists but is not a usable oauth2 bundle.
func (s *Store) loadOAuth2(host string) (oauth2Bundle, bool, error) {
	var ct, kind string
	row := s.db.QueryRow(`SELECT token_ciphertext, kind FROM credentials WHERE target_host = ?`, host)
	if err := row.Scan(&ct, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return oauth2Bundle{}, false, nil
		}
		return oauth2Bundle{}, false, fmt.Errorf("credstore: load oauth2: %w", err)
	}
	if kind != KindOAuth2Bearer {
		return oauth2Bundle{}, true, fmt.Errorf("credstore: credential for %q is kind %q, not %s", host, kind, KindOAuth2Bearer)
	}
	pt, err := s.decrypt(ct)
	if err != nil {
		return oauth2Bundle{}, true, err
	}
	var b oauth2Bundle
	if err := json.Unmarshal([]byte(pt), &b); err != nil {
		return oauth2Bundle{}, true, fmt.Errorf("credstore: oauth2 bundle parse: %w", err)
	}
	return b, true, nil
}

// storeOAuth2 re-encrypts and writes the (mutated) bundle back for host, without changing
// id/name/created_at.
func (s *Store) storeOAuth2(host string, b oauth2Bundle) error {
	ct, err := s.encryptBundle(b)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE credentials SET token_ciphertext = ? WHERE target_host = ? AND kind = ?`,
		ct, host, KindOAuth2Bearer)
	if err != nil {
		return fmt.Errorf("credstore: store oauth2: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("credstore: store oauth2: no oauth2 credential for %q", host)
	}
	return nil
}

func (s *Store) encryptBundle(b oauth2Bundle) (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("credstore: marshal oauth2 bundle: %w", err)
	}
	return s.encrypt(string(raw))
}
