package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// heavyCommandRunes is how much of a heavy command its record keeps.
const heavyCommandRunes = 80

func newHeavyCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "heavy [-- <command> [args...]]",
		Short: "Run a test, lint, typecheck or build in a slot shared by every runner, or show the slots",
		Long: "heavy -- <command> waits for one of heavySlots slots shared by every runner and the main " +
			"session of this state folder, then runs the command and exits with its code. While it " +
			"waits it prints who holds the slots, at once and every minute. Without a command, heavy " +
			"shows who holds each slot and who waits. With heavySlots 0 the command runs at once.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON && len(args) > 0 {
				return errors.New("--json shows the slots; it takes no command")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			dir, err := stateDir()
			if err != nil {
				return err
			}
			store, err := state.Open(dir)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				return printHeavy(cmd.OutOrStdout(), store, cfg.HeavySlots, asJSON)
			}
			code, err := runHeavy(cmd.Context(), store, cfg.HeavySlots, args, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	// The wrapped command's own flags are never hrk's.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().BoolVar(&asJSON, "json", false, "show the slots as JSON")
	return cmd
}

// runHeavy runs args in a heavy slot and returns its exit code. SIGINT and
// SIGTERM go to the command while it runs, and end the wait before it.
func runHeavy(ctx context.Context, store *state.Store, slots int, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	child := exec.Command(args[0], args[1:]...)
	if child.Err != nil {
		fmt.Fprintf(stderr, "hrk heavy: %v\n", child.Err)
		return 127, nil
	}
	child.Stdin, child.Stdout, child.Stderr = stdin, stdout, stderr

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var running atomic.Pointer[os.Process]
	caught := make(chan os.Signal, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigs:
				if p := running.Load(); p != nil {
					_ = p.Signal(sig)
					continue
				}
				select {
				case caught <- sig:
				default:
				}
				cancel()
			}
		}
	}()

	task := os.Getenv("HRK_TASK")
	if task == "" {
		task = "main"
	}
	cwd, _ := os.Getwd()
	rec := state.HeavyRecord{Task: task, Cwd: cwd, Command: truncate(redact.Secrets(strings.Join(args, " ")), heavyCommandRunes)}
	release, lock, err := store.AcquireHeavy(ctx, slots, rec, func(st state.HeavyStatus) {
		fmt.Fprintln(stderr, waitingLine(st))
	})
	if err != nil {
		select {
		case sig := <-caught:
			return signalCode(sig), nil
		default:
			return 0, err
		}
	}
	defer release()
	if lock != nil {
		// The command holds the slot too, so it stays held while the
		// command runs even if this process is killed.
		child.ExtraFiles = []*os.File{lock}
	}
	if err := child.Start(); err != nil {
		fmt.Fprintf(stderr, "hrk heavy: %v\n", err)
		return 126, nil
	}
	running.Store(child.Process)
	if lock != nil {
		// Best effort: without the pid a leftover process holding the slot
		// is not told from a running command.
		_ = store.RecordHeavyCommand(lock, child.Process.Pid)
	}
	select {
	case sig := <-caught: // came between the slot and the start
		_ = child.Process.Signal(sig)
	default:
	}
	err = child.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return signalCode(ws.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	return 0, err
}

// signalCode is the shell's exit code for a command ended by sig.
func signalCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}

// waitingLine is what a waiter prints: who holds the slots, and since when.
func waitingLine(st state.HeavyStatus) string {
	holders := make([]string, len(st.Holders))
	for i, h := range st.Holders {
		holders[i] = h.Task + " " + h.Command + " " + age(h.Since)
		if h.Orphaned {
			holders[i] += " (orphaned)"
		}
	}
	return fmt.Sprintf("hrk heavy: waiting for a slot (%d in use: %s)", len(st.Holders), strings.Join(holders, ", "))
}

func printHeavy(w io.Writer, store *state.Store, slots int, asJSON bool) error {
	st, err := store.HeavyStatus(slots)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(st)
	}
	if slots <= 0 {
		_, err := fmt.Fprintln(w, "heavySlots: 0 (off)")
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "heavySlots: %d\n", slots)
	held := map[int]state.HeavyRecord{}
	for _, h := range st.Holders {
		held[h.Slot] = h
	}
	for n := 1; n <= slots; n++ {
		if h, ok := held[n]; ok {
			fmt.Fprintf(&b, "slot %d: %s\n", n, heavyLine(h))
		} else {
			fmt.Fprintf(&b, "slot %d: free\n", n)
		}
	}
	if len(st.Waiting) == 0 {
		b.WriteString("waiting: none\n")
	} else {
		b.WriteString("waiting:\n")
		for _, h := range st.Waiting {
			fmt.Fprintf(&b, "  %s\n", heavyLine(h))
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}

func heavyLine(h state.HeavyRecord) string {
	line := fmt.Sprintf("%s · %s · %s · pid %d · %s", h.Task, h.Command, age(h.Since), h.PID, h.Cwd)
	if h.Orphaned {
		line += fmt.Sprintf(" · orphaned: pid %d and command pid %d are gone, a leftover process still holds the lock", h.PID, h.CommandPID)
	}
	return line
}
