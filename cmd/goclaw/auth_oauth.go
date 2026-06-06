package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/shindakun/goclaw/internal/credstore"
)

// Google's well-known OAuth2 endpoints (the only provider supported in this first pass).
// atproto/Bluesky and others are deliberately out of scope here; see docs/oauth-credentials.md.
const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
)

// authAddOAuth implements `goclaw auth add-oauth`: store an OAuth2 (refreshable) credential
// via one of three consent paths, in order of preference:
//
//   - --refresh-token <rt>   you already have a refresh token (e.g. from the OAuth
//     playground or a prior consent): store it directly, no browser.
//   - default (loopback)     open the browser to Google's consent screen, catch the
//     ?code= on a short-lived 127.0.0.1 server IN THIS COMMAND (never the daemon),
//     exchange it for a refresh token, store it.
//   - --no-browser           print the consent URL, you paste the resulting code back;
//     same exchange, no local server (for headless hosts).
//
// The stored credential is kind=oauth2-bearer; the proxy refreshes and injects Bearer per
// request for the target host (proxy-inject delivery, the chosen fork B).
func authAddOAuth(store *credstore.Store, args []string) error {
	fs := flag.NewFlagSet("add-oauth", flag.ContinueOnError)
	var (
		name         = fs.String("name", "", "credential name (required), e.g. gmail")
		provider     = fs.String("provider", "google", "OAuth2 provider (only \"google\" supported)")
		targetURL    = fs.String("target-api-url", "", "API base whose host this authenticates (required), e.g. https://gmail.googleapis.com")
		clientID     = fs.String("client-id", "", "OAuth2 client id (from a GCP Desktop OAuth client)")
		clientSecret = fs.String("client-secret", "", "OAuth2 client secret")
		scopes       = fs.String("scopes", "", "space- or comma-separated OAuth2 scopes, e.g. https://www.googleapis.com/auth/gmail.readonly")
		tokenURL     = fs.String("token-url", googleTokenURL, "OAuth2 token endpoint")
		refreshToken = fs.String("refresh-token", "", "store this refresh token directly (skip the consent flow)")
		noBrowser    = fs.Bool("no-browser", false, "headless: print the consent URL and read the pasted code (no local server)")
	)
	fs.Usage = func() {
		fmt.Println("Usage: goclaw auth add-oauth --name gmail --target-api-url https://gmail.googleapis.com \\")
		fmt.Println("         --client-id <id> --client-secret <secret> --scopes <scopes> [--refresh-token <rt> | --no-browser]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !store.HasKey() {
		return fmt.Errorf("GOCLAW_SECRET_ENCRYPTION_KEY is unset or not a 32-byte base64 key.\n" +
			"Generate one with:  head -c 32 /dev/urandom | base64\n" +
			"then set it in your environment/.env before storing credentials")
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*targetURL) == "" {
		fs.Usage()
		return fmt.Errorf("--name and --target-api-url are required")
	}
	if *provider != "google" {
		return fmt.Errorf("only --provider google is supported in this version (got %q)", *provider)
	}

	scopeList := splitScopes(*scopes)

	rt := strings.TrimSpace(*refreshToken)
	if rt == "" {
		// No refresh token given: run a consent flow to obtain one. Both flows need a
		// client id/secret.
		if strings.TrimSpace(*clientID) == "" || strings.TrimSpace(*clientSecret) == "" {
			return fmt.Errorf("--client-id and --client-secret are required for the consent flow " +
				"(or pass --refresh-token to skip it)")
		}
		var err error
		if *noBrowser {
			rt, err = consentNoBrowser(*clientID, *clientSecret, *tokenURL, scopeList)
		} else {
			rt, err = consentLoopback(*clientID, *clientSecret, *tokenURL, scopeList)
		}
		if err != nil {
			return err
		}
	}

	id, err := store.AddOAuth2(credstore.OAuth2Params{
		Name:         *name,
		TargetURL:    *targetURL,
		RefreshToken: rt,
		ClientID:     *clientID,
		ClientSecret: *clientSecret,
		TokenURL:     *tokenURL,
		Scopes:       scopeList,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stored OAuth2 credential %q for %s\n", *name, *targetURL)
	fmt.Printf("  id: %s\n", id)
	fmt.Println("  The proxy will refresh and inject a Bearer token for that host per request.")
	fmt.Println("  The refresh token and access tokens never enter the agent container.")
	return nil
}

// splitScopes accepts scopes separated by spaces or commas and returns a clean list.
func splitScopes(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// buildAuthCodeURL constructs Google's Authorization Code consent URL. access_type=offline
// + prompt=consent forces a refresh_token to be issued even on a repeat authorization.
func buildAuthCodeURL(clientID, redirectURI string, scopes []string) string {
	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"scope":         {strings.Join(scopes, " ")},
	}
	return googleAuthURL + "?" + q.Encode()
}

// exchangeCode swaps an authorization code for tokens at tokenURL and returns the
// refresh_token. redirectURI must match the one used to obtain the code.
func exchangeCode(ctx context.Context, clientID, clientSecret, tokenURL, code, redirectURI string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("code exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("code exchange: token endpoint returned %s", resp.Status)
	}
	var tr struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("code exchange: decode: %w", err)
	}
	if tr.RefreshToken == "" {
		return "", fmt.Errorf("code exchange: no refresh_token in response " +
			"(Google omits it if the user already granted consent; revoke prior access or the flow forces prompt=consent)")
	}
	return tr.RefreshToken, nil
}

// authHTTPClient is overridable in tests (point at a fake token endpoint).
var authHTTPClient = &http.Client{Timeout: 30 * time.Second}

// consentTimeout bounds how long the loopback flow waits for the user to finish in the
// browser before giving up.
const consentTimeout = 5 * time.Minute

// consentLoopback runs the browser-based Authorization Code flow: bind an ephemeral
// 127.0.0.1 listener, open the browser to the consent URL with that loopback as the
// redirect, catch the ?code=, exchange it. The local server lives only for this command.
func consentLoopback(clientID, clientSecret, tokenURL string, scopes []string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("loopback listen: %w", err)
	}
	defer func() { _ = ln.Close() }()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)

	type result struct {
		code string
		err  error
	}
	resultc := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			_, _ = fmt.Fprintf(w, "Authorization failed: %s. You can close this tab.", e)
			resultc <- result{err: fmt.Errorf("consent denied: %s", e)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintln(w, "Authorization complete. You can close this tab and return to the terminal.")
		resultc <- result{code: code}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	authURL := buildAuthCodeURL(clientID, redirectURI, scopes)
	fmt.Println("Opening your browser to authorize. If it does not open, visit:")
	fmt.Println("  " + authURL)
	_ = openBrowser(authURL)

	select {
	case res := <-resultc:
		if res.err != nil {
			return "", res.err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return exchangeCode(ctx, clientID, clientSecret, tokenURL, res.code, redirectURI)
	case <-time.After(consentTimeout):
		return "", fmt.Errorf("timed out waiting for browser authorization (%s)", consentTimeout)
	}
}

// consentNoBrowser is the headless fallback: print the consent URL (with the out-of-band
// redirect), the user authorizes elsewhere and pastes the resulting code back. No local
// server. Uses Google's "urn:ietf:wg:oauth:2.0:oob"-style manual redirect via the
// loopback string the user copies; here we use the OOB display redirect.
func consentNoBrowser(clientID, clientSecret, tokenURL string, scopes []string) (string, error) {
	// For a no-server flow the redirect must be one the provider will DISPLAY the code on.
	// Google deprecated the OOB urn, so the practical headless path is: use a loopback
	// redirect_uri the user can read the ?code= out of the address bar after the redirect
	// fails to connect. We surface that instruction explicitly.
	const redirectURI = "http://127.0.0.1:0/"
	authURL := buildAuthCodeURL(clientID, redirectURI, scopes)
	fmt.Println("Headless consent. On any machine with a browser, open:")
	fmt.Println("  " + authURL)
	fmt.Println("After authorizing, the browser is redirected to a 127.0.0.1 URL that will not load.")
	fmt.Println("Copy the value of the `code` query parameter from that URL and paste it here.")
	fmt.Print("code: ")

	var code string
	if _, err := fmt.Scanln(&code); err != nil {
		return "", fmt.Errorf("reading pasted code: %w", err)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", fmt.Errorf("no code pasted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return exchangeCode(ctx, clientID, clientSecret, tokenURL, code, redirectURI)
}

// openBrowser best-effort opens url in the platform browser. A failure is non-fatal: the
// URL is also printed for manual opening.
func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	args = append(args, url)
	return exec.Command(cmd, args...).Start()
}
