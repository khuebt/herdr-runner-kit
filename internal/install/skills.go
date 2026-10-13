package install

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
	"github.com/khuebt/herdr-runner-kit/skill"
)

// Change is one file setup or uninstall touched, or chose not to.
type Change struct {
	Path   string
	Action string // created, updated, unchanged, refused, removed, kept
	Diff   string // for refused: how the user's file differs from ours
}

// SkillDir is where the main-agent skill lives in a Claude config folder.
func SkillDir(claudeConfig string) string {
	return filepath.Join(claudeConfig, "skills", skill.Name)
}

// WriteSkill writes the embedded skill into claudeConfig. A file that is
// missing is created and a file that still carries an intact marker is
// replaced. Any other file is the user's: it is refused, with a diff, unless
// force is set.
func WriteSkill(claudeConfig, version string, force bool) ([]Change, error) {
	var changes []Change
	err := fs.WalkDir(skill.Files, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !installed(rel) {
			return err
		}
		src, err := skill.Files.ReadFile(rel)
		if err != nil {
			return err
		}
		want := Stamp(src, version)
		path := filepath.Join(SkillDir(claudeConfig), rel)
		c := Change{Path: path}
		have, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			c.Action = "created"
		case err != nil:
			return err
		case bytes.Equal(have, want):
			c.Action = "unchanged"
		default:
			if _, intact, _ := Marker(have); !intact && !force {
				c.Action, c.Diff = "refused", diff(path, want)
				changes = append(changes, c)
				return nil
			}
			c.Action = "updated"
		}
		if c.Action != "unchanged" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := fsutil.WriteFileAtomic(path, want, 0o644); err != nil {
				return err
			}
		}
		changes = append(changes, c)
		return nil
	})
	return changes, err
}

// RemoveSkill deletes the skill files that still carry an intact marker. A
// file the user edited is kept, and so is any file the kit did not write.
func RemoveSkill(claudeConfig string) ([]Change, error) {
	var changes []Change
	dir := SkillDir(claudeConfig)
	err := fs.WalkDir(skill.Files, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !installed(rel) {
			return err
		}
		path := filepath.Join(dir, rel)
		have, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, intact, _ := Marker(have); !intact {
			changes = append(changes, Change{Path: path, Action: "kept"})
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		changes = append(changes, Change{Path: path, Action: "removed"})
		return nil
	})
	// Folders go only when empty, so nothing of the user's is lost.
	_ = os.Remove(filepath.Join(dir, "references"))
	_ = os.Remove(dir)
	return changes, err
}

// installed reports whether the embedded file rel belongs in the installed
// skill. Project stage templates and the runner preamble stay in the
// binary: the kit renders them, and the main agent never reads them.
func installed(rel string) bool {
	return filepath.Ext(rel) == ".md" && !strings.HasPrefix(rel, skill.KitOnlyDir+"/")
}

// SkillVersion reports the version that installed the skill in claudeConfig,
// and whether its SKILL.md is unchanged since.
func SkillVersion(claudeConfig string) (version string, intact bool, err error) {
	data, err := os.ReadFile(filepath.Join(SkillDir(claudeConfig), "SKILL.md"))
	if err != nil {
		return "", false, err
	}
	version, intact, _ = Marker(data)
	return version, intact, nil
}

// diff shows how the user's file differs from what setup would write. It uses
// the system diff when there is one; without it, the refusal still names the
// file.
func diff(path string, want []byte) string {
	tmp, err := os.CreateTemp("", "herdr-runner-kit-*.md")
	if err != nil {
		return ""
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(want); err != nil {
		_ = tmp.Close()
		return ""
	}
	_ = tmp.Close()
	out, _ := exec.Command("diff", "-u", "--label", path, "--label", "herdr-runner-kit", path, tmp.Name()).Output()
	return string(out)
}
