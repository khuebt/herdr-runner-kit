package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func actions(changes []Change) string {
	var out []string
	for _, c := range changes {
		out = append(out, filepath.Base(c.Path)+"="+c.Action)
	}
	return strings.Join(out, " ")
}

func TestStampAndMarker(t *testing.T) {
	src := []byte("---\nname: x\n---\n\n# Body\n")
	got := Stamp(src, "v1.2.3")
	if !strings.HasPrefix(string(got), "---\nname: x\n---\n<!-- herdr-runner-kit v1.2.3 ") {
		t.Fatalf("marker must follow the front matter:\n%s", got)
	}
	if v, intact, found := Marker(got); v != "v1.2.3" || !intact || !found {
		t.Fatalf("Marker = %q %v %v", v, intact, found)
	}
	edited := []byte(strings.Replace(string(got), "# Body", "# Mine", 1))
	if _, intact, found := Marker(edited); intact || !found {
		t.Fatal("an edited file must not count as intact")
	}
	if _, _, found := Marker([]byte("# plain\n")); found {
		t.Fatal("a file without a marker has none")
	}
	if !strings.HasPrefix(string(Stamp([]byte("# ref\n"), "v1")), "<!-- herdr-runner-kit v1 ") {
		t.Fatal("without front matter the marker goes first")
	}
}

func TestWriteSkill(t *testing.T) {
	cfg := t.TempDir()
	changes, err := WriteSkill(cfg, "v0.1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(changes); got != "SKILL.md=created commands.md=created" {
		t.Fatalf("fresh install: %s", got)
	}
	if changes, _ = WriteSkill(cfg, "v0.1.0", false); actions(changes) != "SKILL.md=unchanged commands.md=unchanged" {
		t.Fatalf("rerun: %s", actions(changes))
	}
	if changes, _ = WriteSkill(cfg, "v0.2.0", false); actions(changes) != "SKILL.md=updated commands.md=updated" {
		t.Fatalf("new version: %s", actions(changes))
	}
	if v, intact, err := SkillVersion(cfg); v != "v0.2.0" || !intact || err != nil {
		t.Fatalf("SkillVersion = %q %v %v", v, intact, err)
	}

	skillPath := filepath.Join(SkillDir(cfg), "SKILL.md")
	data, _ := os.ReadFile(skillPath)
	if err := os.WriteFile(skillPath, append(data, "my note\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, _ = WriteSkill(cfg, "v0.2.0", false)
	if actions(changes) != "SKILL.md=refused commands.md=unchanged" {
		t.Fatalf("edited file: %s", actions(changes))
	}
	if data, _ := os.ReadFile(skillPath); !strings.HasSuffix(string(data), "my note\n") {
		t.Fatal("a refused file must not be touched")
	}
	if changes, _ = WriteSkill(cfg, "v0.2.0", true); actions(changes) != "SKILL.md=updated commands.md=unchanged" {
		t.Fatalf("force: %s", actions(changes))
	}

	if err := os.WriteFile(skillPath, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err = RemoveSkill(cfg)
	if err != nil || actions(changes) != "SKILL.md=kept commands.md=removed" {
		t.Fatalf("remove: %s %v", actions(changes), err)
	}
	if _, err := os.Stat(skillPath); err != nil {
		t.Fatal("remove must keep the user's file")
	}
}

const userSettings = `{
  "model": "opus",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          { "type": "command", "command": "a && b > /dev/null" }
        ]
      }
    ],
    "PreToolUse": []
  },
  "zzz": 1
}
`

func TestHookMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(userSettings), 0o640); err != nil {
		t.Fatal(err)
	}
	if a, err := AddHook(path, "/bin/hrk"); a != "created" || err != nil {
		t.Fatalf("install = %s %v", a, err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	for _, want := range []string{`"a && b > /dev/null"`, `"asyncRewake": true`, `"timeout": 28800`, "exit 0 " + hookTag} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in:\n%s", want, s)
		}
	}
	if strings.Index(s, `"model"`) > strings.Index(s, `"hooks"`) || strings.Index(s, `"hooks"`) > strings.Index(s, `"zzz"`) ||
		strings.Index(s, `"Stop"`) > strings.Index(s, `"PreToolUse"`) {
		t.Fatalf("key order changed:\n%s", s)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v", info.Mode().Perm())
	}
	if cmd, _ := HookFor(path); cmd != HookCommand("/bin/hrk") {
		t.Fatalf("HookFor = %q", cmd)
	}

	if a, _ := AddHook(path, "/bin/hrk"); a != "unchanged" {
		t.Fatalf("rerun = %s", a)
	}
	if after, _ := os.ReadFile(path); string(after) != s {
		t.Fatal("an unchanged install must not rewrite the file")
	}
	reordered := regexp.MustCompile(`"asyncRewake": true,(\s*)"timeout": 28800`).
		ReplaceAllString(s, `"timeout": 28800,$1"asyncRewake": true`)
	if reordered == s {
		t.Fatal("test setup: could not reorder the hook's keys")
	}
	if err := os.WriteFile(path, []byte(reordered), 0o640); err != nil {
		t.Fatal(err)
	}
	if a, _ := AddHook(path, "/bin/hrk"); a != "unchanged" {
		t.Fatalf("reordered keys = %s", a)
	}
	if a, _ := AddHook(path, "/opt/hrk"); a != "updated" {
		t.Fatalf("moved binary = %s", a)
	}
	if data, _ := os.ReadFile(path); strings.Count(string(data), hookTag) != 1 {
		t.Fatal("update must replace the old entry, not add one")
	}

	if removed, err := RemoveHook(path); !removed || err != nil {
		t.Fatalf("remove = %v %v", removed, err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), hookTag) || !strings.Contains(string(data), `"a && b > /dev/null"`) {
		t.Fatalf("remove must delete only our entry:\n%s", data)
	}
	if removed, _ := RemoveHook(path); removed {
		t.Fatal("second remove finds nothing")
	}
}

func TestHookOnMissingSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "settings.json")
	if removed, err := RemoveHook(path); removed || err != nil {
		t.Fatalf("remove on missing file = %v %v", removed, err)
	}
	if a, err := AddHook(path, "/bin/hrk"); a != "created" || err != nil {
		t.Fatalf("install = %s %v", a, err)
	}
	if removed, _ := RemoveHook(path); !removed {
		t.Fatal("remove after install")
	}
	if data, _ := os.ReadFile(path); strings.TrimSpace(string(data)) != "{}" {
		t.Fatalf("empty hooks must be dropped, got %s", data)
	}
}

