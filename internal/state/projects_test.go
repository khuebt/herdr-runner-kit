package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProjectNames(t *testing.T) {
	for _, ok := range []string{"a", "demo-app", "0x", strings.Repeat("a", 40)} {
		if err := CheckProjectName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "Demo", "a_b", "a/b", "..", strings.Repeat("a", 41)} {
		if err := CheckProjectName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestProjectReserveCommitForget(t *testing.T) {
	s := openTestStore(t)
	root := resolvedTemp(t)
	app := filepath.Join(root, "app")
	p, err := s.ReserveProject("demo-app", app)
	if err != nil || !p.Reserved || p.Path != app {
		t.Fatalf("reserve: %+v %v", p, err)
	}
	if _, found, _ := s.ProjectFor(app); found {
		t.Fatal("a reservation must not own tasks")
	}
	if err := s.CommitProject("demo-app"); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.ProjectFor(filepath.Join(app, "src", "x")); err != nil || !found || got.Name != "demo-app" {
		t.Fatalf("ProjectFor inside: %+v %v %v", got, found, err)
	}
	if _, found, _ := s.ProjectFor(root); found {
		t.Fatal("parent folder matched the project")
	}
	if err := s.SetProjectWorkspace("demo-app", "w3"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectWorkspace("other", "w3"); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("workspace of an unknown project: %v", err)
	}
	if err := s.ReleaseProject("demo-app"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ProjectByName("demo-app"); err != nil || got.WorkspaceID != "w3" {
		t.Fatalf("after release: %+v %v", got, err)
	}
	unlock, err := s.LockProject("demo-app", "record")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := s.ForgetProject("demo-app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectByName("demo-app"); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("forgotten project: %v", err)
	}
	if _, err := os.Stat(s.ProjectDir("demo-app")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project files kept: %v", err)
	}
	if err := s.ForgetProject("demo-app"); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("second forget: %v", err)
	}
}

func TestProjectReserveRefusesOverlaps(t *testing.T) {
	s := openTestStore(t)
	root := resolvedTemp(t)
	app := filepath.Join(root, "app")
	if _, err := s.ReserveProject("app", app); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path string }{
		{"app", filepath.Join(root, "other")},     // same name
		{"copy", app},                             // same path
		{"copy", app + "/"},                       // same path, written differently
		{"inner", filepath.Join(app, "packages")}, // inside
		{"outer", root},                           // above
	} {
		if _, err := s.ReserveProject(c.name, c.path); err == nil {
			t.Errorf("reserve %s at %s succeeded", c.name, c.path)
		}
	}
	// A sibling that only shares the prefix is a different project.
	if _, err := s.ReserveProject("app2", app+"2"); err != nil {
		t.Fatalf("prefix sibling refused: %v", err)
	}
	if err := s.ReleaseProject("app2"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Projects(); len(list) != 1 || list[0].Name != "app" {
		t.Fatalf("projects = %+v", list)
	}
}

func TestProjectReserveRace(t *testing.T) {
	s := openTestStore(t)
	app := filepath.Join(resolvedTemp(t), "app")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"one", "two"} {
		wg.Go(func() { _, errs[i] = s.ReserveProject(name, app) })
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("want exactly one winner: %v", errs)
	}
	if list, _ := s.Projects(); len(list) != 1 {
		t.Fatalf("projects = %+v", list)
	}
}

func TestCanonicalPathResolvesSymlinks(t *testing.T) {
	root := resolvedTemp(t)
	target := filepath.Join(root, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalPath(filepath.Join(link, "new", "app"))
	if err != nil || got != filepath.Join(target, "new", "app") {
		t.Fatalf("CanonicalPath = %q, %v", got, err)
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		dir, path string
		want      bool
	}{
		{"/a/app", "/a/app/x", true},
		{"/a/app", "/a/app", false},
		{"/a/app", "/a/app2", false},
		{"/a/app", "/a", false},
		{"/a/app", "/a/app/..foo", true},
	} {
		if got := Within(c.dir, c.path); got != c.want {
			t.Errorf("Within(%s, %s) = %v", c.dir, c.path, got)
		}
	}
}

func TestReleaseStaleReservation(t *testing.T) {
	s := openTestStore(t)
	app := filepath.Join(resolvedTemp(t), "app")
	if _, err := s.ReserveProject("demo-app", app); err != nil {
		t.Fatal(err)
	}
	always := func(Project) bool { return true }
	if ok, err := s.ReleaseStaleReservation("demo-app", time.Hour, always); err != nil || ok {
		t.Fatalf("a fresh reservation was released: %v %v", ok, err)
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if ok, err := s.ReleaseStaleReservation("demo-app", time.Hour, func(Project) bool { return false }); err != nil || ok {
		t.Fatalf("a reservation still in use was released: %v %v", ok, err)
	}
	if ok, err := s.ReleaseStaleReservation("demo-app", time.Hour, always); err != nil || !ok {
		t.Fatalf("stale reservation kept: %v %v", ok, err)
	}
	if _, err := s.ProjectByName("demo-app"); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("still registered: %v", err)
	}

	if _, err := s.ReserveProject("other-app", app); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitProject("other-app"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(4 * time.Hour) }
	if ok, err := s.ReleaseStaleReservation("other-app", time.Hour, always); err != nil || ok {
		t.Fatalf("a committed project was released: %v %v", ok, err)
	}
}
