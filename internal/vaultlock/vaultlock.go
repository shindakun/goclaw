// Package vaultlock provides a flock-based single-writer guard for the optional
// shared second-brain vault (brief §11.5). The vault has several possible
// writers — multiple sessions of a group, scheduled maintenance runs, and a
// human in Obsidian — so the host takes an exclusive flock on a lockfile before
// launching any vault-mutating agent run and releases it after. Reads never
// block; this only serializes writes to close the corruption hole.
package vaultlock

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Lock is an held advisory file lock.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive lock on lockPath, creating the file if needed.
// It blocks until the lock is available. Call Release when the vault-mutating
// run completes.
func Acquire(lockPath string) (*Lock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vaultlock: open %q: %w", lockPath, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("vaultlock: flock %q: %w", lockPath, err)
	}
	return &Lock{f: f}, nil
}

// TryAcquire attempts a non-blocking exclusive lock. It returns (nil, false, nil)
// if the lock is currently held by someone else.
func TryAcquire(lockPath string) (*Lock, bool, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("vaultlock: open %q: %w", lockPath, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("vaultlock: flock %q: %w", lockPath, err)
	}
	return &Lock{f: f}, true, nil
}

// Release unlocks and closes the lockfile.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	if err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN); err != nil {
		l.f.Close()
		return fmt.Errorf("vaultlock: unlock: %w", err)
	}
	return l.f.Close()
}
