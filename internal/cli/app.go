package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// openRunner loads the config and state and returns a ready Runner.
func openRunner() (*runner.Runner, error) {
	path, err := config.Path()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("HRK_STATE_DIR")
	if dir == "" {
		if dir, err = state.DefaultDir(); err != nil {
			return nil, err
		}
	}
	store, err := state.Open(dir)
	if err != nil {
		return nil, err
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate own binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	return runner.New(store, cfg, bin), nil
}
