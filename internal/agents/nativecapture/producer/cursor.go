// Package producer performs the fixed SQL capture before sealing evidence.
package producer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"path/filepath"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/capturecodec"
	"github.com/wangjohn/agent-archive/internal/agents/importconfig"
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture/internal/sealed"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

type settledReadBudget struct {
	owner   *sealed.Reservation
	rows    int
	charged int64
}

func (b *settledReadBudget) RemainingRows() int { return b.rows }

func (b *settledReadBudget) RemainingBytes() int64 { return b.owner.Available() }

func (b *settledReadBudget) Charge(rows int, bytes int64) error {
	if rows < 0 || rows > b.rows || bytes < 0 || !b.owner.Reserve(bytes) {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	b.rows -= rows
	b.charged += bytes
	return nil
}

// CaptureSettledCursor performs complete fixed settled I/O and deterministic derivation.
// Only this backend may mint a Proof; public request values alone never authorize.
func CaptureSettledCursor(request *nativefacts.CaptureRequest) (_ *sealed.Proof, err error) {
	lease, err := request.Borrow()
	if err != nil {
		return nil, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			lease.Close()
		}
	}()
	ctx, budget := lease.Context(), lease.Budget()
	reg, err := lease.Registration()
	if err != nil {
		return nil, err
	}
	reviewed, database, at := lease.Reviewed(), lease.Locator(), lease.At()
	if lease.Policy().FilterVersion != archive.FilterVersion || !supportedCaptureSkill(lease.Policy().SkillEvidence) {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("capture policy changed"))
	}

	if unsupportedSettledCaptureOwner(reg, budget) {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("unsupported settled Cursor owner"))
	}
	owner := sealed.NewReservation(lease.Reservation(), 128<<20)
	b := &settledReadBudget{owner: owner, rows: 65536}
	keep := false
	defer func() {
		if !keep {
			owner.Release(b.charged)
		}
	}()
	var selected agentapi.DatabaseChat
	global := sha256.New()
	composer, err := cursorstore.ReadSettledAdmission(ctx, database, reg.SourceKey, 128<<20, archive.MaxRecordBytes, b, func(row agentapi.DatabaseRecord) error {
		return inspectSettledCaptureRow(ctx, row, reg, owner, global, &selected)
	})
	if err != nil {
		return nil, err
	}
	if settledCaptureRoutingChanged(selected, reviewed, reg) {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("settled Cursor review or routing changed"))
	}
	// Keep raw/filter/bundle simultaneous views charged under the captured owner.
	rawExtent := int64(len(composer.Composer))
	for _, bubble := range composer.Bubbles {
		if int64(len(bubble.Value)) > (128<<20)-rawExtent {
			return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
		}
		rawExtent += int64(len(bubble.Value))
	}
	scratch := 2*rawExtent + 32<<20
	if !owner.Reserve(scratch) {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	b.charged += scratch
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(cursorstore.NotChecked(cursorstore.UnknownFormat), err)
	}
	next := 0
	filtered, closeFiltered, err := capturecodec.FilterComposerRecordsBounded(ctx, composer.Composer, func(ctx context.Context) (capturecodec.Bubble, bool, error) {
		if err := ctx.Err(); err != nil {
			return capturecodec.Bubble{}, false, err
		}
		if next >= len(composer.Bubbles) {
			return capturecodec.Bubble{}, false, nil
		}
		row := composer.Bubbles[next]
		next++
		return capturecodec.Bubble{ID: row.ID, Value: row.Value}, true, nil
	}, 32<<20, owner)
	defer closeFiltered()
	if err != nil {
		return nil, err
	}
	if filtered.Boundary.RetainedBytes < 0 || filtered.Boundary.RetainedBytes > 32<<20 || filtered.LocalIdentity.ID != "" && filtered.LocalIdentity.ID != reg.NativeSessionID {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	inspection, err := importconfig.Cursor().InspectImport(ctx, agentapi.ImportInspectionRequest{Session: agentapi.NativeSession{Agent: agentmeta.ID(reg.Harness.Name), NativeID: reg.NativeSessionID}, Source: agentapi.SourceRef{Kind: reg.SourceKind, Key: reg.SourceKey}, Header: agentapi.NativeHeader{NativeID: reg.NativeSessionID, StartedAt: reg.SessionStartedAt}, Filtered: filtered})
	if err != nil {
		return nil, err
	}
	if inspection.IdentityMismatch || !inspection.Conversation {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("settled Cursor conversation changed"))
	}
	bundle, err := capturecodec.NewSourceBundle(reg, filtered, at)
	if err != nil {
		return nil, err
	}
	digest, err := sealed.Digest(ctx, owner, bundle, 32<<20)
	if err != nil {
		return nil, err
	}
	if err = lease.Check(); err != nil {
		return nil, err
	}
	keep = true
	keepLease = true
	return sealed.New(reg, bundle, nativefacts.SettledImportFacts{Chat: selected, GlobalSHA256: hex.EncodeToString(global.Sum(nil))}, digest, owner, b.charged, request, lease, budget), nil
}

