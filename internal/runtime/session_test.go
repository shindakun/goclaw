package runtime

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// withFakePodman swaps execCommand for a fake that records argv and replays a
// scripted exit/stdout via the test binary re-exec trick.
func withFakePodman(t *testing.T, psOutput string, record *[]string) {
	t.Helper()
	orig := execCommand
	t.Cleanup(func() { execCommand = orig })
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*record = append(*record, name+" "+strings.Join(args, " "))
		// Re-exec the test binary in helper mode to emit canned stdout.
		cs := []string{"-test.run=TestHelperProcess", "--", psOutput}
		if len(args) > 0 && args[0] == "ps" {
			cs = append(cs, "ps")
		}
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
}

// TestHelperProcess is not a real test; it stands in for podman. For a `ps`
// invocation it prints the scripted names; otherwise it prints a fake id.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	psOutput := ""
	isPS := false
	if len(args) > 0 {
		psOutput = args[0]
	}
	if len(args) > 1 && args[1] == "ps" {
		isPS = true
	}
	if isPS {
		os.Stdout.WriteString(psOutput)
	} else {
		os.Stdout.WriteString("fake-container-id\n")
	}
	os.Exit(0)
}

func TestEnsureSessionRunner_LaunchesWhenAbsent(t *testing.T) {
	var calls []string
	withFakePodman(t, "" /* ps returns nothing → not running */, &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun)
	sr := SessionRunner{
		Image:        "goclaw-runner:latest",
		AgentGroupID: 1,
		SessionKey:   "telegram:6306189728",
		SessionDir:   "/data/v2-sessions/1/telegram_6306189728",
	}
	if err := m.EnsureSessionRunner(context.Background(), sr); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Expect a ps check then a run.
	if len(calls) != 2 {
		t.Fatalf("expected ps + run, got %d calls: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "ps") {
		t.Fatalf("first call should be ps: %q", calls[0])
	}
	run := calls[1]
	for _, want := range []string{
		"run", "--user 1000:1000", "--init",
		"--name goclaw-1-telegram-6306189728",
		"/data/v2-sessions/1/telegram_6306189728:/session:Z",
		"goclaw-runner:latest",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("run argv missing %q\n  got: %s", want, run)
		}
	}
}

func TestEnsureSessionRunner_SkipsWhenRunning(t *testing.T) {
	var calls []string
	name := "goclaw-1-telegram-6306189728"
	withFakePodman(t, name+" running\n" /* ps -a → running */, &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun)
	sr := SessionRunner{
		Image:        "goclaw-runner:latest",
		AgentGroupID: 1,
		SessionKey:   "telegram:6306189728",
		SessionDir:   "/data/x",
	}
	if err := m.EnsureSessionRunner(context.Background(), sr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Only the ps check; no run.
	if len(calls) != 1 || !strings.Contains(calls[0], "ps") {
		t.Fatalf("expected a single ps call, got: %v", calls)
	}
}

func TestEnsureSessionRunner_RemovesStaleStoppedThenRuns(t *testing.T) {
	var calls []string
	name := "goclaw-1-telegram-6306189728"
	// ps -a reports the name in a non-running state → must rm then run.
	withFakePodman(t, name+" exited\n", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun)
	sr := SessionRunner{
		Image:        "goclaw-runner:latest",
		AgentGroupID: 1,
		SessionKey:   "telegram:6306189728",
		SessionDir:   "/data/x",
	}
	if err := m.EnsureSessionRunner(context.Background(), sr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("expected ps + rm + run, got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "ps") {
		t.Errorf("call 0 should be ps: %q", calls[0])
	}
	if !strings.Contains(calls[1], "rm -f "+name) {
		t.Errorf("call 1 should remove the stale container: %q", calls[1])
	}
	if !strings.Contains(calls[2], "run") {
		t.Errorf("call 2 should be run: %q", calls[2])
	}
}
