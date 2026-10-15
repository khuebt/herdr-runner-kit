package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/install"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
)

// defaultConfig is written by setup when the user has no config file. Every
// key is optional, so it only names the schema and points at the docs.
const defaultConfig = `# herdr-runner-kit configuration. Every key is optional; see docs/config.md.
schemaVersion: 1
`

// claudeConfigs returns the Claude config folders a command targets: the
// --claude-config flags, else $CLAUDE_CONFIG_DIR, else ~/.claude.
func claudeConfigs(dirs []string) ([]string, error) {
	if len(dirs) == 0 {
		if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
			dirs = []string{dir}
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			dirs = []string{filepath.Join(home, ".claude")}
		}
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if rest, ok := strings.CutPrefix(d, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			d = filepath.Join(home, rest)
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	return out, nil
}

func stateDir() (string, error) {
	if dir := os.Getenv("HRK_STATE_DIR"); dir != "" {
		return dir, nil
	}
	return state.DefaultDir()
}

func ownBinary() (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate own binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	return bin, nil
}

func newSetupCommand() *cobra.Command {
	var dirs []string
	var force, noHook bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install the main-agent skill and the auto re-arm hook; create config and state",
		Long: "setup writes the default config if there is none, creates the state folder, and " +
			"installs the main-agent skill and the auto re-arm Stop hook into each Claude config " +
			"folder. When Telegram credentials are configured and the Telegram plugin has no " +
			"access list yet, it allows your chat. Running it again changes nothing.",
		Example: `  hrk setup
  hrk setup --claude-config ~/.claude --claude-config ~/.claude2`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			targets, err := claudeConfigs(dirs)
			if err != nil {
				return err
			}
			bin, err := ownBinary()
			if err != nil {
				return err
			}
			changed := 0
			note := func(format string, a ...any) {
				changed++
				fmt.Fprintf(out, format+"\n", a...)
			}
			// First, so a setup that stops on an error below still leaves hrk.
			if short, err := linkShortName(bin); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: no hrk link at %s: %v\n", shortLinkPath(bin), err)
			} else if short != "" {
				note("linked %s to herdr-runner-kit", short)
			}

			cfgPath, err := config.Path()
			if err != nil {
				return err
			}
			if _, err := os.Stat(cfgPath); errors.Is(err, fs.ErrNotExist) {
				if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(cfgPath, []byte(defaultConfig), 0o644); err != nil {
					return err
				}
				note("created %s", cfgPath)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			dir, err := stateDir()
			if err != nil {
				return err
			}
			_, err = os.Stat(dir)
			fresh := errors.Is(err, fs.ErrNotExist)
			if _, err := state.Open(dir); err != nil {
				return err
			}
			if fresh {
				note("created %s", dir)
			}
			var refused []install.Change
			for _, target := range targets {
				changes, err := install.WriteSkill(target, buildinfo.Version, force)
				if err != nil {
					return err
				}
				for _, c := range changes {
					switch c.Action {
					case "refused":
						refused = append(refused, c)
					case "created", "updated":
						note("%s %s", c.Action, c.Path)
					}
				}
				settings := filepath.Join(target, "settings.json")
				if noHook {
					removed, err := install.RemoveHook(settings)
					if err != nil {
						return err
					}
					if removed {
						note("removed the auto re-arm hook from %s", settings)
					}
				} else {
					action, err := install.AddHook(settings, bin)
					if err != nil {
						return err
					}
					if action != "unchanged" {
						note("%s the auto re-arm hook in %s", action, settings)
					}
				}
				path, err := allowTelegramChat(target, cfg.Telegram)
				if err != nil {
					return err
				}
				if path != "" {
					note("created %s (allowlist: your chat)", path)
				}
			}
			for _, c := range refused {
				fmt.Fprintf(out, "refused %s: you edited it; rerun with --force to replace it\n%s", c.Path, c.Diff)
			}
			if len(refused) > 0 {
				return fmt.Errorf("%d file(s) left as you edited them", len(refused))
			}
			if changed == 0 {
				fmt.Fprintln(out, "no changes")
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&dirs, "claude-config", nil, "Claude config folder to install into (repeatable; default $CLAUDE_CONFIG_DIR or ~/.claude)")
	cmd.Flags().BoolVar(&force, "force", false, "replace skill files you edited")
	cmd.Flags().BoolVar(&noHook, "no-hook", false, "do not install the auto re-arm Stop hook, and remove it if present")
	return cmd
}

// linkShortName puts an hrk symlink at shortLinkPath when bin still has the
// old name herdr-runner-kit, as an install upgraded from before the rename
// does. It returns the link it created, or "" when there was nothing to do.
func linkShortName(bin string) (string, error) {
	if filepath.Base(bin) != "herdr-runner-kit" {
		return "", nil
	}
	short := shortLinkPath(bin)
	if _, err := os.Lstat(short); !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return short, os.Symlink("herdr-runner-kit", short)
}

// shortLinkPath is where the hrk link for bin belongs: beside the
// herdr-runner-kit on PATH when that leads to bin, as a symlink into another
// folder does, else beside bin.
func shortLinkPath(bin string) string {
	dir := filepath.Dir(bin)
	if p, err := exec.LookPath("herdr-runner-kit"); err == nil {
		if r, err := filepath.EvalSymlinks(p); err == nil && r == bin {
			dir = filepath.Dir(p)
		}
	}
	return filepath.Join(dir, "hrk")
}

// allowTelegramChat gives the Telegram channel plugin in claudeConfig an
// access list with the kit's chat ids, so the user's replies reach the main
// session without a pairing step. It never touches an existing list. The bot
// token is not copied: `main --exec` passes it to the session at start.
func allowTelegramChat(claudeConfig string, tg config.Telegram) (string, error) {
	creds, err := telegram.Resolve(tg, os.Getenv)
	if err != nil {
		return "", nil // Telegram is off; doctor says so
	}
	dir := filepath.Join(claudeConfig, "channels", "telegram")
	path := filepath.Join(dir, "access.json")
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	data, err := json.MarshalIndent(map[string]any{
		"dmPolicy": "allowlist", "allowFrom": creds.ChatIDs, "groups": map[string]any{}, "pending": map[string]any{},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(data, '\n'), 0o600)
}

func newUninstallCommand() *cobra.Command {
	var dirs []string
	var purge, yes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the skill and the auto re-arm hook; --purge also deletes config and state",
		Long: "uninstall removes the skill files setup wrote, except ones you edited, and the auto " +
			"re-arm hook. With --purge it also deletes the kit's config and state folders, after " +
			"you confirm. The hrk binary and its herdr-runner-kit alias are left for you to delete.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			targets, err := claudeConfigs(dirs)
			if err != nil {
				return err
			}
			for _, target := range targets {
				changes, err := install.RemoveSkill(target)
				if err != nil {
					return err
				}
				for _, c := range changes {
					if c.Action == "kept" {
						fmt.Fprintf(out, "kept %s: you edited it\n", c.Path)
					} else {
						fmt.Fprintf(out, "removed %s\n", c.Path)
					}
				}
				settings := filepath.Join(target, "settings.json")
				removed, err := install.RemoveHook(settings)
				if err != nil {
					return err
				}
				if removed {
					fmt.Fprintf(out, "removed the auto re-arm hook from %s\n", settings)
				}
			}
			if !purge {
				return nil
			}
			cfgPath, err := config.Path()
			if err != nil {
				return err
			}
			sdir, err := stateDir()
			if err != nil {
				return err
			}
			paths := []string{filepath.Dir(cfgPath), sdir}
			if !yes && !confirm(cmd.InOrStdin(), out, "Delete "+strings.Join(paths, " and ")+"? [y/N] ") {
				return errors.New("purge cancelled")
			}
			for _, p := range paths {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
				fmt.Fprintf(out, "deleted %s\n", p)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&dirs, "claude-config", nil, "Claude config folder to remove from (repeatable; default $CLAUDE_CONFIG_DIR or ~/.claude)")
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the kit's config and state folders")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask before --purge deletes")
	return cmd
}

func confirm(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
