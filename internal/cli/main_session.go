package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/install"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
	"github.com/khuebt/herdr-runner-kit/internal/wait"
)

// telegramChannel is the official Telegram channel plugin. The main session
// runs it, so replies on Telegram reach the main agent.
const telegramChannel = "plugin:" + telegramPlugin

// mainSettings enables the plugin for the main session only. It stays
// disabled in settings.json, so no other session starts its bot poller.
const mainSettings = `{"enabledPlugins":{"` + telegramPlugin + `":true}}`

func newMainCommand() *cobra.Command {
	var run bool
	cmd := &cobra.Command{
		Use:   "main",
		Short: "Print, or run, the command that starts the main session with Telegram",
		Long: "main prints the command that starts the main Claude Code session with the " +
			"Telegram channel and the main-agent skill in its system prompt. With --exec it " +
			"runs it and passes the kit's bot token to the Telegram plugin. The plugin is " +
			"enabled for this session only, so no other Claude session polls the bot. Run " +
			"only one such session per bot: it answers tasks of every repository.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			argv := []string{"claude", "--settings", mainSettings, "--channels", telegramChannel}
			targets, err := claudeConfigs(nil)
			if err != nil {
				return err
			}
			skillFile := filepath.Join(install.SkillDir(targets[0]), "SKILL.md")
			if _, err := os.Stat(skillFile); err == nil {
				argv = append(argv, "--append-system-prompt-file", skillFile)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: no skill at %s, so the session starts without the routing rules; run hrk setup, or set CLAUDE_CONFIG_DIR\n", skillFile)
			}
			if !run {
				shown := slices.Clone(argv)
				shown[2] = "'" + mainSettings + "'"
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "HRK_MAIN=1 "+strings.Join(shown, " "))
				return err
			}
			path, err := exec.LookPath(argv[0])
			if err != nil {
				return fmt.Errorf("claude is not on PATH: %w", err)
			}
			// Exec keeps this PID, so HRK_MAIN names the Claude process the
			// auto re-arm hook belongs to.
			env := withEnv(os.Environ(), "HRK_MAIN", strconv.Itoa(os.Getpid()))
			if os.Getenv("TELEGRAM_BOT_TOKEN") == "" {
				if cfg, err := loadConfig(); err == nil {
					if creds, err := telegram.Resolve(cfg.Telegram, os.Getenv); err == nil {
						env = withEnv(env, "TELEGRAM_BOT_TOKEN", creds.Token)
					}
				}
			}
			return syscall.Exec(path, argv, env)
		},
	}
	cmd.Flags().BoolVar(&run, "exec", false, "start the main session now, with the bot token from the kit's config")
	return cmd
}

// withEnv sets key in env, dropping earlier entries: exec does not dedupe,
// and a lookup would find an inherited empty value first.
func withEnv(env []string, key, val string) []string {
	env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, key+"=") })
	return append(env, key+"="+val)
}

// newAutoarmCommand is the main session's asynchronous Stop hook. It runs
// wait in the background after every turn of the main agent. On news it
// prints the lines and exits with install.NewsExit, which the hook turns into
// the 2 that wakes the agent; when nothing is running, or another wait already
// holds the cursor, it exits quietly. Errors are swallowed on purpose: waking
// the agent with one would end in another Stop, and so another error, in a
// loop.
func newAutoarmCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__autoarm",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var payload struct {
				Cwd       string `json:"cwd"`
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(io.LimitReader(cmd.InOrStdin(), 1<<20)).Decode(&payload)
			main := os.Getenv("HRK_MAIN")
			if os.Getenv("HRK_TASK") != "" || main == "" && payload.SessionID == "" {
				return nil // not the main session; skip opening the state
			}
			if payload.Cwd == "" {
				payload.Cwd, _ = os.Getwd()
			}
			r, err := openRunner()
			if err != nil || !isMainSession(os.Getenv, payload.SessionID, r.Store) {
				return nil
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			ctx = whileAlive(ctx, mainPID(main))
			if code := autoarm(ctx, r, runner.CallerOwner(ctx, payload.Cwd), cmd.ErrOrStderr()); code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
}

// autoarmHolder names the hook in the cursor lock.
const autoarmHolder = "hook"

