package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/shindakun/goclaw/internal/db"
)

// A TRANSIENT handler failure (network/API/CLI down) must NOT consume the message or emit a
// reply: the inbound stays queued for retry, and processing stops at that message so order
// is preserved. This is the runner half of the "a turn that fired during an outage was lost"
// fix: a failed turn must not look like a completed one.
func TestProcessSession_TransientFailureLeavesMessageQueued(t *testing.T) {
	dir := db.SessionDir(t.TempDir(), 1, "telegram:42")
	sess, err := db.OpenSessionDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	// Two pending messages.
	if _, err := sess.EnqueueInbound("telegram", "42", "u", "user", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.EnqueueInbound("telegram", "42", "u", "user", "second"); err != nil {
		t.Fatal(err)
	}

	r := &runner{log: quietLogger()}
	// Handler: succeed on "first", fail TRANSIENTLY on "second".
	handler := func(_ context.Context, _ *db.SessionDBs, _, text string) (string, error) {
		if text == "second" {
			return "", fmt.Errorf("transient agent failure: network down")
		}
		return "reply: " + text, nil
	}

	n, err := r.processSessionWith(context.Background(), dir, handler)
	if err != nil {
		t.Fatalf("processSessionWith: %v", err)
	}
	if n != 1 {
		t.Fatalf("answered = %d, want 1 (only the first message completed)", n)
	}

	// "first" was delivered; "second" was NOT (no error reply enqueued).
	out, _ := sess.PendingOutbound()
	if len(out) != 1 || out[0].Text != "reply: first" {
		t.Fatalf("outbound = %+v, want exactly the first reply (a transient failure must not deliver an error)", out)
	}

	// "second" is STILL pending inbound (not consumed), so it is retried next pass.
	pend, _ := sess.PendingInbound()
	if len(pend) != 1 || pend[0].Text != "second" {
		t.Fatalf("pending inbound = %+v, want the un-consumed 'second' left for retry", pend)
	}
}

// A transient failure that NEVER clears must not defer forever: after
// maxTransientAttempts polls the runner gives up, delivers a visible error reply
// (which reaches the operator and stops the stuck typing indicator via delivery),
// and CONSUMES the message so it stops looping.
func TestProcessSession_GivesUpAfterMaxAttempts(t *testing.T) {
	dir := db.SessionDir(t.TempDir(), 1, "telegram:42")
	sess, err := db.OpenSessionDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if _, err := sess.EnqueueInbound("telegram", "42", "u", "user", "boom"); err != nil {
		t.Fatal(err)
	}

	r := &runner{log: quietLogger()}
	calls := 0
	handler := func(_ context.Context, _ *db.SessionDBs, _, _ string) (string, error) {
		calls++
		return "", fmt.Errorf("transient agent failure: api out of tokens")
	}

	// Simulate the poll loop: each call is one pass. Under the cap it defers (no
	// outbound, still pending); at the cap it reports + consumes.
	for pass := 1; pass <= maxTransientAttempts; pass++ {
		n, err := r.processSessionWith(context.Background(), dir, handler)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if n != 0 {
			t.Fatalf("pass %d: answered = %d, want 0 (it failed)", pass, n)
		}
		out, _ := sess.PendingOutbound()
		pend, _ := sess.PendingInbound()
		if pass < maxTransientAttempts {
			if len(out) != 0 {
				t.Fatalf("pass %d: must not deliver before the cap, got %+v", pass, out)
			}
			if len(pend) != 1 {
				t.Fatalf("pass %d: message must stay queued for retry, pending = %+v", pass, pend)
			}
		} else {
			// At the cap: an error reply is enqueued and the message is consumed.
			if len(out) != 1 || !strings.Contains(out[0].Text, "couldn't reach the model") {
				t.Fatalf("at the cap: want an error reply, got %+v", out)
			}
			if len(pend) != 0 {
				t.Fatalf("at the cap: message must be consumed, pending = %+v", pend)
			}
		}
	}
	if calls != maxTransientAttempts {
		t.Fatalf("handler called %d times, want %d", calls, maxTransientAttempts)
	}
}

// A couple of transient blips followed by success must answer normally and NOT emit
// the give-up error: the success path resets the attempt counter.
func TestProcessSession_RecoversBeforeCap(t *testing.T) {
	dir := db.SessionDir(t.TempDir(), 1, "telegram:42")
	sess, err := db.OpenSessionDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	inID, err := sess.EnqueueInbound("telegram", "42", "u", "user", "hi")
	if err != nil {
		t.Fatal(err)
	}

	r := &runner{log: quietLogger()}
	calls := 0
	handler := func(_ context.Context, _ *db.SessionDBs, _, text string) (string, error) {
		calls++
		if calls <= 2 { // fail the first two passes, then recover
			return "", fmt.Errorf("transient agent failure: blip")
		}
		return "reply: " + text, nil
	}

	for pass := 1; pass <= 3; pass++ {
		if _, err := r.processSessionWith(context.Background(), dir, handler); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	out, _ := sess.PendingOutbound()
	if len(out) != 1 || out[0].Text != "reply: hi" {
		t.Fatalf("want exactly the real reply (no give-up error), got %+v", out)
	}
	if pend, _ := sess.PendingInbound(); len(pend) != 0 {
		t.Fatalf("message should be consumed after success, pending = %+v", pend)
	}
	// The counter was cleared on success, so no stale attempts key lingers.
	if v, ok, _ := sess.GetMeta(metaAttemptsPrefix + strconv.FormatInt(inID, 10)); ok {
		t.Fatalf("attempt counter should be cleared after success, got %q", v)
	}
}

// The happy path still consumes and replies to every message.
func TestProcessSession_AllSucceed(t *testing.T) {
	dir := db.SessionDir(t.TempDir(), 1, "telegram:42")
	sess, err := db.OpenSessionDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	for _, txt := range []string{"a", "b"} {
		if _, err := sess.EnqueueInbound("telegram", "42", "u", "user", txt); err != nil {
			t.Fatal(err)
		}
	}

	r := &runner{log: quietLogger()}
	handler := func(_ context.Context, _ *db.SessionDBs, _, text string) (string, error) {
		return "ok:" + text, nil
	}
	n, err := r.processSessionWith(context.Background(), dir, handler)
	if err != nil || n != 2 {
		t.Fatalf("answered = %d err = %v, want 2/nil", n, err)
	}
	if pend, _ := sess.PendingInbound(); len(pend) != 0 {
		t.Fatalf("all messages should be consumed; pending = %+v", pend)
	}
}
