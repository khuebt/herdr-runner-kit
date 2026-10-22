// Package telegram sends runner news to the user's Telegram chat. It only
// sends: the Telegram channel plugin in the main session owns the bot's
// update polling, so replies reach the main agent, never this package.
package telegram

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/config"
)

// ErrNoCredentials means no bot token or chat id was found; notify is off.
var ErrNoCredentials = errors.New("telegram is not configured")

// Creds are read at send time and never copied into kit state.
type Creds struct {
	Token   string
	ChatIDs []string
	Source  string // file the token came from, for doctor
}

// Resolve finds the bot token and chat ids. With telegram.envFile set, both
// come from that dotenv file. Otherwise they come from the channel plugin:
// the token from <dir>/.env and the chats from <dir>/access.json allowFrom,
// where <dir> is telegram.stateDir, $TELEGRAM_STATE_DIR, or
// ${CLAUDE_CONFIG_DIR:-~/.claude}/channels/telegram. telegram.chatId
// overrides the chat ids from either source.
//
// A scratch state, HRK_STATE_DIR naming a folder other than the default
// one, reads only $TELEGRAM_STATE_DIR and is off without it: a live check
// on a copy of the state and config never reaches the user's bot through
// the config or the plugin's default folder.
func Resolve(cfg config.Telegram, getenv func(string) string) (Creds, error) {
	if ScratchState(getenv) {
		dir := getenv("TELEGRAM_STATE_DIR")
		if dir == "" {
			return Creds{}, fmt.Errorf("%w: HRK_STATE_DIR is a scratch state, which uses only $TELEGRAM_STATE_DIR", ErrNoCredentials)
		}
		cfg = config.Telegram{StateDir: dir}
	}
	var c Creds
	if cfg.EnvFile != "" {
		path := expandHome(cfg.EnvFile, getenv)
		vars, err := readDotenv(path)
		if err != nil {
			return Creds{}, err
		}
		c = Creds{Token: vars["TELEGRAM_BOT_TOKEN"], ChatIDs: splitIDs(vars["TELEGRAM_CHAT_ID"]), Source: path}
	} else {
		dir := pluginDir(cfg, getenv)
		envPath := filepath.Join(dir, ".env")
		vars, err := readDotenv(envPath)
		if errors.Is(err, os.ErrNotExist) {
			return Creds{}, fmt.Errorf("%w: no %s", ErrNoCredentials, envPath)
		}
		if err != nil {
			return Creds{}, err
		}
		c = Creds{Token: vars["TELEGRAM_BOT_TOKEN"], Source: envPath}
		if c.ChatIDs, err = allowFrom(filepath.Join(dir, "access.json")); err != nil {
			return Creds{}, err
		}
	}
	if cfg.ChatID != "" {
		c.ChatIDs = splitIDs(cfg.ChatID)
	}
	switch {
	case c.Token == "":
		return Creds{}, fmt.Errorf("%w: no TELEGRAM_BOT_TOKEN in %s", ErrNoCredentials, c.Source)
	case len(c.ChatIDs) == 0:
		return Creds{}, fmt.Errorf("%w: no chat id (set telegram.chatId, or pair a chat with the plugin)", ErrNoCredentials)
	}
	return c, nil
}

// ScratchState reports whether HRK_STATE_DIR names a state folder other
// than the default ${XDG_STATE_HOME:-~/.local/state}/herdr-runner-kit.
func ScratchState(getenv func(string) string) bool {
	dir := getenv("HRK_STATE_DIR")
	if dir == "" {
		return false
	}
	base := getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(getenv("HOME"), ".local", "state")
	}
	def := filepath.Join(base, "herdr-runner-kit")
	if filepath.Clean(dir) == def {
		return false
	}
	a, errA := os.Stat(dir)
	b, errB := os.Stat(def)
	return errA != nil || errB != nil || !os.SameFile(a, b)
}

func pluginDir(cfg config.Telegram, getenv func(string) string) string {
	if cfg.StateDir != "" {
		return expandHome(cfg.StateDir, getenv)
	}
	if dir := getenv("TELEGRAM_STATE_DIR"); dir != "" {
		return dir
	}
	base := getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		base = filepath.Join(getenv("HOME"), ".claude")
	}
	return filepath.Join(base, "channels", "telegram")
}

func expandHome(path string, getenv func(string) string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(getenv("HOME"), rest)
	}
	return path
}

// readDotenv parses KEY=VALUE lines, with optional "export " and quotes.
func readDotenv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		vars[strings.TrimSpace(k)] = v
	}
	return vars, sc.Err()
}

func allowFrom(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var access struct {
		AllowFrom []json.RawMessage `json:"allowFrom"`
	}
	if err := json.Unmarshal(data, &access); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	ids := make([]string, 0, len(access.AllowFrom))
	for _, raw := range access.AllowFrom {
		ids = append(ids, strings.Trim(string(raw), `"`))
	}
	return ids, nil
}

func splitIDs(s string) []string {
	var ids []string
	for id := range strings.SplitSeq(s, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
