package plugin

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIRCD is a minimal plaintext IRC server for the channel round-trip test. It
// accepts ONE client (the irc plugin), answers registration with a 001 welcome, and
// then lets the test inject raw lines toward the client and observe the PRIVMSGs the
// client sends back. It speaks only the slice of IRC the plugin uses.
type fakeIRCD struct {
	ln   net.Listener
	addr string

	mu       sync.Mutex
	conn     net.Conn
	joined   chan struct{} // closed when the client sends JOIN
	privmsgs chan string   // raw PRIVMSG lines the client sent
	once     sync.Once
}

// startFakeIRCD binds a loopback listener and serves one client in the background.
func startFakeIRCD(t *testing.T) *fakeIRCD {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake ircd listen: %v", err)
	}
	s := &fakeIRCD{
		ln:       ln,
		addr:     ln.Addr().String(),
		joined:   make(chan struct{}),
		privmsgs: make(chan string, 8),
	}
	go s.serve()
	return s
}

func (s *fakeIRCD) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return // listener closed
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()

	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "USER "):
			// Registration complete enough: send the welcome the plugin waits for.
			_, _ = conn.Write([]byte(":fake 001 goclawbot :Welcome\r\n"))
		case strings.HasPrefix(line, "JOIN "):
			s.once.Do(func() { close(s.joined) })
		case strings.HasPrefix(line, "PRIVMSG "):
			select {
			case s.privmsgs <- line:
			default:
			}
		}
	}
}

// send writes a raw line (CRLF appended) to the connected client.
func (s *fakeIRCD) send(t *testing.T, line string) {
	t.Helper()
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		t.Fatal("fake ircd: no client connected yet")
	}
	if _, err := conn.Write([]byte(line + "\r\n")); err != nil {
		t.Fatalf("fake ircd send: %v", err)
	}
}

// waitForJoin blocks until the client has JOINed (so a subsequent send is not racing
// registration).
func (s *fakeIRCD) waitForJoin(t *testing.T) {
	t.Helper()
	select {
	case <-s.joined:
	case <-time.After(10 * time.Second):
		t.Fatal("fake ircd: client never sent JOIN")
	}
}

// waitForPrivmsg returns the next PRIVMSG line the client sent.
func (s *fakeIRCD) waitForPrivmsg(t *testing.T) string {
	t.Helper()
	select {
	case m := <-s.privmsgs:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("fake ircd: client never sent a PRIVMSG")
		return ""
	}
}

func (s *fakeIRCD) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.mu.Unlock()
}
