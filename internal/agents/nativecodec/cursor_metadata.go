package nativecodec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// MaxCursorComposerVersion is the newest qualified composer metadata version.
const MaxCursorComposerVersion = 18

// CursorComposerMetadata is what one composerData value contributes.
type CursorComposerMetadata struct {
	Chat agentapi.DatabaseChat
	// counted is whether it is a chat: messages, and not a draft.
	Counted bool
	// subagents are the composer IDs of its subagents.
	Subagents []string
	// newer is set when its _v is newer than this release knows.
	Newer bool
}

// DecodeCursorComposerData decodes one composerData value; ok is false when it is
// not a shape this release knows. Only the fields that decide the count are
// strict: isDraft and the message lists, and _v must be a positive integer.
// A _v newer than MaxCursorComposerVersion is still read, and marked newer, when
// those fields decode. A composerId or workspaceIdentifier.id of another
// shape marks the chat Malformed (that chat's unsafe_format). The rest are
// read leniently and ignored when they are some other shape.
func DecodeCursorComposerData(key string, value []byte) (CursorComposerMetadata, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil || fields == nil {
		return CursorComposerMetadata{}, false
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
			return CursorComposerMetadata{}, false
		}
		newer = v > MaxCursorComposerVersion
	}
	var isDraft bool
	if raw, ok := present("isDraft"); ok && json.Unmarshal(raw, &isDraft) != nil {
		return CursorComposerMetadata{}, false
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
				return CursorComposerMetadata{}, false
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
				folder = CursorWorkspaceFolder(ws["uri"])
				if rawID, ok := ws["id"]; ok {
					if json.Unmarshal(rawID, &workspaceID) != nil {
						malformed = true
					}
				}
			}
		}
	}
	return CursorComposerMetadata{
		Chat: agentapi.DatabaseChat{
			CursorFacts:         CursorReviewFacts(fields),
			ID:                  id,
			KeyID:               keyID,
			CreatedAt:           createdAt,
			Folder:              folder,
			WorkspaceID:         workspaceID,
			WorkspaceIdentifier: workspaceIdentifier,
			Malformed:           malformed,
		},
		Counted:   counted,
		Subagents: subagents,
		Newer:     newer,
	}, true
}

// CursorWorkspaceFolder turns workspaceIdentifier.uri into a local folder:
// either a file:// URI string or a VS Code URI object (scheme, fsPath, path,
// external). Anything else, or a folder that is not local, is "".
func CursorWorkspaceFolder(raw json.RawMessage) string {
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

// CursorReviewFacts extracts the conservative metadata used for Cursor review.
func CursorReviewFacts(fields map[string]json.RawMessage) agentapi.CursorComposerFacts {
	versionRaw, versionPresent := fields["_v"]
	var version int
	if versionPresent && json.Unmarshal(versionRaw, &version) != nil {
		version = 0
	}
	facts := agentapi.CursorComposerFacts{Relationships: agentapi.CursorRelationshipsAbsent, VersionPresent: versionPresent, Version: version}
	raw, present := fields["subagentComposerIds"]
	if !present {
		return facts
	}
	facts.Relationships = agentapi.CursorRelationshipsMalformed
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return facts
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil || len(ids) > 64 {
		return facts
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || len(id) > 255 || seen[id] {
			return facts
		}
		seen[id] = true
	}
	sort.Strings(ids)
	body, err := json.Marshal(ids)
	if err != nil {
		return facts
	}
	sum := sha256.Sum256(body)
	facts.ChildIDsSHA256 = hex.EncodeToString(sum[:])
	facts.Relationships = agentapi.CursorRelationshipsValid
	return facts
}
