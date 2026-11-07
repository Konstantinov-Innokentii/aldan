package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
)

// sandboxSection finds any TOML form of the section outside comments: a header,
// a dotted key or an inline table.
var sandboxSection = regexp.MustCompile(`(?m)^[ \t]*(\[[ \t]*sandbox_workspace_write\b|sandbox_workspace_write[ \t]*[.=])`)

// install adds entries and keeps everything else. Running it twice changes nothing.
func install(home string, out io.Writer) error {
	// The directory must exist before Codex uses it as a writable root.
	data := filepath.Join(home, ".aldan")
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(data, 0o700); err != nil {
		return err
	}
	// One update per file, so that the .bak holds the contents from before install.
	err := updateJSON(filepath.Join(home, ".claude", "settings.json"), out, func(settings map[string]any) (bool, error) {
		hooked, err := addHook(settings, map[string]any{"type": "command", "command": syncCommand, "timeout": 60, "async": true})
		if err != nil {
			return false, err
		}
		allowed, err := addPermission(settings, "Bash(aldan:*)")
		return hooked || allowed, err
	})
	if err != nil {
		return err
	}
	err = updateJSON(filepath.Join(home, ".codex", "hooks.json"), out, func(settings map[string]any) (bool, error) {
		return addHook(settings, map[string]any{"type": "command", "command": syncCommand, "timeout": 60})
	})
	if err != nil {
		return err
	}
	if err := addWritableRoot(filepath.Join(home, ".codex", "config.toml"), data, out); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(home, ".claude", "CLAUDE.md"), filepath.Join(home, ".codex", "AGENTS.md")} {
		if err := addNote(path, out); err != nil {
			return err
		}
	}
	return nil
}

// addNote appends the agent note, unless the file already has it.
func addNote(path string, out io.Writer) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if bytes.Contains(old, []byte(agentNote)) {
		return nil
	}
	updated := slices.Clone(old)
	if len(updated) > 0 {
		updated = append(bytes.TrimRight(updated, "\n"), "\n\n"...)
	}
	return write(path, old, append(updated, agentNote...), out)
}

func updateJSON(path string, out io.Writer, change func(map[string]any) (bool, error)) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	settings := map[string]any{}
	if len(bytes.TrimSpace(old)) > 0 {
		if settings, err = decodeObject(old); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	changed, err := change(settings)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		return nil
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false) // existing hook commands hold shell redirects
	if err := encoder.Encode(settings); err != nil {
		return err
	}
	return write(path, old, encoded.Bytes(), out)
}

// child returns the object at key, adding one if it is absent. A value of
// another type is an error, because replacing it would lose a setting.
func child(parent map[string]any, key string) (map[string]any, error) {
	value, present := parent[key]
	if !present {
		object := map[string]any{}
		parent[key] = object
		return object, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%q is not an object", key)
	}
	return object, nil
}

func list(parent map[string]any, key string) ([]any, error) {
	value, present := parent[key]
	if !present {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%q is not a list", key)
	}
	return items, nil
}

// syncCommand keeps the index fresh after every turn.
const syncCommand = "aldan sync >/dev/null"

// addHook adds a Stop hook, unless one with the same command is there.
func addHook(settings map[string]any, hook map[string]any) (bool, error) {
	hooks, err := child(settings, "hooks")
	if err != nil {
		return false, err
	}
	stops, err := list(hooks, "Stop")
	if err != nil {
		return false, err
	}
	for _, stop := range stops {
		group, _ := stop.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, entry := range entries {
			if existing, _ := entry.(map[string]any); existing["command"] == hook["command"] {
				return false, nil
			}
		}
	}
	hooks["Stop"] = append(stops, map[string]any{"hooks": []any{hook}})
	return true, nil
}

// addPermission lets the watcher run without a permission prompt.
func addPermission(settings map[string]any, rule string) (bool, error) {
	permissions, err := child(settings, "permissions")
	if err != nil {
		return false, err
	}
	allow, err := list(permissions, "allow")
	if err != nil {
		return false, err
	}
	if slices.Contains(allow, any(rule)) {
		return false, nil
	}
	permissions["allow"] = append(allow, rule)
	return true, nil
}

