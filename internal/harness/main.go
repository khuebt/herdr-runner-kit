package harness

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

//go:embed hrk-main.ts
var mainExtension string

// ParseMainSpec permits runtime defaults only for the two extension-backed mains.
// Runner specs deliberately keep their model requirement.
func ParseMainSpec(value string) (task.AgentSpec, error) {
	value = strings.TrimSpace(value)
	if value == string(task.Pi) || value == string(task.Omp) {
		return task.AgentSpec{Harness: task.Harness(value)}, nil
	}
	spec, err := task.ParseAgentSpec(value)
	if err == nil && spec.Harness == task.Codex {
		return task.AgentSpec{}, fmt.Errorf("codex main sessions are not supported; use claude, pi or omp")
	}
	return spec, err
}

// MainUsesExtension distinguishes the main lifecycle without runner launch policy.
func MainUsesExtension(h task.Harness) bool { return h == task.Pi || h == task.Omp }

// MainArgs never inherits runner approval flags, input locks or configuration.
// session is a verified registered path, or a newly reserved empty runtime file.
func MainArgs(spec task.AgentSpec, extension, prompt, session string) []string {
	args := []string{string(spec.Harness)}
	if MainUsesExtension(spec.Harness) {
		flag := "-e"
		if spec.Harness == task.Omp {
			flag = "--hook"
		}
		args = append(args, flag, extension, "--append-system-prompt", prompt)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		flag := "--thinking"
		if spec.Harness == task.Claude {
			flag = "--effort"
		}
		args = append(args, flag, spec.Effort)
	}
	if session != "" {
		flag := "--resume"
		if spec.Harness == task.Pi {
			flag = "--session"
		}
		args = append(args, flag, session)
	}
	return args
}

// MainExtension renders the main-only asset. The launcher owns its immutable,
// content-versioned location and must pass HRK_MAIN_INTERACTIVE=1 only for a TUI.
func MainExtension(bin string, h task.Harness) (File, error) {
	if !filepath.IsAbs(bin) {
		return File{}, fmt.Errorf("main binary must be an absolute path, got %q", bin)
	}
	if h != task.Pi && h != task.Omp {
		return File{}, fmt.Errorf("main extension does not support %q", h)
	}
	b, _ := json.Marshal(bin)
	v, _ := json.Marshal(h)
	src := strings.NewReplacer(piBinPlaceholder, string(b), piHarnessPlaceholder, string(v)).Replace(mainExtension)
	return File{Name: "hrk-main.ts", Data: []byte(src)}, nil
}

// MainUsage reads main-session usage through its adapter. Empty harness means
// legacy Claude; supported distinguishes missing usage from an absent parser.
func MainUsage(h task.Harness, transcript string, from, to time.Time) (usage *task.Usage, supported bool, err error) {
	if h == "" {
		h = task.Claude
	}
	a, err := For(h)
	if err != nil {
		return nil, false, err
	}
	if h != task.Claude {
		return nil, false, nil
	}
	u, err := a.Usage([]string{transcript}, from, to)
	return u, true, err
}
