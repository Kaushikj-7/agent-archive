package agy

import (
	"context"
	"io"
	"reflect"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
)

// SourceProvider reads verified Antigravity transcript files.
type SourceProvider struct{}

// Describe declares append-only JSONL source semantics.
func (SourceProvider) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return (sourceio.FileProvider{}).Describe(ref)
}

// OpenPass delegates bounded file ownership to the shared source provider.
func (SourceProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	return (sourceio.FileProvider{}).OpenPass(ctx, env)
}

// Activities obtains content-free modification times for known transcripts.
func (SourceProvider) Activities(ctx context.Context, env agentapi.SourceEnvironment, refs []agentapi.SourceRef) (map[agentapi.SourceRef]time.Time, error) {
	return (sourceio.FileProvider{}).Activities(ctx, env, refs)
}

// Filter owns the AGY privacy adapter and shared retained-source behavior.
type Filter struct{ nativeAdapter }

type nativeAdapter struct{}

func (nativeAdapter) Name() string    { return "agy" }
func (nativeAdapter) Version() string { return "1.0.0" }

// FilterJSONL applies the adapter to a retained record stream.
func (Filter) FilterJSONL(r io.Reader) (archive.FilteredTranscript, error) {
	return filterAgyReader(r)
}

// EvidenceExtends reports whether a later retained snapshot preserves every
// previously retained AGY record in order.
func (Filter) EvidenceExtends(previous, candidate archive.SourceBundle) bool {
	if len(candidate.NativeRecords) < len(previous.NativeRecords) {
		return false
	}
	for i, record := range previous.NativeRecords {
		if !reflect.DeepEqual(record, candidate.NativeRecords[i]) {
			return false
		}
	}
	return true
}

// Filter delegates JSONL filtering to the AGY adapter.
func (Filter) Filter(ctx context.Context, input agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	return sourceio.FilterJSONL(ctx, input, c, filterAgyReader)
}

// Refilter reapplies the current privacy policy to retained records.
func (Filter) Refilter(ctx context.Context, bundle archive.SourceBundle, _ time.Time) (archive.FilteredTranscript, error) {
	return sourceio.RefilterJSONL(ctx, Filter{}, bundle)
}
