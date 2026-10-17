package runner

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

var (
	compatTime = regexp.MustCompile(`"\d{4}-\d\d-\d\dT[^"]*"`)
	compatPID  = regexp.MustCompile(`"launcherPid":\s*\d+`)
	compatDirs = regexp.MustCompile(`"(owner|claudeConfigDir)":(\s*)"[^"]*"`)
)

// reportFiles reports o for a fresh running task in a folder outside git
// and returns its task file and the report's event line, with paths, times
// and pids replaced so they compare across runs.
func reportFiles(t *testing.T, o ReportOptions) (taskJSON, eventJSON string) {
	t.Helper()
	r, _, _ := newTestRunner(t)
	dir := plainDir(t)
	tk := running(t, r, SpawnOptions{Agent: "codex", Prompt: "x", Cwd: dir, Title: "fix login"})
	if _, err := r.Report(tk.ID, o); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(r.Store.TaskDir(tk.ID), "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(r.Store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	norm := func(s string) string {
		s = strings.ReplaceAll(s, dir, "<cwd>")
		s = compatTime.ReplaceAllString(s, `"<ts>"`)
		s = compatDirs.ReplaceAllString(s, `"$1":$2"<dir>"`)
		return compatPID.ReplaceAllString(s, `"launcherPid": 0`)
	}
	return norm(string(raw)), norm(lines[len(lines)-1])
}

