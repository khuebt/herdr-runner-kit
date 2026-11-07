// Package skill holds the main-agent prompt and its references.
package skill

import (
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
)

// Name is the skill's folder name under <claude config>/skills.
const Name = "herdr-runner-kit"

// KitOnlyDir holds the project stage templates and the runner preamble. The
// kit renders them; setup does not install them with the skill.
const KitOnlyDir = "references/project"

// Files holds SKILL.md, its references and the kit-only templates.
//
//go:embed SKILL.md references
var Files embed.FS

// MaterializeMain writes the embedded prompt and agent-visible references into
// a launcher-owned asset directory. It returns the absolute system prompt path;
// callers choose a content-versioned directory before launching a session.
func MaterializeMain(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("main asset directory must be absolute: %q", dir)
	}
	err := fs.WalkDir(Files, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(rel) != ".md" || strings.HasPrefix(rel, KitOnlyDir+"/") {
			return err
		}
		data, err := Files.ReadFile(rel)
		if err != nil {
			return err
		}
		if rel == "SKILL.md" {
			// The runtime reads the prompt from any cwd, independently of Claude setup.
			base := (&url.URL{Path: filepath.ToSlash(filepath.Join(dir, "references")) + "/"}).EscapedPath()
			data = []byte(strings.ReplaceAll(string(data), "](references/", "]("+base))
		}
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return fsutil.WriteFileAtomic(path, data, 0o600)
	})
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "SKILL.md"), nil
}
