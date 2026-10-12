package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
)

func runCommand(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out.String()
}

func TestVersionText(t *testing.T) {
	got := runCommand(t, "version")
	if !strings.HasPrefix(got, "herdr-runner-kit dev ") {
		t.Fatalf("unexpected version line: %q", got)
	}
	if !strings.Contains(got, "schema v1, contract v1") {
		t.Fatalf("version line lacks schema and contract: %q", got)
	}
}

func TestVersionJSON(t *testing.T) {
	var info buildinfo.Info
	if err := json.Unmarshal([]byte(runCommand(t, "version", "--json")), &info); err != nil {
		t.Fatal(err)
	}
	if info != buildinfo.Get() {
		t.Fatalf("got %+v, want %+v", info, buildinfo.Get())
	}
}