// A report a runner of the first contract makes (one --question, or done
// without decisions) writes exactly the task and event it wrote before.
func TestReportContractV1ReportsAreUnchanged(t *testing.T) {
	const taskHead = `{
  "schemaVersion": 1,
  "id": "t1",
  "kind": "code",
  "agent": "codex",
  "prompt": "x",
  "title": "fix login",
  "cwd": "<cwd>",
  "owner": "<dir>",
  "mainWorkspace": "w1",
  "claudeConfigDir": "<dir>",
  "gate": "auto",
  "herdr": {
    "paneId": "w1:pt1"
  },
`
	const taskTail = `  "launcherPid": 0,
  "launchDeadline": "<ts>",
  "createdAt": "<ts>",
  "updatedAt": "<ts>"
}
`
	for _, c := range []struct {
		name            string
		o               ReportOptions
		taskBody, event string
	}{
		{
			name: "needs-human",
			o:    ReportOptions{Status: task.NeedsHuman, Questions: []string{"Drop it? (recommended: yes)"}, Summary: "context"},
			taskBody: `  "summary": "context",
  "question": "Drop it? (recommended: yes)",
  "status": "needs-human",
`,
			event: `{"v":1,"ts":"<ts>","task":"t1","owner":"<dir>","type":"needs-human","status":"needs-human","summary":"context","question":"Drop it? (recommended: yes)"}`,
		},
		{
			name: "done",
			o:    ReportOptions{Status: task.Done, Summary: "Fixed."},
			taskBody: `  "summary": "Fixed.",
  "finishedAt": "<ts>",
  "status": "done",
`,
			event: `{"v":1,"ts":"<ts>","task":"t1","owner":"<dir>","type":"done","status":"done","summary":"Fixed."}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			gotTask, gotEvent := reportFiles(t, c.o)
			if want := taskHead + c.taskBody + taskTail; gotTask != want {
				t.Errorf("task.json:\n%s\nwant:\n%s", gotTask, want)
			}
			if gotEvent != c.event {
				t.Errorf("event:\n%s\nwant:\n%s", gotEvent, c.event)
			}
		})
	}
}

func TestReportRejectsBadRoundsAndDecisions(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	six := []string{"a?", "b?", "c?", "d?", "e?", "f?"}
	for _, o := range []ReportOptions{
		{Status: task.NeedsHuman, Questions: six},
		{Status: task.NeedsHuman, Questions: []string{"a?", strings.Repeat("é", MaxQuestionRunes+1)}},
		{Status: task.NeedsHuman, Questions: []string{"a?"}, Decisions: []string{"x -> y"}},
		{Status: task.Failed, Summary: "s", Decisions: []string{"x -> y"}},
		{Status: task.Done, Summary: "s", Decisions: []string{"no arrow"}},
		{Status: task.Done, Summary: "s", Decisions: []string{" -> y"}},
		{Status: task.Done, Summary: "s", Decisions: []string{strings.Repeat("x", MaxDecisionRunes) + " -> y"}},
		{Status: task.Done, Summary: "s", Decisions: slices.Repeat([]string{"x -> y"}, MaxDecisions+1)},
		{Status: task.NeedsHuman, Questions: []string{" ", ""}},
	} {
		if _, err := r.Report(tk.ID, o); err == nil {
			t.Errorf("Report(%+v) succeeded", o)
		}
	}
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running {
		t.Errorf("a rejected report changed the task: %s", got.Status)
	}
	// The limits themselves are allowed.
	five := []string{"a?", "b?", "c?", "d?", strings.Repeat("é", MaxQuestionRunes)}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: five}); err != nil {
		t.Errorf("five questions: %v", err)
	}
}

func TestReportQuestionRound(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	got, err := r.Report(tk.ID, ReportOptions{
		Status:    task.NeedsHuman,
		Questions: []string{" Which database?\na) Postgres (recommended) b) SQLite ", "", "Which host? a) VPS (recommended)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "1. Which database? a) Postgres (recommended) b) SQLite\n2. Which host? a) VPS (recommended)"
	if got.Question != want {
		t.Errorf("task question = %q", got.Question)
	}
	if ev := lastEvent(t, r); ev.Question != want {
		t.Errorf("event question = %q", ev.Question)
	}
	if round := task.QuestionRound(got.Question); len(round) != 2 || round[1] != "Which host? a) VPS (recommended)" {
		t.Errorf("round = %q", round)
	}
	// The round is answered like a single question.
	if _, err := r.Answer(context.Background(), tk.ID, "1a 2a"); err != nil {
		t.Fatal(err)
	}
}

func TestReportDecisions(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	got, err := r.Report(tk.ID, ReportOptions{
		Status: task.Done, Summary: "Export added.",
		Decisions: []string{" CSV or JSON? -> CSV ", "Page size? -> 50"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CSV or JSON? -> CSV", "Page size? -> 50"}
	if !slices.Equal(got.Decisions, want) {
		t.Errorf("task decisions = %q", got.Decisions)
	}
	if ev := lastEvent(t, r); !slices.Equal(ev.Decisions, want) {
		t.Errorf("event decisions = %q", ev.Decisions)
	}
}

func TestReportAfterAnAnswerInTheTab(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"Drop it?"}}); err != nil {
		t.Fatal(err)
	}
	// The user answered in the runner's tab; the runner reports next.
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "Dropped."}); err != nil {
		t.Fatal(err)
	}
	all, _, _ := r.Store.ReadEventsSince(0)
	n := len(all)
	if all[n-2].Type != "answered" || all[n-2].Summary != answeredInTab || all[n-1].Type != "done" {
		t.Errorf("events = %+v", all[n-2:])
	}

	// A report with no question open, or after answer, adds no such event.
	other := running(t, r, SpawnOptions{Prompt: "y", Cwd: plainDir(t)})
	if _, err := r.Report(other.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"Keep it?"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Answer(context.Background(), other.ID, "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(other.ID, ReportOptions{Status: task.Done, Summary: "Kept."}); err != nil {
		t.Fatal(err)
	}
	want := []string{"spawned", "needs-human", "answered", "done"}
	if got := events(t, r, other.ID); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestAnswerEventHidesSecrets(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"Where is the key?"}}); err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live0123456789abcdefXYZ"
	if _, err := r.Answer(context.Background(), tk.ID, "use API_KEY="+secret); err != nil {
		t.Fatal(err)
	}
	if ev := lastEvent(t, r); ev.Type != "answered" || strings.Contains(ev.Summary, secret) || !strings.Contains(ev.Summary, redact.Mark) {
		t.Errorf("answered = %+v", ev)
	}
}

func TestReportGitStateStacked(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk, wt := worktreeRunning(t, r, "hrk/t1-x")

	// Without a stacked branch the snapshot has no stacked key.
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"Go on?"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(r.Store.TaskDir(tk.ID), "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"stacked"`) || strings.Contains(lastEvent(t, r).Git, "stacked") {
		t.Errorf("stacked without a stacked branch:\n%s", raw)
	}

	git(t, wt, "branch", "feat/parser")
	writeFile(t, filepath.Join(wt, "b.txt"))
	git(t, wt, "add", "b.txt")
	git(t, wt, "commit", "-q", "-m", "feat: b")
	got, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Git == nil || !slices.Equal(got.Git.Stacked, []string{"feat/parser"}) {
		t.Fatalf("git = %+v", got.Git)
	}
	if ev := lastEvent(t, r); !strings.HasSuffix(ev.Git, ", stacked: feat/parser, head "+gitHead(t, wt)) {
		t.Errorf("event git = %q", ev.Git)
	}
}

func TestContractV3Rules(t *testing.T) {
	c := Contract(&task.Task{ID: "t1"}, "/bin/hrk", "/out")
	for _, want := range []string{
		"report contract v3)",
		`--decision "<question> -> <choice>"`,
		"one round of up to 5 questions",
		"Never ask for, type or echo a secret value",
		"Ask where it is stored instead.",
		"Do not run rm -r on directories, alone or inside compound commands, because the agent stops on a safety prompt for it; write scratch output into a new folder under /out instead.",
		`/bin/hrk report handoff --output <handoff file> --summary "<what is done and what is left>"`,
		`- In a done summary, name each check you ran and its result, such as "go test ./... passed"; name any check you could not run and why. Never claim a check you did not run.`,
		"- For a read-only lookup, give it to a fresh read-only subagent on a cheaper model, such as Claude Code's Explore agent with model: sonnet, and brief it with paths; never fork your whole context for it.",
		"Only when the kit tells you your context is nearly full: commit or save your work at a stable point, write a handoff under /out naming what is done, what is left, the checks you ran and any background process you started (command and pid), then run report handoff and stop.",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("contract lacks %q:\n%s", want, c)
		}
	}
}

