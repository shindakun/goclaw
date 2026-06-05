package plugin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ircBin builds the irc demo channel plugin from the sibling goclawkit module into a
// temp path, or skips if the SDK module is not present next to goclaw. Mirrors rollBin.
func ircBin(t *testing.T) string {
	t.Helper()
	kit := "../../../goclawkit/cmd/irc"
	if _, err := os.Stat(kit); err != nil {
		t.Skipf("goclawkit not found at %s; skipping live channel test", kit)
	}
	out := filepath.Join(t.TempDir(), "irc")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/irc")
	cmd.Dir = "../../../goclawkit"
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build irc: %v\n%s", err, b)
	}
	return out
}

// TestChannelClient_HandshakeRejectsTool proves LaunchChannel refuses a plugin that
// does not announce kind=channel. We point it at the roll binary (a tool) and require
// the handshake to fail with the kind mismatch, NOT to hang or accept it.
func TestChannelClient_HandshakeRejectsTool(t *testing.T) {
	bin := rollBin(t) // roll announces kind=tool
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := LaunchChannel(ctx, "roll", bin, os.Environ(), quietLog())
	if err == nil {
		t.Fatal("LaunchChannel accepted a kind=tool plugin; want a kind mismatch error")
	}
	if !strings.Contains(err.Error(), "want channel") {
		t.Fatalf("expected a kind-mismatch error, got %v", err)
	}
}

// TestChannelClient_IRCRoundTrip drives the real goclawkit irc plugin against an
// in-test plaintext fake IRC server through ChannelClient: the fake sends a mention,
// the client surfaces it as a ChannelInbound, then SendOutbound posts a reply the fake
// observes as a PRIVMSG. This exercises the full host<->channel-plugin protocol over
// real OS pipes plus a real (plaintext) socket, end to end.
func TestChannelClient_IRCRoundTrip(t *testing.T) {
	bin := ircBin(t)

	srv := startFakeIRCD(t)
	defer srv.close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	env := append(os.Environ(),
		"IRC_PLAINTEXT=1",
		"IRC_SERVER="+srv.addr,
		"IRC_NICK=goclawbot",
		"IRC_CHANNEL=#goclawtester",
	)
	c, err := LaunchChannel(ctx, "irc", bin, env, quietLog())
	if err != nil {
		t.Fatalf("launch channel: %v", err)
	}
	defer func() { _ = c.Close() }()

	if c.Info().Name != "irc" || c.Info().Kind != "channel" {
		t.Fatalf("unexpected info: %+v", c.Info())
	}

	// The fake waits for JOIN, then sends a mention from another user.
	srv.waitForJoin(t)
	srv.send(t, ":steve!u@h PRIVMSG #goclawtester :goclawbot: ping?")

	// The client should surface that as an inbound, with the mention prefix stripped.
	select {
	case in := <-c.Inbound():
		if in.ChatID != "#goclawtester" || in.Text != "ping?" || in.SenderID != "steve" {
			t.Fatalf("inbound = %+v, want chat #goclawtester text \"ping?\" sender steve", in)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no inbound received from the channel plugin")
	}

	// Send a reply; the fake should observe it as a PRIVMSG to the channel.
	if err := c.SendOutbound(ctx, ChannelOutbound{Channel: "irc", ChatID: "#goclawtester", Text: "pong!"}); err != nil {
		t.Fatalf("send outbound: %v", err)
	}
	if got := srv.waitForPrivmsg(t); got != "PRIVMSG #goclawtester :pong!" {
		t.Fatalf("server saw %q, want PRIVMSG #goclawtester :pong!", got)
	}
}
