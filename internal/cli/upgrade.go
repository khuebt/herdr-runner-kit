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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
	"github.com/khuebt/herdr-runner-kit/internal/install"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/upgrade"
)

// newUpgradeClient, upgradeTarget and execSetup are replaced in tests.
var (
	newUpgradeClient = upgrade.New
	upgradeTarget    = ownBinary
	execSetup        = syscall.Exec
)

func newUpgradeCommand() *cobra.Command {
	var version string
	var check, yes bool
	var dirs []string
	cmd := &cobra.Command{
		Use:   "upgrade [--version vX.Y.Z] [--check]",
		Short: "Install the latest release, or a named one, then rerun setup",
		Long: "upgrade downloads a release from GitHub, verifies it against the release's " +
			"checksums.txt, swaps it in for this binary, and runs the new binary's setup. " +
			"Running runners are not interrupted. --check only shows what would change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			c := newUpgradeClient()
			current := buildinfo.Version
			var rel upgrade.Release
			var err error
			if version != "" {
				rel, err = c.Tag(ctx, "v"+strings.TrimPrefix(version, "v"))
			} else {
				rel, err = c.Latest(ctx)
			}
			if err != nil {
				return fmt.Errorf("find release: %w", err)
			}
			newer := upgrade.Compare(rel.Tag, current)
			if check {
				fmt.Fprintf(out, "current: %s\nlatest:  %s\n", current, rel.Tag)
				if newer <= 0 {
					fmt.Fprintln(out, "up to date")
					return nil
				}
				printNotes(ctx, out, c, current, rel)
				fmt.Fprintln(out, "run hrk upgrade to install it")
				return nil
			}
			switch {
			case newer == 0:
				fmt.Fprintf(out, "already at %s\n", current)
				return nil
			case newer < 0 && version == "":
				return fmt.Errorf("the latest release %s is older than this binary (%s); name it with --version to downgrade", rel.Tag, current)
			}
			if newer > 0 && upgrade.MayBreak(current, rel.Tag) {
				fmt.Fprintf(out, "%s may hold incompatible changes. Read its notes first:\n", rel.Tag)
				printNotes(ctx, out, c, current, rel)
				if !yes && !confirm(cmd.InOrStdin(), out, "Upgrade? [y/N] ") {
					return errors.New("upgrade cancelled")
				}
			}
			bin, err := upgradeTarget()
			if err != nil {
				return err
			}
			if err := c.Install(ctx, rel, bin, checkStaged); err != nil {
				return err
			}
			fmt.Fprintf(out, "upgraded %s to %s\n", current, rel.Tag)
			if os.Getenv("HRK_NO_SETUP") == "1" {
				return nil
			}
			// The new binary's setup refreshes the skill and hook, and its
			// first state load runs any migration.
			configs, err := upgradeConfigs(dirs)
			if err != nil {
				return err
			}
			argv := []string{bin, "setup"}
			for _, d := range configs {
				fmt.Fprintf(out, "refreshing %s\n", d)
				argv = append(argv, "--claude-config", d)
			}
			return execSetup(bin, argv, os.Environ())
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "install this release instead of the latest, such as v0.2.0; allows a downgrade")
	cmd.Flags().BoolVar(&check, "check", false, "only print the current and latest versions and what changed")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask before a release that may hold incompatible changes")
	cmd.Flags().StringArrayVar(&dirs, "claude-config", nil, "Claude config folder for the setup that follows (repeatable); $CLAUDE_CONFIG_DIR or ~/.claude and every ~/.claude* folder holding the kit's skill are refreshed too")
	return cmd
}

// upgradeConfigs returns the Claude config folders the setup after an
// upgrade refreshes, each once: the given ones, the default one, and every
// ~/.claude* folder that holds the kit's skill.
func upgradeConfigs(dirs []string) ([]string, error) {
	given, err := claudeConfigs(dirs)
	if err != nil {
		return nil, err
	}
	all := given
	if len(dirs) > 0 {
		def, err := claudeConfigs(nil)
		if err != nil {
			return nil, err
		}
		all = append(all, def...)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	found, _ := filepath.Glob(filepath.Join(home, ".claude*"))
	for _, d := range found {
		if _, _, err := install.SkillVersion(d); err == nil {
			all = append(all, d)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range all {
		key := d
		if resolved, err := filepath.EvalSymlinks(d); err == nil {
			key = resolved
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// printNotes prints the release notes from current up to rel.
func printNotes(ctx context.Context, out io.Writer, c *upgrade.Client, current string, rel upgrade.Release) {
	notes := []upgrade.Release{rel}
	if all, err := c.List(ctx); err == nil {
		if between := upgrade.Between(all, current, rel.Tag); len(between) > 0 {
			notes = between
		}
	}
	for _, r := range notes {
		fmt.Fprintf(out, "\n%s\n", releaseNotes(r))
	}
	fmt.Fprintln(out)
}

// releaseNotes is a release's notes under one heading. release-please notes
// open with their own "## 0.1.0 (date)" heading, so only others get one.
func releaseNotes(r upgrade.Release) string {
	body := strings.TrimSpace(r.Body)
	if strings.HasPrefix(body, "#") {
		return body
	}
	return "## " + r.Tag + "\n" + body
}

// checkStaged vetoes a binary that cannot work on the state on disk: an
// older schema cannot read it, and a newer one would migrate it under runners
// that still report against the old layout.
func checkStaged(staged string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, staged, "version", "--json").Output()
	if err != nil {
		return fmt.Errorf("the new binary does not run here: %w", err)
	}
	var info buildinfo.Info
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("the new binary's version: %w", err)
	}
	dir, err := stateDir()
	if err != nil {
		return err
	}
	onDisk, err := state.DiskSchema(dir)
	if err != nil {
		return err
	}
	switch {
	case onDisk == 0 || info.SchemaVersion == onDisk:
		return nil
	case info.SchemaVersion < onDisk:
		return fmt.Errorf("%s reads state schema %d, older than the state in %s (%d); it cannot go back that far",
			info.Version, info.SchemaVersion, dir, onDisk)
	}
	live, err := state.LiveTasks(dir)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		return fmt.Errorf("%s migrates the state, and runners are still working (%s); finish or stop them, then upgrade",
			info.Version, strings.Join(live, ", "))
	}
	return nil
}

// updateNote reports a newer release, asking GitHub at most once a day.
func updateNote(ctx context.Context, maxAge time.Duration) (latest string, newer bool, err error) {
	cache, err := upgrade.CachePath()
	if err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	latest, err = newUpgradeClient().LatestCached(ctx, cache, maxAge)
	if err != nil {
		return "", false, err
	}
	return latest, upgrade.Compare(latest, buildinfo.Version) > 0, nil
}
