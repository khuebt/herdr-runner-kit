package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/telegram"
)

func withTelegram(t *testing.T, r *Runner, h http.HandlerFunc) {
	t.Helper()
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("TELEGRAM_BOT_TOKEN=1:abc\nTELEGRAM_CHAT_ID=42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Config.Telegram.EnvFile = envFile
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	r.Telegram = &telegram.Client{BaseURL: srv.URL, HTTP: &http.Client{Timeout: telegram.SendTimeout}}
}

func lastEvent(t *testing.T, r *Runner) state.Event {
	t.Helper()
	all, _, _ := r.Store.ReadEventsSince(0)
	return all[len(all)-1]
}

func TestRecordNotifiesSelectedTypes(t *testing.T) {
	r, _, _ := newTestRunner(t)
	var sent []string
	r.Notify = func(ev state.Event) { sent = append(sent, ev.Type) }
	r.Config.Telegram.Notify = []string{"needs-human", "failed"}
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"q?"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sent, []string{"needs-human"}) {
		t.Errorf("notified = %v (spawned, started and done must not notify)", sent)
	}
}

func TestSendNotification(t *testing.T) {
	r, _, _ := newTestRunner(t)
	var texts atomic.Value
	withTelegram(t, r, func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Text string }
		_ = json.NewDecoder(req.Body).Decode(&body)
		texts.Store(body.Text)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	tk := running(t, r, SpawnOptions{Prompt: "x", Title: "fix login", Cwd: plainDir(t)})
	ev := state.Event{Task: tk.ID, Type: "needs-human", Question: "Drop v1?"}
	if err := r.SendNotification(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if body, _ := texts.Load().(string); body != "#t1 needs a decision · fix login\nDrop v1?\nReply: #t1 <answer>" {
		t.Errorf("body = %s", body)
	}
	if last := lastEvent(t, r); last.Type != "notified" || last.Summary != "needs-human" {
		t.Errorf("event = %+v", last)
	}
}

// A runner on a scratch state sends nothing, even with the user's bot in
// the config.
func TestSendNotificationFromAScratchStateSendsNothing(t *testing.T) {
	r, _, e := newTestRunner(t)
	var calls atomic.Int32
	withTelegram(t, r, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	r.Config.Telegram.Notify = []string{"failed", "needs-human"}
	e["HOME"], e["HRK_STATE_DIR"] = t.TempDir(), r.Store.Dir()
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	for _, typ := range []string{"needs-human", "failed"} {
		if err := r.SendNotification(context.Background(), state.Event{Task: tk.ID, Type: typ, Summary: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("a scratch state sent %d Telegram messages", n)
	}
	if ev := events(t, r, tk.ID); slices.Contains(ev, "notified") || slices.Contains(ev, "notify-failed") {
		t.Errorf("events = %v", ev)
	}
}

func TestSendNotificationFailureIsRecorded(t *testing.T) {
	r, _, _ := newTestRunner(t)
	withTelegram(t, r, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	start := time.Now()
	if err := r.SendNotification(context.Background(), state.Event{Task: tk.ID, Type: "failed", Summary: "x"}); err == nil {
		t.Fatal("want an error")
	}
	if last := lastEvent(t, r); last.Type != "notify-failed" || strings.Contains(last.Summary, "1:abc") {
		t.Errorf("event = %+v", last)
	}
	if time.Since(start) > telegram.SendTimeout+time.Second {
		t.Error("notify took longer than its timeout")
	}
}

func TestDoneWithDependentsDoesNotNotify(t *testing.T) {
	r, _, _ := newTestRunner(t)
	var calls atomic.Int32
	withTelegram(t, r, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	dir := plainDir(t)
	p := running(t, r, SpawnOptions{Prompt: "a", Cwd: dir})
	spawnStarting(t, r, SpawnOptions{Prompt: "b", Cwd: dir, After: []string{p.ID}})
	if err := r.SendNotification(context.Background(), state.Event{Task: p.ID, Type: "done", Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Error("a parent's done was sent")
	}
}

func TestNoCredentialsIsSilent(t *testing.T) {
	r, _, e := newTestRunner(t)
	r.Config.Telegram.EnvFile = ""
	e["HOME"] = t.TempDir() // no plugin files there
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	if err := r.SendNotification(context.Background(), state.Event{Task: tk.ID, Type: "failed"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if last := lastEvent(t, r); last.Type == "notify-failed" || last.Type == "notified" {
		t.Errorf("event = %+v", last)
	}
}

func TestRouteTelegram(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Runner, *fakeHerdr, string) {
		r, fake, _ := newTestRunner(t)
		return r, fake, plainDir(t)
	}
	ask := func(t *testing.T, r *Runner, dir string) *task.Task {
		tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: dir})
		if _, err := r.Report(tk.ID, ReportOptions{Status: task.NeedsHuman, Questions: []string{"A or B?"}}); err != nil {
			t.Fatal(err)
		}
		return tk
	}

	t.Run("tag", func(t *testing.T) {
		r, fake, dir := setup(t)
		ask(t, r, dir)
		b := ask(t, r, dir)
		line, err := r.RouteTelegram(ctx, "#T2 dùng phương án A")
		if err != nil || line != b.ID+" got the answer and is working" {
			t.Fatalf("%q %v", line, err)
		}
		if p := fake.call("agent", "prompt"); p[2] != "w1:p"+b.ID || p[3] != "dùng phương án A" {
			t.Errorf("prompt = %q", p)
		}
	})
	t.Run("one waiter without tag", func(t *testing.T) {
		r, _, dir := setup(t)
		a := ask(t, r, dir)
		running(t, r, SpawnOptions{Prompt: "busy", Cwd: dir})
		if line, err := r.RouteTelegram(ctx, "B"); err != nil || !strings.HasPrefix(line, a.ID+" got the answer") {
			t.Fatalf("%q %v", line, err)
		}
	})
	t.Run("several waiters", func(t *testing.T) {
		r, _, dir := setup(t)
		ask(t, r, dir)
		ask(t, r, dir)
		if line, _ := r.RouteTelegram(ctx, "A"); line != "ambiguous: waiting t1, t2; ask for a #id tag" {
			t.Errorf("line = %q", line)
		}
	})
	t.Run("unknown tag", func(t *testing.T) {
		r, _, _ := setup(t)
		if _, err := r.RouteTelegram(ctx, "#t9 hi"); err == nil {
			t.Error("unknown task accepted")
		}
	})
	t.Run("meta", func(t *testing.T) {
		r, _, dir := setup(t)
		ask(t, r, dir)
		if line, _ := r.RouteTelegram(ctx, "status?"); !strings.HasPrefix(line, "meta:") {
			t.Errorf("line = %q", line)
		}
	})
	t.Run("project status", func(t *testing.T) {
		r, _, dir := setup(t)
		if _, err := r.Store.ReserveProject("demo-app", plainDir(t)); err != nil {
			t.Fatal(err)
		}
		if err := r.Store.CommitProject("demo-app"); err != nil {
			t.Fatal(err)
		}
		// Nothing waits: it is a question for the main session.
		if line, err := r.RouteTelegram(ctx, "Status demo-app?"); err != nil || line != "meta: project status demo-app" {
			t.Fatalf("%q %v", line, err)
		}
		// The only waiting runner may have asked for exactly this reply.
		a := ask(t, r, dir)
		line, err := r.RouteTelegram(ctx, "status demo-app")
		if err != nil || line != `ambiguous: "status demo-app" could be an answer for `+a.ID+"; ask, or show project status" {
			t.Fatalf("%q %v", line, err)
		}
		if got, _ := r.Store.Get(a.ID); got.Status != task.NeedsHuman {
			t.Errorf("the reply reached %s: %s", a.ID, got.Status)
		}
		// An unregistered name is an ordinary reply.
		if line, err := r.RouteTelegram(ctx, "status other-app"); err != nil || !strings.HasPrefix(line, a.ID+" got the answer") {
			t.Fatalf("%q %v", line, err)
		}
	})
	t.Run("gate approval is meta", func(t *testing.T) {
		r, _, dir := setup(t)
		if _, err := r.Store.ReserveProject("demo-app", plainDir(t)); err != nil {
			t.Fatal(err)
		}
		if err := r.Store.CommitProject("demo-app"); err != nil {
			t.Fatal(err)
		}
		held := spawnStarting(t, r, SpawnOptions{Prompt: "cook", Cwd: dir, Gate: task.GateHuman})
		for text, want := range map[string]string{
			"approve spec demo-app":  "meta: user approves spec of demo-app; confirm and run project approve spec --project demo-app",
			"Approve Pack demo-app.": "meta: user approves pack of demo-app; confirm and run project approve pack --project demo-app",
			"approve spec other-app": `meta: user approves spec of "other-app", which is not a registered project; ask which project they mean`,
		} {
			if line, err := r.RouteTelegram(ctx, text); err != nil || line != want {
				t.Errorf("%q: %q %v", text, line, err)
			}
		}
		if got, _ := r.Store.Get(held.ID); got.Status != task.Held || got.Prompt != "cook" {
			t.Errorf("a gate approval reached the held task: %+v", got)
		}
	})
	t.Run("held approval and note", func(t *testing.T) {
		r, _, dir := setup(t)
		h1 := spawnStarting(t, r, SpawnOptions{Prompt: "cook", Cwd: dir, Gate: task.GateHuman})
		if line, err := r.RouteTelegram(ctx, "duyệt"); err != nil || line != h1.ID+" released" {
			t.Fatalf("%q %v", line, err)
		}
		if got, _ := r.Store.Get(h1.ID); got.Status != task.Starting || got.Prompt != "cook" {
			t.Errorf("approved: %+v", got)
		}
		h2 := spawnStarting(t, r, SpawnOptions{Prompt: "cook", Cwd: dir, Gate: task.GateHuman})
		if _, err := r.RouteTelegram(ctx, "#"+h2.ID+" only the API part"); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.Store.Get(h2.ID); !strings.HasSuffix(got.Prompt, "Note from the user: only the API part") {
			t.Errorf("note: %q", got.Prompt)
		}
	})
	t.Run("a dependency hold leaves the only question unambiguous", func(t *testing.T) {
		r, _, dir := setup(t)
		h := spawnStarting(t, r, SpawnOptions{Prompt: "cook", Cwd: dir, Gate: task.GateHuman, Reason: "needs D2"})
		a := ask(t, r, dir)
		if line, err := r.RouteTelegram(ctx, "B"); err != nil || !strings.HasPrefix(line, a.ID+" got the answer") {
			t.Fatalf("%q %v", line, err)
		}
		if got, _ := r.Store.Get(h.ID); got.Status != task.Held {
			t.Errorf("the answer reached the held task: %+v", got)
		}
	})
	t.Run("held with a reason needs a tag", func(t *testing.T) {
		r, _, dir := setup(t)
		h := spawnStarting(t, r, SpawnOptions{Prompt: "cook", Title: "checkout", Cwd: dir, Gate: task.GateHuman, Reason: "needs D2"})
		if ev := lastEvent(t, r); ev.Summary != "held for release: checkout (needs D2)" {
			t.Errorf("held event = %+v", ev)
		}
		for _, text := range []string{"ok", "release it, the keys are in"} {
			line, err := r.RouteTelegram(ctx, text)
			if err != nil || line != "ambiguous: "+h.ID+` is held (needs D2); release it with "#`+h.ID+` ok" once that is settled` {
				t.Errorf("%q: %q %v", text, line, err)
			}
		}
		if got, _ := r.Store.Get(h.ID); got.Status != task.Held || got.Prompt != "cook" {
			t.Fatalf("an untagged reply released it: %+v", got)
		}
		if line, err := r.RouteTelegram(ctx, "#"+h.ID+" ok"); err != nil || line != h.ID+" released" {
			t.Fatalf("%q %v", line, err)
		}
	})
	t.Run("running gets a message, done refuses", func(t *testing.T) {
		r, _, dir := setup(t)
		tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: dir})
		if line, err := r.RouteTelegram(ctx, "#"+tk.ID+" also add tests"); err != nil || !strings.HasPrefix(line, tk.ID+" received the message") {
			t.Fatalf("%q %v", line, err)
		}
		if _, err := r.Report(tk.ID, ReportOptions{Status: task.Done, Summary: "ok"}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.RouteTelegram(ctx, "#"+tk.ID+" more"); err == nil {
			t.Error("routed to a done task")
		}
	})
}
