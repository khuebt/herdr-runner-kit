// Package archtest holds the kit's boundary tests: the layer each package
// belongs to, which packages a layer may import, and where the core may
// still name a harness. docs/architecture.md describes the same tables;
// change both together.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const module = "github.com/khuebt/herdr-runner-kit/"

// root is the repository root, relative to this package.
const root = "../.."

// layers maps every package to its layer.
var layers = map[string]string{
	"internal/task": "data", "internal/state": "data", "internal/config": "data",
	"internal/buildinfo": "data", "internal/fsutil": "data", "internal/redact": "data",
	"skill":           "data", // the embedded skill files
	"internal/runner": "core", "internal/wait": "core",
	"internal/harness": "harness",
	"internal/herdr":   "integration", "internal/telegram": "integration",
	"internal/install": "integration", "internal/upgrade": "integration",
	"internal/project": "workflow", "internal/planrun": "workflow",
	"internal/cli": "root", "cmd/hrk": "root",
}

// mayImport lists, per layer, the layers ("@data") and packages it may
// import. The composition root may import everything.
var mayImport = map[string][]string{
	"data":        {"@data"},
	"core":        {"@data", "@core", "internal/harness", "internal/herdr", "internal/telegram", "internal/planrun", "internal/project"},
	"harness":     {"internal/task", "internal/fsutil"},
	"integration": {"@data"},
	"workflow":    {"@data", "internal/harness", "internal/herdr"},
}

// harnessNames lists where code outside internal/harness and internal/task
// may still name a harness, as "file:function" with the reason. Each entry
// is a feature of one harness, not a per-harness switch.
var harnessNames = map[string]string{
	"internal/runner/fallback.go:agentKey":        "a Claude config folder is part of a Claude agent's identity",
	"internal/runner/spawn.go:newTask":            "--claude-config applies to Claude runners only",
	"internal/config/config.go:Validate":          "quotaFallback[].claudeConfig applies to Claude agents only",
	"internal/cli/doctor.go:runnerClaudeConfigs":  "doctor checks the Claude config folders Claude tasks recorded",
	"internal/project/templates.go:RenderOutputs": "output paths render no skill, so any harness gives the same paths",
}

// source is one parsed Go file and its path from the repository root.
type source struct {
	path string
	*ast.File
}

// sources parses every non-test Go file under internal, cmd and skill, keyed by
// its package directory.
func sources(t *testing.T, mode parser.Mode) map[string][]source {
	t.Helper()
	fset := token.NewFileSet()
	pkgs := map[string][]source{}
	for _, top := range []string{"internal", "cmd", "skill"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, mode)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			pkg := rel[:strings.LastIndex(rel, "/")]
			pkgs[pkg] = append(pkgs[pkg], source{rel, f})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return pkgs
}

func allowed(layer, pkg string) bool {
	if layer == "root" {
		return true
	}
	for _, a := range mayImport[layer] {
		if a == pkg || a == "@"+layers[pkg] {
			return true
		}
	}
	return false
}

func TestLayerImports(t *testing.T) {
	for pkg, files := range sources(t, parser.ImportsOnly) {
		if pkg == "internal/archtest" {
			continue
		}
		layer, ok := layers[pkg]
		if !ok {
			t.Errorf("%s has no layer; add it to layers and to docs/architecture.md", pkg)
			continue
		}
		for _, f := range files {
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				dep, ok := strings.CutPrefix(path, module)
				if !ok || dep == pkg {
					continue
				}
				if !allowed(layer, dep) {
					t.Errorf("%s (%s) imports %s (%s); update the layer table and docs/architecture.md together if this is intended",
						pkg, layer, dep, layers[dep])
				}
			}
		}
	}
}

// TestHarnessImportsNoCore keeps the adapters free of the core and the
// data they never need, whatever the layer table says.
func TestHarnessImportsNoCore(t *testing.T) {
	core := []string{"internal/runner", "internal/wait", "internal/state", "internal/config", "internal/cli"}
	for _, f := range sources(t, parser.ImportsOnly)["internal/harness"] {
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if slices.Contains(core, strings.TrimPrefix(path, module)) {
				t.Errorf("internal/harness imports %s", path)
			}
		}
	}
}

// TestCoreNamesNoHarness keeps harness decisions behind harness.Adapter:
// outside internal/harness and internal/task, no harness constant other than
// task.Claude appears, and task.Claude only in the allowlisted Claude-only
// features. The core lifecycle packages also never spell a harness name as
// a string.
func TestCoreNamesNoHarness(t *testing.T) {
	names := map[string]bool{}
	for _, h := range task.Harnesses() {
		names[string(h)] = true
	}
	used := map[string]bool{}
	for pkg, files := range sources(t, 0) {
		if pkg == "internal/harness" || pkg == "internal/task" {
			continue
		}
		for _, f := range files {
			for _, decl := range f.Decls {
				name := ""
				if fn, ok := decl.(*ast.FuncDecl); ok {
					name = fn.Name.Name
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && layers[pkg] == "core" {
						if v, err := strconv.Unquote(lit.Value); err == nil && names[v] {
							t.Errorf("%s:%s spells the harness name %q; ask harness.Adapter instead", f.path, name, v)
						}
						return true
					}
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "task" || !names[strings.ToLower(sel.Sel.Name)] {
						return true
					}
					key := f.path + ":" + name
					if _, ok := harnessNames[key]; ok && sel.Sel.Name == "Claude" {
						used[key] = true
						return true
					}
					t.Errorf("%s names task.%s; move the decision behind harness.Adapter, or allowlist a Claude-only feature with its reason",
						key, sel.Sel.Name)
					return true
				})
			}
		}
	}
	for key := range harnessNames {
		if !used[key] {
			t.Errorf("allowlist entry %s no longer names task.Claude; remove it", key)
		}
	}
}
