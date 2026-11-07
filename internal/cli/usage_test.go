package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// The synthetic Claude transcript in the harness testdata: 4 calls, 2965
// units with its subagent and workflow agent's 250, its calls on 2026-10-17 between 01:00 and
// 01:10 UTC.
var usageFixture = filepath.Join("..", "harness", "testdata", "transcript-claude", "s1.jsonl")

func usageState(t *testing.T) *state.Store {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("HRK_STATE_DIR", stateDir)
	t.Setenv("HRK_TASK", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	claude := []task.Transcript{{Harness: task.Claude, Path: usageFixture}}
	gone := []task.Transcript{{Harness: task.Claude, Path: filepath.Join(t.TempDir(), "gone.jsonl")}}
	for _, tk := range []*task.Task{
		{Kind: "code", Agent: "claude", Owner: "/repo", Status: task.Done, FinishedAt: day, Transcripts: claude},
		{Kind: "code", Agent: "codex", Owner: "/repo", Status: task.Done, FinishedAt: day},
		{
			Kind: "fix", Agent: "claude", Owner: "/other", Status: task.Done, FinishedAt: day, Transcripts: gone,
			Usage: &task.Usage{Calls: 10, MaxContext: 50_000, Units: 100_000},
		},
		{Kind: "code", Agent: "claude", Owner: "/repo", Status: task.Done, FinishedAt: day.AddDate(0, 0, -3), Transcripts: claude},
	} {
		tk.Cwd, tk.Gate = tk.Owner, task.GateAuto
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordMainSession("old-main"); err != nil { // no transcript kept
		t.Fatal(err)
	}
	if err := store.RecordMainTranscript("main-session-id", usageFixture, ""); err != nil {
		t.Fatal(err)
	}
	return store
}

// usageLines maps each line's first field to the rest of its fields.
func usageLines(out string) map[string]string {
	rows := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if f[0] == "main" && len(f) > 1 && strings.HasPrefix(f[1], "main-") {
			f = append([]string{f[0] + " " + f[1]}, f[2:]...)
		}
		rows[f[0]] = strings.Join(f[1:], " ")
	}
	return rows
}

func TestUsagePerTaskWithMain(t *testing.T) {
	usageState(t)
	out, err := runCLI("usage", "--all", "--since", "2026-10-17T00:00:00Z", "--main")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"TASK":          "KIND AGENT STATUS CALLS MAX-CTX SUBAGENTS UNITS",
		"t1":            "code claude done 4 1k 2 3k",
		"t2":            "code codex done - - - -",
		"t3":            "fix claude done 10 50k 0 100k",
		"main main-ses": "main - - 4 1k 2 3k",
		"total":         "18 50k 4 106k",
	}
	got := usageLines(out)
	if len(got) != len(want) {
		t.Errorf("rows:\n%s", out)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q\n%s", k, got[k], w, out)
		}
	}
}

