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
func Resolve(cfg config.Telegram, getenv func(string) string) (Creds, error) {
	var c Creds
	if cfg.EnvFile != "" {
		vars, err := readDotenv(expandHome(cfg.EnvFile, getenv))
		if err != nil {
			return Creds{}, err
		}
		c = Creds{Token: vars["TELEGRAM_BOT_TOKEN"], ChatIDs: splitIDs(vars["TELEGRAM_CHAT_ID"]), Source: cfg.EnvFile}
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
