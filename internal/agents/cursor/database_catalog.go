package cursor

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// DatabaseCatalogInspector owns Cursor composer metadata without reading message bodies.
type DatabaseCatalogInspector struct{}

// maxComposerVersion is the newest composerData _v this release knows. Newer
// rows are still counted when the fields the count needs decode, and the plan
// reports how many there were.
const maxComposerVersion = 18

// cursorComposerQuery reads the composerData rows. The range, rather than
// LIKE, uses the key's unique index, so the read touches only those rows and
// not every message row, and holds its lock only briefly.
const cursorComposerQuery = `SELECT key, value FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;'`

// InspectCatalog streams compact Cursor chat identities and workspace relationships from native database records.
func (DatabaseCatalogInspector) InspectCatalog(ctx context.Context, host agentapi.DatabaseCatalogHost) (agentapi.DatabaseCatalog, error) {
	var chats []agentapi.DatabaseChat
	newer := 0
	subagents := map[string]bool{}
	parents := map[string][]string{}
	err := host.Query(ctx, cursorComposerQuery, func(record agentapi.DatabaseRecord) error {
		if record.Value == nil {
			return nil
		}
		d, ok := decodeComposerData(record.Key, record.Value)
		if !ok {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		if d.newer {
			newer++
		}
		for _, id := range d.subagents {
			subagents[id] = true
		}
		if len(d.subagents) > 0 {
			parents[d.chat.ID] = append(parents[d.chat.ID], d.subagents...)
		}
		if d.counted {
			chats = append(chats, d.chat)
		}
		return nil
	})
	if err != nil {
		return agentapi.DatabaseCatalog{}, err
	}
	// A subagent's composer is part of its parent chat, not a chat of its
	// own.
	counted := map[string]bool{}
	kept := chats[:0]
	for _, c := range chats {
		counted[c.ID] = true
		if !subagents[c.ID] {
			kept = append(kept, c)
		}
	}
	res := agentapi.DatabaseCatalog{Chats: kept, NewerFormat: newer}
	for parent, ids := range parents {
		for _, id := range ids {
			if counted[id] {
				if res.Subagents == nil {
					res.Subagents = map[string][]string{}
				}
				res.Subagents[parent] = append(res.Subagents[parent], id)
			}
		}
	}
	return res, nil
}

// composer is what one composerData value contributes.
type composer struct {
	chat agentapi.DatabaseChat
	// counted is whether it is a chat: messages, and not a draft.
	counted bool
	// subagents are the composer IDs of its subagents.
	subagents []string
	// newer is set when its _v is newer than this release knows.
	newer bool
}

// decodeComposerData decodes one composerData value; ok is false when it is
// not a shape this release knows. Only the fields that decide the count are
// strict: isDraft and the message lists, and _v must be a positive integer.
// A _v newer than maxComposerVersion is still read, and marked newer, when
// those fields decode. A composerId or workspaceIdentifier.id of another
// shape marks the chat Malformed (that chat's unsafe_format). The rest are
// read leniently and ignored when they are some other shape.
func decodeComposerData(key string, value []byte) (composer, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil || fields == nil {
		return composer{}, false
	}
	present := func(name string) (json.RawMessage, bool) {
		raw, ok := fields[name]
		if !ok || string(raw) == "null" {
			return nil, false
		}
		return raw, true
	}
	newer := false
	if raw, ok := present("_v"); ok {
		var v int
		if json.Unmarshal(raw, &v) != nil || v < 1 {
			return composer{}, false
		}
		newer = v > maxComposerVersion
	}
	var isDraft bool
	if raw, ok := present("isDraft"); ok && json.Unmarshal(raw, &isDraft) != nil {
		return composer{}, false
	}
	// fullConversationHeadersOnly lists the messages; older chats keep them
	// inline in conversation instead. Messages are counted as cursorstore
	// reads a chat (decodeHeaders): from the headers when the chat has them,
	// even an empty list, and from conversation only when it has none, so a
	// chat is planned only when the collector would find messages in it.
	// Each header's own shape is not checked here: a header the reader can't
	// use makes that one chat unsafe_format when it is read, whereas a
	// listing error leaves the whole database unchecked.
	messages := 0
	for _, name := range []string{"fullConversationHeadersOnly", "conversation"} {
		if raw, ok := present(name); ok {
			var list []json.RawMessage
			if json.Unmarshal(raw, &list) != nil {
				return composer{}, false
			}
			messages = len(list)
			break
		}
	}

	keyID := strings.TrimPrefix(key, "composerData:")
	var id string
	malformed := false
	if raw, ok := present("composerId"); ok {
		if json.Unmarshal(raw, &id) != nil {
			malformed = true
		}
	}
	if id == "" {
		id = keyID
	}
	var subagents []string
	if raw, ok := present("subagentComposerIds"); ok {
		var ids []json.RawMessage
		if json.Unmarshal(raw, &ids) == nil {
			for _, rawID := range ids {
				var subagentID string
				if json.Unmarshal(rawID, &subagentID) == nil && subagentID != "" {
					subagents = append(subagents, subagentID)
				}
			}
		}
	}
	counted := !isDraft && messages > 0

	// The rest is read only for a chat that is counted.
	var createdAt time.Time
	var folder, workspaceID string
	workspaceIdentifier := false
	if counted {
		if raw, ok := present("createdAt"); ok {
			var ms float64
			if json.Unmarshal(raw, &ms) == nil && ms > 0 {
				createdAt = time.UnixMilli(int64(ms)).UTC()
			}
		}
		if raw, ok := present("workspaceIdentifier"); ok {
			// Present in any shape: a reference this release cannot resolve
			// (a remote URI, an unknown shape) still names a workspace.
			workspaceIdentifier = true
			var ws map[string]json.RawMessage
			if json.Unmarshal(raw, &ws) == nil {
				folder = composerWorkspaceFolder(ws["uri"])
				if rawID, ok := ws["id"]; ok {
					if json.Unmarshal(rawID, &workspaceID) != nil {
						malformed = true
					}
				}
			}
		}
	}
	return composer{
		chat: agentapi.DatabaseChat{
			ID:                  id,
			KeyID:               keyID,
			CreatedAt:           createdAt,
			Folder:              folder,
			WorkspaceID:         workspaceID,
			WorkspaceIdentifier: workspaceIdentifier,
			Malformed:           malformed,
		},
		counted:   counted,
		subagents: subagents,
		newer:     newer,
	}, true
}

// composerWorkspaceFolder turns workspaceIdentifier.uri into a local folder:
// either a file:// URI string or a VS Code URI object (scheme, fsPath, path,
// external). Anything else, or a folder that is not local, is "".
func composerWorkspaceFolder(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	fromURI := func(s string) string {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !filepath.IsAbs(u.Path) {
			return ""
		}
		return filepath.Clean(u.Path)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return fromURI(s)
	}
	var obj struct {
		Scheme    string `json:"scheme"`
		Authority string `json:"authority"`
		FSPath    string `json:"fsPath"`
		Path      string `json:"path"`
		External  string `json:"external"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	if obj.Scheme != "" && obj.Scheme != "file" {
		return ""
	}
	if obj.Authority != "" {
		return ""
	}
	for _, p := range []string{obj.FSPath, obj.Path} {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
	}
	if obj.External != "" {
		return fromURI(obj.External)
	}
	return ""
}
