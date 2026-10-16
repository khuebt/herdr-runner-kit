package runner

import (
	"fmt"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
)

// scopeUnit names the transient systemd scope an agent session runs in. The
// time suffix keeps it unique: a fallback switch keeps the attempt number,
// and processes the previous agent left behind keep its scope alive.
func scopeUnit(id string, attempt int, now time.Time) string {
	return fmt.Sprintf("hrk-%s-%d-%d", id, attempt, now.Unix())
}

// scopeCommand moves the pane's shell into its own scope, capped at
// memoryMax, so the agent started from it and every command the agent runs
// are capped too. herdr starts only an agent kind's own executable, so the
// scope wraps the shell instead. systemd-run --scope registers the scope
// and executes in place: with exec, the shell keeps the pid herdr knows
// and its exported environment. MemorySwapMax=0 makes the cap hard: a
// command over it is killed instead of swapping. OOMPolicy=continue keeps
// the scope, and so the agent, running when the kernel kills a command
// inside it, and --collect drops the unit once it is empty, even when it
// ended failed.
func scopeCommand(unit, memoryMax string) string {
	args := []string{
		"exec", "systemd-run", "--user", "--scope", "--quiet", "--collect", "--unit=" + unit,
		"-p", "MemoryMax=" + memoryMax, "-p", "MemorySwapMax=0", "-p", "OOMPolicy=continue", "--",
	}
	for i, a := range args {
		args[i] = harness.ShellQuote(a)
	}
	return strings.Join(args, " ") + ` "$SHELL"`
}
