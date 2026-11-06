package wait

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// DeliveryConflict also matches ErrAlreadyWaiting, as legacy wait callers expect.
type DeliveryConflict struct{ Owner, Reason string }

func (e *DeliveryConflict) Error() string {
	return "main news conflict for " + e.Owner + ": " + e.Reason + "; resume pi/omp to reconcile delivery"
}
func (e *DeliveryConflict) Unwrap() error { return state.ErrAlreadyWaiting }

// Delivery owns the cursor flock until Disconnect or Close. Its methods are
// serialized by the bridge command loop; cancel Prepare before closing it.
type Delivery struct {
	r       *runner.Runner
	options Options
	target  state.MainSession
	news    *state.MainNews
	unlock  func()
}

// ProcessStart identifies a parent independently of PID reuse. An empty result
// means the process is gone. Inspection failures fail closed.
func ProcessStart(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid parent PID")
	}
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect parent process: %w", err)
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return "", fmt.Errorf("invalid parent process stat")
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 20 {
			return "", fmt.Errorf("incomplete parent process stat")
		}
		if fields[0] == "Z" || fields[0] == "X" {
			return "", nil
		}
		boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil {
			return "", fmt.Errorf("inspect boot identity: %w", err)
		}
		return strings.TrimSpace(string(boot)) + ":" + fields[19], nil
	}
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("parent identity unsupported on %s", runtime.GOOS)
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("inspect parent: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspect parent start time: %w", err)
	}
	start := strings.TrimSpace(string(out))
	if start == "" {
		return "", fmt.Errorf("missing parent start time")
	}
	return start, nil
}

func leaseAlive(lease *state.MainNewsLease) (bool, error) {
	if lease == nil {
		return false, nil
	}
	start, err := ProcessStart(lease.PID)
	return start != "" && start == lease.ProcessStart, err
}

// AcquireDelivery takes the same lock as legacy wait. A dead child never
// releases its living parent's persisted lease to a different main.
func AcquireDelivery(r *runner.Runner, o Options, lease state.MainNewsLease, target state.MainSession) (*Delivery, error) {
	if o.Owner == "" || len(o.Tasks) > 0 || (o.Cursor != "" && o.Cursor != state.CursorName(o.Owner)) {
		return nil, fmt.Errorf("main delivery requires the owner's existing cursor")
	}
	if target.Owner != o.Owner {
		return nil, fmt.Errorf("delivery target owner mismatch")
	}
	if err := state.ValidateMainNewsTarget(target); err != nil {
		return nil, err
	}
	if lease.Nonce == "" {
		return nil, fmt.Errorf("delivery requires a reconnect nonce")
	}
	alive, err := leaseAlive(&lease)
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, fmt.Errorf("delivery parent identity is no longer alive")
	}
	o = waitOptions(o)
	o.FlushOnTimeout = false // technical retries must not flush quiet news
	unlock, err := r.Store.LockCursor(o.Cursor, "main-news")
	if err != nil {
		if errors.Is(err, state.ErrAlreadyWaiting) {
			return nil, &DeliveryConflict{Owner: o.Owner, Reason: err.Error()}
		}
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			unlock()
		}
	}()
	n, err := r.Store.ReadMainNews(o.Owner)
	if err != nil {
		return nil, err
	}
	if n != nil {
		alive, err := leaseAlive(n.Lease)
		if err != nil {
			return nil, err
		}
		if alive && *n.Lease != lease {
			return nil, &DeliveryConflict{Owner: o.Owner, Reason: "another main parent still owns the lease"}
		}
	} else {
		n = &state.MainNews{Version: state.MainNewsVersion, Owner: o.Owner, Cursor: o.Cursor, ActiveSince: time.Now().UTC()}
		if err := r.Store.InitCursor(o.Cursor); err != nil {
			return nil, err
		}
		off, _, err := r.Store.Cursor(o.Cursor)
		if err != nil {
			return nil, err
		}
		n.Log, err = logCheckpoint(r.Store, off)
		if err != nil {
			return nil, err
		}
	}
	if n.Lease == nil || *n.Lease != lease {
		n.ActiveSince = time.Now().UTC()
	}
	n.Lease = &lease
	d := &Delivery{r: r, options: o, target: target, news: n, unlock: unlock}
	if err := d.checkLog(); err != nil {
		return nil, err
	}
	if n.Generation == ^uint64(0) {
		return nil, fmt.Errorf("delivery generation exhausted")
	}
	n.Generation++
	if err := r.Store.SaveMainNews(*n); err != nil {
		return nil, err
	}
	success = true
	return d, nil
}

