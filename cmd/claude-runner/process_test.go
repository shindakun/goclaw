package main

import (
	"context"
	"fmt"
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
