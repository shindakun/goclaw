// Package credstore is the encrypted credential store backing the bundled
// credential proxy (brief §8). Tokens (API keys) are stored in the central
// goclaw.db encrypted at rest with AES-256-GCM. The encryption key comes from
// the environment (GOCLAW_SECRET_ENCRYPTION_KEY), never from the data dir, so a
// stolen data dir or DB dump does not include the key.
//
// The proxy resolves an outbound request to a credential by its target HOST and
// injects the decrypted token; the plaintext token lives only in host memory for
// the moment a request is forwarded, never in the agent container.
package credstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// keyLen is the required AES-256 key length in bytes.
const keyLen = 32

// ErrNoKey is returned when an operation needs the encryption key but it is
// unset or invalid. Callers surface a clear "set GOCLAW_SECRET_ENCRYPTION_KEY"
// message.
var ErrNoKey = errors.New("credstore: GOCLAW_SECRET_ENCRYPTION_KEY is unset or not a 32-byte base64 key")

// Store reads and writes encrypted credentials in the central DB.
type Store struct {
	db  *sql.DB
	key []byte // 32 bytes, or nil if not configured
}

// Credential is one stored entry (token omitted - never returned in listings).
type Credential struct {
	ID         string
	Name       string
	TargetURL  string
	TargetHost string
	// Preview is a masked form of the token for display (e.g. "sk-a…9999").
	Preview string
}

// New builds a Store. encKeyB64 is the base64-encoded 32-byte key from
// GOCLAW_SECRET_ENCRYPTION_KEY; an empty or malformed key yields a Store that
// can still be constructed but fails write/read ops with ErrNoKey (so commands
// can give a clear error rather than panicking).
func New(db *sql.DB, encKeyB64 string) *Store {
	s := &Store{db: db}
	if encKeyB64 != "" {
		if k, err := base64.StdEncoding.DecodeString(encKeyB64); err == nil && len(k) == keyLen {
			s.key = k
		}
	}
	return s
}

// HasKey reports whether a usable encryption key is configured.
func (s *Store) HasKey() bool { return len(s.key) == keyLen }

// Add encrypts token and stores a new credential for the given name + target
// URL, returning the assigned UUID. The target host is parsed from targetURL and
// is the key the proxy matches on; adding a second credential for the same host
// replaces the first (the host has a unique index).
func (s *Store) Add(name, targetURL, token string) (string, error) {
	if !s.HasKey() {
		return "", ErrNoKey
	}
	host, err := hostOf(targetURL)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("credstore: name is required")
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("credstore: token is required")
	}
	ct, err := s.encrypt(token)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	// Replace any existing credential for this host (unique index on target_host).
	_, err = s.db.Exec(`
		INSERT INTO credentials (id, name, target_url, target_host, token_ciphertext)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(target_host) DO UPDATE SET
			id = excluded.id, name = excluded.name,
			target_url = excluded.target_url, token_ciphertext = excluded.token_ciphertext,
			created_at = datetime('now')`,
		id, name, targetURL, host, ct)
	if err != nil {
		return "", fmt.Errorf("credstore: add: %w", err)
	}
	return id, nil
}

// List returns all stored credentials with masked token previews, ordered by
// name. Does not decrypt full tokens for display beyond the preview.
func (s *Store) List() ([]Credential, error) {
	rows, err := s.db.Query(`
		SELECT id, name, target_url, target_host, token_ciphertext
		FROM credentials ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("credstore: list: %w", err)
	}
	defer rows.Close()

	var out []Credential
	for rows.Next() {
		var c Credential
		var ct string
		if err := rows.Scan(&c.ID, &c.Name, &c.TargetURL, &c.TargetHost, &ct); err != nil {
			return nil, err
		}
		// Best-effort preview; if decryption fails (wrong key), show a marker
		// rather than failing the whole listing.
		if tok, derr := s.decrypt(ct); derr == nil {
			c.Preview = preview(tok)
		} else {
			c.Preview = "<undecryptable: wrong key?>"
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Delete removes the credential with the given id. Returns false if no row
// matched (so the caller can report "no such id").
func (s *Store) Delete(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM credentials WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("credstore: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ResolveByHost returns the decrypted token for the credential whose target host
// matches host, and the target URL to forward to. ok is false when there is no
// credential for that host. Used by the proxy on each request.
func (s *Store) ResolveByHost(host string) (token, targetURL string, ok bool, err error) {
	if !s.HasKey() {
		return "", "", false, ErrNoKey
	}
	var ct string
	row := s.db.QueryRow(`SELECT token_ciphertext, target_url FROM credentials WHERE target_host = ?`, host)
	if err := row.Scan(&ct, &targetURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("credstore: resolve: %w", err)
	}
	token, err = s.decrypt(ct)
	if err != nil {
		return "", "", false, err
	}
	return token, targetURL, true, nil
}

// Hosts returns the set of target hosts that have a stored credential, so the
// host can decide whether to run the credential proxy and which placeholders to
// set on a runner (e.g. a GH_TOKEN placeholder when a github.com credential
// exists).
func (s *Store) Hosts() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT target_host FROM credentials`)
	if err != nil {
		return nil, fmt.Errorf("credstore: hosts: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// encrypt seals plaintext with AES-256-GCM, returning "iv:tag:ciphertext" in
// base64 (the tag is appended to the ciphertext by Seal, so the stored form is
// iv:sealed).
func (s *Store) encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(iv) + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// decrypt reverses encrypt.
func (s *Store) decrypt(stored string) (string, error) {
	ivB64, sealedB64, ok := strings.Cut(stored, ":")
	if !ok {
		return "", fmt.Errorf("credstore: malformed ciphertext")
	}
	iv, err := base64.StdEncoding.DecodeString(ivB64)
	if err != nil {
		return "", err
	}
	sealed, err := base64.StdEncoding.DecodeString(sealedB64)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("credstore: decrypt (wrong key or tampered): %w", err)
	}
	return string(pt), nil
}

// hostOf parses the host out of a target URL, requiring an absolute http(s) URL.
func hostOf(targetURL string) (string, error) {
	u, err := url.Parse(targetURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("credstore: target-api-url must be an absolute http(s) URL, got %q", targetURL)
	}
	return u.Host, nil
}

// preview masks a token for display, keeping a few leading and trailing chars.
func preview(tok string) string {
	if len(tok) <= 8 {
		return strings.Repeat("•", len(tok))
	}
	return tok[:4] + "…" + tok[len(tok)-4:]
}
