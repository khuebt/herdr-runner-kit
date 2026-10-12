package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTask(t *testing.T, s *Store) *task.Task {
	t.Helper()
	tk := &task.Task{Kind: "code", Agent: "codex", Prompt: "fix it", Cwd: "/repo", Owner: "/repo", Gate: task.GateAuto, Status: task.Starting}
	if err := s.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestCreateAssignsSequentialIDs(t *testing.T) {
	s := openStore(t)
	for i := 1; i <= 3; i++ {
		if got := newTask(t, s).ID; got != task.FormatID(i) {
			t.Fatalf("id %d = %s", i, got)
		}
	}
	list, err := s.List()
	if err != nil || len(list) != 3 || list[2].ID != "t3" {
		t.Fatalf("List() = %v, %v", list, err)
	}
}

func TestConcurrentCreateNeverReusesAnID(t *testing.T) {
	s := openStore(t)
	var wg sync.WaitGroup
	ids := make(chan string, 40)
	for range 40 {
		wg.Go(func() {
			tk := &task.Task{Kind: "code", Agent: "codex", Status: task.Starting}
			if err := s.Create(tk); err != nil {
				t.Error(err)
				return
			}
			ids <- tk.ID
		})
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("id %s issued twice", id)
		}
		seen[id] = true
	}
	if len(seen) != 40 {
		t.Fatalf("got %d ids", len(seen))
	}
}

func TestTerminalStatusIsSticky(t *testing.T) {
	s := openStore(t)
	tk := newTask(t, s)
	// The runner reports done before the slow launcher marks it running.
	if _, changed, err := s.SetStatus(tk.ID, task.Done); err != nil || !changed {
		t.Fatalf("set done: changed=%v err=%v", changed, err)
	}
	got, changed, err := s.SetStatus(tk.ID, task.Running, task.Starting)
	if err != nil || changed || got.Status != task.Done {
		t.Fatalf("running after done: status=%s changed=%v err=%v", got.Status, changed, err)
	}
	got, changed, err = s.SetStatus(tk.ID, task.Failed)
	if err != nil || changed || got.Status != task.Done {
		t.Fatalf("failed after done: status=%s changed=%v err=%v", got.Status, changed, err)
	}
}

func TestSetStatusCompareAndSet(t *testing.T) {
	s := openStore(t)
	tk := newTask(t, s)
	if _, changed, _ := s.SetStatus(tk.ID, task.Running, task.Queued); changed {
		t.Fatal("changed from starting although only queued was allowed")
	}
	got, changed, err := s.SetStatus(tk.ID, task.Running, task.Queued, task.Starting)
	if err != nil || !changed || got.Status != task.Running {
		t.Fatalf("status=%s changed=%v err=%v", got.Status, changed, err)
	}
	if _, _, err := s.SetStatus(tk.ID, "finished"); err == nil {
		t.Fatal("accepted an unknown status")
	}
}

func TestConcurrentStatusRaceEndsTerminal(t *testing.T) {
	s := openStore(t)
	tk := newTask(t, s)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			to := task.Running
			if i == 7 {
				to = task.Done
			}
			if _, _, err := s.SetStatus(tk.ID, to); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err := s.Get(tk.ID)
	if err != nil || got.Status != task.Done {
		t.Fatalf("status = %v, %v; want done", got.Status, err)
	}
}

func TestUpdateUnknownTask(t *testing.T) {
	s := openStore(t)
	if _, err := s.Update("t9", func(*task.Task) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get("t9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateErrorWritesNothing(t *testing.T) {
	s := openStore(t)
	tk := newTask(t, s)
	_, err := s.Update(tk.ID, func(t *task.Task) error {
		t.Prompt = "changed"
		return errors.New("abort")
	})
	if err == nil {
		t.Fatal("want error")
	}
	got, _ := s.Get(tk.ID)
	if got.Prompt != "fix it" {
		t.Fatalf("prompt = %q, update was written", got.Prompt)
	}
}

func TestPermissions(t *testing.T) {
	s := openStore(t)
	tk := newTask(t, s)
	if err := s.Append(Event{Task: tk.ID, Type: "spawned"}); err != nil {
		t.Fatal(err)
	}
	check := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
	check(s.Dir(), 0o700)
	check(s.TaskDir(tk.ID), 0o700)
	check(filepath.Join(s.TaskDir(tk.ID), "task.json"), 0o600)
	check(filepath.Join(s.Dir(), "seq"), 0o600)
	check(s.EventsPath(), 0o600)
}

func TestNewerTaskSchemaIsRefused(t *testing.T) {
	s := openStore(t)
	tk := newTask(t, s)
	path := filepath.Join(s.TaskDir(tk.ID), "task.json")
	data, _ := os.ReadFile(path)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	raw["schemaVersion"] = currentSchema + 1
	data, _ = json.Marshal(raw)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(tk.ID)
	if err == nil || !strings.Contains(err.Error(), "upgrade herdr-runner-kit") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewerStateSchemaIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"schemaVersion": %d}`, currentSchema+1)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil || !strings.Contains(err.Error(), "upgrade herdr-runner-kit") {
		t.Fatalf("Open err = %v", err)
	}
}

func TestMigrationsRunInOrder(t *testing.T) {
	dir := t.TempDir()
	if err := writeMeta(filepath.Join(dir, "state.json"), 1); err != nil {
		t.Fatal(err)
	}
	var ran []int
	list := []migration{
		{from: 1, run: func(string) error { ran = append(ran, 1); return nil }},
		{from: 2, run: func(string) error { ran = append(ran, 2); return nil }},
	}
	if err := runMigrations(dir, list, 3); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ran) != "[1 2]" {
		t.Fatalf("ran %v", ran)
	}
	// Already current: nothing runs again.
	ran = nil
	if err := runMigrations(dir, list, 3); err != nil || ran != nil {
		t.Fatalf("second run: ran=%v err=%v", ran, err)
	}
}

func TestMigrationFailureKeepsVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := writeMeta(path, 1); err != nil {
		t.Fatal(err)
	}
	list := []migration{{from: 1, run: func(string) error { return errors.New("disk full") }}}
	if err := runMigrations(dir, list, 2); err == nil {
		t.Fatal("want error")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"schemaVersion":1`) {
		t.Fatalf("state.json = %s", data)
	}
}

func TestMissingMigrationIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := writeMeta(filepath.Join(dir, "state.json"), 1); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(dir, nil, 2); err == nil {
		t.Fatal("want error for a missing migration step")
	}
}
