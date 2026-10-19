// Package hooks owns lifecycle configuration only. It never trusts hooks or runs an agent.
package hookconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Spec contains native rules supplied by an integration, never an agent switch.
type Spec struct {
	SchemaVersionKey       string
	CommandMarkerOwnership bool
	PreserveForeignBytes   bool
	Name                   string
	Events                 []string
	Flat                   bool
	Version                bool
	Owner                  string
	PrototypeOwner         string
	RootEnvironment        string
	DefaultDirectory       string
	Filename               string
}

// Configurator edits injected JSON values using the byte preserving editor.
type Configurator struct{ Spec Spec }

// Hook contains installation ownership observations for pure edits.
type Hook agentapi.HookOwner

// Hook is what setup installs into each application's hook configuration:
// the executable the hook runs and, when agent-archive's data directory is
// not the default, that directory. An application starts hooks with its own
// environment (Cursor launched from the Dock has none of the user's shell
// variables), so a relocated data directory must travel in the command
// itself, as the LaunchAgent's does.
//
// A Hook also names the installation whose handlers setup replaces and
// uninstall removes: each handler carries its data directory in its command
// (none for the default one), and a handler with another directory belongs
// to another installation and is never touched (see classify).
// Command is the shell command installed for harness. Every path is single
// quoted, so no path can run as shell syntax.
func (h Hook) command(spec Spec) (string, error) {
	if !filepath.IsAbs(h.Executable) || strings.ContainsAny(h.Executable, "\x00\r\n") {
		return "", errors.New("executable must be an absolute path")
	}

	command := quote(h.Executable) + " _hook --harness " + spec.Name + " # " + spec.Owner
	if h.DataHome == "" {
		return command, nil
	}
	if !filepath.IsAbs(h.DataHome) || strings.ContainsAny(h.DataHome, "\x00\r\n") {
		return "", errors.New("data directory must be an absolute path")
	}
	return "AGENT_ARCHIVE_HOME=" + quote(h.DataHome) + " " + command, nil
}

// Merge preserves unrelated handlers and top-level settings: only the
// "hooks" member (and Cursor's "version") is rewritten, and everything else
// in the file keeps its bytes (see document).
func merge(existing []byte, spec Spec, hook Hook) ([]byte, error) {
	command, err := hook.command(spec)
	if err != nil {
		return nil, err
	}
	names := spec.Events
	app := spec
	doc, err := parseDocument(existing)
	if err != nil {
		return nil, err
	}
	if app.Version {
		v, ok := doc.Root.Get("version")
		if ok && !isOne(v) {
			return nil, errors.New("unsupported Cursor hook configuration version")
		}
		if !ok {
			// Where Cursor's own files have it: before "hooks".
			if err = doc.SetBefore("hooks", "version", json.Number("1")); err != nil {
				return nil, err
			}
		}
	}
	if app.SchemaVersionKey != "" {
		value, present := doc.Root.Get(app.SchemaVersionKey)
		if present && !isOne(value) {
			return nil, errors.New("unsupported native hook settings schema")
		}
		if !present {
			if len(doc.Root.Members) != 0 {
				return nil, errors.New("native hook settings require a qualified schema")
			}
			if err := doc.SetBefore("hooks", app.SchemaVersionKey, json.Number("1")); err != nil {
				return nil, err
			}
		}
	}
	hs, err := hooksObject(doc.Root)
	if err != nil {
		return nil, err
	}
	if hs == nil {
		hs = &object{}
	}
	preserved := foreignHookNodes(existing, hs, app, hook)
	emptied, err := stripOwned(hs, app, hook)
	if err != nil {
		return nil, err
	}
	for event, empty := range emptied {
		// An event an earlier release installed and this one no longer uses.
		if empty && !slices.Contains(names, event) {
			hs.Remove(event, 0)
		}
	}
	for _, event := range names {
		// Keys in the order earlier releases wrote them (sorted), so rerunning
		// setup over an existing installation changes nothing.
		handler := &object{Members: []member{{Key: "command", Value: command}, {Key: "timeout", Value: json.Number("2")}}}
		var entry any = handler
		if !app.Flat {
			handler = &object{Members: []member{{Key: "command", Value: command}, {Key: "statusMessage", Value: spec.Owner}, {Key: "timeout", Value: json.Number("2")}, {Key: "type", Value: "command"}}}
			if app.CommandMarkerOwnership {
				handler.Remove("statusMessage", 0)
			}
			entry = &object{Members: []member{{Key: "hooks", Value: []any{handler}}}}
		}
		list, _ := getList(hs, event)
		hs.Set(event, append(list, entry))
	}
	restoreForeignHookNodes(hs, preserved)
	if err = doc.Set("hooks", hs); err != nil {
		return nil, err
	}
	return doc.Bytes(), nil
}

