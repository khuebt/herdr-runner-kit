package project

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
)

func openStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fullRecord sets every field, so a round trip covers the whole schema.
func fullRecord() Record {
	at := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	rec := NewRecord("demo-app")
	rec.Type, rec.Track, rec.ReferenceRepo = "ui", "lite", "../reference-app"
	rec.Share = Share{Stitch: true}
	rec.Gates.Spec = Gate{Status: "approved", Version: "v2", By: "Lan", At: at, Note: "ok", Content: "3b18e512dba79e4c8300dd08aeb37f8e728b8dad"}
	rec.Gates.Pack = Gate{At: at, Waived: true}
	rec.Reapproval = Reapproval{Needed: true, Reason: "scope grew"}
	rec.Milestones = []Milestone{{
		ID: "M1", Title: "Reading lists", Status: MilestoneInProgress, Planned: at, Started: at, Branch: "release/M1",
		Features: []Feature{{ID: "F1", Title: "Add a list"}, {ID: "F2", Title: "Share a list", DependsOn: []string{"F1"}}},
	}}
	rec.Changes = []Change{{
		ID: "CR-1", Title: "Export to CSV", Class: "scope", Status: "open", Blocking: true, Milestone: "M1", At: at,
		Path: "handbook/product/changes/CR-1-export-to-csv.md", Task: "t7",
	}}
	rec.Backlog = []BacklogItem{{ID: "B1", Text: "Dark mode", At: at, Status: "moved", To: "M2"}}
	rec.Dependencies = []Dependency{{
		ID: "D1", Name: "Mail API key", Owner: "ops", Status: "waiting", Milestone: "M1",
		Workaround: "log mails", SecretLocation: "team vault",
	}}
	rec.Decisions = []Decision{{At: at, Text: "project created"}}
	rec.Graduated = &Graduated{At: at, Tag: "v0.3.0", Export: "/tmp/export"}
	return rec
}

func TestRecordSnapshotRoundTrip(t *testing.T) {
	rec := fullRecord()
	rec.Pending = 4 // kit state only, never in the snapshot
	data, err := Render(rec, SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "pending") {
		t.Errorf("snapshot leaks the pending count:\n%s", data)
	}
	// Task ids are local to this machine and never committed.
	if strings.Contains(string(data), "t7") {
		t.Errorf("snapshot holds the local task id:\n%s", data)
	}
	got, err := ParseSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	rec.Pending, rec.Changes[0].Task = 0, ""
	if !reflect.DeepEqual(got, rec) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, rec)
	}
}

func TestRecordStrictParse(t *testing.T) {
	valid := "---\nmanifestVersion: 1\nname: demo-app\ntype: cli\ntrack: standard\ntagFormat: v0.{N}.0\nparallelWarning: 3\n---\nbody\n"
	if rec, err := ParseSnapshot([]byte(valid)); err != nil || rec.Type != "cli" {
		t.Fatalf("valid snapshot: %+v %v", rec, err)
	}
	for _, c := range []struct{ name, doc, want string }{
		{"unknown field", strings.Replace(valid, "track: standard\n", "track: standard\nextra: 1\n", 1), "field extra not found"},
		{"newer manifest", strings.Replace(valid, "manifestVersion: 1\n", "manifestVersion: 3\nfuture: x\n", 1), "upgrade herdr-runner-kit"},
		{"no manifest", strings.Replace(valid, "manifestVersion: 1\n", "", 1), "no manifestVersion"},
		{"bad type", strings.Replace(valid, "type: cli", "type: game", 1), "project type"},
		{"bad name", strings.Replace(valid, "name: demo-app", "name: Demo", 1), "invalid project name"},
		{"no front matter", "# demo\n", "no front matter"},
		{"unclosed", "---\nmanifestVersion: 1\n", "not closed"},
	} {
		if _, err := ParseSnapshot([]byte(c.doc)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestRecordLoadSaveUpdate(t *testing.T) {
	s := openStore(t)
	if _, err := Load(s, "demo-app"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("missing record: %v", err)
	}
	if err := Save(s, NewRecord("demo-app")); err != nil {
		t.Fatal(err)
	}
	rec, err := Update(s, "demo-app", func(r *Record) error {
		r.Decisions = append(r.Decisions, Decision{At: time.Now().UTC(), Text: "use SQLite"})
		return nil
	})
	if err != nil || rec.Pending != 1 {
		t.Fatalf("update: %+v %v", rec, err)
	}
	if _, err := Update(s, "demo-app", func(r *Record) error { r.Type = "game"; return nil }); err == nil {
		t.Fatal("an invalid record was saved")
	}
	if _, err := Update(s, "demo-app", func(*Record) error { return errors.New("no") }); err == nil {
		t.Fatal("fn error ignored")
	}
	got, err := Load(s, "demo-app")
	if err != nil || got.Pending != 1 || got.Type != "unknown" || len(got.Decisions) != 1 {
		t.Fatalf("load: %+v %v", got, err)
	}
	info, err := os.Stat(filepath.Join(s.ProjectDir("demo-app"), "record.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record file: %v %v", info, err)
	}
	// A record of an older version is read and written back as the
	// current one, so an older binary refuses it with "upgrade".
	older := `{"manifestVersion": 1, "name": "demo-app", "type": "cli", "track": "standard", "tagFormat": "v0.{N}.0", "parallelWarning": 3}`
	if err := os.WriteFile(filepath.Join(s.ProjectDir("demo-app"), "record.json"), []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(s, "demo-app", func(*Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := load(t, s); got.ManifestVersion != ManifestVersion || got.Type != "cli" {
		t.Fatalf("older record after an update: %+v", got)
	}
	newer := `{"manifestVersion": 3, "name": "demo-app"}`
	if err := os.WriteFile(filepath.Join(s.ProjectDir("demo-app"), "record.json"), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s, "demo-app"); err == nil || !strings.Contains(err.Error(), "upgrade herdr-runner-kit") {
		t.Fatalf("newer record: %v", err)
	}
}

func TestLoadShortensOldLongDecisions(t *testing.T) {
	s := openStore(t)
	rec := fullRecord()
	at := rec.Decisions[0].At
	rec.Decisions = append(rec.Decisions, Decision{At: at, Text: strings.Repeat("feedback ", 230)})
	if err := Save(s, NewRecord("demo-app")); err != nil {
		t.Fatal(err)
	}
	if err := write(s, rec); err != nil { // written before notes were bounded
		t.Fatal(err)
	}
	got, err := Load(s, "demo-app")
	if err != nil {
		t.Fatal(err)
	}
	long := []rune(got.Decisions[1].Text)
	if len(long) != maxNote || long[len(long)-1] != '…' || got.Decisions[0].Text != "project created" || got.Pending != 1 {
		t.Fatalf("loaded %d runes, pending %d", len(long), got.Pending)
	}
	if text := Status(&got, state.Project{Name: "demo-app", Path: "/work/demo-app"}, nil, time.Now()).Text(); !strings.Contains(text, "note: 1 old decision entries will be shortened at the next commit\n") {
		t.Errorf("status lacks the note:\n%s", text)
	}
	// The next save writes the short entry, and later loads count nothing.
	if _, err := Update(s, "demo-app", func(*Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	again, err := Load(s, "demo-app")
	if err != nil {
		t.Fatal(err)
	}
	if again.Decisions[1].Text != got.Decisions[1].Text || again.Pending != 2 || again.clipped != 0 {
		t.Errorf("after save: %q pending %d clipped %d", again.Decisions[1].Text, again.Pending, again.clipped)
	}
}
