package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/install"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
	"github.com/khuebt/herdr-runner-kit/internal/upgrade"
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
	var tools bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that herdr, the agents, Telegram and the kit's own files are ready",
		Long: "doctor checks every dependency and setting the kit relies on and prints the fix " +
			"for each problem. It changes nothing. It exits 1 when a check fails; warnings " +
			"point at optional features, or at kit state that breaks a rule of the state " +
			"model (the state lines). --tools also lists the optional tools project " +
			"stage templates suggest when present; a missing tool is never a failure.",
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
			for _, r := range checkState(ctx) {
				show("state", r)
			}
			show("updates", checkUpdates(ctx))
			show("herdr", checkHerdr(ctx))
			integrations := herdrIntegrations(ctx)
			cfg, _ := loadConfig() // the kit check reports a config error
			used := configHarnesses(cfg)
			gate := func(a harness.Adapter, r checkResult) checkResult {
				if a.Checks().Required {
					return r
				}
				return optional(r, used[a.Harness()])
			}
			for _, a := range harness.All() {
				c := a.Checks()
				show("herdr integration "+c.Integration, gate(a, checkIntegration(integrations, c.Integration)))
			}
			for _, a := range harness.All() {
				c := a.Checks()
				show(c.Bin, gate(a, checkHelp(ctx, c.Bin, c.Flags...)))
			}
			show("git", checkHelp(ctx, "git"))
			show("runner memory cap", memoryCapCheck(cfg.RunnerMemoryMax, runner.ScopeProblem(ctx, cfg.RunnerMemoryMax, runtime.GOOS)))
			if runtime.GOOS == "linux" {
				unit, pref, err := herdrOOMPreference(ctx)
				show("herdr oomd", oomPreferenceCheck(runtime.GOOS, unit, pref, err))
			}
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
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			repo := runner.Toplevel(ctx, cwd)
			for _, dir := range runnerClaudeConfigs(targets) {
				show("claude config "+claudeConfigLabel(dir), checkClaudeConfig(dir, cwd, repo))
			}
			if tools {
				_, lines := newToolProbe(targets).detect(ctx)
				for _, l := range lines {
					show("tool "+l.name, l.result)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&dirs, "claude-config", nil, "Claude config folder to check (repeatable; default $CLAUDE_CONFIG_DIR or ~/.claude)")
	cmd.Flags().BoolVar(&tools, "tools", false, "also list optional tools: lavish, npx, draw.io, Penpot, a headless PDF browser, soffice, STITCH_API_KEY")
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
				"hrk upgrade")
		}
	}
	bin, err := ownBinary()
	if err != nil {
		return fail(err.Error(), "")
	}
	onPath, err := exec.LookPath("hrk")
	if err != nil {
		return fail("hrk is not on PATH", notOnPathFix(bin))
	}
	if resolved, err := filepath.EvalSymlinks(onPath); err == nil {
		onPath = resolved
	}
	if onPath != bin {
		return warn("hrk on PATH is "+onPath+", not this binary ("+bin+")", "run doctor with the binary on PATH")
	}
	if old, resolved := staleAlias(bin); old != "" {
		return warn("herdr-runner-kit on PATH is "+resolved+", not this binary ("+bin+")", "delete "+old+", or link it here: ln -sf "+bin+" "+old)
	}
	return ok("%s, state schema v%d", buildinfo.Version, buildinfo.SchemaVersion)
}

// stateTimeout bounds the state check, so a large store cannot hang doctor.
const stateTimeout = 10 * time.Second

// checkState checks every rule of docs/state-model.md on the store: one ok
// line, or one warning per broken rule. It never fails doctor.
func checkState(ctx context.Context) []checkResult {
	dir, err := stateDir()
	if err != nil {
		return nil // the kit check reports it
	}
	if _, err := os.Stat(filepath.Join(dir, "tasks")); errors.Is(err, fs.ErrNotExist) {
		return []checkResult{ok("no state yet")}
	}
	store, err := state.Open(dir)
	if err != nil {
		return []checkResult{warn(err.Error(), "")}
	}
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	s, vs, err := stateViolations(ctx, &runner.Runner{Store: store}, true)
	var out []checkResult
	for _, v := range vs {
		fix := cmp.Or(stateFixes[v.Rule], stateFixBug)
		if v.Task != "" {
			fix = strings.ReplaceAll(fix, "<id>", v.Task)
		}
		out = append(out, warn(v.String(), fix))
	}
	if err != nil {
		out = append(out, warn(err.Error(), ""))
	}
	if len(out) == 0 {
		return []checkResult{ok("%d tasks, %d projects, every rule holds", len(s.Tasks), len(s.Projects))}
	}
	return out
}

