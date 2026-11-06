package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func typedMain(t *testing.T, h task.Harness, id string) MainSession {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return MainSession{Harness: h, ID: id, Transcript: filepath.Join(cwd, "session.jsonl"), Cwd: cwd, Owner: cwd, PaneID: "pane-1"}
}

func TestTypedMainSessionIdentityAndLegacyIsolation(t *testing.T) {
	s := openStore(t)
	for _, h := range []task.Harness{task.Pi, task.Omp} {
		m := typedMain(t, h, "same")
		if err := s.RecordTypedMainSession(m); err != nil {
			t.Fatal(err)
		}
	}
	if s.IsMainSession("same") || MainSessionListed(s.Dir(), "same") || s.MainSessionOwner("same") != "" {
		t.Fatal("typed identity leaked into Claude helpers")
	}
	if err := s.RecordMainTranscript("same", "/claude/same.jsonl", "/claude"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMainSession("same"); err != nil {
		t.Fatal(err)
	}
	if len(s.MainSessions()) != 3 || s.MainSessionOwner("same") != "/claude" {
		t.Fatalf("identity collision: %+v", s.MainSessions())
	}
	data, err := os.ReadFile(s.mainSessionsPath())
	if err != nil || string(data) != "same\t/claude/same.jsonl\t/claude\n" {
		t.Fatalf("legacy TSV changed: %q, %v", data, err)
	}
	rows, err := s.TypedMainSessions()
	if err != nil || len(rows) != 2 || rows[0].UpdatedAt.IsZero() {
		t.Fatalf("typed records: %+v, %v", rows, err)
	}
	m := rows[0]
	m.Transcript = filepath.Join(m.Cwd, "new.jsonl") // the transcript need not exist
	s.now = func() time.Time { return rows[0].UpdatedAt.Add(time.Minute) }
	if err := s.RecordTypedMainSession(m); err != nil {
		t.Fatal(err)
	}
	updated, err := s.TypedMainSessions()
	if err != nil || len(updated) != 2 || updated[1].Transcript != m.Transcript || !updated[1].UpdatedAt.After(rows[0].UpdatedAt) {
		t.Fatalf("upsert: %+v, %v", updated, err)
	}
	m.Owner = "/different"
	if err := s.RecordTypedMainSession(m); err == nil {
		t.Fatal("owner changed")
	}
}

func TestTypedMainSessionCanonicalCwdAndValidation(t *testing.T) {
	s := openStore(t)
	m := typedMain(t, task.Pi, "/path-like/id")
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(m.Cwd, link); err != nil {
		t.Fatal(err)
	}
	want := m.Cwd
	m.Cwd = link
	m.Owner = task.ProjectOwner("demo")
	if err := s.RecordTypedMainSession(m); err != nil {
		t.Fatal(err)
	}
	rows, err := s.TypedMainSessions()
	if err != nil || rows[0].Cwd != want {
		t.Fatalf("canonical cwd: %+v, %v", rows, err)
	}
	for name, change := range map[string]func(*MainSession){
		"harness":       func(m *MainSession) { m.Harness = "unknown" },
		"empty harness": func(m *MainSession) { m.Harness = "" },
		"id":            func(m *MainSession) { m.ID = " " },
		"control id":    func(m *MainSession) { m.ID = "a\nb" },
		"path":          func(m *MainSession) { m.Transcript = "relative" },
		"missing path":  func(m *MainSession) { m.Transcript = "" },
		"cwd":           func(m *MainSession) { m.Cwd = "relative" },
		"unclean cwd":   func(m *MainSession) { m.Cwd += "/.." },
		"owner":         func(m *MainSession) { m.Owner = "relative" },
		"project":       func(m *MainSession) { m.Owner = task.ProjectOwner("../bad") },
		"pane":          func(m *MainSession) { m.PaneID = "pane\x00" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := m
			change(&bad)
			if err := s.RecordTypedMainSession(bad); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestTypedMainSessionRejectsInvalidCanonicalCwd(t *testing.T) {
	s := openStore(t)
	m := typedMain(t, task.Pi, "m")
	invalid := filepath.Join(t.TempDir(), "bad\nname")
	if err := os.Mkdir(invalid, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(invalid, alias); err != nil {
		t.Fatal(err)
	}
	m.Cwd = alias
	if err := s.RecordTypedMainSession(m); err == nil {
		t.Fatal("canonical cwd with control characters accepted")
	}
	if rows, err := s.TypedMainSessions(); err != nil || len(rows) != 0 {
		t.Fatalf("invalid registration corrupted metadata: %+v, %v", rows, err)
	}
}

func TestTypedMainSessionRetention(t *testing.T) {
	s := openStore(t)
	m := typedMain(t, task.Pi, "")
	other := typedMain(t, task.Pi, "")
	for _, base := range []MainSession{m, other} {
		for _, h := range []task.Harness{task.Pi, task.Omp} {
			for _, owner := range []string{m.Owner, other.Owner} {
				for i := range 6 {
					record := base
					record.Harness, record.Owner = h, owner
					record.ID = fmt.Sprintf("%s/%s/%d", base.Cwd, owner, i)
					if err := s.RecordTypedMainSession(record); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	rows, err := s.TypedMainSessions()
	if err != nil || len(rows) != 32 {
		t.Fatalf("per harness/owner/cwd retention: %d, %v", len(rows), err)
	}
	for i := range 70 {
		m.ID, m.Owner = fmt.Sprint(i), fmt.Sprintf("/owner/%d", i)
		if err := s.RecordTypedMainSession(m); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = s.TypedMainSessions()
	if err != nil || len(rows) != 64 || rows[0].ID != "6" || rows[63].ID != "69" {
		t.Fatalf("global retention: %+v, %v", rows, err)
	}
}

func TestTypedMainSessionConcurrentRegistration(t *testing.T) {
	s := openStore(t)
	m := typedMain(t, task.Pi, "")
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			record := m
			record.ID, record.Owner = fmt.Sprint(i), fmt.Sprintf("/owner/%d", i)
			if err := s.RecordTypedMainSession(record); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rows, err := s.TypedMainSessions()
	if err != nil || len(rows) != 24 {
		t.Fatalf("lost registrations: %d, %v", len(rows), err)
	}
	info, err := os.Stat(filepath.Join(s.Dir(), typedMainSessionsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata permissions: %v, %v", info, err)
	}
}

func TestTypedMainSessionCorruptionIsNotAbsence(t *testing.T) {
	s := openStore(t)
	m := typedMain(t, task.Omp, "m")
	if rows, err := s.TypedMainSessions(); err != nil || len(rows) != 0 {
		t.Fatalf("missing metadata: %v, %v", rows, err)
	}
	m.UpdatedAt = time.Now()
	duplicate, err := json.Marshal(mainSessionRecords{Version: 1, Sessions: []MainSession{m, m}})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"{", "null", `{}`, `{"version":2,"sessions":[]}`, `{"version":1,"sessions":[{}]}`, string(duplicate)} {
		path := filepath.Join(s.Dir(), typedMainSessionsFile)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TypedMainSessions(); err == nil {
			t.Errorf("accepted corrupt data %q", data)
		}
		if err := s.RecordTypedMainSession(m); err == nil {
			t.Error("overwrote corrupt metadata")
		}
		if err := s.RecordMainSession("legacy"); err != nil || !s.IsMainSession("legacy") {
			t.Fatalf("corrupt typed records blocked legacy: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !slices.Equal(got, []byte(data)) {
			t.Fatalf("corrupt metadata modified: %q, %v", got, err)
		}
	}
}
