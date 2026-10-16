// Package importconfig supplies pure native historical inspection mechanics.
// Each integration declares its own record vocabulary and identity/start rules.
package importconfig

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Provider is a native declaration, consumed through actual backfill injection.
type Provider struct {
	FileCreatedStart         bool
	RequireSessionMembership bool
	PreferHeaderStart        bool
	NativeStart              bool
	ConversationTypes        []string
}

// ImportPolicy declares the configured native start evidence requirement.
func (p Provider) ImportPolicy(agentapi.SourceRef) agentapi.ImportPolicy {
	if p.FileCreatedStart {
		return agentapi.ImportPolicy{Start: agentapi.ImportFileCreatedStart}
	}
	return agentapi.ImportPolicy{Start: agentapi.ImportNativeStart}
}

// InspectImport interprets retained conversation, session identity and start evidence.
func (p Provider) InspectImport(ctx context.Context, r agentapi.ImportInspectionRequest) (out agentapi.ImportInspection, err error) {
	for _, text := range r.Filtered.Text {
		if strings.TrimSpace(text) != "" {
			out.Conversation = true
			break
		}
	}
	for _, record := range r.Filtered.Records {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var value struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(record, &value) == nil && p.recordConversation(value.Type, value.Role) {
			out.Conversation = true
			break
		}
	}
	return p.finalize(ctx, r, out.Conversation, r.Filtered.NativeStartAt)
}

func (p Provider) finalize(ctx context.Context, r agentapi.ImportInspectionRequest, conversation bool, nativeStart time.Time) (out agentapi.ImportInspection, err error) {
	out.Conversation = conversation
	if p.RequireSessionMembership && (len(r.Filtered.SessionIDs) > 0 || out.Conversation) && !slices.Contains(r.Filtered.SessionIDs, r.Session.NativeID) {
		out.IdentityMismatch = true
	}
	if p.PreferHeaderStart && !r.Header.StartedAt.IsZero() {
		out.StartedAt = r.Header.StartedAt
	} else if p.NativeStart {
		out.StartedAt = nativeStart
	}
	return out, ctx.Err()
}

// Cursor supplies the existing fixed Cursor historical interpretation policy.
func Cursor() agentapi.ImportInspector {
	return Provider{FileCreatedStart: true, ConversationTypes: []string{"user", "assistant", "message", "response_item", "tool_use", "tool_result", "tool_call"}}
}

// RecordConversation applies the retained JSON record predicate to a sanitized
// object without another JSON decode. Invalid field types reject the entire row,
// matching unmarshalling into the retained inspector's string fields.
func (p Provider) RecordConversation(record map[string]any) bool {
	values := [2]string{}
	keys := [2]string{}
	// JSON encoding sorts map keys, while struct decoding matches field names
	// case-insensitively. The last non-null spelling wins; any invalid spelling
	// causes the retained decoder to reject the whole row.
	for key, value := range record {
		for i, field := range []string{"type", "role"} {
			if !strings.EqualFold(key, field) || value == nil {
				continue
			}
			text, ok := value.(string)
			if !ok {
				return false
			}
			if key >= keys[i] {
				keys[i], values[i] = key, text
			}
		}
	}
	return p.recordConversation(values[0], values[1])
}

func (p Provider) recordConversation(kind, role string) bool {
	return slices.Contains(p.ConversationTypes, kind) || role != ""
}

// SupportsImportSummary excludes policies needing retained session membership.
func (p Provider) SupportsImportSummary() bool { return !p.RequireSessionMembership }

// InspectImportSummary uses the same header and native-start finalizer as rows.
func (p Provider) InspectImportSummary(ctx context.Context, r agentapi.ImportInspectionRequest, summary agentapi.ImportSummary) (agentapi.ImportInspection, error) {
	if !p.SupportsImportSummary() {
		return agentapi.ImportInspection{}, errors.New("import summary cannot prove session membership")
	}
	return p.finalize(ctx, r, summary.Conversation, summary.NativeStartAt)
}