// Remove strips every handler hook's installation installed for harness
// from existing (and the prototype's), leaving unrelated handlers, another
// installation's, and top-level settings exactly as they were. It reports
// whether anything was actually removed so a caller can skip rewriting a
// file that never contained our entries. Like Merge, it only ever matches
// our own marker (or the exact known prototype handler), never a substring
// of an unrelated command. An event, or the whole "hooks" member, that only
// ever held our handlers is dropped, so a file setup only added hooks to
// gets its original bytes back. hook's Executable is not used.
func remove(existing []byte, spec Spec, hook Hook) ([]byte, bool, error) {
	if len(strings.TrimSpace(string(existing))) == 0 {
		return existing, false, nil
	}
	doc, err := parseDocument(existing)
	if err != nil {
		return nil, false, err
	}
	hs, err := hooksObject(doc.Root)
	if err != nil {
		return nil, false, err
	}
	if hs == nil {
		return existing, false, nil
	}
	preserved := foreignHookNodes(existing, hs, spec, hook)
	emptied, err := stripOwned(hs, spec, hook)
	if err != nil {
		return nil, false, err
	}
	if len(emptied) == 0 {
		return existing, false, nil
	}
	for event, empty := range emptied {
		if empty {
			hs.Remove(event, 0)
		}
	}
	if len(hs.Members) == 0 {
		doc.Remove("hooks")
		// Setup adds Cursor's "version" beside the hooks it installs; with
		// no hooks left and nothing else in the file, it goes too, and the
		// file is left as setup would have found an empty one.
		if v, ok := doc.Root.Get("version"); spec.Version && ok && isOne(v) && len(doc.Root.Members) == 1 {
			doc.Clear()
		}
	} else {
		restoreForeignHookNodes(hs, preserved)
		if err = doc.Set("hooks", hs); err != nil {
			return nil, false, err
		}
	}
	return doc.Bytes(), true, nil
}

// Empty reports whether data is a hook file with nothing in it: no settings
// at all, just an empty JSON object (what Remove leaves of a file setup
// created). Such a file means the same to every application as no file.
func Empty(data []byte) bool {
	if len(bytes.TrimSpace(data)) == 0 {
		return true
	}
	doc, err := parseDocument(data)
	return err == nil && len(doc.Root.Members) == 0
}

// hooksObject returns the "hooks" member, or nil when there is none (or it
// is null).
func hooksObject(root *object) (*object, error) {
	raw, ok := root.Get("hooks")
	if !ok || raw == nil {
		return nil, nil
	}
	hs, ok := raw.(*object)
	if !ok {
		return nil, errors.New("invalid hooks object")
	}
	// The applications keep the last of two same-named keys; editing one of
	// them would silently drop the other's handlers, as it would for the
	// top-level "hooks" key itself (see parseDocument).
	if where, key, dup := duplicateKey(hs, `"hooks"`); dup {
		return nil, fmt.Errorf("%w: %s has more than one %q key; remove the duplicate", errInvalidConfiguration, where, key)
	}
	return hs, nil
}

// duplicateKey finds an object anywhere in value that names one key twice,
// and says where it is (from where) and which key.
func duplicateKey(value any, where string) (at, key string, found bool) {
	switch v := value.(type) {
	case *object:
		seen := map[string]bool{}
		for _, m := range v.Members {
			if seen[m.Key] {
				return where, m.Key, true
			}
			seen[m.Key] = true
		}
		for _, m := range v.Members {
			if at, key, found = duplicateKey(m.Value, fmt.Sprintf("%s.%q", where, m.Key)); found {
				return at, key, true
			}
		}
	case []any:
		for i, item := range v {
			if at, key, found = duplicateKey(item, fmt.Sprintf("%s[%d]", where, i)); found {
				return at, key, true
			}
		}
	}
	return "", "", false
}

func getList(hs *object, event string) ([]any, error) {
	raw, _ := hs.Get(event)
	list, ok := raw.([]any)
	if !ok && raw != nil {
		return nil, fmt.Errorf("invalid hook list for %s", event)
	}
	return list, nil
}

func isOne(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	f, err := n.Float64()
	return err == nil && f == 1
}

// handlerKind is whose a hook handler is.
type handlerKind int

const (
	// kindUnrelated is a handler agent-archive did not install.
	kindUnrelated handlerKind = iota
	// kindOwn is one this installation installed.
	kindOwn
	// kindPrototype is one the pre-release prototype installed; every
	// installation replaces and removes it.
	kindPrototype
	// kindOther is one another installation (another data directory)
	// installed, which is never touched.
	kindOther
)