func TestReportCutsALongSingleQuestion(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	got, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{strings.Repeat("é", MaxQuestionRunes+50)}})
	if err != nil {
		t.Fatalf("a long single question was refused: %v", err)
	}
	if q := []rune(got.Question); len(q) != MaxQuestionRunes || q[len(q)-1] != '…' {
		t.Errorf("question has %d runes, ends %q", len(q), q[len(q)-1])
	}
}

func TestReportHidesSecrets(t *testing.T) {
	r, _, _ := newTestRunner(t)
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	got, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Summary: "found " + token, Questions: []string{"use " + token + "?", "or password=hunter2?"}})
	if err != nil {
		t.Fatal(err)
	}
	ev := lastEvent(t, r)
	for _, text := range []string{got.Summary, got.Question, ev.Summary, ev.Question} {
		if strings.Contains(text, token) || strings.Contains(text, "hunter2") || !strings.Contains(text, redact.Mark) {
			t.Errorf("report keeps a secret: %q", text)
		}
	}
	if _, err := r.Send(context.Background(), tk.ID, "the key is "+token); err != nil {
		t.Fatal(err)
	}
	if ev := lastEvent(t, r); ev.Type != "sent" || strings.Contains(ev.Summary, token) {
		t.Errorf("sent event = %+v", ev)
	}
	got, err = r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "done", Decisions: []string{"which key -> " + token}})
	if err != nil {
		t.Fatal(err)
	}
	if ev := lastEvent(t, r); strings.Contains(got.Decisions[0], token) || strings.Contains(ev.Decisions[0], token) {
		t.Errorf("decision keeps a secret: %q %q", got.Decisions, ev.Decisions)
	}
}

func TestContractHeavyRuleFollowsHeavySlots(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := &task.Task{ID: "t1", Cwd: "/work/other"}
	rule := "\n- Run tests, lint, typecheck and builds as " + r.Bin + " heavy -- <command>; it waits for a free slot shared by every runner and the main session."
	got, err := r.contract(tk, "/out", task.Claude)
	if err != nil || !strings.HasSuffix(got, rule) || !strings.Contains(got, "report contract v3)") {
		t.Errorf("heavySlots %d: contract lacks the heavy rule: %v\n%s", r.Config.HeavySlots, err, got)
	}
	r.Config.HeavySlots = 0
	if got, err = r.contract(tk, "/out", task.Claude); err != nil || strings.Contains(got, " heavy -- ") {
		t.Errorf("heavySlots 0: contract has the heavy rule: %v\n%s", err, got)
	}
}
