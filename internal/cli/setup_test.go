package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupEnv(t *testing.T) (home, claude1, claude2 string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	// The default state folder: a scratch HRK_STATE_DIR turns Telegram off.
	t.Setenv("XDG_STATE_HOME", home)
	t.Setenv("HRK_STATE_DIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("TELEGRAM_STATE_DIR", "")
	claude1, claude2 = filepath.Join(home, ".claude"), filepath.Join(home, ".claude2")
	return home, claude1, claude2
}

func TestSetupIsIdempotent(t *testing.T) {
	home, c1, c2 := setupEnv(t)
	env := filepath.Join(home, "bot.env")
	if err := os.WriteFile(env, []byte("TELEGRAM_BOT_TOKEN=1:abc\nTELEGRAM_CHAT_ID=42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, ".config", "herdr-runner-kit", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("telegram:\n  envFile: "+env+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := runCommand(t, "setup", "--claude-config", c1, "--claude-config", c2)
	for _, want := range []string{
		"created " + filepath.Join(home, "herdr-runner-kit"),
		"created " + filepath.Join(c1, "skills", "herdr-runner-kit", "SKILL.md"),
		"created " + filepath.Join(c2, "skills", "herdr-runner-kit", "references", "commands.md"),
		"created the auto re-arm hook in " + filepath.Join(c2, "settings.json"),
		"created " + filepath.Join(c1, "channels", "telegram", "access.json"),
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("first setup lacks %q:\n%s", want, first)
		}
	}
	if got := runCommand(t, "setup", "--claude-config", c1, "--claude-config", c2); got != "no changes\n" {
		t.Fatalf("second setup changed something:\n%s", got)
	}

	data, err := os.ReadFile(filepath.Join(c2, "channels", "telegram", "access.json"))
	if err != nil {
		t.Fatal(err)
	}
	var access struct {
		DMPolicy  string   `json:"dmPolicy"`
		AllowFrom []string `json:"allowFrom"`
	}
	if err := json.Unmarshal(data, &access); err != nil || access.DMPolicy != "allowlist" || len(access.AllowFrom) != 1 || access.AllowFrom[0] != "42" {
		t.Fatalf("access.json = %s (%v)", data, err)
	}
	if strings.Contains(string(data), "abc") {
		t.Fatal("setup must not copy the bot token")
	}
	if _, err := os.Stat(filepath.Join(c1, "channels", "telegram", ".env")); !os.IsNotExist(err) {
		t.Fatal("setup must not write the plugin's .env")
	}

	out := runCommand(t, "uninstall", "--claude-config", c1, "--claude-config", c2)
	if !strings.Contains(out, "removed the auto re-arm hook from "+filepath.Join(c1, "settings.json")) {
		t.Fatalf("uninstall:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(c1, "skills", "herdr-runner-kit")); !os.IsNotExist(err) {
		t.Fatal("uninstall must remove the skill folder")
	}
}

func TestSetupWritesDefaultConfig(t *testing.T) {
	home, c1, _ := setupEnv(t)
	out := runCommand(t, "setup", "--claude-config", c1, "--no-hook")
	cfg := filepath.Join(home, ".config", "herdr-runner-kit", "config.yaml")
	if !strings.Contains(out, "created "+cfg) {
		t.Fatalf("setup did not create the config:\n%s", out)
	}
	if strings.Contains(out, "hook") {
		t.Fatalf("--no-hook installed a hook:\n%s", out)
	}
	if _, err := loadConfig(); err != nil {
		t.Fatalf("default config does not load: %v", err)
	}
}

func TestUninstallPurgeNeedsConfirmation(t *testing.T) {
	home, c1, _ := setupEnv(t)
	runCommand(t, "setup", "--claude-config", c1)
	root := NewRootCommand()
	root.SetIn(strings.NewReader("n\n"))
	root.SetOut(&strings.Builder{})
	root.SetArgs([]string{"uninstall", "--claude-config", c1, "--purge"})
	if err := root.Execute(); err == nil {
		t.Fatal("purge without a yes must fail")
	}
	if _, err := os.Stat(filepath.Join(home, "herdr-runner-kit")); err != nil {
		t.Fatal("a cancelled purge must keep the state")
	}
	runCommand(t, "uninstall", "--claude-config", c1, "--purge", "--yes")
	for _, p := range []string{filepath.Join(home, "herdr-runner-kit"), filepath.Join(home, ".config", "herdr-runner-kit")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("purge kept %s", p)
		}
	}
}

func TestLinkShortName(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	old := filepath.Join(dir, "herdr-runner-kit")
	if got, err := linkShortName(old); err != nil || got != filepath.Join(dir, "hrk") {
		t.Fatalf("link: %q %v", got, err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "hrk")); target != "herdr-runner-kit" {
		t.Fatalf("hrk points at %q", target)
	}
	if got, err := linkShortName(old); err != nil || got != "" {
		t.Fatalf("second link: %q %v", got, err)
	}
	if got, err := linkShortName(filepath.Join(dir, "hrk")); err != nil || got != "" {
		t.Fatalf("new name: %q %v", got, err)
	}
}

func TestLinkShortNameBesideTheSymlinkOnPath(t *testing.T) {
	onPath := t.TempDir()
	// ownBinary resolves symlinks, as in macOS's /var -> /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "herdr-runner-kit")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(onPath, "herdr-runner-kit")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath)
	if got, err := linkShortName(bin); err != nil || got != filepath.Join(onPath, "hrk") {
		t.Fatalf("link: %q %v", got, err)
	}
	if r, err := filepath.EvalSymlinks(filepath.Join(onPath, "hrk")); err != nil || r != bin {
		t.Fatalf("hrk leads to %q %v", r, err)
	}
}