// stateViolations takes a snapshot of r's store with the runtime settle
// window and returns the rules it breaks, with the rules that need git when
// git is set. Those are dropped when ctx ends first, because a git command
// cut short reads as a missing branch or ref.
func stateViolations(ctx context.Context, r *runner.Runner, git bool) (runner.Snapshot, []runner.Violation, error) {
	s, err := r.Snapshot()
	if err != nil {
		return s, nil, err
	}
	s.Settle = runner.SettleWindow
	vs := runner.CheckInvariants(s)
	if git {
		g := r.CheckGit(ctx, s)
		if ctx.Err() != nil {
			return s, vs, fmt.Errorf("the git checks did not finish: %w", ctx.Err())
		}
		vs = append(vs, g...)
	}
	return s, vs, nil
}

// warnState prints a "state check:" line through warnf for each rule the
// store breaks that keep selects; t is the task the violation names, or
// nil. A line means a kit bug; the command that calls it still succeeds.
func warnState(ctx context.Context, r *runner.Runner, warnf func(string), git bool, keep func(v runner.Violation, t *task.Task) bool) {
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	s, vs, err := stateViolations(ctx, r, git)
	byID := map[string]*task.Task{}
	for _, t := range s.Tasks {
		byID[t.ID] = t
	}
	for _, v := range vs {
		if keep(v, byID[v.Task]) {
			warnf("state check: " + v.String())
		}
	}
	if err != nil {
		warnf("state check: " + err.Error())
	}
}

// stateFixBug is the fix of a broken rule only a kit bug can cause.
const stateFixBug = "a kit bug: report this line with hrk show of the task it names"

// stateFixes is the fix doctor prints for each broken rule.
var stateFixes = map[string]string{
	"waiting-task-has-a-path":           "run `hrk wait` once so the settle sweep decides it, or `hrk stop <id>`",
	"plan-run-finishes":                 "run `hrk wait` once so the settle sweep emits plan-done",
	"stopped-gets-no-agent":             "`hrk gc` closes its runner",
	"links-resolve":                     "`hrk stop <id>` while it waits; " + stateFixBug,
	"one-live-runner-per-step":          "`hrk stop` the older runner",
	"child-follows-current-parents":     "run `hrk project milestone chain <M>` again; it replaces the stale queued runner",
	"base-is-a-parent-branch":           "run `hrk project milestone chain <M>` again; it replaces the stale queued runner",
	"chain-matches-plan":                "`hrk stop <id>`, then run `hrk project milestone chain <M>` again",
	"done-milestone-has-release":        "`hrk project milestone reopen <M> --reason WHY`, then close and done it again",
	"done-milestone-has-no-live-runner": "`hrk stop <id>`",
	"base-branch-exists":                "`hrk stop <id>`; a chain step: run its parent again through `hrk project milestone chain <M>`",
	"cleanup-spares-live-work":          "`hrk stop <id>` and spawn the work again",
	"live-task-has-checkout":            "`hrk stop <id>` and spawn the work again",
	"merged-tip-has-ref":                "run `git update-ref <ref> <commit>` with the ref and commit named, in the task's repository",
	"cursor-within-log":                 "stop every `hrk wait`, then delete that cursor file in the state folder; the next wait starts at the end of the log",
}

// notOnPathFix is the fix when no hrk is on PATH. A binary still named
// herdr-runner-kit, from before the rename, needs an hrk link, which setup
// makes when it can.
func notOnPathFix(bin string) string {
	if filepath.Base(bin) == "herdr-runner-kit" {
		short := shortLinkPath(bin)
		return "ln -sf herdr-runner-kit " + short + ", and keep " + filepath.Dir(short) + " on PATH"
	}
	return "put " + filepath.Dir(bin) + " on PATH, or install the binary into a folder on PATH"
}

