package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestLearningCountsReopensTabAnswersAndSwitches(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	ev := func(s int, id, typ string, st task.Status) state.Event {
		return state.Event{TS: at(s), Task: id, Type: typ, Status: st}
	}
	events := []state.Event{
		// t1 finishes, is reopened by a send, and finishes again.
		ev(0, "t1", "spawned", task.Starting),
		ev(5, "t1", "started", task.Running),
		ev(60, "t1", "nudged", task.Running),
		ev(100, "t1", "done", task.Done),
		ev(200, "t1", "sent", task.Running),
		ev(300, "t1", "done", task.Done),
		// t2 is held, asks twice: answered in its tab, then by the main session.
		ev(0, "t2", "held", task.Held),
		ev(30, "t2", "spawned", task.Starting),
		ev(35, "t2", "started", task.Running),
		ev(40, "t2", "needs-human", task.NeedsHuman),
		ev(100, "t2", "needs-human", task.NeedsHuman),
		ev(110, "t2", "answered", task.Running),
		ev(120, "t2", "sent", task.Running),
		ev(150, "t2", "done", task.Done),
		// t3 switches agents, ends a turn unreported, and is still asking.
		ev(0, "t3", "spawned", task.Starting),
		ev(2, "t3", "started", task.Running),
		ev(50, "t3", "agent-switched", task.Running),
		ev(70, "t3", "turn-ended-unreported", task.Running),
		ev(80, "t3", "needs-human", task.NeedsHuman),
		// t4 is held long before its spawn, and a send that leaves it
		// done reopens nothing.
		ev(0, "t4", "held", task.Held),
		ev(600, "t4", "spawned", task.Starting),
		ev(601, "t4", "started", task.Running),
		ev(610, "t4", "needs-human", task.NeedsHuman),
		ev(620, "t4", "answered", task.Running),
		ev(650, "t4", "done", task.Done),
		ev(700, "t4", "sent", task.Done),
		{TS: at(90), Type: "woke", Summary: "t3"},
	}
	tasks := []*task.Task{
		{ID: "t1", Kind: "code", Agent: "codex", Status: task.Done},
		{ID: "t2", Kind: "research", Agent: "claude", Status: task.Done},
		{ID: "t3", Kind: "code", Agent: "claude", FallbackFrom: []string{"codex"}, Status: task.NeedsHuman},
		{ID: "t4", Kind: "fix", Agent: "claude", Status: task.Done},
	}
	var out strings.Builder
	if err := printLearning(&out, events, tasks, nil, at(100)); err != nil {
		t.Fatal(err)
	}
	rows := map[string][]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		f := strings.Fields(line)
		rows[f[0]] = f
	}
	want := map[string]string{
		//    kind agents status attempts nudges sends answers needs-human unreported switches duration waited
		"t1": "code codex done 2 1 1 0 0 0 0 5m0s 0s",
		// Time held before the spawn is no wait: waited stays within duration.
		"t2": "research claude done 1 0 1 2 2 0 0 2m0s 1m10s",
		"t3": "code codex->claude needs-human 1 0 0 0 1 1 1 - 20s",
		"t4": "fix claude done 1 0 1 1 1 0 0 50s 10s",
	}
	for id, w := range want {
		if got := strings.Join(rows[id][1:], " "); got != w {
			t.Errorf("%s = %q, want %q\n%s", id, got, w, out.String())
		}
	}
	if len(rows) != 5 { // the header and four tasks; the wake-up is no task
		t.Errorf("rows:\n%s", out.String())
	}

	out.Reset()
	if err := printLearning(&out, events, tasks, []string{"t2"}, at(100)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "t1") || !strings.Contains(out.String(), "t2") {
		t.Errorf("--task t2:\n%s", out.String())
	}
}
