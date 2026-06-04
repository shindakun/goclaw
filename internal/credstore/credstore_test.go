package credstore

import (
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/shindakun/goclaw/internal/db"
)

func testKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, keyLen)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func testStore(t *testing.T, encKey string) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "central.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d.DB, encKey)
}

func TestAddListResolveDelete(t *testing.T) {
	s := testStore(t, testKey(t))

	id, err := s.Add("anthropic", "https://api.anthropic.com", "sk-ant-secret-1234")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if id == "" {
		t.Fatal("expected a uuid")
	}

	// List shows a masked preview, never the full token.
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	c := list[0]
	if c.Name != "anthropic" || c.TargetHost != "api.anthropic.com" {
		t.Fatalf("unexpected entry: %+v", c)
	}
	if c.Preview == "sk-ant-secret-1234" || c.Preview == "" {
		t.Fatalf("preview should be masked, got %q", c.Preview)
	}

	// Resolve by host returns the real token + target.
	tok, target, ok, err := s.ResolveByHost("api.anthropic.com")
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if tok != "sk-ant-secret-1234" {
		t.Fatalf("decrypted token mismatch: %q", tok)
	}
	if target != "https://api.anthropic.com" {
		t.Fatalf("target mismatch: %q", target)
	}

	// Unknown host -> not ok, no error.
	if _, _, ok, err := s.ResolveByHost("example.com"); ok || err != nil {
		t.Fatalf("unknown host should be !ok: ok=%v err=%v", ok, err)
	}

	// Delete by id.
	gone, err := s.Delete(id)
	if err != nil || !gone {
		t.Fatalf("delete: gone=%v err=%v", gone, err)
	}
	if _, _, ok, _ := s.ResolveByHost("api.anthropic.com"); ok {
		t.Fatal("resolve after delete should be !ok")
	}
}

func TestAddReplacesSameHost(t *testing.T) {
	s := testStore(t, testKey(t))
	if _, err := s.Add("anthropic", "https://api.anthropic.com", "old-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("anthropic-new", "https://api.anthropic.com", "new-token"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("same host should replace, got %d entries", len(list))
	}
	tok, _, _, _ := s.ResolveByHost("api.anthropic.com")
	if tok != "new-token" {
		t.Fatalf("expected the new token, got %q", tok)
	}
}

func TestNoKey(t *testing.T) {
	s := testStore(t, "") // no/blank key
	if s.HasKey() {
		t.Fatal("HasKey should be false without a key")
	}
	if _, err := s.Add("x", "https://api.anthropic.com", "t"); err != ErrNoKey {
		t.Fatalf("Add without key should return ErrNoKey, got %v", err)
	}
	if _, _, _, err := s.ResolveByHost("api.anthropic.com"); err != ErrNoKey {
		t.Fatalf("Resolve without key should return ErrNoKey, got %v", err)
	}
}

func TestWrongKeyCannotDecrypt(t *testing.T) {
	enc := testKey(t)
	s := testStore(t, enc)
	if _, err := s.Add("anthropic", "https://api.anthropic.com", "secret"); err != nil {
		t.Fatal(err)
	}
	// Reopen the SAME db rows through a store with a DIFFERENT key.
	// (Simulate by constructing a new store over the same db with another key.)
	other := New(s.db, testKey(t))
	if _, _, _, err := other.ResolveByHost("api.anthropic.com"); err == nil {
		t.Fatal("decrypt with the wrong key must fail")
	}
}

func TestBadTargetURL(t *testing.T) {
	s := testStore(t, testKey(t))
	for _, bad := range []string{"api.anthropic.com", "ftp://x", "", "not a url"} {
		if _, err := s.Add("n", bad, "t"); err == nil {
			t.Errorf("expected error for bad target url %q", bad)
		}
	}
}

func TestEncryptRoundTripUniqueIV(t *testing.T) {
	s := testStore(t, testKey(t))
	a, _ := s.encrypt("same-plaintext")
	b, _ := s.encrypt("same-plaintext")
	if a == b {
		t.Fatal("ciphertext should differ per call (random IV)")
	}
	pt, err := s.decrypt(a)
	if err != nil || pt != "same-plaintext" {
		t.Fatalf("roundtrip: %q err=%v", pt, err)
	}
}