func TestUsageWindowOwnerAndJSON(t *testing.T) {
	usageState(t)
	out, err := runCLI("usage", "--owner", "/repo", "--until", "2026-10-16", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Rows []struct {
			Task  string      `json:"task"`
			Usage *task.Usage `json:"usage"`
		} `json:"rows"`
		Total task.Usage `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if len(got.Rows) != 1 || got.Rows[0].Task != "t4" || got.Total.Units != 2965 || got.Total.SubagentUnits != 250 {
		t.Errorf("--owner /repo --until 2026-10-16 = %+v", got)
	}

	// A main call outside the window is not counted.
	out, err = runCLI("usage", "--owner", "/nobody", "--main", "--since", "2026-10-17T01:30:00Z")
	if err != nil || strings.Contains(out, "main ") {
		t.Errorf("--main with every call before the window = %q, %v", out, err)
	}
}

func TestUsageByOwner(t *testing.T) {
	usageState(t)
	out, err := runCLI("usage", "--all", "--by", "owner", "--main", "--since", "2026-10-17T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"OWNER":  "TASKS CALLS MAX-CTX SUBAGENTS UNITS",
		"/repo":  "2 4 1k 2 3k",
		"/other": "1 10 50k 0 100k",
		"main":   "1 4 1k 2 3k",
		"total":  "18 50k 4 106k",
	}
	got := usageLines(out)
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q\n%s", k, got[k], w, out)
		}
	}
	if _, err := runCLI("usage", "--by", "day"); err == nil {
		t.Error("--by day was accepted")
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":                     {},
		"24h":                  now.Add(-24 * time.Hour),
		"7d":                   now.AddDate(0, 0, -7),
		"2026-10-17T09:30:00Z": time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC),
		"2026-10-17":           time.Date(2026, 10, 17, 0, 0, 0, 0, time.Local),
		"2026-10-17 09:30":     time.Date(2026, 10, 17, 9, 30, 0, 0, time.Local),
	} {
		if got, err := parseWhen(in, now); err != nil || !got.Equal(want) {
			t.Errorf("parseWhen(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseWhen("yesterday", now); err == nil {
		t.Error("parseWhen accepted yesterday")
	}
}

func TestUsageUnsupportedMainCLI(t *testing.T) {
	store := usageState(t)
	cwd := t.TempDir()
	for _, h := range []task.Harness{task.Pi, task.Omp} {
		// Even a Claude-shaped transcript must never be parsed for another harness.
		transcript, err := filepath.Abs(usageFixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordTypedMainSession(state.MainSession{Harness: h, ID: "shared-id", Transcript: transcript, Owner: cwd, Cwd: cwd}); err != nil {
			t.Fatal(err)
		}
	}
	for _, mixed := range []bool{false, true} {
		for _, owners := range []bool{false, true} {
			t.Run(fmt.Sprintf("mixed=%v/owners=%v", mixed, owners), func(t *testing.T) {
				since := "2026-10-18"
				if mixed {
					since = "2026-10-17"
				}
				args := []string{"usage", "--owner", "/nobody", "--main", "--since", since}
				if owners {
					args = append(args, "--by", "owner")
				}
				out, err := runCLI(append(args, "--json")...)
				if err != nil {
					t.Fatal(err)
				}
				var got struct {
					Rows        []usageRow  `json:"rows"`
					Total       *task.Usage `json:"total"`
					Unsupported int         `json:"unsupported"`
					Status      string      `json:"status"`
				}
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
				wantStatus := "not supported"
				if mixed {
					wantStatus = "partial"
				}
				if got.Unsupported != 2 || got.Status != wantStatus || (got.Total != nil) != mixed {
					t.Fatalf("aggregate: %s", out)
				}
				if mixed && got.Total.Calls != 4 {
					t.Fatalf("measured total: %s", out)
				}
				if owners {
					if len(got.Rows) != 1 || got.Rows[0].Unsupported != 2 || got.Rows[0].Status != wantStatus || (got.Rows[0].Usage != nil) != mixed {
						t.Fatalf("group: %s", out)
					}
				} else {
					unsupported := 0
					for _, r := range got.Rows {
						if r.Agent == "pi" || r.Agent == "omp" {
							unsupported++
							if r.Status != "not supported" || r.Usage != nil || r.Unsupported != 1 {
								t.Fatalf("row: %+v", r)
							}
						}
					}
					if unsupported != 2 {
						t.Fatalf("missing unsupported rows: %s", out)
					}
				}
				text, err := runCLI(args...)
				if err != nil || !strings.Contains(text, wantStatus) || !strings.Contains(text, "2 main session(s)") {
					t.Fatalf("text: %s, %v", text, err)
				}
			})
		}
	}
}

func TestUsageClaudeJSONContract(t *testing.T) {
	usageState(t)
	out, err := runCLI("usage", "--all", "--main", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["rows"] == nil || got["total"] == nil || string(got["total"]) == "null" || strings.Contains(out, `"unsupported"`) {
		t.Fatalf("legacy JSON contract changed: %s", out)
	}
	out, err = runCLI("usage", "--owner", "/nobody", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `"total": null`) {
		t.Fatalf("empty legacy total changed: %s", out)
	}
}

func TestUsageMissingMainTranscript(t *testing.T) {
	rows := mainUsage([]state.MainSession{
		{ID: "legacy", Transcript: filepath.Join(t.TempDir(), "missing")},
		{Harness: task.Pi, ID: "pi-missing"},
		{Harness: task.Omp, ID: "omp-missing", Transcript: filepath.Join(t.TempDir(), "missing")},
	}, usageOptions{})
	if len(rows) != 3 || rows[0].Unsupported != 0 || rows[0].Usage != nil {
		t.Fatalf("legacy unreadable: %+v", rows)
	}
	for _, r := range rows[1:] {
		if r.Unsupported != 1 || r.Status != "not supported" || r.Usage != nil {
			t.Fatalf("missing unsupported: %+v", r)
		}
	}
}

func TestUsageCorruptMainMetadata(t *testing.T) {
	store := usageState(t)
	if err := os.WriteFile(filepath.Join(store.Dir(), "main-sessions.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI("usage", "--main", "--json"); err == nil {
		t.Fatal("corrupt metadata silently omitted from main usage")
	}
	if _, err := runCLI("usage", "--json"); err != nil {
		t.Fatalf("main metadata broke runner-only usage: %v", err)
	}
}