// isMainSession reports whether the hook runs in the main session. Only it
// consumes news here: another Claude session in the same folder would take
// the cursor and hide news from main, and a runner has its own turn-end hook.
// The session hrk main starts has HRK_MAIN, and its Claude session id is
// recorded; a resume (herdr restoring it, or claude --resume) loses the
// environment but keeps the id.
func isMainSession(getenv func(string) string, sessionID string, s *state.Store) bool {
	if getenv("HRK_TASK") != "" {
		return false
	}
	if getenv("HRK_MAIN") != "" {
		_ = s.RecordMainSession(sessionID)
		return true
	}
	return s.IsMainSession(sessionID)
}

// mainPID is the PID of the main session's Claude process: HRK_MAIN when it
// holds one, else the parent of the hook's shell. It returns 0 when neither
// is known, and then whileAlive watches only the shell.
func mainPID(env string) int {
	if pid, err := strconv.Atoi(env); err == nil && pid > 1 {
		return pid
	}
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(os.Getppid())).Output()
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 1 {
		return 0
	}
	return pid
}

// aliveEvery is how often whileAlive looks for its process.
var aliveEvery = 5 * time.Second

// whileAlive cancels ctx once process pid is gone, or once the hook's own
// shell is, which happens when Claude kills only the shell on its timeout. A
// hook that outlived either would otherwise hold the cursor lock, and swallow
// the news it can no longer deliver. A pid of 1 or less watches only the
// shell.
func whileAlive(ctx context.Context, pid int) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	parent := os.Getppid()
	go func() {
		tick := time.NewTicker(aliveEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if os.Getppid() != parent || (pid > 1 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx
}

// autoarm waits for the news of the owner and of every project, re-arming
// across wait's own timeout. It returns install.NewsExit after printing news
// for Claude, and 0 when there is nothing to report. Projects go by the owner
// of each task, never by the registry, so a project started after the hook
// armed is watched, and a registry that cannot be read changes nothing.
func autoarm(ctx context.Context, r *runner.Runner, owner string, stderr io.Writer) int {
	watched := func(t *task.Task) bool { return t.Owner == owner || task.IsProjectOwner(t.Owner) }
	// A folder that never spawned a task, with no project task either, has
	// no news, so skip wait and its cursor file.
	tasks, err := r.Store.List()
	if err != nil || !slices.ContainsFunc(tasks, watched) {
		return 0
	}
	var gcLock func()
	var retry time.Time
	for {
		res, err := wait.Run(ctx, r, wait.Options{Owner: owner, Projects: true, Holder: autoarmHolder})
		switch {
		case err != nil:
			return 0
		case res.Idle != "":
			// wait closes finished runners while it watches. With nothing
			// left to watch, one hook per owner stays to close them on
			// time; every turn starts a hook, and the others leave.
			if gcLock == nil {
				if gcLock, err = r.Store.LockCursor(state.CursorName(owner)+"-gc", "gc"); err != nil {
					return 0
				}
				defer gcLock()
			}
			tasks, err := r.Store.List()
			if err != nil {
				return 0
			}
			tasks = slices.DeleteFunc(tasks, func(t *task.Task) bool { return !watched(t) })
			due, ok := runner.NextGC(tasks, "", r.Config.KeepFor)
			if !ok {
				return 0
			}
			// A runner gc skipped or could not close stays due, so the next
			// try waits at least GCRetry.
			select {
			case <-ctx.Done():
				return 0
			case <-time.After(max(time.Until(due), time.Until(retry))):
			}
			retry = time.Now().Add(runner.GCRetry)
			owners := []string{owner}
			for _, t := range tasks {
				if !slices.Contains(owners, t.Owner) {
					owners = append(owners, t.Owner)
				}
			}
			for _, o := range owners {
				if _, err := r.GC(ctx, o, runner.GCAge, false, false); err != nil {
					return 0
				}
			}
		case res.TimedOut:
		default:
			fmt.Fprintf(stderr, "herdr-runner-kit news\n%s\nRelay this to the user as is. This hook re-arms wait when your turn ends.\n",
				strings.Join(res.Lines, "\n"))
			return install.NewsExit
		}
	}
}

func newNotifyCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__notify <event-json>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var ev state.Event
			if err := json.Unmarshal([]byte(args[0]), &ev); err != nil {
				return err
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			return r.SendNotification(context.Background(), ev)
		},
	}
}
