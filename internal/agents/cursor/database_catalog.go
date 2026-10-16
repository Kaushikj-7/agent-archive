package cursor

import (
	"context"
	"encoding/json"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// DatabaseCatalogInspector owns Cursor composer metadata without reading message bodies.
type DatabaseCatalogInspector struct{}

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

type composer struct {
	chat      agentapi.DatabaseChat
	counted   bool
	subagents []string
	newer     bool
}

func decodeComposerData(key string, raw []byte) (composer, bool) {
	d, ok := nativecodec.DecodeCursorComposerData(key, raw)
	return composer{chat: d.Chat, counted: d.Counted, subagents: d.Subagents, newer: d.Newer}, ok
}

func composerWorkspaceFolder(raw json.RawMessage) string {
	return nativecodec.CursorWorkspaceFolder(raw)
}
