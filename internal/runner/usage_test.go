package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// fixtureUnits are the cost units of the synthetic Claude transcript in the
// harness testdata, its subagent included.
const fixtureUnits = 2943

// copyTranscript copies the synthetic Claude transcript and its subagent
// into a new folder and returns the session's path.
func copyTranscript(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "harness", "testdata", "transcript-claude")
	dst := t.TempDir()
	for _, rel := range []string{"s1.jsonl", filepath.Join("s1", "subagents", "agent-a1.jsonl")} {
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, rel), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dst, "s1.jsonl")
}

func stopPayload(transcript string) []byte {
	return fmt.Appendf(nil, `{"hook_event_name":"Stop","transcript_path":%q,"background_tasks":[]}`, transcript)
}

func TestTurnEndRecordsTheTranscriptOnce(t *testing.T) {
	r, _, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	path := copyTranscript(t)
	for range 2 {
		if _, err := r.TurnEnd(context.Background(), tk.ID, task.Claude, stopPayload(path)); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := r.Store.Get(tk.ID)
	if want := []task.Transcript{{Harness: task.Claude, Path: path}}; len(got.Transcripts) != 1 || got.Transcripts[0] != want[0] {
		t.Errorf("transcripts = %+v, want %+v", got.Transcripts, want)
	}
	if got.Usage != nil {
		t.Errorf("a running task's usage was read: %+v", got.Usage)
	}
}

func TestUsageCoversEveryAttemptAtTheTurnAfterTheReport(t *testing.T) {
	r, _, e := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	first, second := copyTranscript(t), copyTranscript(t)
	ctx := context.Background()
	if _, err := r.TurnEnd(ctx, tk.ID, task.Claude, stopPayload(first)); err != nil {
		t.Fatal(err)
	}
	// A context handoff starts the second session.
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error { t.Attempt = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	e["HRK_ATTEMPT"] = "2"
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "fixed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TurnEnd(ctx, tk.ID, task.Claude, stopPayload(second)); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Usage == nil || got.Usage.Units != 2*fixtureUnits || got.Usage.Subagents != 2 || got.Usage.Calls != 6 {
		t.Fatalf("usage = %+v, want both sessions' %d units", got.Usage, 2*fixtureUnits)
	}
}

func TestTurnEndLeavesTheLastCallToTheUsageReader(t *testing.T) {
	r, _, _ := newTestRunner(t)
	var started []string
	r.StartUsage = func(id string) error { started = append(started, id); return nil }
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "fixed"}); err != nil {
		t.Fatal(err)
	}
	// Claude writes the closing reply after its Stop hooks return.
	path := filepath.Join(t.TempDir(), "s.jsonl")
	call := func(id, stop string) string {
		return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":"claude-haiku-5-5","stop_reason":%q,"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n", id, stop)
	}
	if err := os.WriteFile(path, []byte(call("m1", "tool_use")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TurnEnd(context.Background(), tk.ID, task.Claude, stopPayload(path)); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Usage == nil || got.Usage.Calls != 1 || len(started) != 1 || started[0] != tk.ID {
		t.Fatalf("usage = %+v, readers started %q; want the written call and one reader", got.Usage, started)
	}
	sleeps := 0
	r.Sleep = func(time.Duration) {
		if sleeps++; sleeps == 2 {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString(call("m2", "end_turn"))
			_ = f.Close()
		}
	}
	r.AwaitUsage(tk.ID)
	if got, _ := r.Store.Get(tk.ID); got.Usage == nil || got.Usage.Calls != 2 {
		t.Errorf("usage after the reader = %+v, want the closing reply's call counted", got.Usage)
	}
}

func TestCloseRefreshesUsage(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := finishedRunner(t, r, fake)
	path := copyTranscript(t)
	if _, err := r.Store.Update(tk.ID, func(t *task.Task) error {
		t.Transcripts = []task.Transcript{{Harness: task.Claude, Path: path}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stop(context.Background(), tk.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Get(tk.ID)
	if got.Usage == nil || got.Usage.Units != fixtureUnits {
		t.Errorf("usage after close = %+v, want %d units", got.Usage, fixtureUnits)
	}
}

func TestUnreadableTranscriptNeverFails(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	gone := filepath.Join(t.TempDir(), "gone.jsonl")
	ctx := context.Background()
	if _, err := r.TurnEnd(ctx, tk.ID, task.Claude, stopPayload(gone)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "fixed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TurnEnd(ctx, tk.ID, task.Claude, stopPayload(gone)); err != nil {
		t.Fatalf("turn end with a missing transcript: %v", err)
	}
	fake.on("pane process-info", ok(`{"result":{"process_info":{"shell_pid":100}}}`))
	if _, err := r.Stop(ctx, tk.ID, false); err != nil {
		t.Fatalf("close with a missing transcript: %v", err)
	}
	if got, _ := r.Store.Get(tk.ID); got.Usage != nil {
		t.Errorf("usage from a missing transcript: %+v", got.Usage)
	}
}

func TestReadUsageSkipsHarnessesWithoutUsage(t *testing.T) {
	path := copyTranscript(t)
	u := ReadUsage([]task.Transcript{{Harness: task.Codex, Path: path}}, time.Time{}, time.Time{})
	if u != nil {
		t.Errorf("codex usage = %+v, want nil", u)
	}
	u = ReadUsage([]task.Transcript{{Harness: task.Codex, Path: path}, {Harness: task.Claude, Path: path}}, time.Time{}, time.Time{})
	if u == nil || u.Units != fixtureUnits {
		t.Errorf("mixed usage = %+v, want the Claude session's", u)
	}
}
