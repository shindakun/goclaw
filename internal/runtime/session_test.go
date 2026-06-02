package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withFakePodman swaps execCommand for a fake that records argv and replays
// scripted `ps` output via the test binary re-exec trick. The `ps` output flips
// from psBefore to psAfter once a `run` has been seen — modeling a container
// that is absent before launch and running after (so the post-launch health
// check passes).
func withFakePodman(t *testing.T, psBefore, psAfter string, record *[]string) {
	t.Helper()
	orig := execCommand
	t.Cleanup(func() { execCommand = orig })
	ran := false
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*record = append(*record, name+" "+strings.Join(args, " "))
		isPS := len(args) > 0 && args[0] == "ps"
		psOutput := psBefore
		if ran {
			psOutput = psAfter
		}
		if len(args) > 0 && args[0] == "run" {
			ran = true
		}
		cs := []string{"-test.run=TestHelperProcess", "--", psOutput}
		if isPS {
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

func TestEnsureGroupRunner_LaunchesWhenAbsent(t *testing.T) {
	var calls []string
	// Absent before launch; running after (post-launch health check passes).
	withFakePodman(t, "", "goclaw-1 running\n", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun)
	// Use a RELATIVE group dir: podman would treat a relative -v source as a
	// named volume, so the launcher must resolve it to an absolute path.
	relDir := filepath.Join("data", "sessions", "1")
	gr := GroupRunner{
		Image:        "goclaw-runner:latest",
		AgentGroupID: 1,
		GroupDir:     relDir,
	}
	if err := m.EnsureGroupRunner(context.Background(), gr); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Expect: ps (pre-check) + run + ps (post-launch health check).
	if len(calls) != 3 {
		t.Fatalf("expected ps + run + ps, got %d calls: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "ps") {
		t.Fatalf("first call should be ps: %q", calls[0])
	}
	if !strings.Contains(calls[2], "ps") {
		t.Fatalf("third call should be the post-launch ps: %q", calls[2])
	}
	run := calls[1]

	absDir, _ := filepath.Abs(relDir)
	for _, want := range []string{
		"run", "--user 1000:1000", "--init",
		"--name goclaw-1",
		absDir + ":/sessions:Z", // mount source must be absolute
		"goclaw-runner:latest",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("run argv missing %q\n  got: %s", want, run)
		}
	}
	// Guard the regression directly: the -v source must not be the relative form.
	if strings.Contains(run, " "+relDir+":/sessions") {
		t.Errorf("mount source is relative (would become a named volume): %s", run)
	}
}

func TestEnsureGroupRunner_SkipsWhenRunning(t *testing.T) {
	var calls []string
	name := "goclaw-1"
	withFakePodman(t, name+" running\n" /* already running */, name+" running\n", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun)
	gr := GroupRunner{
		Image:        "goclaw-runner:latest",
		AgentGroupID: 1,
		GroupDir:     "/data/x",
	}
	if err := m.EnsureGroupRunner(context.Background(), gr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Only the ps check; no run.
	if len(calls) != 1 || !strings.Contains(calls[0], "ps") {
		t.Fatalf("expected a single ps call, got: %v", calls)
	}
}

func TestEnsureGroupRunner_RemovesStaleStoppedThenRuns(t *testing.T) {
	var calls []string
	name := "goclaw-1"
	// Before: ps -a reports the name stopped → must rm then run.
	// After: ps -a reports it running → post-launch health check passes.
	withFakePodman(t, name+" exited\n", name+" running\n", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun)
	gr := GroupRunner{
		Image:        "goclaw-runner:latest",
		AgentGroupID: 1,
		GroupDir:     "/data/x",
	}
	if err := m.EnsureGroupRunner(context.Background(), gr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// ps (pre) + rm (stale) + run + ps (post-launch health check).
	if len(calls) != 4 {
		t.Fatalf("expected ps + rm + run + ps, got %d: %v", len(calls), calls)
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
	if !strings.Contains(calls[3], "ps") {
		t.Errorf("call 3 should be the post-launch ps: %q", calls[3])
	}
}
