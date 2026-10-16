package cursor

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// LabelContract pins how a chat name is read from Cursor's database: the
// composerData:<id> row's name, as the cursor-composer filter reads it.
const LabelContract = "cursor-composer-name-v1"

// labelRequests and labelTimeout bound one pass's lookup, as Codex's are.
const (
	labelRequests = 64
	labelTimeout  = 2 * time.Second
)

// labelRowBytes bounds one composerData row decoded for its name. A larger
// row (an old chat with its conversation inline) is left unavailable.
const labelRowBytes = 4 << 20

var labelComposerID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// LabelProvider reads Cursor chat names from the global state.vscdb for
// admitted Cursor sessions, whatever their source format: the agent
// transcripts Cursor sessions are captured from carry no name. It never
// enumerates the database and never reads a message row.
type LabelProvider struct {
	// read, when set (by a test), replaces cursorstore.ReadComposerRows.
	read func(ctx context.Context, db string, ids []string, limit int64) (map[string]cursorstore.ComposerRow, error)
}

// RegistrationLabelContext derives lookup context from the admitted
// registration: the composer ID is the session's native ID.
func (LabelProvider) RegistrationLabelContext(reg archive.SessionRegistration) (agentapi.LabelContext, bool) {
	if reg.Harness.Name != "cursor" || !labelComposerID.MatchString(reg.NativeSessionID) {
		return agentapi.LabelContext{}, false
	}
	return agentapi.LabelContext{NativeID: reg.NativeSessionID, Ordinary: true}, true
}

// LookupLabels reads every requested chat's composerData row in one read of
// the database (one transaction, so one consistent state) and decodes its
// name. env.Homes name Codex homes and are ignored; the database is
// env.CursorDatabase.
func (p LabelProvider) LookupLabels(ctx context.Context, env agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	out := map[string]archive.SessionLabel{}
	if env.CursorDatabase == "" || len(requests) == 0 || len(requests) > labelRequests {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, labelTimeout)
	defer cancel()
	ids := []string{}
	wanted := map[string]bool{}
	for _, request := range requests {
		id := request.Registration.NativeSessionID
		if _, ok := (LabelProvider{}).RegistrationLabelContext(request.Registration); !ok || request.Context.NativeID != id {
			continue
		}
		if !wanted[id] {
			wanted[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out
	}
	read := p.read
	if read == nil {
		read = cursorstore.ReadComposerRows
	}
	rows, err := read(ctx, env.CursorDatabase, ids, labelRowBytes)
	if err != nil {
		return out
	}
	for _, request := range requests {
		id := request.Registration.NativeSessionID
		row, found := rows[id]
		if !wanted[id] || !found || row.Oversized {
			// A chat missing from the database proves nothing about its name.
			continue
		}
		label, ok := composerLabel(id, row.Value)
		if !ok {
			continue
		}
		if filtered, ok := archive.FilterSessionLabel(label); ok {
			out[request.Registration.ArchiveSessionID] = filtered
		}
	}
	return out
}

// composerLabel decodes a composerData value's name by the cursor-composer
// filter's rules: text, trimmed, is the name; a missing, null or blank name
// is a chat Cursor has not named; anything else is not a name.
func composerLabel(id string, raw []byte) (archive.SessionLabel, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return archive.SessionLabel{}, false
	}
	if rawID, ok := fields["composerId"]; ok {
		var owner string
		if json.Unmarshal(rawID, &owner) != nil || owner != id {
			return archive.SessionLabel{}, false
		}
	}
	label := archive.SessionLabel{NativeID: id, State: archive.SessionLabelAbsent, Source: archive.SessionLabelDatabase, Contract: LabelContract}
	rawName, ok := fields["name"]
	if !ok || string(rawName) == "null" {
		return label, true
	}
	var name string
	if json.Unmarshal(rawName, &name) != nil {
		return archive.SessionLabel{}, false
	}
	if name = strings.TrimSpace(name); name != "" {
		label.State, label.Name = archive.SessionLabelPresent, name
	}
	return label, true
}

// supportedSessionLabel reports whether retained label evidence is one this
// integration reads chat names from.
func supportedSessionLabel(label archive.SessionLabel) bool {
	return label.Contract == LabelContract && label.Source == archive.SessionLabelDatabase && labelComposerID.MatchString(label.NativeID)
}
