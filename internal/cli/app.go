package cli

import (
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
	return runner.New(store, cfg, bin), nil
}