// The hook runs through sh: only news (exit 3) may wake Claude (exit 2), and
// a panic, which also exits 2, must not.
func TestHookCommandExitMapping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "it's")
	for code, want := range map[int]int{3: 2, 2: 0, 0: 0, 1: 0} {
		script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = __autoarm ] || exit 9\nexit %d\n", code)
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		err := exec.Command("sh", "-c", HookCommand(bin)).Run()
		got := 0
		if ee, ok := err.(*exec.ExitError); ok {
			got = ee.ExitCode()
		}
		if got != want {
			t.Fatalf("binary exit %d: hook exit %d, want %d", code, got, want)
		}
	}
	if err := exec.Command("sh", "-c", HookCommand(filepath.Join(dir, "gone"))).Run(); err != nil {
		t.Fatalf("a missing binary must exit 0: %v", err)
	}
}

func TestHookSharesGroupWithUserHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	shared := `{"hooks":{"Stop":[{"matcher":"x","hooks":[{"type":"command","command":"say done"},` +
		`{"type":"command","command":"old __autoarm ` + hookTag + `","asyncRewake":true}]}]}}`
	if err := os.WriteFile(path, []byte(shared), 0o600); err != nil {
		t.Fatal(err)
	}
	if a, err := AddHook(path, "/bin/hrk"); a != "updated" || err != nil {
		t.Fatalf("add = %s %v", a, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"say done"`) || !strings.Contains(string(data), `"matcher": "x"`) || strings.Contains(string(data), "old __autoarm") {
		t.Fatalf("update must keep the user's hook and group keys:\n%s", data)
	}
	if removed, err := RemoveHook(path); !removed || err != nil {
		t.Fatalf("remove = %v %v", removed, err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `"say done"`) || strings.Contains(string(data), hookTag) {
		t.Fatalf("remove must keep the user's hook:\n%s", data)
	}
}

func TestHookFollowsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink("dotfiles/settings.json", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dotfiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := AddHook(link, "/bin/hrk"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link must stay a link")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "dotfiles", "settings.json")); err != nil || !strings.Contains(string(data), hookTag) {
		t.Fatalf("hook must land in the link's target: %v", err)
	}
}
