package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/harness"
	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/wait"
)

const (
	mainNewsVersion    = 1
	mainNewsNonceEnv   = "HRK_MAIN_NONCE"
	mainNewsHarnessEnv = "HRK_MAIN_HARNESS"
)

// Receipts request verification; all ACK fields come from durable child state.
type mainNewsRequest struct {
	Version     int          `json:"version"`
	Type        string       `json:"type"`
	RequestID   string       `json:"requestId"`
	Generation  uint64       `json:"generation,omitempty"`
	BatchID     string       `json:"batchId,omitempty"`
	Harness     task.Harness `json:"harness,omitempty"`
	SessionID   string       `json:"sessionId,omitempty"`
	SessionFile string       `json:"sessionFile,omitempty"`
	Cwd         string       `json:"cwd,omitempty"`
	ParentPID   int          `json:"parentPID,omitempty"`
	Nonce       string       `json:"nonce,omitempty"`
	Quiesced    bool         `json:"quiesced,omitempty"`
}

type mainNewsResponse struct {
	Version    int                  `json:"version"`
	Type       string               `json:"type"`
	RequestID  string               `json:"requestId"`
	Generation uint64               `json:"generation,omitempty"`
	BatchID    string               `json:"batchId,omitempty"`
	Batch      *state.MainNewsBatch `json:"batch,omitempty"`
	Message    string               `json:"message,omitempty"`
}

type mainNewsBridge struct {
	r            *runner.Runner
	owner, nonce string
	harness      task.Harness
	pid          int
	output       io.Writer
	delivery     *wait.Delivery
	target       state.MainSession
	pending      *state.MainNewsBatch
	registration string
	replaySafe   bool
	generation   uint64
}

func newMainNewsCommand() *cobra.Command {
	return &cobra.Command{
		Use: "__news", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pid, err := strconv.Atoi(os.Getenv("HRK_MAIN"))
			owner, nonce := os.Getenv(runner.MainOwnerEnv), os.Getenv(mainNewsNonceEnv)
			selected := task.Harness(os.Getenv(mainNewsHarnessEnv))
			if err != nil || pid <= 1 || pid != os.Getppid() || owner == "" || selected == "" || nonce == "" || len(nonce) > 128 || os.Getenv("HRK_TASK") != "" {
				return errors.New("main news requires its direct main parent, fixed owner and nonce, and no HRK_TASK")
			}
			r, err := openRunner()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer cancel()
			input, ok := cmd.InOrStdin().(io.ReadCloser)
			if !ok {
				input = io.NopCloser(cmd.InOrStdin())
			}
			if file, ok := input.(*os.File); ok {
				input, err = mainNewsPipe(file)
				if err != nil {
					return err
				}
			}
			defer func() { _ = input.Close() }()
			output := cmd.OutOrStdout()
			if file, ok := output.(*os.File); ok {
				pipe, err := mainNewsPipe(file)
				if err != nil {
					return err
				}
				defer func() { _ = pipe.Close() }()
				output = pipe
			}
			bridge := &mainNewsBridge{r: r, owner: owner, nonce: nonce, harness: selected, pid: pid, output: output}
			return bridge.run(whileAlive(ctx, pid), input)
		},
	}
}

// Inherited standard pipes start in blocking mode. Give Go's poller its own
// descriptors so cancellation can interrupt stdin and deadline-bound stdout.
func mainNewsPipe(file *os.File) (*os.File, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return file, nil
	}
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}

func transcriptTarget(target state.MainSession) harness.MainTranscriptTarget {
	return harness.MainTranscriptTarget{Harness: target.Harness, ID: target.ID, File: target.Transcript, Cwd: target.Cwd}
}

func (b *mainNewsBridge) marshalResponse(req, kind, batchID, message string, batch *state.MainNewsBatch) ([]byte, error) {
	response := mainNewsResponse{Version: mainNewsVersion, Type: kind, RequestID: req, BatchID: batchID, Message: message, Batch: batch}
	if b.delivery != nil {
		response.Generation = b.generation
	}
	data, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("news response exceeds 4 MiB; batch stays prepared until a compatible larger protocol limit is available")
	}
	return append(data, '\n'), nil
}

func (b *mainNewsBridge) respond(req, kind, batchID, message string, batch *state.MainNewsBatch) error {
	data, err := b.marshalResponse(req, kind, batchID, message, batch)
	if err != nil {
		return err
	}
	return b.writeResponse(data)
}

func (b *mainNewsBridge) fail(req, batchID string, cause error) error {
	return errors.Join(cause, b.respond(req, "error", batchID, cause.Error(), nil))
}

