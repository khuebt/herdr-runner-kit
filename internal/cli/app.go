package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// loadConfig reads the user's config file, or the defaults without one.
func loadConfig() (config.Config, error) {
	path, err := config.Path()
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(path)
}

// openRunner loads the config and state and returns a ready Runner.
func openRunner() (*runner.Runner, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(dir)
	if err != nil {
		return nil, err
	}
	bin, err := ownBinary()
	if err != nil {
		return nil, err
	}
	r := runner.New(store, cfg, bin)
	r.Scope = memoryScope(cfg.RunnerMemoryMax)
	return r, nil
}

// memoryScope returns the cap each runner's agent runs under in its own
// systemd scope, or "" where scopes cannot be used. It checks once, on the
// first launch, so commands that launch nothing pay nothing.
func memoryScope(memoryMax string) func(context.Context) string {
	var once sync.Once
	var usable string
	return func(ctx context.Context) string {
		once.Do(func() {
			if scopeProblem(ctx, memoryMax, runtime.GOOS) == nil {
				usable = memoryMax
			}
		})
		return usable
	}
}

// errNoSystemd is scopeProblem's answer on a system without systemd.
var errNoSystemd = errors.New("not available")

// scopeProblem says why runners cannot run in a memory-capped systemd
// scope on goos, or returns nil when they can.
func scopeProblem(ctx context.Context, memoryMax, goos string) error {
	if memoryMax == config.MemoryOff {
		return errors.New("runnerMemoryMax is off")
	}
	if goos != "linux" {
		if goos == "darwin" {
			goos = "macOS"
		}
		return fmt.Errorf("%w on %s", errNoSystemd, goos)
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return errors.New("systemd-run is not on PATH")
	}
	if out, err := output(ctx, "systemd-run", "--user", "--scope", "--quiet", "--collect", "true"); err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("systemd-run --user --scope does not work here: %s", msg)
	}
	return nil
}
