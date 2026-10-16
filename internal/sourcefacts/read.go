package sourcefacts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/trace"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const (
	// HeaderBytes caps all preauthorization reading for one source.
	HeaderBytes = 256 << 10
	// HeaderRecords bounds leading records inspected for native task evidence.
	HeaderRecords = 64
)

// Header is a content-free, bounded observation of one Codex source.
type Header struct {
	SourceInfo os.FileInfo `json:"-"`
	// Identity survives understood history-pending outcomes as lookup evidence only.
	Identity        *codexmeta.CodexIdentity
	NativeCreatedAt time.Time
	// FormatFacts are bounded metadata-only observations, never task evidence or permission.
	FormatFacts          *CodexMeta
	Meta                 CodexMeta
	Started              time.Time
	FirstTaskAt          time.Time
	Profile              CodexProfile
	Outcome              string
	Bytes                int64
	NativeReadBytes      int64
	NativeReadOperations int64
}

// OpenRegular opens within an approved root without blocking on FIFOs or
// permitting a symlink escape. The caller owns the returned file.
func OpenRegular(root, path string) (*os.File, error) {
	return transcriptio.OpenRootRegular(root, path)
}

// ReadHeader opens a bounded source safely. Paths and native IDs are never
// included in its outcome codes or errors.
func ReadHeader(ctx context.Context, root, path string) Header {
	snapshot, err := transcriptio.Open(RootOpener{Root: root}, path, transcriptio.OpenPolicy{Root: root, RejectSymlinks: true})
	if err != nil {
		return Header{Outcome: "source_unavailable"}
	}
	defer func() { _ = snapshot.Close() }()
	reader := &measuredHeaderReader{reader: snapshot.Reader(ctx)}
	h := ReadCodexHeader(reader, path)
	trace.Add(trace.HeaderBytes, reader.bytes)
	h.NativeReadBytes, h.NativeReadOperations = reader.bytes, reader.operations
	if snapshot.Check() != nil || ctx.Err() != nil {
		return Header{Outcome: "source_changed", Bytes: h.Bytes, NativeReadBytes: h.NativeReadBytes, NativeReadOperations: h.NativeReadOperations}
	}
	h.SourceInfo = snapshot.SourceInfo()
	return h
}

type measuredHeaderReader struct {
	reader     io.Reader
	bytes      int64
	operations int64
}

func (r *measuredHeaderReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.bytes += int64(n)
	r.operations++
	return n, err
}

// ReadCodexHeader inspects metadata and the FIRST task event, stopping before
// full transcript parsing. It does not retain prompt or response content.
func ReadCodexHeader(reader io.Reader, path string) (h Header) {
	h = Header{Outcome: "incomplete_metadata"}
	// Rejected or partial records need only retry bookkeeping. Never persist
	// unsupported, potentially enormous metadata strings or raw values.
	defer func() {
		if h.Outcome != "native_format" {
			h.Meta = CodexMeta{}
			h.Started = time.Time{}
			h.FirstTaskAt = time.Time{}
			h.Profile = ""
		}
	}()
	r := bufio.NewReaderSize(io.LimitReader(reader, HeaderBytes), 32<<10)
	for i := range HeaderRecords {
		line, err := r.ReadBytes('\n')
		h.Bytes += int64(len(line))
		if len(line) > 64<<10 {
			h.Outcome = "oversized_metadata"
			return h
		}
		if err != nil {
			return h
		}
		line = bytes.TrimSpace(line)
		if !json.Valid(line) {
			h.Outcome = "invalid_metadata"
			return h
		}
		if i == 0 {
			meta, start, found, e := ParseCodexMeta(line)
			if !found || e != nil {
				h.Outcome = "invalid_metadata"
				return h
			}
			meta.Timestamp = start.Format(time.RFC3339Nano)
			h.Meta, h.Started = safeMeta(meta), start
			if len(meta.ID) > 128 || len(meta.Originator) > 256 || len(meta.Version) > 128 {
				h.Meta = CodexMeta{}
				h.Outcome = "oversized_metadata"
				return h
			}
			identity, identityOutcome := meta.Identity(path)
			captureOutcome := meta.CaptureOutcome(path)
			knownHistory := captureOutcome == codexmeta.NativeFormat || captureOutcome == codexmeta.ChildHistoryPending || captureOutcome == codexmeta.ForkHistoryPending || captureOutcome == codexmeta.RelatedHistoryPending
			if identityOutcome == "" && knownHistory && !start.IsZero() {
				h.Identity = &identity
				h.NativeCreatedAt = start
			}
			if identityOutcome != "" {
				h.Outcome = string(identityOutcome)
				return h
			}
			h.Profile = CodexFormatProfile(meta)
			if outcome := meta.FormatOutcome(); outcome != codexmeta.NativeFormat {
				h.Outcome = string(outcome)
				return h
			}
			facts := h.Meta
			h.FormatFacts = &facts
			continue
		}
		// Raw physical ordinals include every record, even records privacy later drops.
		// An absent boundary excludes nothing; explicit zero is preserved.
		if beforeOwnBoundary(h.Identity, uint64(i)) {
			continue
		}
		if needsRelatedReader(h.Identity) {
			h.Outcome = "related_history_pending"
			return h
		}

		if seen, native := NativeFirstTask(line); seen {
			h.FirstTaskAt = FirstTaskAt(line)
			if !native || h.FirstTaskAt.Before(h.Started.Add(-time.Second)) {
				h.Outcome = "inherited_history"
			} else {
				h.Outcome = "native_format"
			}
			return h
		}
	}
	return h
}

func safeMeta(m CodexMeta) CodexMeta {
	if source, known := m.ExecutionSourceFacts(); known {
		m.Source = source
	} else {
		m.Source = nil
	}

	// Relationships live in the separately validated typed identity. Format
	// projections must not substitute booleans for native relationship fields.
	m.ForkedFrom, m.ForkOrdinal, m.Parent, m.HistoryBase, m.SubagentOrdinal = nil, nil, nil, nil, nil
	return m
}

func beforeOwnBoundary(identity *codexmeta.CodexIdentity, ordinal uint64) bool {
	if identity == nil {
		return false
	}
	own := identity.SubagentOrdinal
	if fork := identity.ForkOrdinal; fork != nil && (own == nil || *fork > *own) {
		own = fork
	}
	return own != nil && ordinal < *own
}

func needsRelatedReader(identity *codexmeta.CodexIdentity) bool {
	return identity != nil && (identity.HistoryBase != nil || identity.RolloutID != identity.ThreadID)
}
