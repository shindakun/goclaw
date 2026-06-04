package vaultlock

import (
	"path/filepath"
	"testing"
)

func TestAcquireRelease(t *testing.T) {
	lp := filepath.Join(t.TempDir(), "vault.lock")
	l, err := Acquire(lp)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// After release the lock is free, so a second acquire must succeed.
	l2, err := Acquire(lp)
	if err != nil {
		t.Fatalf("re-Acquire after release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

func TestTryAcquire_FreeLock(t *testing.T) {
	lp := filepath.Join(t.TempDir(), "vault.lock")
	l, ok, err := TryAcquire(lp)
	if err != nil || !ok {
		t.Fatalf("TryAcquire on free lock: ok=%v err=%v", ok, err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// While one handle holds the lock, a non-blocking TryAcquire from another handle
// on the same path must report contention (ok=false), not error or deadlock.
func TestTryAcquire_HeldLockContends(t *testing.T) {
	lp := filepath.Join(t.TempDir(), "vault.lock")
	held, err := Acquire(lp)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	l, ok, err := TryAcquire(lp)
	if err != nil {
		t.Fatalf("TryAcquire while held: unexpected err %v", err)
	}
	if ok {
		if l != nil {
			_ = l.Release()
		}
		t.Fatal("TryAcquire returned ok=true while lock was held")
	}
	if l != nil {
		t.Fatal("TryAcquire returned a non-nil lock on contention")
	}

	// Once the holder releases, TryAcquire must succeed.
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, ok, err := TryAcquire(lp)
	if err != nil || !ok {
		t.Fatalf("TryAcquire after release: ok=%v err=%v", ok, err)
	}
	_ = l2.Release()
}

func TestRelease_NilSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Fatalf("nil Release should be a no-op, got %v", err)
	}
	if err := (&Lock{}).Release(); err != nil {
		t.Fatalf("empty Lock Release should be a no-op, got %v", err)
	}
}

func TestAcquire_BadPath(t *testing.T) {
	// A path under a nonexistent directory cannot be opened.
	_, err := Acquire(filepath.Join(t.TempDir(), "no-such-dir", "vault.lock"))
	if err == nil {
		t.Fatal("expected error opening lock under missing dir")
	}
}