func (b *mainNewsBridge) writeResponse(data []byte) error {
	// A parent that stops reading stdout must not trap the child in write forever.
	if file, ok := b.output.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0 {
			if err := file.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				return err
			}
		}
	}
	n, err := b.output.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (b *mainNewsBridge) register(req mainNewsRequest) error {
	if req.Type != "register" || req.Generation != 0 || req.ParentPID != b.pid || req.Nonce != b.nonce || req.Harness != b.harness || !req.Quiesced {
		return errors.New("register requires matching parent, harness and nonce, and a quiesced previous connection")
	}
	start, err := wait.ProcessStart(b.pid)
	if err != nil {
		return err
	}
	if start == "" {
		return errors.New("main parent has exited")
	}
	b.target = state.MainSession{Harness: req.Harness, ID: req.SessionID, Transcript: req.SessionFile, Cwd: req.Cwd, Owner: b.owner, UpdatedAt: time.Now().UTC()}
	if err := state.ValidateMainNewsTarget(b.target); err != nil {
		return err
	}
	if _, err := harness.VerifyMainTranscript(transcriptTarget(b.target), nil); err != nil {
		return err
	}
	previous, err := b.r.Store.ReadMainNews(b.owner)
	if err != nil {
		return err
	}
	b.replaySafe = previous == nil || previous.Lease == nil
	if previous != nil && previous.Lease != nil {
		oldStart, err := wait.ProcessStart(previous.Lease.PID)
		if err != nil {
			return err
		}
		b.replaySafe = oldStart == "" || oldStart != previous.Lease.ProcessStart
	}
	b.delivery, err = wait.AcquireDelivery(b.r, wait.Options{Owner: b.owner}, state.MainNewsLease{PID: b.pid, ProcessStart: start, Nonce: b.nonce}, b.target)
	if err != nil {
		return err
	}
	b.generation = b.delivery.Generation()
	// AcquireDelivery owns the flock. Reject stale pre-acquisition evidence if
	// another connection acquired it in between the snapshot and our claim.
	expected := uint64(1)
	if previous != nil {
		expected = previous.Generation + 1
	}
	if b.generation != expected {
		return &wait.DeliveryConflict{Owner: b.owner, Reason: "registration raced another connection; reconnect to reconcile"}
	}
	if err := b.r.Store.RecordTypedMainSession(b.target); err != nil {
		return err
	}
	if previous == nil || previous.Lease == nil || previous.Lease.PID != b.pid || previous.Lease.ProcessStart != start {
		if err := wait.MarkActive(b.r.Store, wait.Options{Owner: b.owner}); err != nil {
			return err
		}
	}
	b.registration = req.RequestID
	return b.respond(req.RequestID, "ready", "", "", nil)
}

func (b *mainNewsBridge) verifyPending() (bool, error) {
	p := b.pending
	return harness.VerifyMainTranscript(transcriptTarget(p.Target), &harness.MainTranscriptReceipt{
		BatchID: p.BatchID, Owner: b.owner, Digest: p.Digest, Content: strings.Join(p.Lines, "\n"),
	})
}

func (b *mainNewsBridge) commit(req string) error {
	p := b.pending
	if err := b.delivery.Commit(state.MainNewsAck{Owner: b.owner, BatchID: p.BatchID, Digest: p.Digest, Target: p.Target, Generation: b.generation}); err != nil {
		return b.fail(req, p.BatchID, err)
	}
	b.pending = nil
	return b.respond(req, "committed", p.BatchID, "", nil)
}

func (b *mainNewsBridge) publish() error {
	p := b.pending
	found, err := b.verifyPending()
	if err != nil {
		return b.respond(b.registration, "error", p.BatchID, err.Error(), nil)
	}
	if found {
		return b.commit(b.registration)
	}
	if p.Target.Harness != b.target.Harness || p.Target.ID != b.target.ID || p.Target.Transcript != b.target.Transcript || p.Target.Cwd != b.target.Cwd {
		return b.respond(b.registration, "conflict", p.BatchID, fmt.Sprintf("pending news belongs to %s session %q; explicitly resume %q", p.Target.Harness, p.Target.ID, p.Target.Transcript), nil)
	}
	if p.Attempt == "appending" && !b.replaySafe {
		return b.respond(b.registration, "conflict", p.BatchID, "prior append may still finish; verify its receipt or exit the main process and explicitly resume", nil)
	}
	emitted := *p
	emitted.Attempt = "appending"
	data, err := b.marshalResponse(b.registration, "batch", p.BatchID, "", &emitted)
	if err != nil {
		return b.respond(b.registration, "error", p.BatchID, err.Error(), nil)
	}
	if err := b.delivery.BeginAttempt(p.BatchID); err != nil {
		return b.fail(b.registration, p.BatchID, err)
	}
	b.pending = &emitted
	return b.writeResponse(data)
}