// staleAlias finds a herdr-runner-kit on PATH that is another binary than
// bin, such as one left by a go install from before the rename. It returns
// its path and where it leads, or "" when there is none.
func staleAlias(bin string) (string, string) {
	old, err := exec.LookPath("herdr-runner-kit")
	if err != nil {
		return "", ""
	}
	resolved := old
	if r, err := filepath.EvalSymlinks(old); err == nil {
		resolved = r
	}
	if resolved == bin {
		return "", ""
	}
	return old, resolved
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

// optional turns the failure of a check for an optional harness into a
// warning when the config does not use that harness.
func optional(r checkResult, used bool) checkResult {
	if !used && r.level == "fail" {
		r.level = "warn"
		r.detail += " (not used by your config)"
	}
	return r
}

// configHarnesses lists the harnesses the config's kinds and quotaFallback
// name. A config that failed to load is the zero Config, which names none.
func configHarnesses(cfg config.Config) map[task.Harness]bool {
	used := map[task.Harness]bool{}
	add := func(agent string) {
		if spec, err := task.ParseAgentSpec(agent); err == nil {
			used[spec.Harness] = true
		}
	}
	for _, k := range cfg.Kinds {
		add(k.Agent)
	}
	for _, f := range cfg.QuotaFallback {
		add(f.Agent)
	}
	return used
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

// memoryCapCheck reports whether each runner runs under its own memory
// cap, given the reason it cannot from runner.ScopeProblem.
func memoryCapCheck(memoryMax string, problem error) checkResult {
	const shared = "runners share herdr's memory"
	switch {
	case problem == nil:
		return ok("%s per runner (systemd scope)", memoryMax)
	case memoryMax == config.MemoryOff:
		return ok("off; %s", shared)
	case errors.Is(problem, runner.ErrNoSystemd):
		return ok("%s; %s", problem, shared)
	}
	return warn(problem.Error()+"; "+shared,
		"make `busctl --user status` reach the systemd user manager where herdr runs, or set runnerMemoryMax: off to silence this")
}

// oomPreferenceCheck reports whether systemd-oomd is told to spare herdr's
// unit. unit is "" when herdr does not run as a systemd service. Off Linux
// it returns the zero result, which doctor does not print.
func oomPreferenceCheck(goos, unit, preference string, err error) checkResult {
	switch {
	case goos != "linux":
		return checkResult{}
	case err != nil:
		return warn("could not read herdr's oomd preference: "+err.Error(), "check that `systemctl --user` reaches the systemd user manager where herdr runs")
	case unit == "":
		return ok("herdr does not run as a systemd service; nothing to check")
	case preference == "avoid" || preference == "omit":
		return ok("%s: ManagedOOMPreference=%s", unit, preference)
	}
	return warn(unit+" has no ManagedOOMPreference; under memory pressure systemd-oomd may kill herdr and every session in it",
		"systemctl --user edit "+unit+", add `[Service]` and `ManagedOOMPreference=avoid`, then systemctl --user daemon-reload (applies at the next herdr restart)")
}

// herdrOOMPreference finds the systemd unit of the herdr server that owns
// the socket herdr reports, and reads its ManagedOOMPreference. unit is ""
// when the server does not run in a .service cgroup. Linux only.
func herdrOOMPreference(ctx context.Context) (unit, preference string, err error) {
	status, _ := output(ctx, "herdr", "status", "server")
	var socket string
	for line := range strings.SplitSeq(status, "\n") {
		if v, found := strings.CutPrefix(line, "socket:"); found {
			socket = strings.TrimSpace(v)
		}
	}
	if socket == "" {
		return "", "", errors.New("`herdr status server` names no socket")
	}
	pid, err := socketOwner(socket)
	if err != nil {
		return "", "", err
	}
	cgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", "", err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(cgroup)), "\n") {
		if path, found := strings.CutPrefix(line, "0::"); found {
			unit = filepath.Base(path)
		}
	}
	if !strings.HasSuffix(unit, ".service") {
		return "", "", nil
	}
	// systemctl over busctl: busctl needs the unit's escaped object path.
	out, err := output(ctx, "systemctl", "--user", "show", unit, "-p", "LoadState", "-p", "ManagedOOMPreference")
	if err != nil {
		return unit, "", fmt.Errorf("systemctl --user show %s: %s", unit, firstLine(out, err))
	}
	props := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, found := strings.Cut(line, "="); found {
			props[k] = strings.TrimSpace(v)
		}
	}
	if props["LoadState"] != "loaded" {
		// A system service or a gone unit: the user manager only reports defaults.
		return unit, "", fmt.Errorf("%s is not a loaded user unit (LoadState=%s)", unit, props["LoadState"])
	}
	return unit, props["ManagedOOMPreference"], nil
}

