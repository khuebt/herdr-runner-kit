package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/runner"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
	"github.com/khuebt/herdr-runner-kit/internal/wait"
)

// The test subprocess uses the real Cobra command, pipe transport and disk state.
func TestMainNewsChild(_ *testing.T) {
	switch os.Getenv("HRK_NEWS_TEST_MODE") {
	case "child":
		cmd := NewRootCommand()
		cmd.SetArgs([]string{"main", "__news"})
		if err := cmd.Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestMainNewsChild$")
		cmd.Env = withEnv(withEnv(os.Environ(), "HRK_NEWS_TEST_MODE", "child"), "HRK_MAIN", strconv.Itoa(os.Getpid()))
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("HRK_NEWS_CHILD_PID"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
			os.Exit(2)
		}
		if err := cmd.Wait(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
}

type newsProcess struct {
	cmd           *exec.Cmd
	input, output *os.File
	decoder       *json.Decoder
	ended         chan struct{}
	err           error
	generation    uint64
}

func startNewsProcess(t *testing.T, store *state.Store, owner, nonce string, parent bool) *newsProcess {
	t.Helper()
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainNewsChild$")
	cmd.Env = os.Environ()
	mode := "child"
	if parent {
		mode = "parent"
	}
	for key, value := range map[string]string{"HRK_NEWS_TEST_MODE": mode, "HRK_MAIN": strconv.Itoa(os.Getpid()), runner.MainOwnerEnv: owner, mainNewsNonceEnv: nonce, mainNewsHarnessEnv: string(task.Pi), "HRK_TASK": "", "HRK_STATE_DIR": store.Dir(), "XDG_CONFIG_HOME": t.TempDir(), "HRK_TELEGRAM": "off", "HRK_NEWS_CHILD_PID": filepath.Join(store.Dir(), "child-pid")} {
		cmd.Env = withEnv(cmd.Env, key, value)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	_ = output.Close()
	p := &newsProcess{cmd: cmd, input: writer, output: reader, decoder: json.NewDecoder(reader), ended: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.ended) }()
	t.Cleanup(func() {
		_ = writer.Close()
		select {
		case <-p.ended:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-p.ended
		}
		_ = reader.Close()
		_ = log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Log(string(data))
		}
	})
	return p
}

func (p *newsProcess) send(t *testing.T, request mainNewsRequest) {
	t.Helper()
	request.Version = 1
	if request.RequestID == "" {
		request.RequestID = request.Type
	}
	if request.Type != "register" && request.Generation == 0 {
		request.Generation = p.generation
	}
	if err := json.NewEncoder(p.input).Encode(request); err != nil {
		t.Fatal(err)
	}
}

func (p *newsProcess) read(t *testing.T, kind string) mainNewsResponse {
	t.Helper()
	if err := p.output.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response mainNewsResponse
	if err := p.decoder.Decode(&response); err != nil {
		t.Fatalf("read %s: %v", kind, err)
	}
	if response.Type != kind || response.Version != 1 {
		t.Fatalf("want %s, got %+v", kind, response)
	}
	if response.Generation != 0 {
		p.generation = response.Generation
	}
	return response
}

func (p *newsProcess) finish(t *testing.T) {
	t.Helper()
	_ = p.input.Close()
	select {
	case <-p.ended:
	case <-time.After(3 * time.Second):
		t.Fatal("news process did not stop")
	}
}

func newsFixture(t *testing.T) (*state.Store, mainNewsRequest) {
	t.Helper()
	owner := t.TempDir()
	s, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(owner, "session.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "test-main", "cwd": owner})
	if err := os.WriteFile(file, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, mainNewsRequest{Type: "register", Harness: task.Pi, SessionID: "test-main", SessionFile: file, Cwd: owner, ParentPID: os.Getpid(), Nonce: "parent-nonce", Quiesced: true}
}

func newsEvent(t *testing.T, s *state.Store, owner string) {
	t.Helper()
	if err := s.Append(state.Event{Owner: owner, Type: "needs-human", Task: "t1", Question: "local fixture question"}); err != nil {
		t.Fatal(err)
	}
}

func appendNewsReceipt(t *testing.T, p *state.MainNewsBatch) {
	t.Helper()
	entry := map[string]any{"type": "custom_message", "id": "custom-receipt", "customType": "hrk-news", "content": strings.Join(p.Lines, "\n"), "details": map[string]string{"batchId": p.BatchID, "owner": p.Target.Owner, "digest": p.Digest}}
	data, _ := json.Marshal(entry)
	f, err := os.OpenFile(p.Target.Transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMainNewsReceiptAndLateAppend(t *testing.T) {
	s, register := newsFixture(t)
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	p.send(t, mainNewsRequest{Type: "activity"})
	p.read(t, "ready")
	newsEvent(t, s, register.Cwd)
	batch := p.read(t, "batch").Batch
	if batch == nil {
		t.Fatal("missing batch")
	}
	if off, _, err := s.Cursor(state.CursorName(register.Cwd)); err != nil || off != batch.From {
		t.Fatalf("cursor moved before receipt: %d %v", off, err)
	}
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID})
	p.read(t, "error")
	// A dead child is not a dead parent. Reconnect must not emit a second append.
	_ = p.cmd.Process.Kill()
	p.finish(t)
	p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	p.read(t, "conflict")
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID, Generation: p.generation - 1})
	p.read(t, "error")
	appendNewsReceipt(t, batch) // the old generation finishes after the new child connects
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID})
	p.read(t, "committed")
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID})
	p.read(t, "committed")
	p.finish(t)
	p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID})
	p.read(t, "committed")
	n, err := s.ReadMainNews(register.Cwd)
	if err != nil || n.Pending != nil || n.LastAck.BatchID != batch.BatchID {
		t.Fatalf("not committed: %+v %v", n, err)
	}
	if off, _, err := s.Cursor(state.CursorName(register.Cwd)); err != nil || off != batch.To {
		t.Fatalf("cursor not committed: %d %v", off, err)
	}
	p.send(t, mainNewsRequest{Type: "close", Quiesced: true})
	p.read(t, "ready")
	p.finish(t)
	n, err = s.ReadMainNews(register.Cwd)
	if err != nil || n.Lease != nil {
		t.Fatalf("lease not released: %+v %v", n, err)
	}
}