// Only this loop calls Delivery. Prepare runs in a joined goroutine while stdin
// remains responsive; activity only writes the independent activity marker.
func (b *mainNewsBridge) run(ctx context.Context, input io.ReadCloser) error {
	ctx, cancel := context.WithCancel(ctx)
	type incoming struct {
		request mainNewsRequest
		err     error
	}
	requests, readerDone := make(chan incoming), make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		scanner.Split(func(data []byte, eof bool) (int, []byte, error) {
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				return i + 1, data[:i], nil
			}
			if eof && len(data) != 0 {
				return 0, nil, errors.New("incomplete JSONL request")
			}
			return 0, nil, nil
		})
		for scanner.Scan() {
			var req mainNewsRequest
			decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
			decoder.DisallowUnknownFields()
			err := decoder.Decode(&req)
			if err == nil {
				var extra any
				if decoder.Decode(&extra) != io.EOF {
					err = errors.New("expected one JSON object per line")
				}
			}
			select {
			case requests <- incoming{req, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case requests <- incoming{err: err}:
		case <-ctx.Done():
		}
	}()
	type prepared struct {
		batch *state.MainNewsBatch
		err   error
	}
	var preparing <-chan prepared
	var stopPrepare context.CancelFunc
	join := func() {
		if preparing != nil {
			stopPrepare()
			<-preparing
			preparing = nil
		}
	}
	defer func() {
		cancel()
		_ = input.Close()
		<-readerDone
		join()
		if b.delivery != nil {
			b.delivery.Disconnect()
		}
	}()
	for {
		if b.delivery != nil && b.pending == nil && preparing == nil {
			prepareCtx, stop := context.WithCancel(ctx)
			stopPrepare = stop
			result := make(chan prepared, 1)
			preparing = result
			go func() { batch, err := b.delivery.Prepare(prepareCtx); result <- prepared{batch, err} }()
		}
		select {
		case <-ctx.Done():
			return nil
		case result := <-preparing:
			stopPrepare()
			preparing = nil
			if result.err != nil {
				return b.respond(b.registration, "error", "", result.err.Error(), nil)
			}
			if result.batch == nil {
				continue
			}
			b.pending = result.batch
			if err := b.publish(); err != nil {
				return err
			}
		case in := <-requests:
			if in.err == io.EOF {
				return nil
			}
			if in.err != nil {
				return b.respond("", "error", "", "invalid or oversized JSONL request", nil)
			}
			req := in.request
			if req.Version != mainNewsVersion || req.RequestID == "" || len(req.RequestID) > 128 {
				return b.respond(req.RequestID, "error", req.BatchID, "invalid protocol version or request id", nil)
			}
			if b.delivery == nil {
				if err := b.register(req); err != nil {
					kind := "error"
					if errors.Is(err, state.ErrAlreadyWaiting) {
						kind = "conflict"
					}
					return b.respond(req.RequestID, kind, "", err.Error(), nil)
				}
				continue
			}
			if req.Generation != b.generation {
				if err := b.respond(req.RequestID, "error", req.BatchID, "stale connection generation", nil); err != nil {
					return err
				}
				continue
			}
			switch req.Type {
			case "activity":
				if err := wait.MarkActive(b.r.Store, wait.Options{Owner: b.owner}); err != nil {
					return err
				}
				if err := b.respond(req.RequestID, "ready", "", "", nil); err != nil {
					return err
				}
			case "receipt":
				// Retained ACK replies need no Delivery mutation, even while Prepare
				// is waiting for a later batch under the same lock.
				n, err := b.r.Store.ReadMainNews(b.owner)
				if err != nil {
					return b.fail(req.RequestID, req.BatchID, err)
				}
				if n == nil {
					return b.fail(req.RequestID, req.BatchID, errors.New("registered main news state is missing; restore it before reconnecting"))
				}
				if n.LastAck != nil && n.LastAck.BatchID == req.BatchID {
					if err := b.respond(req.RequestID, "committed", req.BatchID, "", nil); err != nil {
						return err
					}
					continue
				}
				if b.pending == nil || req.BatchID != b.pending.BatchID {
					if err := b.respond(req.RequestID, "error", req.BatchID, "no matching pending receipt", nil); err != nil {
						return err
					}
					continue
				}
				found, err := b.verifyPending()
				if err != nil || !found {
					message := "receipt not on disk; retain pending and do not resend"
					if err != nil {
						message = err.Error()
					}
					if err := b.respond(req.RequestID, "error", req.BatchID, message, nil); err != nil {
						return err
					}
					continue
				}
				if err := b.commit(req.RequestID); err != nil {
					return err
				}
			case "close":
				join()
				if !req.Quiesced {
					return b.respond(req.RequestID, "error", "", "close requires quiesced append generation", nil)
				}
				if err := b.delivery.Close(); err != nil {
					return b.respond(req.RequestID, "conflict", "", err.Error(), nil)
				}
				return b.respond(req.RequestID, "ready", "", "closed", nil)
			default:
				return b.respond(req.RequestID, "error", "", fmt.Sprintf("unexpected request type %q", req.Type), nil)
			}
		}
	}
}