// socketOwner returns the pid of a process holding a socket bound to path:
// every socket in /proc/net/unix with that path belongs to the server that
// listens there. ponytail: scans every /proc/<pid>/fd; SO_PEERCRED would
// need a Linux-only file.
func socketOwner(path string) (int, error) {
	data, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return 0, err
	}
	inodes := map[string]bool{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 8 && f[7] == path {
			inodes["socket:["+f[6]+"]"] = true
		}
	}
	if len(inodes) == 0 {
		return 0, fmt.Errorf("no socket is bound to %s", path)
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)) // other users' processes are unreadable
		for _, fd := range fds {
			if link, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); inodes[link] {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("no readable process holds %s", path)
}

// firstLine is a command's first output line, or its error when it printed nothing.
func firstLine(out string, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); line != "" {
		return line
	}
	return err.Error()
}

// checkTelegramCreds also returns the file the kit reads the token from.
// checkUpdates notes a newer release, looking on GitHub at most once a day.
func checkUpdates(ctx context.Context) checkResult {
	cfg, err := loadConfig()
	switch {
	case err == nil && !cfg.UpdateCheck:
		return ok("not checked (updateCheck: false)")
	case upgrade.Major(buildinfo.Version) < 0:
		return ok("not checked: %s build", buildinfo.Version)
	}
	latest, newer, err := updateNote(ctx, 24*time.Hour)
	switch {
	case err != nil:
		return ok("could not check for a newer release: %v", err)
	case newer:
		return warn(latest+" is available (this is "+buildinfo.Version+")", "hrk upgrade --check, then hrk upgrade")
	}
	return ok("%s is the latest release", buildinfo.Version)
}

func checkTelegramCreds() (checkResult, string) {
	cfg, err := loadConfig()
	if err != nil {
		return fail(err.Error(), ""), ""
	}
	creds, err := telegram.Resolve(cfg.Telegram, os.Getenv)
	if errors.Is(err, telegram.ErrNoCredentials) {
		fix := "set telegram.envFile, or set up the Telegram plugin; see docs/telegram.md"
		if telegram.Off(os.Getenv) {
			fix = "unset " + telegram.OffEnv + " to turn it on"
		}
		return warn("Telegram is off: "+err.Error(), fix), ""
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
		return fail("not installed", "hrk setup --claude-config "+dir)
	case !intact:
		return warn("SKILL.md was edited by you, so setup no longer updates it", "hrk setup --force --claude-config "+dir)
	case version != buildinfo.Version:
		return fail("installed by "+version+", this binary is "+buildinfo.Version, "hrk setup --claude-config "+dir)
	}
	return ok("%s", version)
}

func checkHook(dir string) checkResult {
	cmd, err := install.HookFor(filepath.Join(dir, "settings.json"))
	if err != nil {
		return fail(err.Error(), "")
	}
	if cmd == "" {
		return warn("not installed; the main agent must rerun wait itself", "hrk setup --claude-config "+dir)
	}
	bin, err := ownBinary()
	if err == nil && cmd != install.HookCommand(bin) {
		return fail("points at another binary: "+cmd, "hrk setup --claude-config "+dir)
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
		return fail("no access list, so your replies are dropped", "hrk setup --claude-config "+dir)
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
		fix := "delete " + env + "; hrk main --exec passes the token to the main session only"
		if abs, err := filepath.Abs(tokenSource); err == nil && abs == env {
			// The kit reads its token from this file, so deleting it alone would turn Telegram off.
			fix = "move the token into a file named by telegram.envFile in the kit's config, then " + fix
		}
		return warn("the plugin's .env holds a bot token, so any Claude session that loads the plugin polls the bot and takes over replies", fix)
	}
	if s.EnabledPlugins[telegramPlugin] {
		return warn("enabled for every session; each one starts the plugin, which fails without a token",
			"CLAUDE_CONFIG_DIR="+dir+" claude plugin disable "+telegramPlugin+"; hrk main enables it for the main session")
	}
	return ok("installed, %d chat(s) allowed; only `hrk main` loads it", len(access.AllowFrom))
}

