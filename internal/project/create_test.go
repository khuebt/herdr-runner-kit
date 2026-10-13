package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
)

// fakeWorkspaces stands in for herdr.
type fakeWorkspaces struct {
	open    string
	created []string
	err     error
}

func (f *fakeWorkspaces) RepoWorkspace(context.Context, string) (string, error) {
	return f.open, f.err
}

func (f *fakeWorkspaces) WorkspaceCreate(_ context.Context, cwd, _ string) (herdr.Location, error) {
	if f.err != nil {
		return herdr.Location{}, f.err
	}
	f.created = append(f.created, cwd)
	return herdr.Location{WorkspaceID: "w9"}, nil
}

// scratch returns a resolved temp folder outside any repository.
func scratch(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func ideaOptions(path string) CreateOptions {
	return CreateOptions{Path: path, Idea: "A small tool that tracks reading lists.", Home: "/nonexistent-home"}
}

func TestCreateFreshProject(t *testing.T) {
	root := scratch(t)
	s := openStore(t)
	ws := &fakeWorkspaces{}
	path := filepath.Join(root, "demo-app")
	o := ideaOptions(path)
	o.Remote = "git@example.com:team/demo-app.git"
	c, err := Create(context.Background(), s, ws, o)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "demo-app" || c.Branch != "main" || c.Workspace != "w9" || c.Remote != o.Remote ||
		c.WorkspaceErr != "" || c.RemoteErr != "" || c.RegistryErr != "" || len(ws.created) != 1 {
		t.Fatalf("created %+v, workspaces %v", c, ws.created)
	}
	if got := run(t, path, "rev-parse", "HEAD"); got != c.Commit {
		t.Errorf("HEAD %s, reported %s", got, c.Commit)
	}
	if got := run(t, path, "log", "--format=%s"); got != "docs: start project" {
		t.Errorf("history %q", got)
	}
	files := strings.Fields(run(t, path, "log", "--all", "--name-only", "--format="))
	slices.Sort(files)
	if want := []string{".gitignore", DecisionsPath, SnapshotPath}; !slices.Equal(files, want) {
		t.Errorf("committed %v, want %v", files, want)
	}
	if got := run(t, path, "remote", "get-url", "origin"); got != o.Remote {
		t.Errorf("origin %q", got)
	}
	// The original brief is on disk, ignored and in no commit.
	brief := filepath.Join(path, BriefDir, IdeaFile)
	if c.Brief != brief {
		t.Errorf("brief at %s, want %s", c.Brief, brief)
	}
	if data, err := os.ReadFile(brief); err != nil || string(data) != o.Idea+"\n" {
		t.Errorf("brief %q %v", data, err)
	}
	run(t, path, "check-ignore", "-q", brief)
	if stat := run(t, path, "log", "--all", "--stat"); strings.Contains(stat, "original") {
		t.Errorf("the brief reached history:\n%s", stat)
	}
	if st := run(t, path, "status", "--porcelain"); st != "" {
		t.Errorf("work tree not clean: %q", st)
	}
	p, err := s.ProjectByName("demo-app")
	if err != nil || p.Reserved || p.Path != path {
		t.Errorf("registry %+v %v", p, err)
	}
	rec, err := Load(s, "demo-app")
	if err != nil || rec.Pending != 0 || rec.Type != "unknown" || rec.Track != "standard" ||
		len(rec.Decisions) != 1 || rec.Decisions[0].Text != "project created (herdr-runner-kit new)" {
		t.Errorf("record %+v %v", rec, err)
	}
	committed, ok, err := committedSnapshot(context.Background(), path)
	if err != nil || !ok || committed.Name != "demo-app" {
		t.Errorf("committed snapshot %+v %v %v", committed, ok, err)
	}
}

func TestCreateBriefFileBranchAndReference(t *testing.T) {
	root := scratch(t)
	s := openStore(t)
	briefFile := filepath.Join(root, "brief.md")
	if err := os.WriteFile(briefFile, []byte("# Brief\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Reading")
	c, err := Create(context.Background(), s, &fakeWorkspaces{open: "w2"}, CreateOptions{
		Path: path, Name: "reading", BriefFile: briefFile, InitialBranch: "trunk",
		Reference: "https://example.com/team/ref.git", Home: "/nonexistent-home",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Branch != "trunk" || c.Workspace != "w2" || c.Remote != "" {
		t.Fatalf("created %+v", c)
	}
	if got := run(t, path, "branch", "--show-current"); got != "trunk" {
		t.Errorf("branch %q", got)
	}
	if data, err := os.ReadFile(filepath.Join(path, BriefDir, "brief.md")); err != nil || string(data) != "# Brief\n" {
		t.Errorf("brief %q %v", data, err)
	}
	if _, err := exec.Command("git", "-C", path, "remote", "get-url", "origin").Output(); err == nil {
		t.Error("origin added without --remote")
	}
	rec, err := Load(s, "reading")
	if err != nil || rec.ReferenceRepo != "https://example.com/team/ref.git" {
		t.Errorf("record %+v %v", rec, err)
	}
}

func TestCreateNonEmptyKeepsFiles(t *testing.T) {
	root := scratch(t)
	path := filepath.Join(root, "demo-app")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(path, "README.md")
	if err := os.WriteFile(readme, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("node_modules"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	if _, err := Create(context.Background(), s, &fakeWorkspaces{}, ideaOptions(path)); err == nil ||
		!strings.Contains(err.Error(), "--allow-non-empty") {
		t.Fatalf("non-empty without the flag: %v", err)
	}
	o := ideaOptions(path)
	o.AllowNonEmpty = true
	if _, err := Create(context.Background(), s, &fakeWorkspaces{}, o); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(readme); err != nil || string(data) != "hello\n" {
		t.Errorf("README changed: %q %v", data, err)
	}
	if got := run(t, path, "show", "HEAD:.gitignore"); got != "node_modules\n"+ignoreLine {
		t.Errorf(".gitignore %q", got)
	}
	if st := run(t, path, "status", "--porcelain"); st != "?? README.md" {
		t.Errorf("status %q: only the kit's files may be committed", st)
	}
}

func TestCreateRefusals(t *testing.T) {
	root := scratch(t)
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "repo")
	holder := filepath.Join(root, "holder")
	full := filepath.Join(root, "full")
	for _, d := range []string{home, repo, filepath.Join(holder, "inner"), full} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run(t, repo, "init", "-q")
	run(t, filepath.Join(holder, "inner"), "init", "-q")
	if err := os.WriteFile(filepath.Join(full, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	registeredPath := filepath.Join(root, "group", "registered")
	if _, err := s.ReserveProject("registered", registeredPath); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitProject("registered"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		edit func(*CreateOptions)
		want string
	}{
		{"root", func(o *CreateOptions) { o.Path = "/" }, "filesystem root"},
		{"home", func(o *CreateOptions) { o.Path = home }, "home folder"},
		{"above home", func(o *CreateOptions) { o.Path = root }, "home folder"},
		{"repository", func(o *CreateOptions) { o.Path = repo }, "already a git repository"},
		{"inside repository", func(o *CreateOptions) { o.Path = filepath.Join(repo, "sub", "app") }, "inside the git repository"},
		{"holds a repository", func(o *CreateOptions) { o.Path, o.AllowNonEmpty = holder, true }, "holds the git repository"},
		{"above a project", func(o *CreateOptions) { o.Path = filepath.Join(root, "group") }, "holds project registered"},
		{"inside a project", func(o *CreateOptions) { o.Path = filepath.Join(registeredPath, "app") }, "inside project registered"},
		{"name taken", func(o *CreateOptions) { o.Name = "registered" }, "already registered"},
		{"not empty", func(o *CreateOptions) { o.Path = full }, "not empty"},
		{"userinfo remote", func(o *CreateOptions) { o.Remote = "https://user:s3cret@example.com/x.git" }, "credentials"},
		{"token remote", func(o *CreateOptions) { o.Remote = "https://ghp_s3cret@example.com/x.git" }, "credentials"},
		{"scp password", func(o *CreateOptions) { o.Remote = "git:s3cret@example.com:x.git" }, "credentials"},
		{"userinfo reference", func(o *CreateOptions) { o.Reference = "https://user:s3cret@example.com/x.git" }, "credentials"},
		{"option remote", func(o *CreateOptions) { o.Remote = "--upload-pack=x" }, "not a valid git URL"},
		{"brief folder", func(o *CreateOptions) { o.Idea, o.BriefFile = "", root }, "not a regular file"},
		{"no brief", func(o *CreateOptions) { o.Idea = " " }, "brief file or an idea"},
		{"bad branch", func(o *CreateOptions) { o.InitialBranch = "a..b" }, "not a valid branch"},
		{"bad name", func(o *CreateOptions) { o.Name = "Bad Name" }, "invalid project name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := ideaOptions(filepath.Join(root, "fresh"))
			o.Home = home
			tc.edit(&o)
			_, err := Create(context.Background(), s, &fakeWorkspaces{}, o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("the error shows the secret: %v", err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "fresh")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused call wrote the folder: %v", err)
	}
	list, err := s.Projects()
	if err != nil || len(list) != 1 {
		t.Errorf("a refused call changed the registry: %+v %v", list, err)
	}
}

func TestCreateCommitFailureKeepsEverything(t *testing.T) {
	root := scratch(t)
	hooks := filepath.Join(root, "hooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\necho blocked by hook >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[core]\n\thooksPath = "+hooks+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	s := openStore(t)
	path := filepath.Join(root, "demo-app")
	_, err := Create(context.Background(), s, &fakeWorkspaces{}, ideaOptions(path))
	if err == nil || !strings.Contains(err.Error(), "blocked by hook") ||
		!strings.Contains(err.Error(), "git -C "+path+" commit") || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Fatalf("err = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(path, BriefDir, IdeaFile)); err != nil || !strings.Contains(string(data), "reading lists") {
		t.Errorf("the stdin brief is gone: %q %v", data, err)
	}
	for _, rel := range []string{".gitignore", SnapshotPath, DecisionsPath} {
		if _, err := os.Stat(filepath.Join(path, rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
	p, err := s.ProjectByName("demo-app")
	if err != nil || p.Reserved {
		t.Errorf("registration not kept: %+v %v", p, err)
	}
	if rec, err := Load(s, "demo-app"); err != nil || rec.Pending == 0 {
		t.Errorf("record %+v %v: the first commit must show as missing", rec, err)
	}
}

func TestCreateHerdrFailureIsNotFatal(t *testing.T) {
	root := scratch(t)
	s := openStore(t)
	path := filepath.Join(root, "demo-app")
	c, err := Create(context.Background(), s, &fakeWorkspaces{err: errors.New("herdr is not running")}, ideaOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != "" || !strings.Contains(c.WorkspaceErr, "herdr is not running") ||
		!strings.Contains(c.WorkspaceErr, "herdr workspace create --cwd "+path) {
		t.Fatalf("created %+v", c)
	}
	if p, err := s.ProjectByName("demo-app"); err != nil || p.Reserved {
		t.Errorf("registry %+v %v", p, err)
	}
}

func TestCheckAndRedactURL(t *testing.T) {
	for _, tc := range []struct {
		url, redacted string
		ok            bool
	}{
		{"git@example.com:team/app.git", "git@example.com:team/app.git", true},
		{"ssh://git@example.com/team/app.git", "ssh://git@example.com/team/app.git", true},
		{"https://example.com/team/app.git", "https://example.com/team/app.git", true},
		{"/srv/git/app.git", "/srv/git/app.git", true},
		{"https://tok@example.com/team/app.git", "https://example.com/team/app.git", false},
		{"https://u:p@example.com/team/app.git", "https://example.com/team/app.git", false},
		{"ssh://u:p@example.com/app.git", "ssh://example.com/app.git", false},
		{"u:p@example.com:app.git", "example.com:app.git", false},
	} {
		if err := CheckURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("CheckURL(%q) = %v", tc.url, err)
		}
		if got := RedactURL(tc.url); got != tc.redacted {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.url, got, tc.redacted)
		}
	}
	if got := RedactURL("fatal: could not read https://u:p@example.com/x"); got != "fatal: could not read https://example.com/x" {
		t.Errorf("redact in text: %q", got)
	}
}
