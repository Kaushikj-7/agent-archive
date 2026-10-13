// Package capturecodec composes fixed pure Cursor transformations for native capture.
package capturecodec

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Bubble is one captured message row with its original encoded value.
type Bubble struct {
	ID    string
	Value json.RawMessage
}

// ComposerMetadata contains only the native metadata used by fixed capture.
type ComposerMetadata struct {
	Chat      agentapi.DatabaseChat
	Counted   bool
	Subagents []string
}

// Reservation borrows captured accounting without granting source authority.
type Reservation interface {
	Reserve(int64) bool
	Release(int64)
	Available() int64
	Refuse(int64)
}

// MaxComposerVersion is the newest qualified composer metadata version.
const MaxComposerVersion = nativecodec.MaxCursorComposerVersion

// ValidateSettledMetadata preserves the fixed native metadata validation.
func ValidateSettledMetadata(ctx context.Context, key string, raw []byte) error {
	return nativecodec.ValidateSettledCursorMetadata(ctx, key, raw)
}

// DecodeComposerData derives the exact metadata used by fixed capture.
func DecodeComposerData(key string, raw []byte) (ComposerMetadata, bool) {
	decoded, ok := nativecodec.DecodeCursorComposerData(key, raw)
	return ComposerMetadata{Chat: decoded.Chat, Counted: decoded.Counted, Subagents: decoded.Subagents}, ok
}

// FilterComposerRecordsBounded preserves the native filter's owner and close lifetime.
func FilterComposerRecordsBounded(ctx context.Context, raw []byte, next func(context.Context) (Bubble, bool, error), limit int64, owner Reservation) (archive.FilteredTranscript, func(), error) {
	return nativecodec.FilterComposerRecordsBounded(ctx, raw, func(ctx context.Context) (nativecodec.CursorBubble, bool, error) {
		row, ok, err := next(ctx)
		return nativecodec.CursorBubble{ID: row.ID, Value: row.Value}, ok, err
	}, limit, owner)
}

// NewSourceBundle uses only the fixed Cursor adapter and no supplemental evidence.
func NewSourceBundle(reg archive.SessionRegistration, filtered archive.FilteredTranscript, at time.Time) (archive.SourceBundle, error) {
	return archive.NewSourceBundle(reg, nativecodec.CursorAdapter{}, filtered, at, nil)
}
