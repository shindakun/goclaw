package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shindakun/goclaw/internal/mounts"
)

// jsonStr returns s as a JSON string literal (with surrounding quotes).
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// withFakePodman swaps execCommand for a fake that records argv and replays
// scripted `ps` output via the test binary re-exec trick. The `ps` output flips
// from psBefore to psAfter once a `run` has been seen - modeling a container
// that is absent before launch and running after (so the post-launch health
// check passes).
func withFakePodman(t *testing.T, psBefore, psAfter string, record *[]string) {
	t.Helper()
	orig := execCommand
	t.Cleanup(func() { execCommand = orig })
	ran := false
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*record = append(*record, name+" "+strings.Join(args, " "))
		kind := "other"
		switch {
		case len(args) > 0 && args[0] == "ps":
			kind = "ps"
		case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
			kind = "imageinspect"
		}
		psOutput := psBefore
		if ran {
			psOutput = psAfter
		}
		if len(args) > 0 && args[0] == "run" {
			ran = true
		}
		cmd := exec.Command(os.Args[0],
			"-test.run=TestHelperProcess", "--", kind, psOutput)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
}

// fakeImageID is the image ID the fake podman reports for `image inspect`, so
// fixtures can match it in the ImageID column to model "running the current
// image".
const fakeImageID = "abc123def456"

// TestHelperProcess is not a real test; it stands in for podman. argv after "--"
// is: <kind> <psOutput>. ps → scripted lines; image inspect → fakeImageID;
// anything else → a fake container id.
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
	kind, psOutput := "", ""
	if len(args) > 0 {
		kind = args[0]
	}
	if len(args) > 1 {
		psOutput = args[1]
	}
	switch kind {
	case "ps":
		_, _ = os.Stdout.WriteString(psOutput)
	case "imageinspect":
		_, _ = os.Stdout.WriteString(fakeImageID + "\n")
	default:
		_, _ = os.Stdout.WriteString("fake-container-id\n")
	}
	os.Exit(0)
}

