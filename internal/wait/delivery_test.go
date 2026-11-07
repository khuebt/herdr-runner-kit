package wait

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func deliveryIdentity(t *testing.T) state.MainNewsLease {
	t.Helper()
	start, err := ProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return state.MainNewsLease{PID: os.Getpid(), ProcessStart: start, Nonce: "parent-reconnect"}
}

func deliveryTarget() state.MainSession {
	return state.MainSession{Owner: owner, Harness: task.Pi, ID: "main-session", Transcript: "/repo/session.jsonl", Cwd: owner, UpdatedAt: time.Now().UTC()}
}

func acquireDelivery(t *testing.T, r *runner.Runner) *Delivery {
	t.Helper()
	d, err := AcquireDelivery(r, Options{Owner: owner, Window: time.Millisecond, Timeout: time.Second}, deliveryIdentity(t), deliveryTarget())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Disconnect)
	return d
}

// Each subprocess is killed at a real persisted boundary, without running its
// deferred cleanup. The test process is the parent unless testing owner death.
func TestDeliveryCrashReplay(t *testing.T) {
	if dir := os.Getenv("HRK_DELIVERY_CHILD"); dir != "" {
		s, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		action := os.Getenv("HRK_DELIVERY_ACTION")
		pid, _ := strconv.Atoi(os.Getenv("HRK_DELIVERY_PARENT"))
		if action == "parent" || action == "compete" {
			pid = os.Getpid()
		}
		start, err := ProcessStart(pid)
		if err != nil {
			t.Fatal(err)
		}
		child, _ := newTestRunner(t)
		child.Store = s
		d, err := AcquireDelivery(child, Options{Owner: owner, Window: time.Millisecond}, state.MainNewsLease{PID: pid, ProcessStart: start, Nonce: "parent-reconnect"}, deliveryTarget())
		if action == "compete" {
			if !errors.Is(err, state.ErrAlreadyWaiting) {
				t.Fatalf("competitor got %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		p, err := d.Prepare(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		switch action {
		case "cursor":
			if err := d.BeginAttempt(p.BatchID); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveCursor(state.CursorName(owner), p.To); err != nil {
				t.Fatal(err)
			}
			p.Attempt = "appending"
		case "commit":
			if err := d.Commit(receipt(d, p)); err != nil {
				t.Fatal(err)
			}
		}
		data, _ := json.Marshal(p)
		if err := os.WriteFile(filepath.Join(dir, "published.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		// The parent sends SIGKILL after reading the complete publication.
		for {
			time.Sleep(time.Second)
		}
	}
	for _, boundary := range []string{"publish", "cursor", "commit", "parent"} {
		t.Run(boundary, func(t *testing.T) {
			r, _ := newTestRunner(t)
			tk := addTask(t, r, task.Done, owner)
			if err := r.Store.SaveCursor(state.CursorName(owner), 0); err != nil {
				t.Fatal(err)
			}
			if err := r.Store.Append(state.Event{Task: tk.ID, Owner: owner, Type: "done", Summary: "original"}); err != nil {
				t.Fatal(err)
			}
			child := deliveryChild(r, boundary)
			var output bytes.Buffer
			child.Stdout = &output
			child.Stderr = &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			}()
			var published state.MainNewsBatch
			deadline := time.Now().Add(5 * time.Second)
			for {
				data, err := os.ReadFile(filepath.Join(r.Store.Dir(), "published.json"))
				if err == nil && json.Unmarshal(data, &published) == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("child did not publish within deadline")
				}
				time.Sleep(10 * time.Millisecond)
			}
			competitor := deliveryChild(r, "compete")
			if out, err := competitor.CombinedOutput(); err != nil {
				t.Fatalf("live-child competitor: %v %s", err, out)
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err := child.Wait()
			waited = true
			if err == nil {
				t.Fatal("child was not killed")
			}
			if boundary != "parent" {
				competitor = deliveryChild(r, "compete")
				if out, err := competitor.CombinedOutput(); err != nil {
					t.Fatalf("dead-child competitor: %v %s", err, out)
				}
			}
			if off, _, _ := r.Store.Cursor(state.CursorName(owner)); boundary == "publish" && off != 0 {
				t.Fatalf("cursor advanced before ACK: %d", off)
			}
			d := acquireDelivery(t, r)
			if boundary == "commit" {
				ack := receipt(d, &published)
				ack.Generation = published.Generation
				if err := d.Commit(ack); err != nil {
					t.Fatalf("lost commit reply: %v", err)
				}
				d.options.Timeout = 30 * time.Millisecond
				if p, err := d.Prepare(context.Background()); err != nil || p != nil {
					t.Fatalf("committed batch repeated: %+v %v", p, err)
				}
			} else {
				replay, err := d.Prepare(context.Background())
				if err != nil || replay == nil || !reflect.DeepEqual(*replay, published) {
					t.Fatalf("replay = %+v, %v; want %+v", replay, err, published)
				}
				if err := d.Commit(receipt(d, replay)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func deliveryChild(r *runner.Runner, action string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeliveryCrashReplay$")
	cmd.Env = append(os.Environ(), "HRK_DELIVERY_CHILD="+r.Store.Dir(), "HRK_DELIVERY_PARENT="+strconv.Itoa(os.Getpid()), "HRK_DELIVERY_ACTION="+action)
	return cmd
}

func preparedDelivery(t *testing.T) (*runner.Runner, *Delivery, *state.MainNewsBatch) {
	t.Helper()
	r, _ := newTestRunner(t)
	d := acquireDelivery(t, r)
	if err := r.Store.Append(state.Event{Task: "t1", Owner: owner, Type: "done", Summary: "first"}); err != nil {
		t.Fatal(err)
	}
	p, err := d.Prepare(context.Background())
	if err != nil || p == nil {
		t.Fatalf("prepare: %+v %v", p, err)
	}
	return r, d, p
}

func receipt(d *Delivery, p *state.MainNewsBatch) state.MainNewsAck {
	return state.MainNewsAck{Owner: owner, BatchID: p.BatchID, Digest: p.Digest, Target: p.Target, Generation: d.Generation()}
}

func TestDeliveryLeaseSurvivesChildAndFencesReconnect(t *testing.T) {
	r, d, p := preparedDelivery(t)
	generation := d.Generation()
	if _, err := AcquireDelivery(r, Options{Owner: owner}, deliveryIdentity(t), deliveryTarget()); !errors.Is(err, state.ErrAlreadyWaiting) {
		t.Fatalf("live child conflict: %v", err)
	}
	d.Disconnect()
	competitor := deliveryIdentity(t)
	competitor.Nonce = "different-parent"
	if _, err := AcquireDelivery(r, Options{Owner: owner}, competitor, deliveryTarget()); !errors.Is(err, state.ErrAlreadyWaiting) {
		t.Fatalf("dead child allowed competitor: %v", err)
	}
	if _, err := Run(context.Background(), r, Options{Owner: owner}); !errors.Is(err, state.ErrAlreadyWaiting) {
		t.Fatalf("legacy bypass: %v", err)
	}
	resumed := acquireDelivery(t, r)
	if resumed.Generation() <= generation {
		t.Fatal("reconnect did not fence old generation")
	}
	ack := receipt(resumed, p)
	ack.Generation = generation
	if err := resumed.Commit(ack); err == nil {
		t.Fatal("accepted stale generation")
	}
	ack.Generation = resumed.Generation()
	if err := resumed.Commit(ack); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Commit(ack); err != nil {
		t.Fatalf("ACK retry: %v", err)
	}
	resumed.Disconnect()
	resumed = acquireDelivery(t, r)
	// Retry the exact pre-reconnect ACK, including its old generation.
	if err := resumed.Commit(ack); err != nil {
		t.Fatalf("ACK retry after reconnect: %v", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), r, Options{Owner: owner})
	if err != nil || res.Idle == "" || len(res.Lines) != 0 {
		t.Fatalf("legacy after close: %+v %v", res, err)
	}
}

func TestDeliveryACKValidationAndCursorCrashRecovery(t *testing.T) {
	r, d, p := preparedDelivery(t)
	for _, mutate := range []func(*state.MainNewsAck){
		func(a *state.MainNewsAck) { a.Owner = "/other" },
		func(a *state.MainNewsAck) { a.BatchID = "other" },
		func(a *state.MainNewsAck) { a.Digest = state.NewsDigest([]string{"other"}) },
		func(a *state.MainNewsAck) { a.Target.ID = "other" },
		func(a *state.MainNewsAck) { a.Target.Harness = task.Omp },
		func(a *state.MainNewsAck) { a.Target.Transcript = "/other/session.jsonl" },
		func(a *state.MainNewsAck) { a.Target.Cwd = "/other" },
	} {
		ack := receipt(d, p)
		mutate(&ack)
		if err := d.Commit(ack); err == nil {
			t.Fatalf("accepted invalid ACK: %+v", ack)
		}
		if off, _, _ := r.Store.Cursor(state.CursorName(owner)); off != p.From {
			t.Fatal("invalid ACK advanced cursor")
		}
	}
	if err := d.BeginAttempt(p.BatchID); err != nil {
		t.Fatal(err)
	}
	// A verifier accepted the receipt and the cursor save succeeded, but the
	// process died before clearing pending. Recovery still returns that batch.
	if err := r.Store.SaveCursor(state.CursorName(owner), p.To); err != nil {
		t.Fatal(err)
	}
	d.Disconnect()
	resumed := acquireDelivery(t, r)
	replay, err := resumed.Prepare(context.Background())
	if err != nil || replay.BatchID != p.BatchID || replay.Attempt != "appending" {
		t.Fatalf("cursor crash replay: %+v %v", replay, err)
	}
	if err := resumed.Commit(receipt(resumed, replay)); err != nil {
		t.Fatal(err)
	}
	n, err := r.Store.ReadMainNews(owner)
	if err != nil || n.Pending != nil || n.LastAck.BatchID != p.BatchID || n.LastAckAt.IsZero() {
		t.Fatalf("committed state: %+v %v", n, err)
	}
}

func TestDeliveryPreservesTargetAndPayload(t *testing.T) {
	r, d, p := preparedDelivery(t)
	d.Disconnect()
	target := deliveryTarget()
	target.ID = "new-session"
	target.Transcript = "/repo/new-session.jsonl"
	resumed, err := AcquireDelivery(r, Options{Owner: owner, Window: time.Millisecond}, deliveryIdentity(t), target)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Disconnect()
	replay, err := resumed.Prepare(context.Background())
	if err != nil || !reflect.DeepEqual(replay, p) {
		t.Fatalf("pending moved to new target: %+v %v", replay, err)
	}
	replay.Lines[0] = "caller changed its copy"
	again, err := resumed.Prepare(context.Background())
	if err != nil || again.Digest != state.NewsDigest(again.Lines) {
		t.Fatalf("caller mutated stored lines: %+v %v", again, err)
	}
	if err := resumed.Commit(receipt(resumed, p)); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Append(state.Event{Owner: owner, Task: "t2", Type: "done"}); err != nil {
		t.Fatal(err)
	}
	next, err := resumed.Prepare(context.Background())
	if err != nil || next.Target.ID != target.ID || next.BatchID == p.BatchID {
		t.Fatalf("next target: %+v %v", next, err)
	}
	events, _, err := r.Store.ReadEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == "woke" {
			t.Fatal("prepare/replay emitted legacy wake measurement")
		}
	}
}

func TestDeliveryRejectsChangedLog(t *testing.T) {
	for _, change := range []string{"truncate", "replace", "rewrite"} {
		t.Run(change, func(t *testing.T) {
			r, d, _ := preparedDelivery(t)
			before, err := os.ReadFile(r.Store.EventsPath())
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "truncate":
				err = os.Truncate(r.Store.EventsPath(), 0)
			case "replace":
				path := r.Store.EventsPath() + ".replacement"
				if err = os.WriteFile(path, before, 0o600); err == nil {
					err = os.Rename(path, r.Store.EventsPath())
				}
			case "rewrite":
				before[0] = ' '
				err = os.WriteFile(r.Store.EventsPath(), before, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Prepare(context.Background()); err == nil {
				t.Fatal("accepted changed event log")
			}
			n, err := r.Store.ReadMainNews(owner)
			if err != nil || n.Pending == nil {
				t.Fatalf("pending lost: %+v %v", n, err)
			}
		})
	}
}

func TestDeliveryDeadParentReclaimedWithPending(t *testing.T) {
	r, d, p := preparedDelivery(t)
	d.Disconnect()
	n, err := r.Store.ReadMainNews(owner)
	if err != nil {
		t.Fatal(err)
	}
	n.Lease.ProcessStart = "old-incarnation-of-this-pid"
	if err := r.Store.SaveMainNews(*n); err != nil {
		t.Fatal(err)
	}
	resumed := acquireDelivery(t, r)
	replay, err := resumed.Prepare(context.Background())
	if err != nil || !reflect.DeepEqual(replay, p) {
		t.Fatalf("reclaimed pending: %+v %v", replay, err)
	}
}

func TestDeliveryQuietTimeoutDoesNotResetActivity(t *testing.T) {
	r, _ := newTestRunner(t)
	addTask(t, r, task.Running, owner)
	d, err := AcquireDelivery(r, Options{Owner: owner, Timeout: 30 * time.Millisecond, Window: time.Millisecond, FlushOnTimeout: true}, deliveryIdentity(t), deliveryTarget())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Disconnect()
	if err := MarkActive(r.Store, Options{Owner: owner}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-49 * time.Minute)
	if err := os.Chtimes(activePath(r.Store, state.CursorName(owner)), at, at); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Append(state.Event{Owner: owner, Task: "t1", Type: "done", Quiet: true}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		p, err := d.Prepare(context.Background())
		if err != nil || p != nil {
			t.Fatalf("technical timeout flushed quiet: %+v %v", p, err)
		}
	}
	info, err := os.Stat(activePath(r.Store, state.CursorName(owner)))
	if err != nil || !info.ModTime().Equal(at) {
		t.Fatalf("retry changed activity: %v", err)
	}
	if off, _, _ := r.Store.Cursor(state.CursorName(owner)); off != 0 {
		t.Fatalf("passed quiet event: %d", off)
	}
	at = time.Now().Add(-50*time.Minute - time.Second)
	if err := os.Chtimes(activePath(r.Store, state.CursorName(owner)), at, at); err != nil {
		t.Fatal(err)
	}
	p, err := d.Prepare(context.Background())
	if err != nil || p == nil {
		t.Fatalf("50 minute flush: %+v %v", p, err)
	}
}

func TestDeliveryIdleWaitsAndSeesAttachments(t *testing.T) {
	r, _ := newTestRunner(t)
	d := acquireDelivery(t, r)
	d.options.Timeout = 150 * time.Millisecond
	began := time.Now()
	p, err := d.Prepare(context.Background())
	if err != nil || p != nil || time.Since(began) < 100*time.Millisecond {
		t.Fatalf("idle spun: %+v %v", p, err)
	}
	if err := r.Store.Append(state.Event{Owner: task.ProjectOwner("attached"), Task: "t1", Type: "done", Summary: "before attach"}); err != nil {
		t.Fatal(err)
	}
	d.options.Timeout = time.Second
	ready := make(chan *state.MainNewsBatch, 1)
	failed := make(chan error, 1)
	go func() { p, err := d.Prepare(context.Background()); ready <- p; failed <- err }()
	time.Sleep(30 * time.Millisecond)
	if err := r.Store.SetMainProjects(owner, []string{"attached"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Append(state.Event{Owner: task.ProjectOwner("attached"), Task: "t2", Type: "done", Summary: "after attach"}); err != nil {
		t.Fatal(err)
	}
	p = <-ready
	err = <-failed
	if err != nil || p == nil || len(p.Lines) != 1 || !strings.Contains(p.Lines[0], "after attach") {
		t.Fatalf("attached: %+v %v", p, err)
	}
	if err := r.Store.SetMainProjects(owner, nil); err != nil {
		t.Fatal(err)
	}
	replay, err := d.Prepare(context.Background())
	if err != nil || !reflect.DeepEqual(replay, p) {
		t.Fatalf("detach altered pending: %+v %v", replay, err)
	}
}

func TestDeliveryIdleRunsOwnerGCAndAcceptsNewTask(t *testing.T) {
	r, _ := newTestRunner(t)
	finished := addTask(t, r, task.Done, owner)
	unrelated := addTask(t, r, task.Done, "/other-owner")
	for _, id := range []string{finished.ID, unrelated.ID} {
		if _, err := r.Store.Update(id, func(tk *task.Task) error {
			tk.Herdr.PaneID = "pane-" + tk.ID
			tk.FinishedAt = time.Now().Add(-time.Hour)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	d := acquireDelivery(t, r)
	d.options.Timeout = 3 * time.Second
	if p, err := d.Prepare(context.Background()); err != nil || p != nil {
		t.Fatalf("idle maintenance: %+v %v", p, err)
	}
	for _, want := range []struct {
		id     string
		closed bool
	}{{finished.ID, true}, {unrelated.ID, false}} {
		got, err := r.Store.Get(want.id)
		if err != nil || got.Herdr.Closed != want.closed {
			t.Fatalf("owner-scoped GC for %s: %+v %v", want.id, got, err)
		}
	}
	newTask := addTask(t, r, task.Held, owner)
	if err := r.Store.Append(state.Event{Owner: owner, Task: newTask.ID, Type: "needs-human", Question: "new task after idle GC"}); err != nil {
		t.Fatal(err)
	}
	p, err := d.Prepare(context.Background())
	if err != nil || p == nil || len(p.Lines) != 1 || !strings.Contains(p.Lines[0], "new task after idle GC") {
		t.Fatalf("new task after maintenance: %+v %v", p, err)
	}
}

func TestDeliveryTaskCursorRemainsIndependent(t *testing.T) {
	r, d, p := preparedDelivery(t)
	res, err := Run(context.Background(), r, Options{Owner: owner, Tasks: []string{"t1"}, Window: time.Millisecond})
	if err != nil || len(res.Lines) != 1 {
		t.Fatalf("task wait: %+v %v", res, err)
	}
	if off, _, _ := r.Store.Cursor(state.CursorName(owner)); off != p.From {
		t.Fatal("task wait moved owner cursor")
	}
	if err := d.Close(); err == nil {
		t.Fatal("close discarded pending")
	}
}

func TestDeliveryProgressPinsLogAndDoesNotBlockNewNews(t *testing.T) {
	r, _ := newTestRunner(t)
	d := acquireDelivery(t, r)
	d.options.Timeout = 30 * time.Millisecond
	if err := r.Store.Append(state.Event{Owner: owner, Type: "progress"}); err != nil {
		t.Fatal(err)
	}
	if p, err := d.Prepare(context.Background()); err != nil || p != nil {
		t.Fatalf("progress: %+v %v", p, err)
	}
	n, err := r.Store.ReadMainNews(owner)
	if err != nil || n.Log.Identity == "" || n.Log.Offset == 0 {
		t.Fatalf("unanchored progress: %+v %v", n, err)
	}
	off, _, _ := r.Store.Cursor(state.CursorName(owner))
	if off != n.Log.Offset {
		t.Fatal("progress cursor did not move")
	}
	if err := r.Store.Append(state.Event{Owner: owner, Type: "done", Task: "t1"}); err != nil {
		t.Fatal(err)
	}
	p, err := d.Prepare(context.Background())
	if err != nil || p == nil || p.From != off {
		t.Fatalf("news after progress: %+v %v", p, err)
	}
}

func TestDeliveryFailedPendingWriteKeepsCursor(t *testing.T) {
	r, _ := newTestRunner(t)
	d := acquireDelivery(t, r)
	if err := r.Store.Append(state.Event{Owner: owner, Type: "done", Task: "t1"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.Store.Dir(), "main-news-"+state.CursorName(owner)+".json")
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := d.Prepare(context.Background()); err == nil || p != nil {
		t.Fatalf("failed state write published: %+v %v", p, err)
	}
	if off, _, _ := r.Store.Cursor(state.CursorName(owner)); off != 0 {
		t.Fatal("failed prepare advanced cursor")
	}
	if d.unlock != nil {
		t.Fatal("failed persistence did not stop delivery")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	resumed := acquireDelivery(t, r)
	if p, err := resumed.Prepare(context.Background()); err != nil || p == nil {
		t.Fatalf("news lost after failure: %+v %v", p, err)
	}
}

func TestDeliveryFailedClearRecoversSavedCursor(t *testing.T) {
	r, d, p := preparedDelivery(t)
	path := filepath.Join(r.Store.Dir(), "main-news-"+state.CursorName(owner)+".json")
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.Commit(receipt(d, p)); err == nil {
		t.Fatal("failed clear reported committed")
	}
	if off, _, _ := r.Store.Cursor(state.CursorName(owner)); off != p.To {
		t.Fatal("clear attempted before cursor save")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	resumed := acquireDelivery(t, r)
	replay, err := resumed.Prepare(context.Background())
	if err != nil || !reflect.DeepEqual(replay, p) {
		t.Fatalf("pending lost after clear failure: %+v %v", replay, err)
	}
	if err := resumed.Commit(receipt(resumed, replay)); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryReconnectDoesNotRearmQuietDeadline(t *testing.T) {
	r, _ := newTestRunner(t)
	addTask(t, r, task.Running, owner)
	d := acquireDelivery(t, r)
	d.news.ActiveSince = time.Now().Add(-50*time.Minute - time.Second)
	if err := r.Store.SaveMainNews(*d.news); err != nil {
		t.Fatal(err)
	}
	d.Disconnect()
	resumed := acquireDelivery(t, r)
	if err := r.Store.Append(state.Event{Owner: owner, Type: "done", Task: "t1", Quiet: true}); err != nil {
		t.Fatal(err)
	}
	p, err := resumed.Prepare(context.Background())
	if err != nil || p == nil {
		t.Fatalf("reconnect rearmed quiet deadline: %+v %v", p, err)
	}
}

func TestDeliveryLeaseBlocksLegacyBeforeAnyPending(t *testing.T) {
	r, _ := newTestRunner(t)
	d := acquireDelivery(t, r)
	d.Disconnect()
	if _, err := Run(context.Background(), r, Options{Owner: owner}); !errors.Is(err, state.ErrAlreadyWaiting) {
		t.Fatalf("legacy bypassed idle parent lease: %v", err)
	}
	resumed := acquireDelivery(t, r)
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), r, Options{Owner: owner}); err != nil {
		t.Fatalf("closed lease blocks legacy: %v", err)
	}
}