func settledCaptureRoutingChanged(selected, reviewed agentapi.DatabaseChat, reg archive.SessionRegistration) bool {
	return selected != reviewed || selected.CreatedAt.IsZero() || !selected.CreatedAt.Equal(reg.SessionStartedAt) || selected.Folder == "" || filepath.Clean(selected.Folder) != filepath.Clean(reg.ProjectRoot)
}

func unsupportedSettledCaptureOwner(reg archive.SessionRegistration, budget *agentapi.NativeReadBudget) bool {
	return budget == nil || reg.Harness.Name != archive.HarnessCursor || reg.SourceKind != archive.SourceKindCursorSQLite || reg.SourceKey != reg.NativeSessionID || reg.HistoricalProject != nil || reg.ParentSessionID != "" || reg.NativeChild || reg.ParentNativeSessionID != "" || reg.NativeRootSessionID != "" || reg.NativeSourceHome != "" || reg.PreviousGenerationID != "" || reg.CaptureFrozen || reg.CodexBinding != nil || reg.CodexAdmission != nil
}

func inspectSettledCaptureRow(ctx context.Context, row agentapi.DatabaseRecord, reg archive.SessionRegistration, owner *sealed.Reservation, global hash.Hash, selected *agentapi.DatabaseChat) error {
	// The same owner reserves bounded JSON maps and sorting scratch before decode.
	scratch := 2*int64(len(row.Value)) + 32<<10
	if !owner.Reserve(scratch) {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	defer owner.Release(scratch)
	if err := capturecodec.ValidateSettledMetadata(ctx, row.Key, row.Value); err != nil {
		return err
	}
	decoded, ok := capturecodec.DecodeComposerData(row.Key, row.Value)
	if !ok || decoded.Chat.Malformed || decoded.Chat.KeyID == "" || len(decoded.Chat.KeyID) > 255 {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	facts := decoded.Chat.CursorFacts
	if facts.VersionPresent && (facts.Version < 1 || facts.Version > capturecodec.MaxComposerVersion) || facts.Relationships == agentapi.CursorRelationshipsMalformed {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	if slices.Contains(decoded.Subagents, reg.SourceKey) {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	// Ordered record hashes preserve the complete observed global routing set.
	sum := sha256.Sum256(row.Value)
	keySum := sha256.Sum256([]byte(row.Key))
	global.Write(keySum[:])
	global.Write(sum[:])
	if decoded.Chat.KeyID == reg.SourceKey {
		if !decoded.Counted || decoded.Chat.Malformed || decoded.Chat.ID != reg.NativeSessionID {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		*selected = decoded.Chat
	}
	return nil
}

func supportedCaptureSkill(mode nativefacts.CaptureSkillMode) bool {
	switch mode {
	case nativefacts.CaptureSkillNone, nativefacts.CaptureSkillMetadata, nativefacts.CaptureSkillBody:
		return true
	default:
		return false
	}
}
