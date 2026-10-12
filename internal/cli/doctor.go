package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
	"github.com/khuebt/herdr-runner-kit/internal/install"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
)

// telegramPlugin is the plugin id under enabledPlugins in settings.json.
const telegramPlugin = "telegram@claude-plugins-official"

type checkResult struct {
	level  string // ok, warn, fail
	detail string
	fix    string
}

func ok(format string, a ...any) checkResult {
	return checkResult{level: "ok", detail: fmt.Sprintf(format, a...)}
}

func warn(detail, fix string) checkResult {
	return checkResult{level: "warn", detail: detail, fix: fix}
}

func fail(detail, fix string) checkResult {
	return checkResult{level: "fail", detail: detail, fix: fix}
}

func newDoctorCommand() *cobra.Command {
	var dirs []string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that herdr, the agents, Telegram and the kit's own files are ready",
		Long: "doctor checks every dependency and setting the kit relies on and prints the fix " +
			"for each problem. It changes nothing. It exits 1 when a check fails; warnings " +
			"only point at optional features.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := claudeConfigs(dirs)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			failed := 0
			show := func(name string, r checkResult) {
				fmt.Fprintf(out, "%-4s %s: %s\n", r.level, name, r.detail)
				if r.fix != "" {
					fmt.Fprintf(out, "     fix: %s\n", r.fix)
				}
				if r.level == "fail" {
					failed++
				}
			}
			ctx := cmd.Context()
			show("kit", checkKit())
			show("herdr", checkHerdr(ctx))
			integrations := herdrIntegrations(ctx)
			for _, name := range []string{"claude", "codex"} {
				show("herdr integration "+name, checkIntegration(integrations, name))
			}
			show("claude", checkHelp(ctx, "claude", "--dangerously-skip-permissions", "--settings", "--model", "--effort"))
			show("codex", checkHelp(ctx, "codex", "--dangerously-bypass-approvals-and-sandbox", "--model", "--config"))
			show("git", checkHelp(ctx, "git"))
			creds, tokenSource := checkTelegramCreds()
			show("telegram credentials", creds)
			for _, dir := range targets {
				settings := readSettings(filepath.Join(dir, "settings.json"))
				show("skill in "+dir, checkSkill(dir))
				show("auto re-arm hook in "+dir, checkHook(dir))
				show("herdr hook in "+dir, checkHerdrHook(dir, settings))
				show("bypass prompt in "+dir, checkBypass(dir, settings))
				show("telegram plugin in "+dir, checkTelegramPlugin(dir, settings, tokenSource))
			}
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&dirs, "claude-config", nil, "Claude config folder to check (repeatable; default $CLAUDE_CONFIG_DIR or ~/.claude)")
	return cmd
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// checkKit covers the binary, config and state: the skill runs the bare
// command name, so the binary on PATH must be this one.
func checkKit() checkResult {
	if _, err := loadConfig(); err != nil {
		return fail(err.Error(), "fix the key named in the error; see docs/config.md")
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err.Error(), "")
	}
	var meta struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		if err := json.Unmarshal(data, &meta); err != nil {
			return fail(fmt.Sprintf("%s/state.json is unreadable: %v", dir, err), "")
		}
		if meta.SchemaVersion > buildinfo.SchemaVersion {
			return fail(fmt.Sprintf("state schema v%d is newer than this binary (v%d)", meta.SchemaVersion, buildinfo.SchemaVersion),
				"herdr-runner-kit upgrade")
		}
	}
	bin, err := ownBinary()
	if err != nil {
		return fail(err.Error(), "")
	}
	onPath, err := exec.LookPath("herdr-runner-kit")
	if err != nil {
		return fail("herdr-runner-kit is not on PATH", "put "+filepath.Dir(bin)+" on PATH, or install the binary into a folder on PATH")
	}
	if resolved, err := filepath.EvalSymlinks(onPath); err == nil {
		onPath = resolved
	}
	if onPath != bin {
		return warn("herdr-runner-kit on PATH is "+onPath+", not this binary ("+bin+")", "run doctor with the binary on PATH")
	}
	return ok("%s, state schema v%d", buildinfo.Version, buildinfo.SchemaVersion)
}

