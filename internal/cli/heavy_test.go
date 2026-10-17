package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// heavyHelperEnv makes the test binary an hrk heavy process: "<state
// dir>|<pid file>". It runs a command in the one slot that writes its pid
// to the pid file and sleeps.
const heavyHelperEnv = "HRK_CLI_HEAVY_HELPER"

func TestMain(m *testing.M) {
	// The runner starts detached children as `<bin> __<command>`. In a test
	// run <bin> is this test binary, which would run the whole suite again
	// and spawn more children, so such a child must exit at once.
	if slices.ContainsFunc(os.Args[1:], func(a string) bool { return strings.HasPrefix(a, "__") }) {
		os.Exit(0)
	}
	if spec := os.Getenv(heavyHelperEnv); spec != "" {
		dir, pidFile, _ := strings.Cut(spec, "|")
		store, err := state.Open(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		code, err := runHeavy(context.Background(), store, 1,
			[]string{"sh", "-c", `echo $$ > "$0"; exec sleep 30`, pidFile}, nil, nil, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// selfRunProbeEnv names the file TestSelfRunProbe writes when it runs.
const selfRunProbeEnv = "HRK_CLI_SELF_RUN_PROBE"

// TestSelfRunProbe only runs for TestDetachedChildDoesNotRunTheSuite.
func TestSelfRunProbe(t *testing.T) {
	if file := os.Getenv(selfRunProbeEnv); file != "" {
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDetachedChildDoesNotRunTheSuite runs the test binary the way the
// runner starts a detached child, `<bin> __notify <event>`, and fails when it
// runs a test: the child would run the suite again and fork without end.
func TestDetachedChildDoesNotRunTheSuite(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "ran")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSelfRunProbe$", "__notify", `{"task":"t1"}`)
	cmd.Env = append(os.Environ(), selfRunProbeEnv+"="+probe)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detached child: %v\n%s", err, out)
	}
	if _, err := os.Stat(probe); err == nil {
		t.Fatal("a test binary started as `<bin> __<command>` ran tests; TestMain must exit first")
	}
}

// heavyState opens a fresh state folder and config for the CLI, with
// heavySlots set to slots.
func heavyState(t *testing.T, slots string) *state.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", dir)
	t.Setenv("HRK_TASK", "")
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	if err := os.MkdirAll(filepath.Join(cfgHome, "herdr-runner-kit"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgHome, "herdr-runner-kit", "config.yaml"), []byte("heavySlots: "+slots+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	poll := state.HeavyPoll
	state.HeavyPoll = 10 * time.Millisecond
	t.Cleanup(func() { state.HeavyPoll = poll })
	return store
}

// syncBuffer is a bytes.Buffer safe to read while a command writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestHeavyExitsWithTheCommandsCode(t *testing.T) {
	store := heavyState(t, "2")
	var out, errOut bytes.Buffer
	code, err := runHeavy(context.Background(), store, 2, []string{"sh", "-c", "echo hi; exit 3"}, nil, &out, &errOut)
	if err != nil || code != 3 || out.String() != "hi\n" {
		t.Errorf("code %d, out %q, %v", code, out.String(), err)
	}
	code, err = runHeavy(context.Background(), store, 2, []string{"no-such-command-hrk"}, nil, &out, &errOut)
	if err != nil || code != 127 || !strings.Contains(errOut.String(), "no-such-command-hrk") {
		t.Errorf("missing command: code %d, stderr %q, %v", code, errOut.String(), err)
	}
	if st, _ := store.HeavyStatus(2); len(st.Holders) != 0 {
		t.Errorf("slots still held after the command: %+v", st.Holders)
	}
}

func TestHeavyForwardsSIGTERM(t *testing.T) {
	store := heavyState(t, "1")
	marker := filepath.Join(t.TempDir(), "started")
	done := make(chan int, 1)
	// No output pipes: the orphaned sleep would hold them open.
	go func() {
		code, _ := runHeavy(context.Background(), store, 1,
			[]string{"sh", "-c", `trap 'exit 7' TERM; touch "$0"; sleep 10 & wait`, marker}, nil, nil, nil)
		done <- code
	}()
	waitUntil(t, "the command to start", func() bool { _, err := os.Stat(marker); return err == nil })
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 7 {
			t.Errorf("code = %d, want the trap's 7", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM never reached the command")
	}
}

func TestHeavyWaitsAndSaysWho(t *testing.T) {
	store := heavyState(t, "1")
	release, _, err := store.AcquireHeavy(context.Background(), 1, state.HeavyRecord{Task: "t105", Command: "npm test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var errOut syncBuffer
	done := make(chan int, 1)
	// No output pipes: the orphaned sleep would hold them open.
	go func() {
		code, _ := runHeavy(context.Background(), store, 1, []string{"true"}, nil, &bytes.Buffer{}, &errOut)
		done <- code
	}()
	waitUntil(t, "the waiting line", func() bool { return store.HeavyWaiting("main") && errOut.String() != "" })
	if got := errOut.String(); !strings.HasPrefix(got, "hrk heavy: waiting for a slot (1 in use: t105 npm test ") {
		t.Errorf("waiting line = %q", got)
	}
	out, err := runCLI("heavy")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"heavySlots: 1\n", "slot 1: t105 · npm test · ", "waiting:\n  main · true · "} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	out, err = runCLI("heavy", "--json")
	var st state.HeavyStatus
	if err != nil || json.Unmarshal([]byte(out), &st) != nil || st.Slots != 1 || len(st.Holders) != 1 || len(st.Waiting) != 1 || st.Waiting[0].Task != "main" {
		t.Errorf("json = %s, %v", out, err)
	}
	release()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never ran after the slot was freed")
	}
	if out, _ := runCLI("heavy"); !strings.Contains(out, "slot 1: free\nwaiting: none\n") {
		t.Errorf("status after = %s", out)
	}
}

func TestHeavyOff(t *testing.T) {
	heavyState(t, "0")
	out, err := runCLI("heavy")
	if err != nil || out != "heavySlots: 0 (off)\n" {
		t.Errorf("status = %q, %v", out, err)
	}
	if _, err := runCLI("heavy", "--json", "--", "true"); err == nil {
		t.Error("--json with a command was accepted")
	}
	if out, err := runCLI("heavy", "--", "true"); err != nil || out != "" {
		t.Errorf("heavy -- true with slots off: %q, %v", out, err)
	}
}

func TestHeavyHidesSecretsInTheRecord(t *testing.T) {
	store := heavyState(t, "1")
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	marker := filepath.Join(t.TempDir(), "started")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runHeavy(context.Background(), store, 1, []string{"sh", "-c", `touch "$0"; sleep 0.3`, marker, token, strings.Repeat("x", 100)}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	waitUntil(t, "the command to start", func() bool { _, err := os.Stat(marker); return err == nil })
	st, _ := store.HeavyStatus(1)
	<-done
	if len(st.Holders) != 1 || strings.Contains(st.Holders[0].Command, token) || len([]rune(st.Holders[0].Command)) != heavyCommandRunes {
		t.Errorf("holders = %+v", st.Holders)
	}
}

func TestHeavySlotStaysHeldWhileTheCommandOutlivesHrk(t *testing.T) {
	store := heavyState(t, "1")
	pidFile := filepath.Join(t.TempDir(), "pid")
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), heavyHelperEnv+"="+store.Dir()+"|"+pidFile)
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	waitUntil(t, "the command to start", func() bool {
		data, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		return pid > 0
	})
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := helper.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()

	if st, _ := store.HeavyStatus(1); len(st.Holders) != 1 || st.Holders[0].CommandPID != pid || st.Holders[0].Orphaned {
		t.Fatalf("holders after hrk heavy was killed = %+v, want the running command's slot, with its pid %d", st.Holders, pid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := store.AcquireHeavy(ctx, 1, state.HeavyRecord{Task: "t2"}, nil); err == nil {
		t.Fatal("took the slot of a command that still runs")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the slot to come free", func() bool {
		st, _ := store.HeavyStatus(1)
		return len(st.Holders) == 0
	})
}

func TestHeavyStatusSaysWhenALeftoverProcessHoldsASlot(t *testing.T) {
	line := heavyLine(state.HeavyRecord{Task: "t1", Command: "gradle test", PID: 11, CommandPID: 12, Orphaned: true})
	if !strings.Contains(line, "orphaned: pid 11 and command pid 12 are gone, a leftover process still holds the lock") {
		t.Errorf("line = %q", line)
	}
	if line := heavyLine(state.HeavyRecord{Task: "t1", PID: 11, CommandPID: 12}); strings.Contains(line, "orphaned") {
		t.Errorf("line = %q", line)
	}
	st := state.HeavyStatus{Holders: []state.HeavyRecord{{Task: "t1", Command: "gradle test", Orphaned: true}}}
	if got := waitingLine(st); !strings.Contains(got, "t1 gradle test") || !strings.Contains(got, "(orphaned)") {
		t.Errorf("waiting line = %q", got)
	}
}
