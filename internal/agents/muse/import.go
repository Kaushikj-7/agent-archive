package muse

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Imports supplies historical observations without issuing capture authority.
func Imports() agentapi.ImportInspector { return importInspector{} }

type importInspector struct{}

// ImportPolicy requires original native creation evidence, never file dates.
func (importInspector) ImportPolicy(agentapi.SourceRef) agentapi.ImportPolicy {
	return agentapi.ImportPolicy{Start: agentapi.ImportNativeStart}
}

// InspectImport derives conversation facts through the ordinary retained parser.
// The caller must have filtered a confined complete source snapshot first.
func (importInspector) InspectImport(ctx context.Context, r agentapi.ImportInspectionRequest) (agentapi.ImportInspection, error) {
	if err := ctx.Err(); err != nil {
		return agentapi.ImportInspection{}, err
	}
	f := r.Filtered
	if r.Session.Agent != agentmeta.Muse || f.Format != "muse-jsonl" || len(f.Text) != 0 || f.History != nil || !f.NativeStartComplete || f.NativeStartAt.IsZero() || len(f.SessionIDs) != 1 || f.SessionIDs[0] == "" {
		return agentapi.ImportInspection{}, errFormat
	}
	b := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, NativeSessionID: f.SessionIDs[0], Capture: archive.SourceCapture{Harness: archive.Harness{Name: "muse"}, AdapterName: "muse", SourceFormat: f.Format, Gaps: f.Gaps}}
	total := 0
	for _, raw := range f.Records {
		if err := ctx.Err(); err != nil {
			return agentapi.ImportInspection{}, err
		}
		if len(raw) > archive.MaxRecordBytes-total {
			return agentapi.ImportInspection{}, archive.ErrRecordTooLarge
		}
		total += len(raw)
		var record map[string]any
		if json.Unmarshal(raw, &record) != nil || len(record) == 0 {
			return agentapi.ImportInspection{}, errFormat
		}
		b.NativeRecords = append(b.NativeRecords, record)
	}
	a, err := (Parser{}).Parse(ctx, b)
	if err != nil {
		return agentapi.ImportInspection{}, err
	}
	if !a.Facts.NativeStartedAt.Equal(f.NativeStartAt) || !r.Header.StartedAt.IsZero() && !r.Header.StartedAt.Equal(a.Facts.NativeStartedAt) {
		return agentapi.ImportInspection{}, errFormat
	}
	if r.Header.Directory != "" && r.Header.Directory != a.Facts.WorkspaceRoot {
		return agentapi.ImportInspection{}, errFormat
	}
	mismatch := a.Facts.IdentityConflict || r.Session.NativeID == "" || !slices.Equal(f.SessionIDs, []string{r.Session.NativeID}) || r.Header.IdentityMismatch || r.Header.NativeID != "" && r.Header.NativeID != r.Session.NativeID
	return agentapi.ImportInspection{Conversation: len(a.View.Turns) != 0, IdentityMismatch: mismatch, StartedAt: a.Facts.NativeStartedAt}, nil
}
