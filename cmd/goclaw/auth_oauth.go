package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shindakun/goclaw/internal/config"
	"github.com/shindakun/goclaw/internal/credstore"
	"github.com/shindakun/goclaw/internal/plugin"
)

// defaultRedirectPort is the loopback callback port used by the consent flow when
// --redirect-port is not given. It is FIXED (not an ephemeral :0) so the redirect_uri is
// stable and can be registered in the provider's OAuth app. Providers like Spotify match
// redirect_uri exactly (incl. port), so a random port could never match. A high port needs
// no root. Register http://127.0.0.1:<port>/ (default below) as a redirect URI.
const defaultRedirectPort = 8888

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
//
// The provider FACTS (auth/token URLs, scopes, refresh-forcing params, scope separator,
// client-auth style, target host) come from the named plugin's `oauth` block in its
// plugin.yml: goclaw owns the mechanism, the plugin owns the provider data, so a new OAuth
// service ships as a plugin with no change here. Flags override individual fields for edge
// cases (self-hosted instances, extra scopes). The operator always supplies the client
// id/secret (never the manifest).
func authAddOAuth(store *credstore.Store, args []string) error {
	fs := flag.NewFlagSet("add-oauth", flag.ContinueOnError)
	var (
		pluginName   = fs.String("plugin", "", "installed plugin whose oauth block to use (required), e.g. gmail")
		name         = fs.String("name", "", "credential name (defaults to the plugin name)")
		clientID     = fs.String("client-id", "", "OAuth2 client id (from the provider's OAuth app)")
		clientSecret = fs.String("client-secret", "", "OAuth2 client secret")
		refreshToken = fs.String("refresh-token", "", "store this refresh token directly (skip the consent flow)")
		noBrowser    = fs.Bool("no-browser", false, "headless: print the consent URL and read the pasted code (no local server)")
		yes          = fs.Bool("yes", false, "skip the confirmation prompt (non-interactive)")
		// redirectPort fixes the loopback callback port so the redirect_uri is STABLE and
		// registerable. Some providers (Spotify) match redirect_uri EXACTLY incl. port, so a
		// random port can never match a registered value. Default to a fixed high port (no
		// root needed). Register http://127.0.0.1:<port>/ in the provider's OAuth app.
		redirectPort = fs.Int("redirect-port", defaultRedirectPort, "fixed loopback port for the consent callback; register http://127.0.0.1:<port>/ in the provider's app")
		// Overrides for the plugin's declared oauth block (rarely needed).
		authURLF   = fs.String("auth-url", "", "override the plugin's consent endpoint")
		tokenURLF  = fs.String("token-url", "", "override the plugin's token endpoint")
		targetURLF = fs.String("target-api-url", "", "override: API base whose host this authenticates")
		scopesF    = fs.String("scopes", "", "override: space/comma-separated OAuth2 scopes")
	)
	fs.Usage = func() {
		fmt.Println("Usage: goclaw auth add-oauth --plugin <name> --client-id <id> --client-secret <secret> \\")
		fmt.Println("         [--refresh-token <rt> | --no-browser] [--scopes <override>] [--auth-url/--token-url/--target-api-url <override>]")
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
	if strings.TrimSpace(*pluginName) == "" {
		fs.Usage()
		return fmt.Errorf("--plugin is required (the plugin whose oauth block declares the provider)")
	}

	// Load the plugin's declared oauth block, then apply any flag overrides.
	spec, err := loadPluginOAuth(*pluginName)
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(*authURLF); v != "" {
		spec.AuthURL = v
	}
	if v := strings.TrimSpace(*tokenURLF); v != "" {
		spec.TokenURL = v
	}
	if v := strings.TrimSpace(*scopesF); v != "" {
		spec.Scopes = splitScopes(v)
	}
	targetURL := "https://" + spec.TargetHost
	if v := strings.TrimSpace(*targetURLF); v != "" {
		targetURL = v
	}
	credName := strings.TrimSpace(*name)
	if credName == "" {
		credName = *pluginName
	}

	if err := validateOAuthSpec(spec, targetURL); err != nil {
		return err
	}

	// Print + confirm: the auth/token URLs come from the plugin, so show exactly where the
	// consent and token exchange will go before doing anything. A malicious plugin pointing
	// auth_url at an attacker is visible here.
	fmt.Println("About to authorize an OAuth2 credential using this plugin's declared provider:")
	fmt.Printf("  plugin:    %s\n", *pluginName)
	fmt.Printf("  provider:  %s\n", orDash(spec.Provider))
	fmt.Printf("  auth URL:  %s\n", spec.AuthURL)
	fmt.Printf("  token URL: %s\n", spec.TokenURL)
	fmt.Printf("  target:    %s\n", targetURL)
	fmt.Printf("  scopes:    %s\n", strings.Join(spec.Scopes, " "))
	if !*yes && strings.TrimSpace(*refreshToken) == "" {
		if !confirm("Proceed with this provider?") {
			return fmt.Errorf("aborted")
		}
	}

	rt := strings.TrimSpace(*refreshToken)
	if rt == "" {
		if strings.TrimSpace(*clientID) == "" || strings.TrimSpace(*clientSecret) == "" {
			return fmt.Errorf("--client-id and --client-secret are required for the consent flow " +
				"(or pass --refresh-token to skip it)")
		}
		if *noBrowser {
			rt, err = consentNoBrowser(spec, *clientID, *clientSecret, *redirectPort)
		} else {
			rt, err = consentLoopback(spec, *clientID, *clientSecret, *redirectPort)
		}
		if err != nil {
			return err
		}
	}

	id, err := store.AddOAuth2(credstore.OAuth2Params{
		Name:         credName,
		TargetURL:    targetURL,
		RefreshToken: rt,
		ClientID:     *clientID,
		ClientSecret: *clientSecret,
		TokenURL:     spec.TokenURL,
		Scopes:       spec.Scopes,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stored OAuth2 credential %q for %s\n", credName, targetURL)
	fmt.Printf("  id: %s\n", id)
	fmt.Println("  The proxy will refresh and inject a Bearer token for that host per request.")
	fmt.Println("  The refresh token and access tokens never enter the agent container.")
	return nil
}

// loadPluginOAuth reads the named plugin's manifest from the host plugins dir and returns a
// copy of its oauth block. Errors if the plugin is absent or declares no oauth block.
func loadPluginOAuth(pluginName string) (plugin.OAuthSpec, error) {
	cfg, err := config.Load()
	if err != nil {
		return plugin.OAuthSpec{}, err
	}
	dir := filepath.Join(cfg.DataDir, "plugins", pluginName)
	man, err := plugin.LoadManifest(dir)
	if err != nil {
		return plugin.OAuthSpec{}, fmt.Errorf("plugin %q: %w (is it installed?)", pluginName, err)
	}
	if man.OAuth == nil {
		return plugin.OAuthSpec{}, fmt.Errorf("plugin %q declares no oauth block in its plugin.yml", pluginName)
	}
	return *man.OAuth, nil
}

// validateOAuthSpec fails closed on a manifest that cannot drive a safe flow: the endpoints
// and target must be absolute https URLs (no http, no relative), so a malformed or
// downgrade-attack manifest is rejected, not used.
func validateOAuthSpec(s plugin.OAuthSpec, targetURL string) error {
	for label, raw := range map[string]string{"auth_url": s.AuthURL, "token_url": s.TokenURL, "target": targetURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("oauth %s must be an absolute https URL, got %q", label, raw)
		}
	}
	if len(s.Scopes) == 0 {
		return fmt.Errorf("oauth block declares no scopes (and none given via --scopes)")
	}
	return nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// confirm prints prompt and reads a y/N answer from stdin (default No).
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	var ans string
	_, _ = fmt.Scanln(&ans)
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
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

// scopeSep returns the scope separator for the spec, defaulting to a single space.
func scopeSep(s plugin.OAuthSpec) string {
	if s.ScopeSeparator != "" {
		return s.ScopeSeparator
	}
	return " "
}

// buildAuthCodeURL constructs the provider's Authorization Code consent URL from the
// plugin's oauth block: its auth_url, the requested scopes (joined per the provider's
// separator), and any provider-specific auth_params (e.g. Google's access_type=offline +
// prompt=consent, which force a refresh token; Microsoft uses an offline_access scope
// instead and declares no params). Nothing here is Google-specific.
func buildAuthCodeURL(spec plugin.OAuthSpec, clientID, redirectURI string) string {
	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(spec.Scopes, scopeSep(spec))},
	}
	for k, v := range spec.AuthParams {
		q.Set(k, v)
	}
	return spec.AuthURL + "?" + q.Encode()
}

// exchangeCode swaps an authorization code for tokens at the spec's token_url and returns
// the refresh_token. Client credentials go in the body or an HTTP Basic header per the
// spec's client_auth ("body" default, "basic" for Spotify/Reddit-style providers).
// redirectURI must match the one used to obtain the code.
func exchangeCode(ctx context.Context, spec plugin.OAuthSpec, clientID, clientSecret, code, redirectURI string) (string, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	useBasic := strings.EqualFold(spec.ClientAuth, "basic")
	if !useBasic {
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if useBasic {
		basic := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
		req.Header.Set("Authorization", "Basic "+basic)
	}

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
			"(some providers omit it if access was already granted; revoke prior access, or the provider's auth_params/offline scope must force one)")
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
func consentLoopback(spec plugin.OAuthSpec, clientID, clientSecret string, port int) (string, error) {
	// Bind the FIXED port so the redirect_uri is stable and registerable (see
	// defaultRedirectPort). A provider that matches redirect_uri exactly (Spotify) requires
	// this; one that ignores the port (Google) does not care.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", fmt.Errorf("loopback listen on 127.0.0.1:%d (in use? pick another with --redirect-port): %w", port, err)
	}
	defer func() { _ = ln.Close() }()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/", port)
	fmt.Printf("Using redirect URI %s (register this EXACTLY in the provider's OAuth app).\n", redirectURI)

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

	authURL := buildAuthCodeURL(spec, clientID, redirectURI)
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
		return exchangeCode(ctx, spec, clientID, clientSecret, res.code, redirectURI)
	case <-time.After(consentTimeout):
		return "", fmt.Errorf("timed out waiting for browser authorization (%s)", consentTimeout)
	}
}

// consentNoBrowser is the headless fallback: print the consent URL (with the out-of-band
// redirect), the user authorizes elsewhere and pastes the resulting code back. No local
// server. Uses Google's "urn:ietf:wg:oauth:2.0:oob"-style manual redirect via the
// loopback string the user copies; here we use the OOB display redirect.
func consentNoBrowser(spec plugin.OAuthSpec, clientID, clientSecret string, port int) (string, error) {
	// For a no-server flow the redirect must be one the provider will DISPLAY the code on.
	// Google deprecated the OOB urn, so the practical headless path is: use a loopback
	// redirect_uri the user can read the ?code= out of the address bar after the redirect
	// fails to connect. Use the SAME fixed port as the loopback flow so the redirect_uri is
	// stable and matches what is registered (strict providers like Spotify require this).
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/", port)
	authURL := buildAuthCodeURL(spec, clientID, redirectURI)
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
	return exchangeCode(ctx, spec, clientID, clientSecret, code, redirectURI)
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
