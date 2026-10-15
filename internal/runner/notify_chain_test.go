package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// telegramLog runs a fake Telegram and the notifier: each event the runner
// hands to Notify is sent at once, in the test's process.
type telegramLog struct {
	mu    sync.Mutex
	texts []string
}

func (l *telegramLog) sent() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.texts)
}

func notifyInline(t *testing.T, r *Runner) *telegramLog {
	t.Helper()
	l := &telegramLog{}
	withTelegram(t, r, func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Text string }
		_ = json.NewDecoder(req.Body).Decode(&body)
		l.mu.Lock()
		l.texts = append(l.texts, body.Text)
		l.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	r.Notify = func(ev state.Event) {
		if err := r.SendNotification(context.Background(), ev); err != nil {
			t.Errorf("notify %s: %v", ev.Type, err)
		}
	}
	return l
}

func TestFirstRoundDoneWithAQueuedChildIsDeferred(t *testing.T) {
	r, _, _ := newTestRunner(t)
	l := notifyInline(t, r)
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Title: "spec", Cwd: dir})
	spawnStarting(t, r, SpawnOptions{Prompt: "b", Cwd: dir, After: []string{p.ID}})
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Done, Summary: "first"}); err != nil {
		t.Fatal(err)
	}
	if got := l.sent(); len(got) != 0 {
		t.Fatalf("a first-round parent done was sent: %q", got)
	}
	if ev := events(t, r, p.ID); !slices.Contains(ev, "notify-deferred") {
		t.Errorf("events = %v", ev)
	}
	// A revision after send is a round the user asked for.
	if _, err := r.Send(context.Background(), p.ID, "revise"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Done, Summary: "revised"}); err != nil {
		t.Fatal(err)
	}
	if got := l.sent(); len(got) != 1 || !strings.Contains(got[0], "revised") {
		t.Errorf("sent = %q", got)
	}
}

func TestDoneWithDecisionsIsSent(t *testing.T) {
	r, _, _ := newTestRunner(t)
	l := notifyInline(t, r)
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Cwd: dir})
	spawnStarting(t, r, SpawnOptions{Prompt: "b", Cwd: dir, After: []string{p.ID}})
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Done, Summary: "assessed", Decisions: []string{"class? -> minor"}}); err != nil {
		t.Fatal(err)
	}
	if got := l.sent(); len(got) != 1 || !strings.Contains(got[0], "Decided: class? -> minor") {
		t.Errorf("sent = %q", got)
	}
}

func TestDoneWithARunningChildIsSent(t *testing.T) {
	r, _, _ := newTestRunner(t)
	l := notifyInline(t, r)
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Cwd: dir})
	c := spawnStarting(t, r, SpawnOptions{Prompt: "b", Cwd: dir, After: []string{p.ID}})
	if _, _, err := r.Store.Transition(c.ID, task.Running, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Done, Summary: "first"}); err != nil {
		t.Fatal(err)
	}
	if got := l.sent(); len(got) != 1 {
		t.Errorf("sent = %q", got)
	}
}

func TestStoppedChainEndSendsTheDeferredDones(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"idle"}}}`))
	r.Config.Telegram.Notify = []string{"done"}
	l := notifyInline(t, r)
	dir := plainDir(t)
	a := running(t, r, SpawnOptions{Prompt: "a", Title: "spec", Cwd: dir})
	b := spawnStarting(t, r, SpawnOptions{Prompt: "b", Title: "ux", Cwd: dir, After: []string{a.ID}})
	end := spawnStarting(t, r, SpawnOptions{Prompt: "c", Title: "pack", Cwd: dir, After: []string{b.ID}})
	if _, err := r.Report(a.ID, ReportOptions{Status: task.Done, Summary: "spec written"}); err != nil {
		t.Fatal(err)
	}
	// b starts once a is done; it reports while the pack still waits.
	if _, _, err := r.Store.Transition(b.ID, task.Running, nil, func(t *task.Task) { t.Herdr.PaneID = "w1:p2" }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(b.ID, ReportOptions{Status: task.Done, Summary: strings.Repeat("ux flows ", 40)}); err != nil {
		t.Fatal(err)
	}
	if got := l.sent(); len(got) != 0 {
		t.Fatalf("deferred dones were sent early: %q", got)
	}
	if _, err := r.Stop(context.Background(), end.ID, false); err != nil {
		t.Fatal(err)
	}
	got := l.sent()
	if len(got) != 1 || !strings.HasPrefix(got[0], "#t3 stopped; the chain before it finished: #t1 spec: spec written; #t2 ux: ux flows") {
		t.Fatalf("sent = %q", got)
	}
	if strings.Count(got[0], "ux flows") > 25 {
		t.Errorf("a summary was not cut: %q", got[0])
	}
	if _, err := r.Stop(context.Background(), end.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := l.sent(); len(got) != 1 {
		t.Errorf("a second stop sent again: %q", got)
	}
}

func TestDoneNotSelectedSendsNothing(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	fake.on("agent get", ok(`{"result":{"agent":{"agent_status":"idle"}}}`))
	r.Config.Telegram.Notify = []string{"needs-human"}
	l := notifyInline(t, r)
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Cwd: dir})
	c := spawnStarting(t, r, SpawnOptions{Prompt: "b", Cwd: dir, After: []string{p.ID}})
	if _, err := r.Report(p.ID, ReportOptions{Status: task.Done, Summary: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stop(context.Background(), c.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := l.sent(); len(got) != 0 {
		t.Errorf("sent = %q", got)
	}
	if ev := events(t, r, p.ID); slices.Contains(ev, "notify-deferred") {
		t.Errorf("events = %v", ev)
	}
}
