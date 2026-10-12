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
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
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
	_ = tw.WriteHeader(&tar.Header{Name: "herdr-runner-kit", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
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