func TestMainNewsOwnerLeaseAndTarget(t *testing.T) {
	s, register := newsFixture(t)
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	other := startNewsProcess(t, s, register.Cwd, "other-nonce", false)
	wrong := register
	wrong.Nonce = "other-nonce"
	other.send(t, wrong)
	other.read(t, "conflict")
	other.finish(t)
	newsEvent(t, s, register.Cwd)
	batch := p.read(t, "batch").Batch
	p.send(t, mainNewsRequest{Type: "close", Quiesced: true})
	p.read(t, "conflict")
	p.finish(t)
	other = startNewsProcess(t, s, register.Cwd, "other-nonce", false)
	other.send(t, wrong)
	other.read(t, "conflict")
	other.finish(t)
	// A new session cannot take the unresolved batch. It may only verify the old file.
	changed := register
	changed.SessionID = "new-main"
	changed.SessionFile = filepath.Join(register.Cwd, "new.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": changed.SessionID, "cwd": changed.Cwd})
	if err := os.WriteFile(changed.SessionFile, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, changed)
	p.read(t, "ready")
	if response := p.read(t, "conflict"); !strings.Contains(response.Message, batch.Target.Transcript) {
		t.Fatal("conflict did not identify original resume file")
	}
	appendNewsReceipt(t, batch)
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID})
	p.read(t, "committed")
	p.finish(t)
}

func TestMainNewsRecoveryScansReceiptBeforeReplay(t *testing.T) {
	s, register := newsFixture(t)
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	newsEvent(t, s, register.Cwd)
	batch := p.read(t, "batch").Batch
	appendNewsReceipt(t, batch)
	_ = p.cmd.Process.Kill()
	p.finish(t)
	unlock, err := s.LockCursor(state.CursorName(register.Cwd), "crash-boundary")
	if err != nil {
		t.Fatal(err)
	}
	err = s.SaveCursor(state.CursorName(register.Cwd), batch.To)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	if got := p.read(t, "committed"); got.BatchID != batch.BatchID {
		t.Fatalf("wrong receipt %+v", got)
	}
	p.finish(t)
}

