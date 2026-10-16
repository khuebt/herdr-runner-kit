package state

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// heavyHelperEnv makes the test binary a slot holder: "<dir>|<slots>|<task>".
// It takes a slot, creates acquired-<task> in dir, and frees the slot once
// release-<task> exists.
const heavyHelperEnv = "HRK_HEAVY_HELPER"

func TestMain(m *testing.M) {
	if spec := os.Getenv(heavyHelperEnv); spec != "" {
		os.Exit(heavyHelper(spec))
	}
	os.Exit(m.Run())
}

func heavyHelper(spec string) int {
	parts := strings.Split(spec, "|")
	slots, _ := strconv.Atoi(parts[1])
	s, err := Open(parts[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	HeavyPoll = 10 * time.Millisecond
	release, _, err := s.AcquireHeavy(context.Background(), slots, HeavyRecord{Task: parts[2], Command: "sleep"}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer release()
	if err := os.WriteFile(filepath.Join(parts[0], "acquired-"+parts[2]), nil, 0o600); err != nil {
		return 2
	}
	for !exists(filepath.Join(parts[0], "release-"+parts[2])) {
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// startHolder starts a helper process that takes one of slots for name.
func startHolder(t *testing.T, s *Store, slots int, name string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%d|%s", heavyHelperEnv, s.Dir(), slots, name))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func acquired(s *Store, name string) bool { return exists(filepath.Join(s.Dir(), "acquired-"+name)) }

func tasksOf(recs []HeavyRecord) string {
	var names []string
	for _, r := range recs {
		names = append(names, r.Task)
	}
	return strings.Join(names, ",")
}

func TestHeavyThirdHolderWaitsForAFreeSlot(t *testing.T) {
	s := openStore(t)
	startHolder(t, s, 2, "a")
	startHolder(t, s, 2, "b")
	waitFor(t, "a and b to hold", func() bool { return acquired(s, "a") && acquired(s, "b") })
	startHolder(t, s, 2, "c")
	waitFor(t, "c to wait", func() bool { return s.HeavyWaiting("c") })
	st, err := s.HeavyStatus(2)
	if err != nil || tasksOf(st.Waiting) != "c" || len(st.Holders) != 2 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	time.Sleep(100 * time.Millisecond)
	if acquired(s, "c") {
		t.Fatal("c ran while both slots were held")
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "release-a"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "c to hold", func() bool { return acquired(s, "c") })
	if s.HeavyWaiting("c") {
		t.Error("c is still listed as waiting")
	}
	st, _ = s.HeavyStatus(2)
	if h := tasksOf(st.Holders); h != "c,b" && h != "b,c" {
		t.Errorf("holders = %q", h)
	}
}

func TestHeavyKilledHolderFreesItsSlot(t *testing.T) {
	s := openStore(t)
	holder := startHolder(t, s, 1, "a")
	waitFor(t, "a to hold", func() bool { return acquired(s, "a") })
	if err := holder.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waited := false
	release, _, err := s.AcquireHeavy(ctx, 1, HeavyRecord{Task: "b"}, func(HeavyStatus) { waited = true })
	if err != nil || waited {
		t.Fatalf("slot of a killed holder not free: waited %v, %v", waited, err)
	}
	release()
}

func TestHeavyStatusSkipsDeadProcesses(t *testing.T) {
	s := openStore(t)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	pid := dead.Process.Pid
	if err := os.MkdirAll(filepath.Join(s.heavyDir(), "waiting"), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := HeavyRecord{Slot: 1, Task: "t9", PID: pid, Command: "npm test", Since: time.Now()}
	if err := writeJSON(filepath.Join(s.heavyDir(), "slot-1.json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(s.waitingPath(pid), rec); err != nil {
		t.Fatal(err)
	}
	st, err := s.HeavyStatus(2)
	if err != nil || len(st.Holders) != 0 || len(st.Waiting) != 0 || s.HeavyWaiting("t9") {
		t.Errorf("stale records listed: %+v, %v", st, err)
	}
}

func TestHeavyOffNeverWaits(t *testing.T) {
	s := openStore(t)
	release, _, err := s.AcquireHeavy(context.Background(), 0, HeavyRecord{Task: "main"}, func(HeavyStatus) { t.Error("waited with slots off") })
	if err != nil {
		t.Fatal(err)
	}
	release()
	if exists(s.heavyDir()) {
		t.Error("slots off still made the heavy folder")
	}
}

func TestHeavyWaiterLeavesOnCancel(t *testing.T) {
	s := openStore(t)
	release, _, err := s.AcquireHeavy(context.Background(), 1, HeavyRecord{Task: "a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var notes []HeavyStatus
	go func() {
		_, _, err := s.AcquireHeavy(ctx, 1, HeavyRecord{Task: "b"}, func(st HeavyStatus) { notes = append(notes, st) })
		done <- err
	}()
	waitFor(t, "b to wait", func() bool { return s.HeavyWaiting("b") })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled waiter took a slot")
	}
	if s.HeavyWaiting("b") {
		t.Error("a cancelled waiter is still listed")
	}
	if len(notes) != 1 || tasksOf(notes[0].Holders) != "a" {
		t.Errorf("notes = %+v", notes)
	}
}

func TestHeavyStatusReadsTheSlotLockNotThePid(t *testing.T) {
	s := openStore(t)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.heavyDir(), "waiting"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A killed holder whose command still has the slot's lock file open.
	lock, ok, err := s.lockHeavySlot(1)
	if err != nil || !ok {
		t.Fatalf("lock = %v, %v", ok, err)
	}
	rec := HeavyRecord{Slot: 1, Task: "t9", PID: dead.Process.Pid, Command: "go test", Since: time.Now()}
	if err := writeJSON(s.slotPath(1, ".json"), rec); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.HeavyStatus(1); tasksOf(st.Holders) != "t9" {
		t.Fatalf("holders = %+v, want the held slot", st.Holders)
	}
	_ = lock.Close()
	if st, _ := s.HeavyStatus(1); len(st.Holders) != 0 || exists(s.slotPath(1, ".json")) {
		t.Errorf("a free slot is listed or keeps its record: %+v", st.Holders)
	}
}

func TestHeavyDeadWaiterRecordIsRemoved(t *testing.T) {
	s := openStore(t)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.heavyDir(), "waiting"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := s.waitingPath(dead.Process.Pid)
	if err := writeJSON(path, HeavyRecord{Task: "t9", PID: dead.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	if s.HeavyWaiting("t9") || exists(path) {
		t.Error("a dead waiter's record outlived a read, for a reused pid to revive")
	}
}
