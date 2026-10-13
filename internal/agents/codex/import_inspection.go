package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
)

// InspectionFor attests the actual injected codec, excluding embedded overrides.
func (Filter) InspectionFor(adapter archive.Adapter) bool { _, ok := adapter.(Filter); return ok }

// SupportsImportInspection restricts compact inspection to ordinary file input.
// Related history keeps its existing retained ordinal and manifest validation.
func (Filter) SupportsImportInspection(in agentapi.NativeInput) bool {
	return in.File != nil && in.Records == nil
}

// InspectFilteredImport applies the same complete capture codec and discards
// encoded safe rows after counting them. No admission or history is inferred.
func (f Filter) InspectFilteredImport(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (agentapi.ImportSummary, error) {
	if !f.SupportsImportInspection(in) {
		return agentapi.ImportSummary{}, errors.New("file input required for import inspection")
	}
	budget := c.Limits.ReadBudget
	reservation := budget.Reservation()
	var encoder func(map[string]any) ([]byte, error)
	var before func(int) (func(), error)
	if budget != nil {
		scanner, err := nativeScannerCharge(ctx, in.File, c.Limits, budget)
		if err != nil {
			return agentapi.ImportSummary{}, err
		}
		if !reservation.Reserve(scanner) {
			return agentapi.ImportSummary{}, errFilterBudget
		}
		defer reservation.Release(scanner)
		before = func(n int) (func(), error) {
			if !reservation.Reserve(int64(n)) {
				return nil, agentapi.ReadBudgetLimit(errors.New("native decoded row exceeds shared budget"))
			}
			return func() { reservation.Release(int64(n)) }, nil
		}
		encoder = func(value map[string]any) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			const scratch = 32 << 10
			if !reservation.Reserve(scratch) {
				return nil, agentapi.ReadBudgetLimit(errors.New("native encoding preflight exceeds shared budget"))
			}
			available := reservation.Available()
			n, err := agentmeta.JSONWireBound(ctx, value, available)
			if errors.Is(err, agentmeta.ErrJSONWireLimit) {
				reservation.Refuse(available + 1)
			}
			reservation.Release(scratch)
			if err != nil {
				return nil, agentapi.ReadBudgetLimit(err)
			}
			if !reservation.Reserve(n) {
				return nil, agentapi.ReadBudgetLimit(errors.New("native encoder exceeds shared budget"))
			}
			defer reservation.Release(n)
			encoded, err := json.Marshal(value)
			if err == nil && int64(len(encoded)) > n {
				return nil, agentapi.ReadBudgetLimit(agentmeta.ErrJSONWireLimit)
			}
			return encoded, err
		}
	}
	summary := agentapi.ImportSummary{}
	policy := importPolicy()
	out, err := sourceio.FilterJSONL(ctx, in, c, func(r io.Reader) (archive.FilteredTranscript, error) {
		return nativecodec.InspectCodexCaptureEncodedJSONL(r, c.Filename, func(safe map[string]any) {
			summary.Conversation = summary.Conversation || policy.RecordConversation(safe)
		}, encoder, before, archive.CaptureBoundary{RetainedRecords: c.Limits.Records, RetainedBytes: int(c.Limits.FilteredBytes)})
	})
	if err != nil {
		return agentapi.ImportSummary{}, err
	}
	summary.NativeStartAt = out.NativeStartAt
	summary.RetainedBytes = int64(out.Boundary.RetainedBytes)
	summary.RetainedRecords = out.Boundary.RetainedRecords
	return summary, nil
}