func TestEnsureGroupRunner_LaunchesWhenAbsent(t *testing.T) {
	var calls []string
	// Absent before launch; running after (post-launch health check passes).
	withFakePodman(t, "", "goclaw-1\trunning\tgoclaw-runner:latest\t"+fakeImageID+"\n", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun, nil)
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

func TestEnsureGroupRunner_MountsChannelSocketDirRW(t *testing.T) {
	var calls []string
	withFakePodman(t, "", "goclaw-1\trunning\tgoclaw-runner:latest\t"+fakeImageID+"\n", &calls)

	sockDir := t.TempDir()
	m := New("podman", "goclaw-runner:latest", RuntimeCrun, nil)
	gr := GroupRunner{
		Image:          "goclaw-runner:latest",
		AgentGroupID:   1,
		GroupDir:       filepath.Join("data", "sessions", "1"),
		ChannelSockDir: sockDir,
	}
	if err := m.EnsureGroupRunner(context.Background(), gr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	run := calls[1]

	absSock, _ := filepath.Abs(sockDir)
	// The channel socket dir is mounted READ-WRITE (":Z", not ":ro") at the agreed
	// container path, so the in-container runner can dial the host's sockets there.
	want := absSock + ":" + channelSockMountPath + ":Z"
	if !strings.Contains(run, want) {
		t.Errorf("run argv missing channel socket mount %q\n  got: %s", want, run)
	}
	if strings.Contains(run, absSock+":"+channelSockMountPath+":ro") {
		t.Errorf("channel socket mount is read-only; must be RW: %s", run)
	}
	// The exported container path must match what the runner dials.
	if ChannelSockContainerPath() != channelSockMountPath {
		t.Errorf("ChannelSockContainerPath()=%q != %q", ChannelSockContainerPath(), channelSockMountPath)
	}
}

func TestEnsureGroupRunner_NoChannelMountWhenUnset(t *testing.T) {
	var calls []string
	withFakePodman(t, "", "goclaw-1\trunning\tgoclaw-runner:latest\t"+fakeImageID+"\n", &calls)
	m := New("podman", "goclaw-runner:latest", RuntimeCrun, nil)
	gr := GroupRunner{Image: "goclaw-runner:latest", AgentGroupID: 1, GroupDir: filepath.Join("data", "sessions", "1")}
	if err := m.EnsureGroupRunner(context.Background(), gr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if strings.Contains(calls[1], channelSockMountPath) {
		t.Errorf("channel socket mount present when ChannelSockDir unset: %s", calls[1])
	}
}

func TestEnsureGroupRunner_SkipsWhenRunning(t *testing.T) {
	var calls []string
	name := "goclaw-1"
	withFakePodman(t, name+"\trunning\tgoclaw-runner:latest\t"+fakeImageID+"\n" /* already running, current image */, name+"\trunning\tgoclaw-runner:latest\t"+fakeImageID+"\n", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun, nil)
	gr := GroupRunner{
		Image:        "goclaw-runner:latest",
		AgentGroupID: 1,
		GroupDir:     "/data/x",
	}
	if err := m.EnsureGroupRunner(context.Background(), gr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// ps (state) + image inspect (current-image check); no rm, no run.
	if !containsCall(calls, "ps") {
		t.Fatalf("expected a ps state check, got: %v", calls)
	}
	if didLaunchOrRemove(calls) {
		t.Fatalf("expected no replace for a current-image container, got: %v", calls)
	}
}

func TestEnsureGroupRunner_RemovesStaleStoppedThenRuns(t *testing.T) {
	var calls []string
	name := "goclaw-1"
	// Before: ps -a reports the name stopped → must rm then run.
	// After: ps -a reports it running → post-launch health check passes.
	withFakePodman(t, name+"\texited\tgoclaw-runner:latest\t"+fakeImageID+"\n", name+"\trunning\tgoclaw-runner:latest\t"+fakeImageID+"\n", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun, nil)
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

func TestRunningGroupIDs_ParsesNames(t *testing.T) {
	var calls []string
	// ps output: three runner containers (ignore any non-runner name).
	withFakePodman(t, "goclaw-1\ngoclaw-42\ngoclaw-7\n", "", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun, nil)
	ids, err := m.RunningGroupIDs(context.Background())
	if err != nil {
		t.Fatalf("RunningGroupIDs: %v", err)
	}
	got := map[int64]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, want := range []int64{1, 42, 7} {
		if !got[want] {
			t.Errorf("expected group id %d in %v", want, ids)
		}
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 ids, got %v", ids)
	}
}

func TestStopGroupRunner_RemovesWhenPresent(t *testing.T) {
	var calls []string
	// containerState sees it running; StopGroupRunner should rm it.
	withFakePodman(t, "goclaw-3\trunning\tgoclaw-runner:latest\t"+fakeImageID+"\n", "", &calls)

	m := New("podman", "goclaw-runner:latest", RuntimeCrun, nil)
	if err := m.StopGroupRunner(context.Background(), 3); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Expect a ps (state check) then rm.
	if len(calls) != 2 || !strings.Contains(calls[0], "ps") || !strings.Contains(calls[1], "rm -f goclaw-3") {
		t.Fatalf("expected ps + rm goclaw-3, got %v", calls)
	}
}

// A running container whose image ID differs from the current image (e.g. the
// tag was rebuilt under the same name) is replaced.
func TestEnsureGroupRunner_ReplacesOnImageChange(t *testing.T) {
	var calls []string
	name := "goclaw-1"
	// Running on an OLD image ID; the fake `image inspect` returns fakeImageID
	// for the current tag, so the IDs differ → replace. After launch it's running.
	withFakePodman(t, name+"\trunning\tlocalhost/goclaw-claude:latest\toldimageid000\n",
		name+"\trunning\tlocalhost/goclaw-claude:latest\t"+fakeImageID+"\n", &calls)

	m := New("podman", "goclaw-claude:latest", RuntimeCrun, nil)
	gr := GroupRunner{Image: "goclaw-claude:latest", AgentGroupID: 1, GroupDir: "/data/x"}
	if err := m.EnsureGroupRunner(context.Background(), gr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !containsCall(calls, "rm -f "+name) {
		t.Errorf("expected a remove of the stale container: %v", calls)
	}
	if !didLaunchOrRemove(calls) {
		t.Errorf("expected a run of the new image: %v", calls)
	}
}

// A running container on the CURRENT image ID is left alone, even when the
// reported image name has a localhost/ prefix.
func TestEnsureGroupRunner_KeepsOnCurrentImage(t *testing.T) {
	var calls []string
	name := "goclaw-1"
	// ImageID matches the fake `image inspect` (fakeImageID) → keep.
	withFakePodman(t, name+"\trunning\tlocalhost/goclaw-claude:latest\t"+fakeImageID+"\n",
		name+"\trunning\tlocalhost/goclaw-claude:latest\t"+fakeImageID+"\n", &calls)

	m := New("podman", "goclaw-claude:latest", RuntimeCrun, nil)
	gr := GroupRunner{Image: "goclaw-claude:latest", AgentGroupID: 1, GroupDir: "/data/x"}
	if err := m.EnsureGroupRunner(context.Background(), gr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if didLaunchOrRemove(calls) {
		t.Fatalf("expected no replace (kept current image), got: %v", calls)
	}
}

// containsCall reports whether any recorded podman invocation contains substr.
func containsCall(calls []string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// podmanSubcmd returns the podman subcommand of a recorded "podman <sub> …"
// call (e.g. "run", "rm", "ps", "image"). Matching the subcommand avoids false
// hits from substrings like the "run" inside "goclaw-runner".
func podmanSubcmd(call string) string {
	f := strings.Fields(call)
	if len(f) >= 2 {
		return f[1]
	}
	return ""
}

// didLaunchOrRemove reports whether any call launched (run) or removed (rm) a
// container - i.e. a replacement happened.
func didLaunchOrRemove(calls []string) bool {
	for _, c := range calls {
		switch podmanSubcmd(c) {
		case "run", "rm":
			return true
		}
	}
	return false
}

func TestSameImage(t *testing.T) {
	cases := []struct {
		reported, desired string
		want              bool
	}{
		{"localhost/goclaw-claude:latest", "goclaw-claude:latest", true},
		{"goclaw-claude:latest", "goclaw-claude:latest", true},
		{"localhost/goclaw-claude:latest", "goclaw-claude", true}, // desired untagged → :latest
		{"localhost/goclaw-runner:latest", "goclaw-claude:latest", false},
		{"docker.io/library/alpine:latest", "goclaw-claude:latest", false},
	}
	for _, c := range cases {
		if got := sameImage(c.reported, c.desired); got != c.want {
			t.Errorf("sameImage(%q,%q)=%v want %v", c.reported, c.desired, got, c.want)
		}
	}
}

func TestEnsureRunner_AppliesAllowlistedExtraMount(t *testing.T) {
	var calls []string
	withFakePodman(t, "", "goclaw-1\trunning\tgoclaw-runner:latest\t"+fakeImageID+"\n", &calls)

	// Allowlist permits a temp dir rw; request that as an extra mount plus a
	// non-allowlisted one (which must be dropped).
	allowed := t.TempDir()
	denied := t.TempDir()
	alJSON := `[{"host_path":` + jsonStr(allowed) + `,"read_write":true}]`
	alPath := filepath.Join(t.TempDir(), "allow.json")
	if err := os.WriteFile(alPath, []byte(alJSON), 0o600); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	al, err := mounts.LoadAllowlist(alPath)
	if err != nil {
		t.Fatalf("load allowlist: %v", err)
	}

	m := New("podman", "goclaw-runner:latest", RuntimeCrun, al)
	// Real temp group dir so EnsureRunner can create the sibling claude-home.
	groupDir := filepath.Join(t.TempDir(), "sessions", "1")
	err = m.EnsureRunner(context.Background(), 1, groupDir,
		mounts.Request{HostPath: allowed, ContainerPath: "/vault", ReadWrite: true},
		mounts.Request{HostPath: denied, ContainerPath: "/secret", ReadWrite: true},
	)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	run := calls[1] // ps, run, ps
	absAllowed, _ := filepath.Abs(allowed)
	// resolveOrClean (symlink resolution) matches what Validate emits on macOS.
	if !strings.Contains(run, ":/vault:Z") {
		t.Errorf("allowlisted mount missing from argv: %s", run)
	}
	if strings.Contains(run, "/secret") {
		t.Errorf("non-allowlisted mount must be dropped, but appears: %s", run)
	}
	_ = absAllowed
}

func TestStopGroupRunner_NoopWhenAbsent(t *testing.T) {
	var calls []string
	withFakePodman(t, "", "", &calls) // ps returns nothing → absent
	m := New("podman", "goclaw-runner:latest", RuntimeCrun, nil)
	if err := m.StopGroupRunner(context.Background(), 9); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "ps") {
		t.Fatalf("expected only a ps check, got %v", calls)
	}
}

// TestEnsureGroupRunner_ConcurrentLaunchesOnce verifies the per-group lock makes
// concurrent EnsureRunner calls (router + sweep racing) launch exactly one
// container, not several. Without the lock, multiple callers pass the
// "absent / not-running" check and each issue `podman run`.
func TestEnsureGroupRunner_ConcurrentLaunchesOnce(t *testing.T) {
	var (
		mu      sync.Mutex
		runs    int  // how many `podman run` were issued
		running bool // becomes true after the first run; ps then reports it running
	)
	orig := execCommand
	t.Cleanup(func() { execCommand = orig })
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		kind := ""
		var out string
		switch {
		case len(args) > 0 && args[0] == "ps":
			if running {
				out = "goclaw-1\trunning\tgoclaw-img:latest\t" + fakeImageID + "\n"
			} else {
				out = ""
			}
			kind = "ps"
		case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
			out, kind = fakeImageID+"\n", "imageinspect"
		case len(args) > 0 && args[0] == "run":
			runs++
			running = true // after a launch, the container is running
			kind = "other"
		default:
			kind = "other"
		}
		mu.Unlock()
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", kind, out)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}

	m := New("podman", "goclaw-img:latest", RuntimeCrun, nil)
	gr := GroupRunner{Image: "goclaw-img:latest", AgentGroupID: 1, GroupDir: "/data/x"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = m.EnsureGroupRunner(context.Background(), gr) }()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("expected exactly 1 container launch under concurrency, got %d", runs)
	}
}
