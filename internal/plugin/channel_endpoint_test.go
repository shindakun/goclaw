package plugin

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestChannelEndpoint_WriteRead(t *testing.T) {
	dir := t.TempDir()
	ep := ChannelEndpoint{Transport: "tcp", Addr: "host.docker.internal:12345", Token: "deadbeef"}
	if err := WriteChannelEndpoint(dir, ep); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadChannelEndpoint(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != ep {
		t.Fatalf("round trip = %+v, want %+v", got, ep)
	}
	// LogString never leaks the token.
	if s := got.LogString(); s == "" || containsStr(s, "deadbeef") {
		t.Fatalf("LogString %q must be non-empty and token-free", s)
	}
	RemoveChannelEndpoint(dir)
	if _, err := ReadChannelEndpoint(dir); err == nil {
		t.Fatal("endpoint still readable after Remove")
	}
}

func TestNewChannelToken_Unique(t *testing.T) {
	a, err := NewChannelToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewChannelToken()
	if a == b || len(a) != channelTokenLen*2 { // hex doubles the byte length
		t.Fatalf("tokens not unique/right length: %q %q", a, b)
	}
}

// TestReadAndCheckToken_Accepts proves a correct leading token line passes and the
// reader resumes AFTER it (so the frame protocol sees the right bytes), while a wrong
// token is rejected.
func TestReadAndCheckToken_Accepts(t *testing.T) {
	const token = "sekret"
	// Good token: server reads token, then the trailing payload survives on the reader.
	srv, cli := net.Pipe()
	go func() {
		_, _ = cli.Write([]byte(token + "\nPAYLOAD"))
	}()
	br, err := ReadAndCheckToken(srv, token)
	if err != nil {
		t.Fatalf("good token rejected: %v", err)
	}
	rest := make([]byte, 7)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.Read(rest); err != nil {
		t.Fatalf("read payload after token: %v", err)
	}
	if string(rest) != "PAYLOAD" {
		t.Fatalf("post-token bytes = %q, want PAYLOAD", rest)
	}
	_ = srv.Close()
	_ = cli.Close()

	// Wrong token: rejected.
	srv2, cli2 := net.Pipe()
	go func() { _, _ = cli2.Write([]byte("wrong\n")) }()
	if _, err := ReadAndCheckToken(srv2, token); err == nil {
		t.Fatal("wrong token accepted")
	}
	_ = srv2.Close()
	_ = cli2.Close()
}

func TestDialChannelEndpoint_Unix(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "x.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()

	conn, err := DialChannelEndpoint(ChannelEndpoint{Transport: "unix", Path: sock}, 2*time.Second)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("listener never accepted the unix dial")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