func TestMainNewsPartialAppendRetainsPendingAcrossReconnect(t *testing.T) {
	for _, boundary := range []string{"partial-json", "missing-newline"} {
		t.Run(boundary, func(t *testing.T) {
			s, register := newsFixture(t)
			p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
			p.send(t, register)
			p.read(t, "ready")
			newsEvent(t, s, register.Cwd)
			batch := p.read(t, "batch").Batch
			entry := map[string]any{"type": "custom_message", "id": "partial-receipt", "customType": "hrk-news", "content": strings.Join(batch.Lines, "\n"), "details": map[string]string{"batchId": batch.BatchID, "owner": batch.Target.Owner, "digest": batch.Digest}}
			data, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, '\n')
			cut := len(data) - 1
			if boundary == "partial-json" {
				cut /= 2
			}
			appendPart := func(part []byte) {
				t.Helper()
				f, err := os.OpenFile(register.SessionFile, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := f.Write(part)
				closeErr := f.Close()
				if err := errors.Join(writeErr, closeErr); err != nil {
					t.Fatal(err)
				}
			}
			assertPending := func() {
				t.Helper()
				n, err := s.ReadMainNews(register.Cwd)
				if err != nil || n == nil || n.Pending == nil || n.Pending.BatchID != batch.BatchID || n.Pending.Digest != batch.Digest || n.LastAck != nil {
					t.Fatalf("partial append lost or acknowledged pending: %+v %v", n, err)
				}
				if offset, _, err := s.Cursor(state.CursorName(register.Cwd)); err != nil || offset != batch.From {
					t.Fatalf("partial append advanced cursor: %d %v", offset, err)
				}
			}
			appendPart(data[:cut])
			p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID})
			if response := p.read(t, "error"); !strings.Contains(response.Message, "incomplete transcript") {
				t.Fatalf("unexpected partial receipt error: %+v", response)
			}
			assertPending()
			_ = p.cmd.Process.Kill()
			p.finish(t)
			p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
			p.send(t, register)
			// An incomplete file cannot even establish a safe registration target.
			p.read(t, "error")
			p.finish(t)
			assertPending()
			appendPart(data[cut:]) // The old runtime completes the same entry, without fsync.
			p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
			p.send(t, register)
			p.read(t, "ready")
			if response := p.read(t, "committed"); response.BatchID != batch.BatchID {
				t.Fatalf("reconnect acknowledged a different batch: %+v", response)
			}
			p.finish(t)
			n, err := s.ReadMainNews(register.Cwd)
			if err != nil || n == nil || n.Pending != nil || n.LastAck == nil || n.LastAck.BatchID != batch.BatchID {
				t.Fatalf("completed append not recovered: %+v %v", n, err)
			}
			if offset, _, err := s.Cursor(state.CursorName(register.Cwd)); err != nil || offset != batch.To {
				t.Fatalf("completed receipt cursor: %d %v", offset, err)
			}
			transcript, err := os.ReadFile(register.SessionFile)
			if err != nil || strings.Count(string(transcript), `"customType":"hrk-news"`) != 1 {
				t.Fatalf("expected one physical receipt: %v", err)
			}
		})
	}
}

func TestMainNewsProtocolRejectsInvalidInput(t *testing.T) {
	for _, raw := range []string{"not-json\n", `{"version":1,"type":"register","requestId":"x","owner":"/spoof"}` + "\n", `{"version":1}` + "\n", strings.Repeat("x", 70<<10) + "\n", `{"version":1,"type":"register","requestId":"x"}`} {
		t.Run(fmt.Sprint(len(raw)), func(t *testing.T) {
			s, register := newsFixture(t)
			p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
			_, _ = io.WriteString(p.input, raw)
			_ = p.input.Close()
			p.read(t, "error")
			p.finish(t)
		})
	}
	for _, change := range []string{"missing", "mismatch", "harness", "wrong-harness", "nonce", "parent", "quiesced"} {
		t.Run(change, func(t *testing.T) {
			s, register := newsFixture(t)
			p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
			switch change {
			case "missing":
				register.SessionFile += ".missing"
			case "mismatch":
				register.SessionID = "wrong"
			case "harness":
				register.Harness = task.Codex
			case "wrong-harness":
				register.Harness = task.Omp
			case "nonce":
				register.Nonce = "wrong"
			case "parent":
				register.ParentPID++
			case "quiesced":
				register.Quiesced = false
			}
			p.send(t, register)
			p.read(t, "error")
			p.finish(t)
			n, err := s.ReadMainNews(register.Cwd)
			if err != nil || n != nil {
				t.Fatalf("invalid register acquired state: %+v %v", n, err)
			}
		})
	}
}

