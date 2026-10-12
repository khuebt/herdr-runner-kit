package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
)

// hookTag ends the Stop hook command, so setup and uninstall can find the
// entry they own among the user's hooks.
const hookTag = "# herdr-runner-kit auto re-arm"

// HookTimeout is how long, in seconds, Claude lets the background hook run.
// The hook's wait re-arms itself on its own 30-minute timeout until then.
const HookTimeout = 8 * 60 * 60

// HookCommand is the Stop hook command for the binary at bin. It does nothing
// when that binary is gone, so a moved install never breaks Claude. Claude
// wakes the agent on exit 2, which is also the exit status of a Go panic, so
// __autoarm reports news with 3 and only that becomes 2.
func HookCommand(bin string) string {
	q := "'" + strings.ReplaceAll(bin, "'", `'\''`) + "'"
	return fmt.Sprintf("[ -x %s ] || exit 0; %s __autoarm; [ $? -eq %d ] && exit 2; exit 0 %s", q, q, NewsExit, hookTag)
}

// NewsExit is the exit status of __autoarm when it printed news.
const NewsExit = 3

type hook struct {
	Type        string `json:"type"`
	Command     string `json:"command"`
	AsyncRewake bool   `json:"asyncRewake,omitempty"`
	Timeout     int    `json:"timeout,omitempty"`
}

// stripOurs removes the kit's hooks from the Stop groups and returns them.
// Ownership is per hook, so the user's hooks in the same group, and keys
// such as matcher, stay as they are. A group is dropped only when it held
// nothing but the kit's hook.
func stripOurs(groups []json.RawMessage) ([]json.RawMessage, []hook, error) {
	var out []json.RawMessage
	var ours []hook
	for _, g := range groups {
		group := &object{}
		var raws []json.RawMessage
		if json.Unmarshal(g, group) != nil {
			out = append(out, g) // not an object: not ours to judge
			continue
		}
		if raw, ok := group.get("hooks"); !ok || json.Unmarshal(raw, &raws) != nil {
			out = append(out, g)
			continue
		}
		var kept []json.RawMessage
		for _, raw := range raws {
			var h hook
			if json.Unmarshal(raw, &h) == nil && strings.HasSuffix(h.Command, hookTag) {
				ours = append(ours, h)
				continue
			}
			kept = append(kept, raw)
		}
		switch {
		case len(kept) == len(raws):
			out = append(out, g)
		case len(kept) > 0:
			raw, err := marshal(kept)
			if err != nil {
				return nil, nil, err
			}
			group.set("hooks", raw)
			b, err := marshal(group)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, b)
		}
	}
	return out, ours, nil
}

// AddHook adds the auto re-arm Stop hook for bin to the settings file,
// or brings an older entry up to date. Every other key and hook is kept in
// its place. It returns created, updated or unchanged.
func AddHook(settingsPath, bin string) (string, error) {
	want := hook{Type: "command", Command: HookCommand(bin), AsyncRewake: true, Timeout: HookTimeout}
	group, err := marshal(map[string][]hook{"hooks": {want}})
	if err != nil {
		return "", err
	}
	action := "created"
	var stripErr error
	err = editStopHooks(settingsPath, func(groups []json.RawMessage) []json.RawMessage {
		others, ours, err := stripOurs(groups)
		if err != nil {
			stripErr = err
			return nil
		}
		// Claude and other tools rewrite settings.json and reorder keys, so
		// the parsed hook is compared, not its bytes.
		if len(ours) == 1 && ours[0] == want {
			action = "unchanged"
			return nil
		}
		if len(ours) > 0 {
			action = "updated"
		}
		return append(others, group)
	})
	if err == nil {
		err = stripErr
	}
	return action, err
}

// RemoveHook deletes the auto re-arm hook. It reports whether there was one.
func RemoveHook(settingsPath string) (bool, error) {
	removed := false
	var stripErr error
	err := editStopHooks(settingsPath, func(groups []json.RawMessage) []json.RawMessage {
		others, ours, err := stripOurs(groups)
		if err != nil || len(ours) == 0 {
			stripErr = err
			return nil
		}
		removed = true
		if others == nil {
			others = []json.RawMessage{}
		}
		return others
	})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err == nil {
		err = stripErr
	}
	return removed, err
}

// HookFor returns the command of the installed auto re-arm hook, if any.
func HookFor(settingsPath string) (string, error) {
	var found string
	err := editStopHooks(settingsPath, func(groups []json.RawMessage) []json.RawMessage {
		if _, ours, err := stripOurs(groups); err == nil {
			for _, h := range ours {
				if h.AsyncRewake {
					found = h.Command
				}
			}
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return found, err
}

// editStopHooks hands the hooks.Stop groups of a settings file to fn. A nil
// result leaves the file untouched; an empty one removes the Stop key, and
// the hooks key when nothing else is left in it. The settings file is
// followed through symlinks, so a dotfiles link stays a link.
func editStopHooks(settingsPath string, fn func([]json.RawMessage) []json.RawMessage) error {
	path, err := linkTarget(settingsPath)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	root := &object{}
	data, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return readErr
	}
	if readErr == nil {
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, root); err != nil {
				return fmt.Errorf("parse %s: %w", settingsPath, err)
			}
		}
	}

	hooks := &object{}
	if raw, ok := root.get("hooks"); ok {
		if err := json.Unmarshal(raw, hooks); err != nil {
			return fmt.Errorf("parse %s: hooks: %w", settingsPath, err)
		}
	}
	var groups []json.RawMessage
	if raw, ok := hooks.get("Stop"); ok {
		if err := json.Unmarshal(raw, &groups); err != nil {
			return fmt.Errorf("parse %s: hooks.Stop: %w", settingsPath, err)
		}
	}
	next := fn(groups)
	if next == nil {
		return readErr // nil, or fs.ErrNotExist for a missing file
	}
	if len(next) == 0 {
		hooks.remove("Stop")
	} else {
		raw, err := marshal(next)
		if err != nil {
			return err
		}
		hooks.set("Stop", raw)
	}
	if len(hooks.keys) == 0 {
		root.remove("hooks")
	} else {
		raw, err := marshal(hooks)
		if err != nil {
			return err
		}
		root.set("hooks", raw)
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, out.Bytes(), mode)
}

// linkTarget follows symlinks, including one whose target does not exist
// yet, so writing never replaces a dotfiles link with a plain file.
func linkTarget(path string) (string, error) {
	for range 40 {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = dest
	}
	return "", fmt.Errorf("%s: too many symlinks", path)
}

// marshal is json.Marshal without HTML escaping, so "&&" and ">" in the
// user's hook commands stay as written.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// object is a JSON object that keeps its keys in file order, so rewriting a
// settings file the user maintains changes only the keys the kit owns.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func (o *object) get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v json.RawMessage) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) remove(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return err
		}
		o.set(tok.(string), v)
	}
	_, err := dec.Token()
	return err
}

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
