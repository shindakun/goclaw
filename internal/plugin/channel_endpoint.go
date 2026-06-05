package plugin

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ChannelEndpoint describes how the in-container runner reaches the host relay for a
// channel plugin. The host WRITES it into the channel's plugin dir as ".endpoint" when
// it opens the relay; the runner READS it from the same dir (mounted into the container)
// and dials accordingly. Carrying the dial target in a per-channel file (not container
// env) is what makes hot-add work: a channel added at runtime gets its own .endpoint
// without touching the already-running container's env.
//
// Two transports, chosen by the host per platform:
//   - "unix": a Unix-domain socket at Path. Works on native Linux (host and container
//     share a kernel), but NOT across the macOS<->podman-VM virtiofs share, where a
//     socket connect returns "operation not supported".
//   - "tcp": a TCP listener the host binds and the container dials at Addr (e.g.
//     host.docker.internal:PORT). Works on macOS and Linux. Because a TCP port is more
//     exposed than a 0600 socket, the first thing written on a tcp connection is Token
//     (hex), which the relay checks in constant time before attaching; a wrong/absent
//     token is dropped.
type ChannelEndpoint struct {
	Transport string `json:"transport"`       // "unix" or "tcp"
	Path      string `json:"path,omitempty"`  // unix socket path (in-container), for "unix"
	Addr      string `json:"addr,omitempty"`  // host:port the container dials, for "tcp"
	Token     string `json:"token,omitempty"` // shared secret the dialer sends first, for "tcp"
}

// LogString is a token-free one-line description of the endpoint, safe to log.
func (ep ChannelEndpoint) LogString() string {
	if ep.Transport == "tcp" {
		return "tcp " + ep.Addr
	}
	return "unix " + ep.Path
}

// endpointFileName is the per-channel dial-info file the host writes and the runner reads.
const endpointFileName = ".endpoint"

// channelTokenLen is the byte length of a tcp endpoint's auth token (32 bytes hex).
const channelTokenLen = 32

// NewChannelToken returns a fresh random hex token for a tcp endpoint.
func NewChannelToken() (string, error) {
	b := make([]byte, channelTokenLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WriteChannelEndpoint writes ep into pluginDir/.endpoint (0600). The host calls this so
// the in-container runner can learn how to dial back.
func WriteChannelEndpoint(pluginDir string, ep ChannelEndpoint) error {
	b, err := json.Marshal(ep)
	if err != nil {
		return fmt.Errorf("endpoint: marshal: %w", err)
	}
	// Write atomically (temp + rename) so the runner never reads a half-written file.
	tmp := filepath.Join(pluginDir, endpointFileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("endpoint: write: %w", err)
	}
	return os.Rename(tmp, filepath.Join(pluginDir, endpointFileName))
}

// ReadChannelEndpoint reads pluginDir/.endpoint. The runner calls this to learn how to
// dial the host relay for a channel plugin.
func ReadChannelEndpoint(pluginDir string) (ChannelEndpoint, error) {
	b, err := os.ReadFile(filepath.Join(pluginDir, endpointFileName))
	if err != nil {
		return ChannelEndpoint{}, err
	}
	var ep ChannelEndpoint
	if err := json.Unmarshal(b, &ep); err != nil {
		return ChannelEndpoint{}, fmt.Errorf("endpoint: parse: %w", err)
	}
	return ep, nil
}

// RemoveChannelEndpoint deletes the endpoint file (on channel close), best-effort.
func RemoveChannelEndpoint(pluginDir string) {
	_ = os.Remove(filepath.Join(pluginDir, endpointFileName))
}

// DialChannelEndpoint dials the host relay per ep and returns the connection. For tcp it
// sends the token first (the relay reads and verifies it before attaching). The returned
// conn carries the framed channel.* protocol.
func DialChannelEndpoint(ep ChannelEndpoint, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var d net.Dialer
	network, addr := "unix", ep.Path
	if ep.Transport == "tcp" {
		network, addr = "tcp", ep.Addr
	}
	for {
		conn, err := d.Dial(network, addr)
		if err == nil {
			if ep.Transport == "tcp" && ep.Token != "" {
				if werr := writeToken(conn, ep.Token); werr != nil {
					_ = conn.Close()
					return nil, fmt.Errorf("endpoint: send token: %w", werr)
				}
			}
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("endpoint: dial %s %s: %w", network, addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// tokenLine frames the token as a single newline-terminated hex line so the relay can
// read exactly the token with bufio before handing the stream to the frame protocol.
func writeToken(w io.Writer, token string) error {
	_, err := io.WriteString(w, token+"\n")
	return err
}

// ReadAndCheckToken reads the leading token line from a tcp connection and compares it in
// constant time to want. It returns a reader that continues AFTER the token line, so the
// frame protocol resumes cleanly (hand it to AttachChannelReader). A mismatch is an error
// (the caller drops the conn).
func ReadAndCheckToken(conn net.Conn, want string) (*bufio.Reader, error) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("endpoint: read token: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear; channels are long-lived
	got := line[:len(line)-1]             // strip newline
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return nil, fmt.Errorf("endpoint: bad channel token")
	}
	return br, nil
}