func TestMainNewsParentDeathReplayAndCleanup(t *testing.T) {
	s, register := newsFixture(t)
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, true)
	register.ParentPID = p.cmd.Process.Pid
	p.send(t, register)
	p.read(t, "ready")
	newsEvent(t, s, register.Cwd)
	batch := p.read(t, "batch").Batch
	childData, err := os.ReadFile(filepath.Join(s.Dir(), "child-pid"))
	if err != nil {
		t.Fatal(err)
	}
	childPID, _ := strconv.Atoi(string(childData))
	_ = p.cmd.Process.Kill()
	<-p.ended
	// Keep the input pipe open so parent monitoring, not EOF, must stop the child.
	deadline := time.Now().Add(7 * time.Second)
	for {
		start, err := wait.ProcessStart(childPID)
		if err != nil {
			t.Fatal(err)
		}
		if start == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("orphaned news child")
		}
		time.Sleep(20 * time.Millisecond)
	}
	register.ParentPID = os.Getpid()
	p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	got := p.read(t, "batch").Batch
	if got.BatchID != batch.BatchID || got.Target != batch.Target {
		t.Fatalf("changed replay: %+v", got)
	}
	appendNewsReceipt(t, got)
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: got.BatchID})
	p.read(t, "committed")
	p.finish(t)
}

func TestMainNewsBrokenStdoutRetainsLease(t *testing.T) {
	s, register := newsFixture(t)
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	_ = p.output.Close()
	p.send(t, mainNewsRequest{Type: "activity"})
	select {
	case <-p.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("broken stdout did not stop child")
	}
	n, err := s.ReadMainNews(register.Cwd)
	if err != nil || n.Lease == nil {
		t.Fatalf("lease lost: %+v %v", n, err)
	}
	unlock, err := s.LockCursor(state.CursorName(register.Cwd), "test")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestMainNewsEnvironment(t *testing.T) {
	for key, value := range map[string]string{"HRK_MAIN": strconv.Itoa(os.Getppid()), runner.MainOwnerEnv: t.TempDir(), mainNewsNonceEnv: "fixed-nonce", mainNewsHarnessEnv: string(task.Pi), "HRK_TASK": ""} {
		t.Setenv(key, value)
	}
	for _, tc := range []struct{ key, value string }{{"HRK_MAIN", "1"}, {"HRK_MAIN", "invalid"}, {"HRK_MAIN", strconv.Itoa(os.Getpid())}, {"HRK_TASK", "runner"}, {runner.MainOwnerEnv, ""}, {mainNewsNonceEnv, ""}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			cmd := NewRootCommand()
			cmd.SetArgs([]string{"main", "__news"})
			if err := cmd.Execute(); err == nil {
				t.Fatal("invalid main environment accepted")
			}
		})
	}
}

func TestMainNewsPreparedReplay(t *testing.T) {
	s, register := newsFixture(t)
	r := testRunner(t)
	r.Store = s
	start, err := wait.ProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	target := state.MainSession{Owner: register.Cwd, Harness: register.Harness, ID: register.SessionID, Transcript: register.SessionFile, Cwd: register.Cwd, UpdatedAt: time.Now().UTC()}
	d, err := wait.AcquireDelivery(r, wait.Options{Owner: register.Cwd, Window: time.Millisecond}, state.MainNewsLease{PID: os.Getpid(), ProcessStart: start, Nonce: register.Nonce}, target)
	if err != nil {
		t.Fatal(err)
	}
	newsEvent(t, s, register.Cwd)
	batch, err := d.Prepare(t.Context())
	d.Disconnect()
	if err != nil {
		t.Fatal(err)
	}
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	if got := p.read(t, "batch").Batch; got.BatchID != batch.BatchID {
		t.Fatal("prepared replay changed batch")
	}
	p.finish(t)
}

