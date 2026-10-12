// Package sourceio reuses verified file reads for native providers.
package sourceio

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// FileProvider supplies verified file snapshots without native attachment assumptions.
type FileProvider struct{}

// Describe validates the locator and declares its consistency policy.
func (p FileProvider) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	if ref.Kind != archive.SourceKindFile {
		return agentapi.SourceSemantics{}, errors.New("unsupported file source kind")
	}
	return agentapi.SourceSemantics{Mutation: agentapi.AppendOnly, Provider: "file"}, nil
}

// OpenPass creates a lazy serial owner without opening source content.
func (p FileProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.Files == nil {
		e.Files = transcriptio.OS{}
	}
	return &filePass{env: e, live: map[*fileSnapshot]bool{}}, nil
}

type filePass struct {
	env      agentapi.SourceEnvironment
	closed   bool
	live     map[*fileSnapshot]bool
	closeErr error
}

func (p *filePass) Signature(ctx context.Context, ref agentapi.SourceRef) (agentapi.SourceObservation, error) {
	if ref.Kind != archive.SourceKindFile {
		return agentapi.SourceObservation{}, agentapi.Wrap(agentapi.Unsafe, errors.New("unsupported file source kind"))
	}
	if p.closed {
		return agentapi.SourceObservation{}, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return agentapi.SourceObservation{}, err
	}
	if p.env.Policy.Root != "" || p.env.Policy.RejectSymlinks {
		f, err := transcriptio.Open(p.env.Files, ref.Path, p.env.Policy)
		if err != nil {
			return agentapi.SourceObservation{}, Classify(err)
		}
		stamp := f.Stamp()
		err = errors.Join(Classify(f.Check()), agentapi.Wrap(agentapi.Cleanup, f.Close()))
		if err != nil {
			return agentapi.SourceObservation{}, err
		}
		return fileObservation(stamp.Size, stamp.ModifiedAt), nil
	}
	info, err := p.env.Files.Lstat(ref.Path)
	if err != nil {
		return agentapi.SourceObservation{}, Classify(err)
	}
	if info.Mode()&os.ModeSymlink != 0 && !p.env.Policy.RejectSymlinks {
		path, pathErr := p.env.Files.EvalSymlinks(ref.Path)
		if pathErr != nil {
			return agentapi.SourceObservation{}, Classify(pathErr)
		}
		info, err = p.env.Files.Lstat(path)
	}
	if err != nil {
		return agentapi.SourceObservation{}, Classify(err)
	}
	if !info.Mode().IsRegular() {
		return agentapi.SourceObservation{}, agentapi.Wrap(agentapi.Unsafe, transcriptio.ErrNotRegularFile)
	}
	return fileObservation(info.Size(), info.ModTime()), nil
}

func fileObservation(size int64, at time.Time) agentapi.SourceObservation {
	return agentapi.SourceObservation{Signature: FileSignature(size, at.UnixNano()), Present: true, Empty: size == 0, Activity: at, Size: size}
}

func (p *filePass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if ref.Kind != archive.SourceKindFile {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("unsupported file source kind"))
	}
	if p.closed {
		return nil, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := transcriptio.Open(p.env.Files, ref.Path, p.env.Policy)
	if err != nil {
		return nil, Classify(err)
	}
	s := &fileSnapshot{file: f, owner: p}
	p.live[s] = true
	if limits.RawBytes > 0 && f.Stamp().Size > limits.RawBytes {
		o := s.Observation()
		if checkErr := f.Check(); checkErr != nil {
			return nil, errors.Join(Classify(checkErr), s.Close())
		}
		return nil, errors.Join(agentapi.RawLimit(f.Stamp().Size, limits.RawBytes, &o), s.Close())
	}

	return s, nil
}

func (p *filePass) Close() error {
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	for s := range p.live {
		p.closeErr = errors.Join(p.closeErr, s.Close())
	}
	return p.closeErr
}

type fileSnapshot struct {
	file   *transcriptio.Snapshot
	owner  *filePass
	closed bool
	err    error
}

func (s *fileSnapshot) Observation() agentapi.SourceObservation {
	stamp := s.file.Stamp()
	return fileObservation(stamp.Size, stamp.ModifiedAt)
}

func (s *fileSnapshot) Input() agentapi.NativeInput {
	return agentapi.NativeInput{File: s}
}

func (s *fileSnapshot) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	delete(s.owner.live, s)
	s.err = agentapi.Wrap(agentapi.Cleanup, s.file.Close())
	return s.err
}

func (s *fileSnapshot) Length() int64 { return s.file.Stamp().Size }

func (s *fileSnapshot) Stamp() transcriptio.Stamp { return s.file.Stamp() }

func (s *fileSnapshot) ReadAt(b []byte, off int64) (int, error) {
	if s.closed || s.owner.closed {
		return 0, agentapi.ErrClosed
	}
	return s.file.ReadAt(b, off)
}

func (s *fileSnapshot) Check() error {
	if s.closed || s.owner.closed {
		return agentapi.ErrClosed
	}
	return Classify(s.file.Check())
}

func (s *fileSnapshot) Records(ctx context.Context, tail bool, window, record int64, visit func([]byte) bool) (transcriptio.RecordWindow, error) {
	if s.closed || s.owner.closed {
		return transcriptio.RecordWindow{}, agentapi.ErrClosed
	}
	return s.file.Records(ctx, tail, window, record, visit)
}

// Classify preserves source causes while making retry decisions explicit.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	var e *agentapi.SourceError
	if errors.As(err, &e) {
		return err
	}
	kind := agentapi.Unavailable
	switch {
	case errors.Is(err, transcriptio.ErrCleanup):
		kind = agentapi.Cleanup
	case errors.Is(err, os.ErrNotExist):
		kind = agentapi.Missing
	case errors.Is(err, transcriptio.ErrNotRegularFile):
		kind = agentapi.Unsafe
	case errors.Is(err, transcriptio.ErrChanged):
		kind = agentapi.Changed
	case errors.Is(err, cursorstore.ErrRecordLimit), errors.Is(err, transcriptio.ErrRecordTooLarge), errors.Is(err, archive.ErrRecordTooLarge), errors.Is(err, agentapi.ErrRawLimit):
		kind = agentapi.Limit
	case errors.Is(err, archive.ErrUnsafeSourceFormat):
		kind = agentapi.FormatMismatch
	default:
		var filterError *archive.FilterError
		if errors.As(err, &filterError) {
			kind = agentapi.Unsafe
		}
	}
	return agentapi.Wrap(kind, err)
}

// Activities observes file activity with one stat per locator and no descriptor reads.
func (p FileProvider) Activities(ctx context.Context, e agentapi.SourceEnvironment, refs []agentapi.SourceRef) (map[agentapi.SourceRef]time.Time, error) {
	pass, err := p.OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pass.Close() }()
	out := map[agentapi.SourceRef]time.Time{}
	for _, r := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if o, err := pass.Signature(ctx, r); err == nil {
			out[r] = o.Activity
		}
	}
	return out, nil
}
