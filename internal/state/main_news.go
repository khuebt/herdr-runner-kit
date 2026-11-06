package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// MainNewsVersion is the persisted delivery protocol version.
const MainNewsVersion = 1

// MainNewsLease survives the delivery child. Only the identified parent may
// reconnect while that parent is alive; Generation fences prior children.
type MainNewsLease struct {
	PID          int    `json:"pid"`
	ProcessStart string `json:"processStart"`
	Nonce        string `json:"nonce"`
}

// MainNewsBatch is immutable after preparation, except for Attempt. Generation
// is the preparing generation; ACKs must carry the current lease generation.
type MainNewsBatch struct {
	BatchID    string      `json:"batchId"`
	From       int64       `json:"from"`
	To         int64       `json:"to"`
	Lines      []string    `json:"lines"`
	Digest     string      `json:"digest"`
	CreatedAt  time.Time   `json:"createdAt"`
	Target     MainSession `json:"target"`
	Generation uint64      `json:"generation"`
	Attempt    string      `json:"attempt"` // prepared or appending; recovery must verify either
}

// MainNewsAck is supplied only by the transcript verifier, never by a model.
// The last ACK is retained so a lost commit reply can be retried exactly.
type MainNewsAck struct {
	Owner      string      `json:"owner"`
	BatchID    string      `json:"batchId"`
	Digest     string      `json:"digest"`
	Target     MainSession `json:"target"`
	Generation uint64      `json:"generation"`
}

// MainNewsLog pins the append-only file and its committed/prepared prefix.
type MainNewsLog struct {
	Identity string `json:"identity"`
	Offset   int64  `json:"offset"`
	Digest   string `json:"digest"`
}

// MainNews is updated only while holding the owner's cursor flock. Pending is
// read before the cursor, including after a crash between cursor save and ACK.
type MainNews struct {
	Version     int            `json:"version"`
	Owner       string         `json:"owner"`
	Cursor      string         `json:"cursor"`
	Generation  uint64         `json:"generation"`
	ActiveSince time.Time      `json:"activeSince"`
	Lease       *MainNewsLease `json:"lease,omitempty"`
	Pending     *MainNewsBatch `json:"pending,omitempty"`
	LastAck     *MainNewsAck   `json:"lastAck,omitempty"`
	LastAckAt   time.Time      `json:"lastAckAt,omitempty"` // transcript receipt commit time, not model action
	Log         MainNewsLog    `json:"log"`
}

// NewsDigest hashes the JSON array, preserving line boundaries and whitespace.
func NewsDigest(lines []string) string {
	data, _ := json.Marshal(lines)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (s *Store) mainNewsPath(owner string) string {
	return filepath.Join(s.dir, "main-news-"+CursorName(owner)+".json")
}

// ReadMainNews returns nil for a new owner and fails closed on corrupt state.
func (s *Store) ReadMainNews(owner string) (*MainNews, error) {
	data, err := os.ReadFile(s.mainNewsPath(owner))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read main news: %w", err)
	}
	var news MainNews
	if err := json.Unmarshal(data, &news); err != nil {
		return nil, fmt.Errorf("decode main news; preserve state and resume with a compatible version: %w", err)
	}
	if news.Owner != owner {
		return nil, fmt.Errorf("main news owner mismatch")
	}
	if err := validateMainNews(news); err != nil {
		return nil, err
	}
	return &news, nil
}

// SaveMainNews atomically persists the complete owner record. The caller must
// hold LockCursor(CursorName(Owner)) across the entire read-modify-write cycle.
func (s *Store) SaveMainNews(news MainNews) error {
	if err := validateMainNews(news); err != nil {
		return err
	}
	data, err := json.Marshal(news)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.mainNewsPath(news.Owner), data)
}

func validateMainNews(n MainNews) error {
	if n.Version != MainNewsVersion || n.Owner == "" || n.Cursor != CursorName(n.Owner) || n.Generation == 0 || n.ActiveSince.IsZero() {
		return fmt.Errorf("invalid main news version, owner, cursor or generation; preserve state for reconciliation")
	}
	if n.Lease != nil && (n.Lease.PID <= 0 || n.Lease.ProcessStart == "" || n.Lease.Nonce == "") {
		return fmt.Errorf("invalid main news lease")
	}
	if n.Log.Offset < 0 || (n.Log.Identity == "" && n.Log.Offset != 0) || !newsHash(n.Log.Digest) {
		return fmt.Errorf("invalid main news event-log checkpoint")
	}
	if p := n.Pending; p != nil {
		if p.BatchID == "" || p.From < 0 || p.To <= p.From || len(p.Lines) == 0 || p.Digest != NewsDigest(p.Lines) || p.CreatedAt.IsZero() || p.Generation == 0 || p.Generation > n.Generation || (p.Attempt != "prepared" && p.Attempt != "appending") || n.Log.Offset != p.To {
			return fmt.Errorf("invalid pending main news; preserve batch for reconciliation")
		}
		if p.Target.Owner != n.Owner {
			return fmt.Errorf("pending main news target owner mismatch")
		}
		if err := validateMainSession(p.Target); err != nil {
			return err
		}
	}
	if a := n.LastAck; a != nil {
		if a.Owner != n.Owner || a.Target.Owner != n.Owner || a.BatchID == "" || !newsHash(a.Digest) || a.Generation == 0 || a.Generation > n.Generation || n.LastAckAt.IsZero() {
			return fmt.Errorf("invalid main news ACK")
		}
		if err := validateMainSession(a.Target); err != nil {
			return err
		}
	}
	return nil
}

func newsHash(s string) bool {
	data, err := hex.DecodeString(s)
	return err == nil && len(data) == sha256.Size
}

// ValidateMainNewsTarget applies the typed main identity contract to a target
// before it can become the immutable destination of a pending batch.
func ValidateMainNewsTarget(target MainSession) error { return validateMainSession(target) }