func TestMainNewsUnreadableOldTarget(t *testing.T) {
	s, register := newsFixture(t)
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	newsEvent(t, s, register.Cwd)
	batch := p.read(t, "batch").Batch
	p.finish(t)
	if err := os.Remove(register.SessionFile); err != nil {
		t.Fatal(err)
	}
	register.SessionFile = filepath.Join(register.Cwd, "new-target.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": register.SessionID, "cwd": register.Cwd})
	if err := os.WriteFile(register.SessionFile, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	p.read(t, "error")
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID})
	p.read(t, "error")
	p.finish(t)
	n, err := s.ReadMainNews(register.Cwd)
	if err != nil || n.Pending == nil || n.Pending.Target.Transcript != batch.Target.Transcript || n.LastAck != nil {
		t.Fatalf("lost unreadable target: %+v %v", n, err)
	}
}

func TestMainNewsBlockedStdout(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	bridge := mainNewsBridge{output: writer}
	started := time.Now()
	if err := bridge.respond("request", "error", "", strings.Repeat("x", 1<<20), nil); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("blocked write: %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("blocked stdout exceeded shutdown budget")
	}
}

func TestMainNewsOversizedBatchStaysPrepared(t *testing.T) {
	s, register := newsFixture(t)
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	decisions := make([]string, 16000)
	for i := range decisions {
		decisions[i] = strings.Repeat("x", 280)
	}
	if err := s.Append(state.Event{Owner: register.Cwd, Task: "t1", Type: "done", Decisions: decisions}); err != nil {
		t.Fatal(err)
	}
	response := p.read(t, "error")
	if !strings.Contains(response.Message, "4 MiB") {
		t.Fatalf("wrong error: %+v", response)
	}
	p.finish(t)
	n, err := s.ReadMainNews(register.Cwd)
	if err != nil || n.Pending == nil || n.Pending.Attempt != "prepared" {
		t.Fatalf("unemitted batch marked appending: %+v %v", n, err)
	}
	p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	p.read(t, "error")
	p.finish(t)
	if off, _, err := s.Cursor(state.CursorName(register.Cwd)); err != nil || off != n.Pending.From {
		t.Fatalf("oversized batch consumed: %d %v", off, err)
	}
}

func TestMainNewsCommitFailureReportsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failure requires non-root user")
	}
	s, register := newsFixture(t)
	p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	newsEvent(t, s, register.Cwd)
	batch := p.read(t, "batch").Batch
	appendNewsReceipt(t, batch)
	if err := os.Chmod(s.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(s.Dir(), 0o700) }()
	p.send(t, mainNewsRequest{Type: "receipt", BatchID: batch.BatchID})
	response := p.read(t, "error")
	if response.BatchID != batch.BatchID {
		t.Fatal("error lost batch identity")
	}
	p.finish(t)
	if err := os.Chmod(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReadMainNews(register.Cwd)
	if err != nil || n.Pending == nil {
		t.Fatalf("failed commit lost pending: %+v %v", n, err)
	}
	p = startNewsProcess(t, s, register.Cwd, register.Nonce, false)
	p.send(t, register)
	p.read(t, "ready")
	p.read(t, "committed")
	p.finish(t)
}

func TestMainNewsSignalAndMissingState(t *testing.T) {
	for _, action := range []string{"sigterm", "missing-state"} {
		t.Run(action, func(t *testing.T) {
			s, register := newsFixture(t)
			p := startNewsProcess(t, s, register.Cwd, register.Nonce, false)
			p.send(t, register)
			p.read(t, "ready")
			if action == "sigterm" {
				if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(s.Dir(), "main-news-"+state.CursorName(register.Cwd)+".json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				p.send(t, mainNewsRequest{Type: "receipt", BatchID: "unknown"})
				p.read(t, "error")
			}
			select {
			case <-p.ended:
			case <-time.After(2 * time.Second):
				t.Fatal("child did not stop within shutdown budget")
			}
			unlock, err := s.LockCursor(state.CursorName(register.Cwd), "test")
			if err != nil {
				t.Fatal(err)
			}
			unlock()
		})
	}
}
