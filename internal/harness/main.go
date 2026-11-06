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