func addWritableRoot(path, root string, out io.Writer) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if sandboxSection.Match(old) {
		listed := regexp.MustCompile(`writable_roots[ \t]*=[ \t]*\[[^\]]*["']` + regexp.QuoteMeta(root) + `["']`)
		if listed.Match(old) {
			return nil
		}
		return fmt.Errorf("%s already has a [sandbox_workspace_write] section: add %s to its writable_roots, then Codex sessions can reach the queue", path, strconv.Quote(root))
	}
	return write(path, old, append(slices.Clone(old), rootSection(root)...), out)
}

func rootSection(root string) string {
	return fmt.Sprintf("\n[sandbox_workspace_write]\nwritable_roots = [%s]\n", strconv.Quote(root))
}

// write keeps the old contents in a .bak file beside the config.
func write(path string, old, updated []byte, out io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if old != nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return err
	}
	fmt.Fprintln(out, "updated", path)
	return nil
}

// uninstall removes what install added, the data directory and the binary.
func uninstall(home, binary string, out io.Writer) error {
	err := updateJSON(filepath.Join(home, ".claude", "settings.json"), out, func(settings map[string]any) (bool, error) {
		unhooked := removeHook(settings)
		disallowed := removePermission(settings, "Bash(aldan:*)")
		return unhooked || disallowed, nil
	})
	if err != nil {
		return err
	}
	err = updateJSON(filepath.Join(home, ".codex", "hooks.json"), out, func(settings map[string]any) (bool, error) {
		return removeHook(settings), nil
	})
	if err != nil {
		return err
	}
	data := filepath.Join(home, ".aldan")
	if err := removeWritableRoot(filepath.Join(home, ".codex", "config.toml"), data, out); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(home, ".claude", "CLAUDE.md"), filepath.Join(home, ".codex", "AGENTS.md")} {
		if err := removeNote(path, out); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(data); err != nil {
		return err
	}
	fmt.Fprintln(out, "removed", data)
	if err := os.Remove(binary); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	fmt.Fprintln(out, "removed", binary)
	return nil
}

// removeHook drops the aldan Stop hook, and any group or list left empty.
func removeHook(settings map[string]any) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	stops, _ := hooks["Stop"].([]any)
	removed := false
	var kept []any
	for _, stop := range stops {
		group, _ := stop.(map[string]any)
		entries, _ := group["hooks"].([]any)
		var left []any
		for _, entry := range entries {
			if hook, _ := entry.(map[string]any); hook["command"] == syncCommand {
				removed = true
				continue
			}
			left = append(left, entry)
		}
		if len(left) == 0 && len(entries) > 0 {
			continue
		}
		if group != nil && len(left) != len(entries) {
			group["hooks"] = left
		}
		kept = append(kept, stop)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, "Stop")
	} else {
		hooks["Stop"] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	return true
}

func removePermission(settings map[string]any, rule string) bool {
	permissions, _ := settings["permissions"].(map[string]any)
	allow, _ := permissions["allow"].([]any)
	index := slices.Index(allow, any(rule))
	if index < 0 {
		return false
	}
	allow = slices.Delete(allow, index, index+1)
	if len(allow) == 0 {
		delete(permissions, "allow")
	} else {
		permissions["allow"] = allow
	}
	if len(permissions) == 0 {
		delete(settings, "permissions")
	}
	return true
}

// removeWritableRoot removes the section install appended. A root that the user
// added in their own section is theirs to remove.
func removeWritableRoot(path, root string, out io.Writer) error {
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	section := []byte(rootSection(root))
	if bytes.Contains(old, section) {
		return write(path, old, bytes.Replace(old, section, nil, 1), out)
	}
	if bytes.Contains(old, []byte(strconv.Quote(root))) {
		fmt.Fprintf(out, "%s: remove %s from writable_roots yourself\n", path, strconv.Quote(root))
	}
	return nil
}

func removeNote(path string, out io.Writer) error {
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(old, []byte(agentNote)) {
		return nil
	}
	updated := bytes.TrimRight(bytes.Replace(old, []byte(agentNote), nil, 1), "\n")
	if len(updated) > 0 {
		updated = append(updated, '\n')
	}
	return write(path, old, updated, out)
}