func checkHerdr(ctx context.Context) checkResult {
	version, err := output(ctx, "herdr", "--version")
	if err != nil {
		return fail("herdr is not installed", "install herdr: https://herdr.dev")
	}
	status, _ := output(ctx, "herdr", "status", "server")
	if !strings.Contains(status, "status: running") {
		return fail(strings.TrimSpace(version)+", server not running", "start herdr")
	}
	return ok("%s, server running", strings.TrimSpace(version))
}

func herdrIntegrations(ctx context.Context) map[string]string {
	out, _ := output(ctx, "herdr", "integration", "status")
	status := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if name, rest, found := strings.Cut(line, ": "); found {
			status[strings.TrimSpace(name)] = rest
		}
	}
	return status
}

func checkIntegration(status map[string]string, name string) checkResult {
	s := status[name]
	if strings.HasPrefix(s, "current") {
		return ok("%s", s)
	}
	if s == "" {
		s = "unknown"
	}
	return fail(s, "herdr integration install "+name)
}

// checkHelp checks that a CLI is installed and that its --help still lists
// the flags the kit passes to it.
func checkHelp(ctx context.Context, name string, flags ...string) checkResult {
	if _, err := exec.LookPath(name); err != nil {
		return fail(name+" is not installed", "install "+name)
	}
	if len(flags) == 0 {
		v, _ := output(ctx, name, "--version")
		return ok("%s", strings.TrimSpace(v))
	}
	help, _ := output(ctx, name, "--help")
	var missing []string
	for _, f := range flags {
		if !strings.Contains(help, f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fail(name+" --help no longer lists "+strings.Join(missing, ", "), "upgrade herdr-runner-kit, or report this as an issue")
	}
	v, _ := output(ctx, name, "--version")
	return ok("%s", strings.TrimSpace(v))
}

// checkTelegramCreds also returns the file the kit reads the token from.
func checkTelegramCreds() (checkResult, string) {
	cfg, err := loadConfig()
	if err != nil {
		return fail(err.Error(), ""), ""
	}
	creds, err := telegram.Resolve(cfg.Telegram, os.Getenv)
	if errors.Is(err, telegram.ErrNoCredentials) {
		return warn("Telegram is off: "+err.Error(), "set telegram.envFile, or set up the Telegram plugin; see docs/telegram.md"), ""
	}
	if err != nil {
		return fail(err.Error(), "see docs/telegram.md"), ""
	}
	return ok("token from %s, %d chat(s)", creds.Source, len(creds.ChatIDs)), creds.Source
}

type claudeSettings struct {
	raw            string
	EnabledPlugins map[string]bool `json:"enabledPlugins"`
	SkipBypass     bool            `json:"skipDangerousModePermissionPrompt"`
}

func readSettings(path string) claudeSettings {
	var s claudeSettings
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	s.raw = string(data)
	return s
}

func checkSkill(dir string) checkResult {
	version, intact, err := install.SkillVersion(dir)
	switch {
	case err != nil:
		return fail("not installed", "herdr-runner-kit setup --claude-config "+dir)
	case !intact:
		return warn("SKILL.md was edited by you, so setup no longer updates it", "herdr-runner-kit setup --force --claude-config "+dir)
	case version != buildinfo.Version:
		return fail("installed by "+version+", this binary is "+buildinfo.Version, "herdr-runner-kit setup --claude-config "+dir)
	}
	return ok("%s", version)
}

func checkHook(dir string) checkResult {
	cmd, err := install.HookFor(filepath.Join(dir, "settings.json"))
	if err != nil {
		return fail(err.Error(), "")
	}
	if cmd == "" {
		return warn("not installed; the main agent must rerun wait itself", "herdr-runner-kit setup --claude-config "+dir)
	}
	bin, err := ownBinary()
	if err == nil && cmd != install.HookCommand(bin) {
		return fail("points at another binary: "+cmd, "herdr-runner-kit setup --claude-config "+dir)
	}
	return ok("installed")
}

// checkHerdrHook checks that herdr's Claude state hook is registered in this
// config's settings.json, not just present as a script, since herdr only
// sees a runner's state through it.
func checkHerdrHook(dir string, s claudeSettings) checkResult {
	if strings.Contains(s.raw, "herdr-agent-state") {
		return ok("registered")
	}
	return fail("herdr's Claude state hook is not registered in "+filepath.Join(dir, "settings.json"),
		"CLAUDE_CONFIG_DIR="+dir+" herdr integration install claude")
}

func checkBypass(dir string, s claudeSettings) checkResult {
	if s.SkipBypass {
		return ok("accepted")
	}
	var global struct {
		Accepted bool `json:"bypassPermissionsModeAccepted"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, ".claude.json")); err == nil {
		_ = json.Unmarshal(data, &global)
	}
	if global.Accepted {
		return ok("accepted")
	}
	return fail("Claude runners would stop at the --dangerously-skip-permissions prompt",
		"run CLAUDE_CONFIG_DIR="+dir+" claude --dangerously-skip-permissions once and accept the prompt")
}

func checkTelegramPlugin(dir string, s claudeSettings, tokenSource string) checkResult {
	var installed struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "plugins", "installed_plugins.json")); err == nil {
		_ = json.Unmarshal(data, &installed)
	}
	if _, found := installed.Plugins[telegramPlugin]; !found {
		return warn("not installed; Telegram replies cannot reach the main session",
			"CLAUDE_CONFIG_DIR="+dir+" claude plugin install "+telegramPlugin+" && CLAUDE_CONFIG_DIR="+dir+" claude plugin disable "+telegramPlugin)
	}
	if _, err := exec.LookPath("bun"); err != nil {
		return fail("the plugin's server needs bun, which is not installed", "curl -fsSL https://bun.sh/install | bash")
	}
	stateDir := filepath.Join(dir, "channels", "telegram")
	var access struct {
		DMPolicy  string            `json:"dmPolicy"`
		AllowFrom []json.RawMessage `json:"allowFrom"`
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "access.json"))
	if err != nil {
		return fail("no access list, so your replies are dropped", "herdr-runner-kit setup --claude-config "+dir)
	}
	if err := json.Unmarshal(data, &access); err != nil {
		return fail("access.json is unreadable: "+err.Error(), "")
	}
	if len(access.AllowFrom) == 0 {
		return fail("access.json allows no chat", "pair your chat: /telegram:access pair <code>")
	}
	if access.DMPolicy != "allowlist" {
		return warn("dmPolicy is "+access.DMPolicy+"; strangers get pairing replies", "/telegram:access policy allowlist")
	}
	if env := filepath.Join(stateDir, ".env"); hasToken(env) {
		fix := "delete " + env + "; herdr-runner-kit main --exec passes the token to the main session only"
		if filepath.Clean(tokenSource) == filepath.Clean(env) {
			// The kit reads its token from this file, so deleting it alone would turn Telegram off.
			fix = "move the token into a file named by telegram.envFile in the kit's config, then " + fix
		}
		return warn("the plugin's .env holds a bot token, so any Claude session that loads the plugin polls the bot and takes over replies", fix)
	}
	if s.EnabledPlugins[telegramPlugin] {
		return warn("enabled for every session; each one starts the plugin, which fails without a token",
			"CLAUDE_CONFIG_DIR="+dir+" claude plugin disable "+telegramPlugin+"; herdr-runner-kit main enables it for the main session")
	}
	return ok("installed, %d chat(s) allowed; only `herdr-runner-kit main` loads it", len(access.AllowFrom))
}

func hasToken(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	data, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	return strings.Contains(string(data), "TELEGRAM_BOT_TOKEN=")
}