// runnerClaudeConfigs returns the Claude config folders runners start with:
// the checked folders plus each folder a Claude task recorded, mapped the way
// spawn maps them, so ~/.claude is "" (Claude's default). A recorded folder
// that no longer exists is skipped, because spawn refuses it now.
func runnerClaudeConfigs(targets []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		dir = harness.DefaultClaudeConfigDir(dir)
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	for _, dir := range targets {
		add(dir)
	}
	sd, err := stateDir()
	if err != nil {
		return out
	}
	// Read the task files directly: doctor changes nothing, and opening the
	// store would create the state folder.
	files, _ := filepath.Glob(filepath.Join(sd, "tasks", "*", "task.json"))
	for _, f := range files {
		var rec struct {
			Agent           string `json:"agent"`
			ClaudeConfigDir string `json:"claudeConfigDir"`
		}
		data, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(data, &rec) != nil {
			continue
		}
		if spec, err := task.ParseAgentSpec(rec.Agent); err != nil || spec.Harness != task.Claude {
			continue
		}
		if rec.ClaudeConfigDir != "" {
			if info, err := os.Stat(rec.ClaudeConfigDir); err != nil || !info.IsDir() {
				continue
			}
		}
		add(rec.ClaudeConfigDir)
	}
	return out
}

// claudeConfigLabel names a runner config folder; "" is Claude's default.
func claudeConfigLabel(dir string) string {
	if dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude")
	}
	return "~/.claude"
}

// checkClaudeConfig fails when Claude has never run with config folder dir,
// since a runner there stops at login and trust screens the kit cannot
// prepare. Whether the current repository is trusted is information only:
// the kit marks each runner's folder trusted at launch.
func checkClaudeConfig(dir, cwd, repo string) checkResult {
	path, err := harness.ClaudeConfigPath(dir)
	if err != nil {
		return fail(err.Error(), "")
	}
	where := repo
	if where == "" {
		where = cwd
	}
	run := "claude"
	if dir != "" {
		run = "CLAUDE_CONFIG_DIR=" + dir + " claude"
	}
	key, err := harness.ClaudeTrusted(path, trustCandidates(cwd, repo))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail("Claude has never run with "+claudeConfigLabel(dir)+": "+path+" is missing",
			"run `"+run+"` once in "+where+", log in and accept the trust dialog")
	case err != nil:
		return fail(path+" is unreadable: "+err.Error(),
			"repair "+path+", or run `"+run+"` once in "+where+" so Claude rewrites it")
	case key != "":
		return ok("%s is trusted (projects[%q].%s in %s)", where, key, harness.ClaudeTrustFlag, path)
	}
	return ok("%s is not trusted in %s (no projects[%q].%s); the kit marks each runner's folder trusted at launch",
		where, path, filepath.Clean(cwd), harness.ClaudeTrustFlag)
}

// trustCandidates lists cwd and each parent up to the repository root, the
// exact keys a trust entry for this repository could have.
func trustCandidates(cwd, repo string) []string {
	cwd, repo = filepath.Clean(cwd), filepath.Clean(repo)
	paths := []string{cwd}
	if repo == "." || (cwd != repo && !strings.HasPrefix(cwd, repo+string(filepath.Separator))) {
		if repo != "." && repo != cwd {
			paths = append(paths, repo)
		}
		return paths
	}
	for p := cwd; p != repo; {
		p = filepath.Dir(p)
		paths = append(paths, p)
	}
	return paths
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