// Generation is the fencing token required for a new transcript receipt.
func (d *Delivery) Generation() uint64 { return d.news.Generation }

// Disconnect releases only the child lock. The parent's lease and any pending
// append remain durable. Reconnect uses the same PID/start/nonce.
func (d *Delivery) Disconnect() {
	if d.unlock != nil {
		d.unlock()
		d.unlock = nil
	}
}

// Close relinquishes the parent lease only after the bridge has fenced all
// inflight appends and reconciled pending news. Disconnect on unexpected exit.
func (d *Delivery) Close() error {
	if d.unlock == nil {
		return fmt.Errorf("delivery disconnected")
	}
	if d.news.Pending != nil {
		return fmt.Errorf("reconcile pending news before closing the parent lease")
	}
	n := *d.news
	n.Lease = nil
	if err := d.r.Store.SaveMainNews(n); err != nil {
		d.Disconnect()
		return err
	}
	d.news = &n
	d.Disconnect()
	return nil
}

func (d *Delivery) checkLog() error {
	if d.unlock == nil {
		return fmt.Errorf("delivery disconnected")
	}
	alive, err := leaseAlive(d.news.Lease)
	if err != nil {
		return err
	}
	if d.news.Lease != nil && !alive {
		return fmt.Errorf("delivery parent exited; reconnect to reconcile pending news")
	}
	checkpoint, err := logCheckpoint(d.r.Store, d.news.Log.Offset)
	if err != nil {
		return err
	}
	if d.news.Log.Identity != "" && checkpoint != d.news.Log {
		return fmt.Errorf("event log replaced or rewritten; preserve main news and restore the original log")
	}
	off, ok, err := d.r.Store.Cursor(d.options.Cursor)
	if err != nil {
		return err
	}
	if !ok || off < 0 {
		return fmt.Errorf("delivery cursor missing or invalid; preserve main news for reconciliation")
	}
	end, err := d.r.Store.EventsEnd()
	if err != nil {
		return err
	}
	if off > end {
		return fmt.Errorf("event log truncated before delivery cursor; restore the original log")
	}
	if p := d.news.Pending; p != nil && off != p.From && off != p.To {
		return fmt.Errorf("cursor diverged from pending news; preserve state for reconciliation")
	}
	return nil
}

// Prepare replays disk state before reading the cursor. With no news it waits
// for events, attachments and periodic backstop/GC work, including when idle.
// A technical timeout returns nil, nil and does not count as parent activity.
func (d *Delivery) Prepare(ctx context.Context) (*state.MainNewsBatch, error) {
	if err := d.checkLog(); err != nil {
		return nil, err
	}
	if d.news.Pending == nil {
		_, err := runLocked(ctx, d.r, d.options, false, &lockedDelivery{
			check: d.checkLog, saveProgress: d.saveProgress, activeSince: d.news.ActiveSince,
			deliver: func(from, to int64, events []state.Event) (Result, error) {
				if err := d.checkLog(); err != nil {
					return Result{}, err
				}
				lines := make([]string, 0, len(events))
				for _, ev := range events {
					tk, _ := d.r.Store.Get(ev.Task)
					lines = append(lines, Line(ev, tk, d.r.Config.KeepFor))
				}
				checkpoint, err := logCheckpoint(d.r.Store, to)
				if err != nil {
					return Result{}, err
				}
				n := *d.news
				n.Log = checkpoint
				n.Pending = &state.MainNewsBatch{
					BatchID: randomBatchID(), From: from, To: to,
					Lines: lines, Digest: state.NewsDigest(lines), CreatedAt: time.Now().UTC(),
					Target: d.target, Generation: n.Generation, Attempt: "prepared",
				}
				if err := d.r.Store.SaveMainNews(n); err != nil {
					d.Disconnect()
					return Result{}, err
				}
				d.news = &n
				return Result{Lines: lines}, nil
			},
		})
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil, nil
			}
			return nil, err
		}
	}
	if d.news.Pending == nil {
		return nil, nil
	}
	p := *d.news.Pending
	p.Lines = slices.Clone(p.Lines)
	return &p, nil
}

// Checkpoint progress before moving its cursor, so even an idle owner pins the
// log identity before a later replacement can reuse the same byte offsets.
func (d *Delivery) saveProgress(offset int64) error {
	checkpoint, err := logCheckpoint(d.r.Store, offset)
	if err != nil {
		return err
	}
	n := *d.news
	n.Log = checkpoint
	if err := d.r.Store.SaveMainNews(n); err != nil {
		d.Disconnect()
		return err
	}
	if err := d.r.Store.SaveCursor(d.options.Cursor, offset); err != nil {
		d.Disconnect()
		return err
	}
	d.news = &n
	return nil
}

