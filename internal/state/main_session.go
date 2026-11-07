package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const typedMainSessionsFile = "main-sessions.json"

type mainSessionRecords struct {
	Version  int           `json:"version"`
	Sessions []MainSession `json:"sessions"`
}

// ReadMainSessions merges the legacy and typed projections without hiding
// malformed typed metadata. On error only the legacy records are returned.
func (s *Store) ReadMainSessions() ([]MainSession, error) {
	typed, err := s.TypedMainSessions()
	return append(readMainSessions(s.mainSessionsPath()), typed...), err
}

// TypedMainSessions reads the versioned companion to the legacy Claude TSV.
// Missing metadata is empty; invalid metadata is an error, never a resume fallback.
func (s *Store) TypedMainSessions() ([]MainSession, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, typedMainSessionsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read typed main sessions: %w", err)
	}
	var records mainSessionRecords
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decode typed main sessions: %w", err)
	}
	if records.Version != 1 || records.Sessions == nil || len(records.Sessions) > mainSessionsMax {
		return nil, fmt.Errorf("invalid typed main sessions version or records")
	}
	seen := map[struct{ harness, id string }]bool{}
	for _, m := range records.Sessions {
		if err := validateMainSession(m); err != nil {
			return nil, err
		}
		key := struct{ harness, id string }{string(m.Harness), m.ID}
		if seen[key] {
			return nil, fmt.Errorf("duplicate typed main session identity")
		}
		seen[key] = true
	}
	return records.Sessions, nil
}

// RecordTypedMainSession upserts one harness-scoped identity under an atomic
// file lock. The original owner is fixed; the session manager supplies Cwd and
// Transcript. No transcript read is required, so missing session files stay known.
func (s *Store) RecordTypedMainSession(m MainSession) error {
	m.UpdatedAt = s.now().UTC()
	if err := validateMainSession(m); err != nil {
		return err
	}
	cwd, err := filepath.EvalSymlinks(m.Cwd)
	if err != nil {
		return fmt.Errorf("canonical main session cwd: %w", err)
	}
	m.Cwd = cwd
	if err := validateMainSession(m); err != nil {
		return err
	}
	unlock, err := s.Lock(typedMainSessionsFile)
	if err != nil {
		return err
	}
	defer unlock()
	sessions, err := s.TypedMainSessions()
	if err != nil {
		return err
	}
	for i, old := range sessions {
		if old.Harness == m.Harness && old.ID == m.ID {
			if old.Owner != m.Owner {
				return fmt.Errorf("main session owner cannot change")
			}
			sessions = slices.Delete(sessions, i, i+1)
			break
		}
	}
	sessions = append(sessions, m)
	groups := map[struct{ harness, owner, cwd string }]int{}
	kept := make([]MainSession, 0, min(len(sessions), mainSessionsMax))
	for _, session := range slices.Backward(sessions) {
		key := struct{ harness, owner, cwd string }{string(session.Harness), session.Owner, session.Cwd}
		if groups[key] < mainSessionsPerFolder {
			groups[key]++
			kept = append(kept, session)
			if len(kept) == mainSessionsMax {
				break
			}
		}
	}
	slices.Reverse(kept)
	data, err := json.Marshal(mainSessionRecords{Version: 1, Sessions: kept})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, typedMainSessionsFile), data)
}

func validateMainSession(m MainSession) error {
	if !slices.Contains(task.Harnesses(), m.Harness) {
		return fmt.Errorf("invalid main session harness")
	}
	for _, value := range []string{m.ID, m.Transcript, m.Owner, m.Cwd, m.PaneID} {
		if !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("invalid characters in main session metadata")
		}
	}
	if strings.TrimSpace(m.ID) == "" || m.UpdatedAt.IsZero() {
		return fmt.Errorf("main session needs an id and updatedAt")
	}
	for _, path := range []string{m.Transcript, m.Cwd} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("main session paths must be clean and absolute")
		}
	}
	if task.IsProjectOwner(m.Owner) {
		if err := CheckProjectName(strings.TrimPrefix(m.Owner, task.ProjectOwner(""))); err != nil {
			return err
		}
	} else if !filepath.IsAbs(m.Owner) || filepath.Clean(m.Owner) != m.Owner {
		return fmt.Errorf("main session owner must be an absolute path or project owner")
	}
	return nil
}