// classify says whose handler is. For kindOther it also returns the data
// directory the handler's command runs with ("" for the default one), or,
// when that cannot be read from the command, the command in unreadable.
func classify(handler *object, app Spec, hook Hook) (kind handlerKind, dataHome, unreadable string) {
	command, _ := handler.Get("command")
	text, _ := command.(string)
	if app.CommandMarkerOwnership && !exactMarkerCommand(text, app) {
		return kindUnrelated, "", ""
	}
	if app.Flat || app.CommandMarkerOwnership {
		if !strings.HasSuffix(text, " # "+app.Owner) {
			return kindUnrelated, "", ""
		}
	} else {
		status, _ := handler.Get("statusMessage")
		if app.PrototypeOwner != "" && status == app.PrototypeOwner {
			return kindPrototype, "", ""
		}
		if status != app.Owner {
			return kindUnrelated, "", ""
		}
	}
	dataHome, ok := CommandDataHome(text)
	if !ok {
		return kindOther, "", text
	}
	if hook.sameInstallation(dataHome) {
		return kindOwn, dataHome, ""
	}
	return kindOther, dataHome, ""
}

// CommandDataHome reads the data directory a hook command setup wrote runs
// with: the AGENT_ARCHIVE_HOME it sets, or "" when it sets none and so runs
// with the default one. ok is false when the command sets the variable in a
// form Command never writes.
func CommandDataHome(command string) (dataHome string, ok bool) {
	rest, found := strings.CutPrefix(command, "AGENT_ARCHIVE_HOME=")
	if !found {
		return "", true
	}
	// Command writes one single-quoted word, each ' in the path written as
	// '"'"' (see quote), followed by a space.
	var b strings.Builder
	for {
		switch {
		case strings.HasPrefix(rest, "'"):
			end := strings.IndexByte(rest[1:], '\'')
			if end < 0 {
				return "", false
			}
			b.WriteString(rest[1 : end+1])
			rest = rest[end+2:]
		case strings.HasPrefix(rest, `"'"`):
			b.WriteByte('\'')
			rest = rest[3:]
		case strings.HasPrefix(rest, " ") && filepath.IsAbs(b.String()):
			return b.String(), true
		default:
			return "", false
		}
	}
}

// sameInstallation reports whether a handler whose command runs with
// dataHome ("" for the default data directory) belongs to h's installation.
// Both sides are compared with their symlinks resolved, and "" stands for
// DefaultDataHome, so the default installation is one installation however
// its directory was spelled. Two spellings of one existing directory that
// differ only in case (APFS and HFS+ are case-insensitive by default) are
// one directory too.
func (h Hook) sameInstallation(dataHome string) bool {
	orDefault := func(dir string) string {
		if dir == "" {
			return h.DefaultDataHome
		}
		return dir
	}
	a, b := orDefault(dataHome), orDefault(h.DataHome)
	if a == b {
		return true
	}
	left, lok := h.Locations[a]
	right, rok := h.Locations[b]
	return lok && rok && left == right
}

// handlerList returns the handlers of one event's list: for Cursor the
// entries themselves, for the others the handlers inside each matcher group.
// It fails on a list of any other shape.
func handlerList(groups []any, app Spec) ([]*object, error) {
	var out []*object
	for _, item := range groups {
		g, ok := item.(*object)
		if !ok {
			return nil, errors.New("invalid hook entry")
		}
		if app.Flat {
			out = append(out, g)
			continue
		}
		raw, _ := g.Get("hooks")
		handlers, ok := raw.([]any)
		if !ok {
			return nil, errors.New("invalid hook handlers")
		}
		for _, h := range handlers {
			handler, ok := h.(*object)
			if !ok {
				return nil, errors.New("invalid hook handler")
			}
			out = append(out, handler)
		}
	}
	return out, nil
}

// replaces reports whether setup replaces (and uninstall removes) a handler
// of kind for h's installation: its own, never another installation's, and
// the prototype's only for the default installation, which is the one that
// succeeds it (setup retires the prototype's job only there too). A second
// or test installation leaves the prototype as it is.
func (h Hook) replaces(kind handlerKind) bool {
	return kind == kindOwn || (kind == kindPrototype && h.DataHome == "")
}

