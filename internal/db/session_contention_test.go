package db

import (
	"strings"
	"sync"
	"testing"
)

// TestSession_ConcurrentOpensNoBusyError reproduces the production race: the router
// (enqueuing inbound) and the delivery loop (opening the session to write the delivery
// ledger) both OpenSession the SAME inbound.db at once. Before the fix, the open ran
// `PRAGMA journal_mode = DELETE` (a header write) BEFORE busy_timeout was in effect and
// before the pool was capped to one connection, so a concurrent open failed with
// "database is locked (SQLITE_BUSY)". With busy_timeout set first on a single-connection
// pool, concurrent opens + writes wait the lock out instead of erroring.
func TestSession_ConcurrentOpensNoBusyError(t *testing.T) {
	base := t.TempDir()
	const (
		groupID = 1
		key     = "telegram:6306189728"
	)
	// Create the session dir + inbound.db once up front.
	if s, err := OpenSession(base, groupID, key); err != nil {
		t.Fatalf("initial open: %v", err)
	} else {
		_ = s.Close()
	}

	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // line everyone up so the opens actually contend
			s, err := OpenSession(base, groupID, key)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = s.Close() }()
			// Do a header-touching write (the ledger) so the connection actually
			// contends for the write lock, the thing that failed before.
			if err := s.MarkDelivered(int64(i + 1)); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			if strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "SQLITE_BUSY") {
				t.Fatalf("concurrent session open hit SQLITE_BUSY (the bug): %v", err)
			}
			t.Fatalf("concurrent session open errored: %v", err)
		}
	}
}
