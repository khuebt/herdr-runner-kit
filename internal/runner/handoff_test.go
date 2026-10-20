package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// handoffFile writes a handoff note in the task's out folder.
func handoffFile(t *testing.T, r *Runner, id string) string {
	t.Helper()
	dir := r.OutDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "handoff-a2.md")
	if err := os.WriteFile(path, []byte("done: parser\nleft: exporter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// quitsThenIdle makes the fake agent in a pane quit on the launcher's
// Ctrl+C, so a relaunch can start the next session there.
func quitsThenIdle(fake *fakeHerdr) {
	fake.on("agent get", ok(`{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`))
	fake.on("agent wait", func(args []string) (string, error) {
		if slices.Contains(args, "unknown") {
			return herdrErr(herdr.CodeAgentNotRunning)(args)
		}
		return `{"result":{"agent":{"agent_status":"working"}}}`, nil
	})
}

func TestHandoffContinuesInAFreshSessionInTheSamePane(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Kind: "research", Skill: "ak:cook", Prompt: "plan.md", Cwd: plainDir(t)})
	var launched []string
	r.StartLauncher = func(id string) (int, error) { launched = append(launched, id); return 5151, nil }
	file := handoffFile(t, r, tk.ID)

	got, err := r.Handoff(tk.ID, ReportOptions{Summary: "parser done", Outputs: []string{file}, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.Starting || got.Attempt != 2 || !slices.Equal(got.Handoffs, []string{file}) || got.LauncherPID != 0 {
		t.Fatalf("task after handoff = %+v", got)
	}
	if !slices.Equal(launched, []string{tk.ID}) {
		t.Fatalf("launchers = %v", launched)
	}
	if got, _ = r.Store.Get(tk.ID); got.LauncherPID != 5151 {
		t.Errorf("launcher pid = %d", got.LauncherPID)
	}
	ev := lastEvent(t, r)
	if ev.Type != "handoff" || ev.Attempt != 2 || !strings.HasPrefix(ev.Summary, "attempt 1 ended near its context limit; attempt 2 continues from "+file) {
		t.Errorf("event = %+v", ev)
	}

	quitsThenIdle(fake)
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if n := fake.count("tab", "create"); n != 0 {
		t.Errorf("tab create calls = %d, want 0", n)
	}
	if keys := fake.call("agent", "send-keys"); !slices.Equal(keys, []string{"agent", "send-keys", "w1:pt1", "ctrl+c", "ctrl+c"}) {
		t.Errorf("send-keys = %q", keys)
	}
	if run := fake.call("pane", "run"); len(run) != 4 || run[2] != "w1:pt1" || !strings.Contains(run[3], "HRK_ATTEMPT=2") {
		t.Errorf("pane run = %q", run)
	}
	if start := strings.Join(lastCall(fake, "agent", "start"), " "); !strings.Contains(start, "--pane w1:pt1") {
		t.Errorf("agent start = %q", start)
	}
	prompt := lastCall(fake, "agent", "prompt")[3]
	if !strings.HasPrefix(prompt, "/ak:cook plan.md\n\n") || !strings.HasSuffix(prompt, handoffNote(got)) ||
		!strings.Contains(prompt, "This is attempt 2 of task "+tk.ID+".") || !strings.Contains(prompt, file) || strings.Contains(prompt, resumeNote) {
		t.Errorf("prompt = %q", prompt)
	}
	if got, _ = r.Store.Get(tk.ID); got.Status != task.Running || got.Attempt != 2 {
		t.Errorf("task = %s attempt %d", got.Status, got.Attempt)
	}
}

func TestHandoffIsCappedAtOne(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	file := handoffFile(t, r, tk.ID)
	if _, err := r.Handoff(tk.ID, ReportOptions{Summary: "s", Outputs: []string{file}, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.Transition(tk.ID, task.Running, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := r.Handoff(tk.ID, ReportOptions{Summary: "s", Outputs: []string{file}, Attempt: 2})
	if err == nil || !strings.Contains(err.Error(), tk.ID+" already used its one context replacement") {
		t.Fatalf("second handoff: %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Attempt != 2 || len(got.Handoffs) != 1 {
		t.Errorf("task = attempt %d handoffs %v", got.Attempt, got.Handoffs)
	}
}

func TestHandoffRefusals(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	file := handoffFile(t, r, tk.ID)
	for _, c := range []struct {
		name string
		o    ReportOptions
		want string
	}{
		{"no summary", ReportOptions{Outputs: []string{file}}, "needs --summary"},
		{"no file", ReportOptions{Summary: "s"}, "exactly one --output"},
		{"two files", ReportOptions{Summary: "s", Outputs: []string{file, file}}, "exactly one --output"},
		{"missing file", ReportOptions{Summary: "s", Outputs: []string{file + ".gone"}}, "no such file"},
		{"folder", ReportOptions{Summary: "s", Outputs: []string{filepath.Dir(file)}}, "is a folder"},
		{"stale attempt", ReportOptions{Summary: "s", Outputs: []string{file}, Attempt: 2}, "is on attempt 1; a report from attempt 2 is ignored"},
	} {
		if _, err := r.Handoff(tk.ID, c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"q?"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Handoff(tk.ID, ReportOptions{Summary: "s", Outputs: []string{file}}); err == nil || !strings.Contains(err.Error(), "only works while it runs") {
		t.Errorf("handoff of a waiting task: %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Attempt != 0 || len(got.Handoffs) != 0 {
		t.Errorf("a refused handoff changed the task: %+v", got)
	}
}

// handedOff returns a running task on its second attempt.
func handedOff(t *testing.T, r *Runner, o SpawnOptions) *task.Task {
	t.Helper()
	tk := running(t, r, o)
	got, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Attempt, t.Handoffs = 2, []string{"/out/handoff-a2.md"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReportFromAnEndedAttemptIsRefused(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := handedOff(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	_, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "old", Attempt: 1})
	if err == nil || err.Error() != tk.ID+" is on attempt 2; a report from attempt 1 is ignored" {
		t.Fatalf("stale report: %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Status != task.Running {
		t.Fatalf("a stale report changed the task to %s", got.Status)
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"q?"}, Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if ev := lastEvent(t, r); ev.Type != "needs-human" || ev.Attempt != 2 {
		t.Errorf("event = %+v", ev)
	}
	// A manual report names no attempt.
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "ok", Manual: true}); err != nil {
		t.Errorf("manual report: %v", err)
	}
}

func TestTurnEndOfAnEndedAttemptIsIgnored(t *testing.T) {
	r, _, e := newTestRunner(t)
	tk := handedOff(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	block, err := r.TurnEnd(context.Background(), tk.ID, task.Claude, []byte(`{}`))
	if err != nil || block != "" {
		t.Fatalf("stale turn-end = %q, %v", block, err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Unreported != 0 || countEvents(t, r, tk.ID, "nudged") != 0 {
		t.Fatalf("a stale turn-end counted: unreported %d", got.Unreported)
	}
	e["HRK_ATTEMPT"] = "2"
	if block, err = r.TurnEnd(context.Background(), tk.ID, task.Claude, []byte(`{}`)); err != nil || block == "" {
		t.Errorf("current turn-end = %q, %v; want a nudge", block, err)
	}
}

func TestQuotaFallbackKeepsTheAttempt(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	r.Config.QuotaFallback = []config.Fallback{{Agent: "codex"}}
	r.StartLauncher = func(string) (int, error) { return 5151, nil }
	tk := handedOff(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.switchAgent(tk, true); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Agent != "codex" || got.Attempt != 2 || got.Status != task.Starting {
		t.Fatalf("task = %s attempt %d %s", got.Agent, got.Attempt, got.Status)
	}
	quitsThenIdle(fake)
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	// The session hit a usage limit; it did not hand off, but the last
	// handoff still describes the attempt.
	prompt := lastCall(fake, "agent", "prompt")[3]
	if !strings.Contains(prompt, resumeNote) || !strings.Contains(prompt, "the handoff of the attempt before it is /out/handoff-a2.md: read it too.") ||
		strings.Contains(prompt, "neared its context limit") {
		t.Errorf("prompt = %q", prompt)
	}
	if run := fake.call("pane", "run"); len(run) != 4 || !strings.Contains(run[3], "HRK_ATTEMPT=2") {
		t.Errorf("pane run = %q", run)
	}
}

func TestRouteTelegramByAttempt(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent wait", ok(`{"result":{"agent":{"agent_status":"working"}}}`))
	tk := handedOff(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	ask := func() {
		t.Helper()
		if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"q?"}, Attempt: 2}); err != nil {
			t.Fatal(err)
		}
	}
	ask()
	_, err := r.RouteTelegram(context.Background(), "#"+tk.ID+"a1 use A")
	if err == nil || err.Error() != "that question was for attempt 1 of "+tk.ID+", which ended" {
		t.Fatalf("stale tag: %v", err)
	}
	if _, err := r.RouteTelegram(context.Background(), "#"+tk.ID+"a3 use A"); err == nil {
		t.Error("a tag for a later attempt was routed")
	}
	if line, err := r.RouteTelegram(context.Background(), "#"+strings.ToUpper(tk.ID)+"A2: use A"); err != nil || !strings.HasPrefix(line, tk.ID+" got the answer") {
		t.Fatalf("current tag = %q, %v", line, err)
	}
	if text := lastCall(fake, "agent", "prompt")[3]; text != "use A" {
		t.Errorf("answer typed = %q", text)
	}
	ask()
	if line, err := r.RouteTelegram(context.Background(), "#"+tk.ID+" use B"); err != nil || !strings.HasPrefix(line, tk.ID+" got the answer") {
		t.Errorf("bare tag = %q, %v", line, err)
	}
}

func TestCheckAttempt(t *testing.T) {
	r, _, _ := newTestRunner(t)
	first := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	second := handedOff(t, r, SpawnOptions{Prompt: "y", Cwd: plainDir(t)})
	for _, c := range []struct {
		id      string
		attempt int
		ok      bool
	}{
		{first.ID, 0, true},
		{first.ID, 1, true},
		{first.ID, 2, false},
		{second.ID, 0, true},
		{second.ID, 2, true},
		{second.ID, 1, false},
	} {
		if err := r.CheckAttempt(c.id, c.attempt); (err == nil) != c.ok {
			t.Errorf("CheckAttempt(%s, %d) = %v", c.id, c.attempt, err)
		}
	}
}

func TestEventsCarryOnlyLaterAttempts(t *testing.T) {
	for attempt, want := range map[int]int{0: 0, 1: 0, 2: 2} {
		if got := eventAttempt(&task.Task{Attempt: attempt}); got != want {
			t.Errorf("eventAttempt(%d) = %d, want %d", attempt, got, want)
		}
	}
	for v, want := range map[string]int{"": 1, "1": 1, "2": 2, " 3 ": 3, "0": 1, "x": 1, "-2": 1} {
		if got := AttemptFromEnv(v); got != want {
			t.Errorf("AttemptFromEnv(%q) = %d, want %d", v, got, want)
		}
	}
}

func TestHandoffAfterAQuotaSwitchSaysWhyItRelaunched(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	r.Config.QuotaFallback = []config.Fallback{{Agent: "codex"}}
	r.StartLauncher = func(string) (int, error) { return 5151, nil }
	tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
	if err := r.switchAgent(tk, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Store.Transition(tk.ID, task.Running, nil, nil); err != nil {
		t.Fatal(err)
	}
	file := handoffFile(t, r, tk.ID)
	if _, err := r.Handoff(tk.ID, ReportOptions{Summary: "s", Outputs: []string{file}, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	quitsThenIdle(fake)
	if err := r.Launch(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	prompt := lastCall(fake, "agent", "prompt")[3]
	if !strings.Contains(prompt, "neared its context limit") || strings.Contains(prompt, resumeNote) {
		t.Errorf("prompt = %q", prompt)
	}
}

// herdr keeps the name of an agent that quit for a moment after it exits, so
// the next session's start can find the name taken by the session it
// replaces.
func TestHandoffWaitsForTheQuitSessionToReleaseItsName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		heldFor  int // name lookups that still find the quit session
		wantName string
	}{
		{"released", 2, "hrk-t1"},
		{"never released", 1000, "hrk-t1-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fake, _ := newTestRunner(t)
			tk := running(t, r, SpawnOptions{Kind: "research", Prompt: "x", Cwd: plainDir(t)})
			if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Herdr.AgentName = "hrk-" + t.ID; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Handoff(tk.ID, ReportOptions{Summary: "s", Outputs: []string{handoffFile(t, r, tk.ID)}, Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			quitsThenIdle(fake)
			lookups := 0
			fake.on("agent get", func(args []string) (string, error) {
				if args[2] == "hrk-t1" {
					if lookups++; lookups > tc.heldFor {
						return herdrErr(herdr.CodeAgentNotFound)(args)
					}
					return `{"result":{"agent":{"agent":null,"agent_status":"idle","name":"hrk-t1"}}}`, nil
				}
				return `{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`, nil
			})
			fake.on("agent start", func(args []string) (string, error) {
				if args[2] == "hrk-t1" && lookups <= tc.heldFor {
					return herdrErr(herdr.CodeAgentNameTaken)(args)
				}
				return `{"result":{"agent":{"agent":"claude","agent_status":"idle"}}}`, nil
			})
			if err := r.Launch(context.Background(), tk.ID); err != nil {
				t.Fatal(err)
			}
			if start := lastCall(fake, "agent", "start"); start[2] != tc.wantName {
				t.Errorf("agent start = %q, want name %s", start, tc.wantName)
			}
			if got, _ := r.Store.Get(tk.ID); got.Status != task.Running || got.Herdr.AgentName != tc.wantName {
				t.Errorf("task = %s agent name %q", got.Status, got.Herdr.AgentName)
			}
		})
	}
}