func randomBatchID() string {
	var id [16]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read terminates the process on entropy failure
	return hex.EncodeToString(id[:])
}

// BeginAttempt must persist before requesting an append. Both states require
// disk transcript reconciliation on recovery; appending is never a receipt.
func (d *Delivery) BeginAttempt(batchID string) error {
	if err := d.checkLog(); err != nil {
		return err
	}
	if d.news.Pending == nil || d.news.Pending.BatchID != batchID {
		return fmt.Errorf("pending batch mismatch")
	}
	n := *d.news
	p := *n.Pending
	p.Attempt = "appending"
	n.Pending = &p
	if err := d.r.Store.SaveMainNews(n); err != nil {
		d.Disconnect()
		return err
	}
	d.news = &n
	return nil
}

// Commit accepts only a receipt already verified against durable transcript
// content. It saves the cursor before clearing pending, then retains one ACK.
func (d *Delivery) Commit(ack state.MainNewsAck) error {
	if err := d.checkLog(); err != nil {
		return err
	}
	// An exact retained ACK is already committed. It may be retried after a
	// reconnect without granting an old generation permission to move a cursor.
	if last := d.news.LastAck; last != nil && sameAck(*last, ack) {
		return nil
	}
	if ack.Owner != d.news.Owner || ack.Generation != d.news.Generation {
		return fmt.Errorf("stale or wrong-owner news ACK")
	}
	p := d.news.Pending
	if p == nil || ack.BatchID != p.BatchID || ack.Digest != p.Digest || !sameTarget(ack.Target, p.Target) {
		return fmt.Errorf("news ACK does not match pending batch and target")
	}
	if err := d.r.Store.SaveCursor(d.options.Cursor, p.To); err != nil {
		d.Disconnect()
		return err
	}
	n := *d.news
	n.Pending = nil
	n.LastAck = &ack
	n.LastAckAt = time.Now().UTC()
	if err := d.r.Store.SaveMainNews(n); err != nil {
		d.Disconnect()
		return err
	}
	d.news = &n
	return nil
}

func sameTarget(a, b state.MainSession) bool {
	return a.Owner == b.Owner && a.Harness == b.Harness && a.ID == b.ID && a.Transcript == b.Transcript && a.Cwd == b.Cwd
}

func sameAck(a, b state.MainNewsAck) bool {
	return a.Owner == b.Owner && a.BatchID == b.BatchID && a.Digest == b.Digest && a.Generation == b.Generation && sameTarget(a.Target, b.Target)
}

func legacyDeliveryAllowed(s *state.Store, o Options) error {
	if o.Cursor != state.CursorName(o.Owner) {
		return nil
	}
	n, err := s.ReadMainNews(o.Owner)
	if err != nil || n == nil {
		return err
	}
	alive, err := leaseAlive(n.Lease)
	if err != nil {
		return err
	}
	if n.Pending != nil || alive {
		return &DeliveryConflict{Owner: o.Owner, Reason: "durable delivery is reserved or pending"}
	}
	checkpoint, err := logCheckpoint(s, n.Log.Offset)
	if err != nil {
		return err
	}
	if n.Log.Identity != "" && checkpoint != n.Log {
		return fmt.Errorf("event log replaced or rewritten; restore original log before legacy wait")
	}
	return nil
}

// logCheckpoint takes the writer's log lock so a published batch cannot depend
// on an event still being written or synced. It detects replacement, truncation
// and changes to the prefix even if a replacement has the same byte length.
func logCheckpoint(s *state.Store, offset int64) (state.MainNewsLog, error) {
	checkpoint := state.MainNewsLog{Offset: offset}
	f, err := os.Open(s.EventsPath())
	if errors.Is(err, fs.ErrNotExist) && offset == 0 {
		sum := sha256.Sum256(nil)
		checkpoint.Digest = hex.EncodeToString(sum[:])
		return checkpoint, nil
	}
	if err != nil {
		return checkpoint, fmt.Errorf("open delivery event log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return checkpoint, err
	}
	info, err := f.Stat()
	if err != nil {
		return checkpoint, err
	}
	if offset < 0 || info.Size() < offset {
		return checkpoint, fmt.Errorf("event log truncated; preserve main news and restore original log")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return checkpoint, fmt.Errorf("filesystem does not expose event log identity")
	}
	checkpoint.Identity = fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	digest := sha256.New()
	if _, err := io.CopyN(digest, f, offset); err != nil {
		return checkpoint, err
	}
	checkpoint.Digest = hex.EncodeToString(digest.Sum(nil))
	return checkpoint, nil
}
