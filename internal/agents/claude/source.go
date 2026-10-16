package claude

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"io"
	"os"

	"sync/atomic"
	"time"
)

// SourceProvider reads verified files and optional child metadata.
type SourceProvider struct{}

// Describe validates the locator and declares its consistency policy.
func (SourceProvider) Describe(r agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return (sourceio.FileProvider{}).Describe(r)
}

// OpenPass creates a lazy serial owner without opening source content.
func (SourceProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p, err := (sourceio.FileProvider{}).OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	return &sourcePass{SourcePass: p, live: map[*sourceSnapshot]bool{}, files: e.Files}, nil
}

type sourcePass struct {
	agentapi.SourcePass
	live   map[*sourceSnapshot]bool
	closed bool
	files  transcriptio.Opener
	err    error
}

func (p *sourcePass) Read(ctx context.Context, r agentapi.SourceRef, l agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.closed {
		return nil, agentapi.ErrClosed
	}
	snap, err := p.SourcePass.Read(ctx, r, l)
	if err != nil {
		return nil, err
	}
	var meta []byte
	if l.SubagentMetadata {
		meta, err = readMetadata(r.Path, p.files)
		if err != nil {
			return nil, errors.Join(err, snap.Close())
		}
	}
	s := &sourceSnapshot{SourceSnapshot: snap, owner: p, meta: meta}
	p.live[s] = true
	return s, nil
}

func (p *sourcePass) Close() error {
	if p.closed {
		return p.err
	}
	p.closed = true
	for s := range p.live {
		p.err = errors.Join(p.err, s.Close())
	}
	p.err = errors.Join(p.err, p.SourcePass.Close())
	return p.err
}

type sourceSnapshot struct {
	agentapi.SourceSnapshot
	owner  *sourcePass
	meta   []byte
	closed bool
	err    error
}

func (s *sourceSnapshot) Input() agentapi.NativeInput {
	in := s.SourceSnapshot.Input()
	in.SubagentMeta = s.meta
	return in
}

func (s *sourceSnapshot) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	s.meta = nil
	delete(s.owner.live, s)
	s.err = s.SourceSnapshot.Close()
	return s.err
}

// Filter delegates the current pure privacy codec behind the native input port.
type Filter struct{ nativecodec.ClaudeAdapter }

// Filter consumes verified native input under shared collection limits.
func (Filter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	a := nativecodec.ClaudeAdapter{}
	fn := a.FilterJSONL
	if in.SubagentMeta != nil {
		fn = func(r io.Reader) (archive.FilteredTranscript, error) {
			return a.FilterSubagentJSONL(r, in.SubagentMeta)
		}
	}
	return sourceio.FilterJSONL(ctx, in, c, fn)
}

// Refilter applies current privacy rules to retained native evidence.
func (f Filter) Refilter(ctx context.Context, b archive.SourceBundle, _ time.Time) (archive.FilteredTranscript, error) {
	return sourceio.RefilterJSONL(ctx, f, b)
}

// RefilterBounded applies current privacy rules within the retained output allowance.
func (f Filter) RefilterBounded(ctx context.Context, b archive.SourceBundle, _ time.Time, limits agentapi.ReadLimits) (archive.FilteredTranscript, error) {
	return sourceio.RefilterJSONL(ctx, boundedRetainedFilter{archive.CaptureBoundary{RetainedRecords: limits.Records, RetainedBytes: int(limits.FilteredBytes)}}, b)
}

type boundedRetainedFilter struct{ boundary archive.CaptureBoundary }

func (f boundedRetainedFilter) FilterJSONL(r io.Reader) (archive.FilteredTranscript, error) {
	return nativecodec.FilterClaudeRetainedBounded(r, f.boundary)
}

// MetadataReads counts eligible optional attachment observations for structural tests.
var MetadataReads atomic.Int64

func readMetadata(path string, files transcriptio.Opener) (data []byte, resultErr error) {
	if files == nil {
		files = transcriptio.OS{}
	}
	p, ok := nativecodec.SubagentMetaPath(path)
	if !ok {
		return nil, nil
	}
	MetadataReads.Add(1)
	found, err := files.Lstat(p)
	if err != nil || !found.Mode().IsRegular() {
		return nil, nil
	}
	f, err := files.OpenRegular(p)
	if err != nil {
		if errors.Is(err, transcriptio.ErrCleanup) || agentapi.HasFailure(err, agentapi.Cleanup) {
			return nil, sourceio.Classify(err)
		}
		return nil, nil
	}
	defer func() { resultErr = errors.Join(resultErr, agentapi.Wrap(agentapi.Cleanup, f.Close())) }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(found, opened) {
		return nil, nil
	}
	data, err = io.ReadAll(io.NewSectionReader(f, 0, nativecodec.MaxSubagentMetaBytes+1))
	if err != nil || len(data) > nativecodec.MaxSubagentMetaBytes {
		return nil, nil
	}
	return data, nil
}

// EvidenceExtends compares retained native facts under unchanged codec versions.
func (Filter) EvidenceExtends(previous, candidate archive.SourceBundle) bool {
	return nativecodec.EvidenceExtends(previous, candidate)
}

// Activities observes content-free file activity while preserving batching.
func (SourceProvider) Activities(ctx context.Context, e agentapi.SourceEnvironment, refs []agentapi.SourceRef) (map[agentapi.SourceRef]time.Time, error) {
	return (sourceio.FileProvider{}).Activities(ctx, e, refs)
}

// NamingOnlyChange isolates typed native title bookkeeping from activity.
func (Filter) NamingOnlyChange(previous, candidate archive.SourceBundle) bool {
	return nativecodec.ClaudeNamingOnlyChange(previous, candidate)
}
