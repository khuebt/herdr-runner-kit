package skill

import (
	"bytes"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The main agent loads SKILL.md on every session, so it must stay cheap.
func TestSkillStaysShort(t *testing.T) {
	data, err := Files.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte("\n")); n > 150 {
		t.Fatalf("SKILL.md has %d lines; keep it at 150 or fewer and move detail to references/", n)
	}
}

func TestMaterializeMain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "main assets (quoted)")
	prompt, err := MaterializeMain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != filepath.Join(dir, "SKILL.md") {
		t.Fatalf("prompt path = %q", prompt)
	}
	data, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	links := regexp.MustCompile(`\]\(([^)]+)\)`).FindAllSubmatch(data, -1)
	if len(links) == 0 {
		t.Fatal("prompt has no reference links")
	}
	for _, link := range links {
		u, err := url.Parse(string(link[1]))
		if err != nil || !filepath.IsAbs(u.Path) || !strings.HasPrefix(u.Path, dir+string(os.PathSeparator)) {
			t.Fatalf("reference is not an absolute local asset: %q (%v)", link[1], err)
		}
		if _, err := os.Stat(u.Path); err != nil {
			t.Fatalf("unresolvable reference %q: %v", link[1], err)
		}
	}
	if err := fs.WalkDir(Files, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(rel) != ".md" {
			return err
		}
		_, err = os.Stat(filepath.Join(dir, rel))
		if strings.HasPrefix(rel, KitOnlyDir+"/") {
			if !os.IsNotExist(err) {
				t.Errorf("kit-only file materialized: %s", rel)
			}
			return nil
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if again, err := MaterializeMain(dir); err != nil || again != prompt {
		t.Fatalf("repeat materialization: %q %v", again, err)
	}
	after, err := os.ReadFile(prompt)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatalf("repeat changed prompt: %v", err)
	}
}

func TestMaterializeMainErrors(t *testing.T) {
	if _, err := MaterializeMain("relative"); err == nil {
		t.Fatal("relative asset directory accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeMain(file); err == nil {
		t.Fatal("filesystem failure ignored")
	}
}