// stripOwned removes this installation's handlers and exact known prototype
// handlers from every event in hs, including events no longer used by the
// current implementation. It never removes by substring alone, and never
// another installation's handler. It returns each event it removed a handler
// from, mapped to whether that left the event empty.
func stripOwned(hs *object, app Spec, hook Hook) (map[string]bool, error) {
	changed := map[string]bool{}
	for i, m := range hs.Members {
		groups, ok := m.Value.([]any)
		if !ok {
			return nil, fmt.Errorf("invalid hook list for %s", m.Key)
		}
		// Checks the shape of the whole list before anything is changed, so
		// the type assertions below cannot fail.
		if _, err := handlerList(groups, app); err != nil {
			return nil, err
		}
		kept := []any{}
		removed := false
		for _, item := range groups {
			g, _ := item.(*object)
			if app.Flat {
				if kind, _, _ := classify(g, app, hook); hook.replaces(kind) {
					removed = true
					continue
				}
				kept = append(kept, g)
				continue
			}
			rawHandlers, _ := g.Get("hooks")
			handlers, _ := rawHandlers.([]any)
			remaining := []any{}
			for _, h := range handlers {
				handler, _ := h.(*object)
				if kind, _, _ := classify(handler, app, hook); hook.replaces(kind) {
					removed = true
					continue
				}
				remaining = append(remaining, h)
			}
			if len(remaining) == len(handlers) {
				kept = append(kept, g)
			} else if len(remaining) > 0 {
				g.Set("hooks", remaining)
				kept = append(kept, g)
			}
		}
		if removed {
			hs.Members[i].Value = kept
			changed[m.Key] = len(kept) == 0
		}
	}
	return changed, nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// foreignHookNodes retains only unowned, unchanged native groups and handlers.
// Expected-byte file changes still guard the entire document at application.
func foreignHookNodes(raw []byte, hs *object, spec Spec, hook Hook) map[*object]json.RawMessage {
	if !spec.PreserveForeignBytes {
		return nil
	}
	preserved := map[*object]json.RawMessage{}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return preserved
	}
	var events map[string][]json.RawMessage
	if json.Unmarshal(doc["hooks"], &events) != nil {
		return preserved
	}
	for _, member := range hs.Members {
		groups, ok := member.Value.([]any)
		if !ok {
			continue
		}
		rawGroups := events[member.Key]
		if len(groups) != len(rawGroups) {
			continue
		}
		for n, value := range groups {
			group, ok := value.(*object)
			if !ok {
				continue
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(rawGroups[n], &fields) != nil {
				continue
			}
			var rawHandlers []json.RawMessage
			if json.Unmarshal(fields["hooks"], &rawHandlers) != nil {
				continue
			}
			h, _ := group.Get("hooks")
			handlers, ok := h.([]any)
			if !ok || len(handlers) != len(rawHandlers) {
				continue
			}
			allForeign := true
			for k, value := range handlers {
				handler, ok := value.(*object)
				if !ok {
					allForeign = false
					continue
				}
				kind, _, _ := classify(handler, spec, hook)
				if hook.replaces(kind) {
					allForeign = false
				} else {
					preserved[handler] = rawHandlers[k]
				}
			}
			if allForeign {
				preserved[group] = rawGroups[n]
			}
		}
	}
	return preserved
}

func restoreForeignHookNodes(hs *object, preserved map[*object]json.RawMessage) {
	if len(preserved) == 0 {
		return
	}
	for _, member := range hs.Members {
		groups, ok := member.Value.([]any)
		if !ok {
			continue
		}
		for n, value := range groups {
			group, ok := value.(*object)
			if !ok {
				continue
			}
			if raw, ok := preserved[group]; ok {
				groups[n] = raw
				continue
			}
			h, _ := group.Get("hooks")
			handlers, ok := h.([]any)
			if !ok {
				continue
			}
			for k, value := range handlers {
				if handler, ok := value.(*object); ok {
					if raw, ok := preserved[handler]; ok {
						handlers[k] = raw
					}
				}
			}
		}
	}
}

func exactMarkerCommand(command string, spec Spec) bool {
	rest, found := strings.CutPrefix(command, "AGENT_ARCHIVE_HOME=")
	if !found {
		return false
	}
	rest, ok := quotedAbsoluteWord(rest)
	if !ok {
		return false
	}
	rest, ok = quotedAbsoluteWord(rest)
	if !ok {
		return false
	}
	return rest == "_hook --harness "+spec.Name+" # "+spec.Owner
}

func quotedAbsoluteWord(rest string) (remainder string, ok bool) {
	var value strings.Builder
	for {
		switch {
		case strings.HasPrefix(rest, "'"):
			n := strings.IndexByte(rest[1:], '\'')
			if n < 0 {
				return "", false
			}
			value.WriteString(rest[1 : n+1])
			rest = rest[n+2:]
		case strings.HasPrefix(rest, `"'"`):
			value.WriteByte('\'')
			rest = rest[3:]
		case strings.HasPrefix(rest, " "):
			word := value.String()
			return rest[1:], filepath.IsAbs(word) && !strings.ContainsAny(word, "\x00\r\n")
		default:
			return "", false
		}
	}
}
