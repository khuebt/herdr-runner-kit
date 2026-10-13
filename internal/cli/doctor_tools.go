package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/project"
)

// penpotTimeout caps the docker query, so a stuck docker daemon never
// holds doctor or project run.
const penpotTimeout = 3 * time.Second

// drawioFlatpak is draw.io desktop's flatpak id.
const drawioFlatpak = "com.jgraph.drawio.desktop"

// toolProbe looks for the optional tools stage templates may suggest.
type toolProbe struct {
	lookPath func(string) (string, error)
	getenv   func(string) string
	// run runs a command and reports whether it succeeded, with its stdout.
	run func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, bool)
	// skillDirs are the skills folders a lavish skill may live in.
	skillDirs []string
}

// newToolProbe returns the probe for this machine. claudeDirs are the Claude
// config folders whose skills count.
func newToolProbe(claudeDirs []string) toolProbe {
	p := toolProbe{lookPath: exec.LookPath, getenv: os.Getenv, run: runQuiet}
	for _, d := range claudeDirs {
		p.skillDirs = append(p.skillDirs, filepath.Join(d, "skills"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		p.skillDirs = append(p.skillDirs, filepath.Join(home, ".agents", "skills"))
	}
	return p
}

func runQuiet(ctx context.Context, timeout time.Duration, name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err == nil
}

// toolLine is one optional tool as doctor shows it.
type toolLine struct {
	name   string
	result checkResult
}

// detect finds the optional tools and describes each one. A missing tool is
// a warning, never a failure: templates leave it out.
func (p toolProbe) detect(ctx context.Context) (project.Tools, []toolLine) {
	var t project.Tools
	var lines []toolLine
	add := func(name string, found bool, detail, missing, fix string) {
		r := ok("%s", detail)
		if !found {
			r = warn(missing, fix)
		}
		lines = append(lines, toolLine{name, r})
	}

	where := p.lavishSkill()
	if where == "" && p.has("npm") {
		if out, done := p.run(ctx, 10*time.Second, "npm", "ls", "-g", "lavish-axi"); done && strings.Contains(out, "lavish-axi") {
			where = "npm global package lavish-axi"
		}
	}
	t.Lavish = where != ""
	add("lavish", t.Lavish, where, "not found; UX and design runners will not offer local HTML review",
		"install the lavish skill, or npm install -g lavish-axi")

	t.Npx = p.has("npx")
	add("npx", t.Npx, "found", "not found", "install Node.js")

	drawio := ""
	switch {
	case p.has("drawio"):
		drawio = "drawio on PATH"
	case p.has("flatpak"):
		if _, done := p.run(ctx, 10*time.Second, "flatpak", "info", drawioFlatpak); done {
			drawio = "flatpak " + drawioFlatpak
		}
	}
	t.Drawio = drawio != ""
	add("draw.io", t.Drawio, drawio, "not found; flows stay in mermaid", "install draw.io desktop")

	if p.has("docker") {
		out, _ := p.run(ctx, penpotTimeout, "docker", "ps", "--format", "{{.Names}}")
		t.Penpot = strings.Contains(strings.ToLower(out), "penpot")
	}
	add("penpot", t.Penpot, "a penpot container is running", "no running penpot container", "")

	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p.has(name) {
			t.PDF = name
			break
		}
	}
	add("pdf", t.PDF != "", t.PDF+" prints PDF headless", "no chromium or google-chrome; an approval pack cannot be printed to PDF",
		"install chromium or google-chrome")

	t.Soffice = p.has("soffice") || p.has("libreoffice")
	add("docx", t.Soffice, "soffice found", "no soffice; an approval pack cannot be written as DOCX", "install LibreOffice")

	// Only whether the key is set: its value is never read into output.
	t.Stitch = p.getenv("STITCH_API_KEY") != ""
	add("stitch", t.Stitch, "STITCH_API_KEY is set; used only after project share stitch on",
		"STITCH_API_KEY is not set; design runners will not use Stitch", "")
	return t, lines
}

func (p toolProbe) has(name string) bool {
	_, err := p.lookPath(name)
	return err == nil
}

// lavishSkill returns the folder of an installed lavish skill, if any.
func (p toolProbe) lavishSkill() string {
	for _, dir := range p.skillDirs {
		for _, name := range []string{"lavish", "lavish-axi"} {
			path := filepath.Join(dir, name)
			if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err == nil {
				return "skill " + path
			}
		}
	}
	return ""
}
