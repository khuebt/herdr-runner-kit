package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
	"github.com/khuebt/herdr-runner-kit/internal/install"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/upgrade"
)

// fakeRelease serves v0.2.0 whose binary is a script reporting the given
// state schema, and points upgrade at a copy of an "old" binary.
func fakeRelease(t *testing.T, schema int) (target string) {
	t.Helper()
	setupEnv(t)
	t.Setenv("HRK_NO_SETUP", "1")
	script := fmt.Sprintf("#!/bin/sh\necho '{\"version\":\"0.2.0\",\"schemaVersion\":%d}'\n", schema)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "hrk", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(script))
	_ = tw.Close()
	_ = gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	name := upgrade.ArchiveName("v0.2.0")
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := upgrade.Release{Tag: "v0.2.0", Body: "- feat: faster wait", Assets: []upgrade.Asset{
			{Name: name, URL: srv.URL + "/a"}, {Name: "checksums.txt", URL: srv.URL + "/s"},
		}}
		switch r.URL.Path {
		case "/repos/" + upgrade.Repo + "/releases/latest", "/repos/" + upgrade.Repo + "/releases/tags/v0.2.0":
			_ = json.NewEncoder(w).Encode(rel)
		case "/repos/" + upgrade.Repo + "/releases":
			_ = json.NewEncoder(w).Encode([]upgrade.Release{rel})
		case "/a":
			_, _ = w.Write(archive)
		case "/s":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	target = filepath.Join(t.TempDir(), "herdr-runner-kit")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldClient, oldTarget := newUpgradeClient, upgradeTarget
	newUpgradeClient = func() *upgrade.Client { return &upgrade.Client{API: srv.URL, HTTP: srv.Client()} }
	upgradeTarget = func() (string, error) { return target, nil }
	t.Cleanup(func() { newUpgradeClient, upgradeTarget = oldClient, oldTarget })
	return target
}

func TestUpgradeCheckAndInstall(t *testing.T) {
	target := fakeRelease(t, 1)
	out := runCommand(t, "upgrade", "--check")
	if !strings.Contains(out, "latest:  v0.2.0") || !strings.Contains(out, "faster wait") {
		t.Fatalf("check: %q", out)
	}
	if data, _ := os.ReadFile(target); string(data) != "old" {
		t.Fatal("--check changed the binary")
	}
	if out := runCommand(t, "upgrade"); !strings.Contains(out, "upgraded dev to v0.2.0") {
		t.Fatalf("upgrade: %q", out)
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), `"schemaVersion":1`) {
		t.Fatalf("binary not replaced: %q", data)
	}
}

func TestUpgradeWaitsForRunnersBeforeAMigration(t *testing.T) {
	target := fakeRelease(t, 2)
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{Kind: "fix", Agent: "claude", Prompt: "x", Title: "x", Owner: "/repo", Gate: task.GateAuto, Status: task.Running}
	if err := s.Create(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr("upgrade"); err == nil || !strings.Contains(err.Error(), tk.ID) {
		t.Fatalf("want a refusal naming %s, got %v", tk.ID, err)
	}
	if data, _ := os.ReadFile(target); string(data) != "old" {
		t.Fatalf("binary changed: %q", data)
	}
}

func TestUpgradeRefreshesEveryConfigFolderWithTheSkill(t *testing.T) {
	fakeRelease(t, 1)
	t.Setenv("HRK_NO_SETUP", "")
	home, c1, c2 := setupEnv(t)
	c3 := filepath.Join(home, ".claude3")
	for _, d := range []string{c2, c3} {
		if err := os.MkdirAll(install.SkillDir(d), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(install.SkillDir(d), "SKILL.md"), []byte("old skill\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude-backup"), 0o700); err != nil { // no skill: left alone
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", c3)
	var argv []string
	old := execSetup
	execSetup = func(_ string, a []string, _ []string) error { argv = a; return nil }
	t.Cleanup(func() { execSetup = old })

	out := runCommand(t, "upgrade", "--claude-config", "~/.claude")
	want := []string{"setup", "--claude-config", c1, "--claude-config", c3, "--claude-config", c2}
	if len(argv) < 1 || !slices.Equal(argv[1:], want) {
		t.Errorf("setup argv = %q, want %q", argv, want)
	}
	for _, d := range []string{c1, c2, c3} {
		if strings.Count(out, "refreshing "+d+"\n") != 1 {
			t.Errorf("output does not name %s once:\n%s", d, out)
		}
	}
}

func runErr(args ...string) (string, error) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.Execute()
	return out.String(), err
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = old })
}

func TestUpgradeDowngradeNeedsVersion(t *testing.T) {
	target := fakeRelease(t, 1)
	withVersion(t, "0.3.0")
	if _, err := runErr("upgrade"); err == nil || !strings.Contains(err.Error(), "--version to downgrade") {
		t.Fatalf("plain upgrade to an older latest: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "old" {
		t.Fatal("binary changed")
	}
	if out, err := runErr("upgrade", "--version", "0.2.0"); err != nil || !strings.Contains(out, "upgraded 0.3.0 to v0.2.0") {
		t.Fatalf("named downgrade: %q %v", out, err)
	}
}

func TestUpgradeRefusesAnOlderSchemaThanTheState(t *testing.T) {
	target := fakeRelease(t, 0)
	withVersion(t, "0.1.0")
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Open(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr("upgrade", "--yes"); err == nil || !strings.Contains(err.Error(), "cannot go back") {
		t.Fatalf("got %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "old" {
		t.Fatal("binary changed")
	}
}

func TestUpgradeAsksBeforeAMinorBumpBefore1(t *testing.T) {
	target := fakeRelease(t, 1)
	withVersion(t, "0.1.0")
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetArgs([]string{"upgrade"})
	root.SetIn(strings.NewReader("n\n"))
	root.SetOut(&out)
	root.SetErr(&out)
	if err := root.Execute(); err == nil || !strings.Contains(out.String(), "faster wait") {
		t.Fatalf("declined: %q %v", out.String(), err)
	}
	if data, _ := os.ReadFile(target); string(data) != "old" {
		t.Fatal("binary changed after no")
	}
	if out, err := runErr("upgrade", "--yes"); err != nil || !strings.Contains(out, "upgraded 0.1.0 to v0.2.0") {
		t.Fatalf("--yes: %q %v", out, err)
	}
}

func TestReleaseNotesHaveOneHeading(t *testing.T) {
	own := upgrade.Release{Tag: "v0.1.0", Body: "## 0.1.0 (2026-10-12)\n\n### Features\n"}
	if got := releaseNotes(own); strings.Contains(got, "v0.1.0") || !strings.HasPrefix(got, "## 0.1.0 (") {
		t.Errorf("own heading: %q", got)
	}
	if got := releaseNotes(upgrade.Release{Tag: "v0.2.0", Body: "- feat: faster wait"}); got != "## v0.2.0\n- feat: faster wait" {
		t.Errorf("no heading: %q", got)
	}
}
