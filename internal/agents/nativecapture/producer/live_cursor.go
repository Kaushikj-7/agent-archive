package producer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/capturecodec"
	"github.com/wangjohn/agent-archive/internal/agents/importconfig"
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture/internal/sealed"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// CaptureLiveCursor performs a fixed bounded live-image read before sealing.
// Only this backend may mint a Proof; public request values alone never authorize.
func CaptureLiveCursor(liveRequest *nativefacts.LiveCaptureRequest) (_ *sealed.LiveProof, err error) {
	liveLease, err := liveRequest.Borrow()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, liveLease.Close()) }()
	request := liveLease.Original()
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
	ctx, budget := liveLease.Context(), lease.Budget()
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
	composer, err := cursorstore.ReadLiveAdmission(ctx, database, reg.SourceKey, 128<<20, archive.MaxRecordBytes, b, liveLease, func(row agentapi.DatabaseRecord) error {
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
	// Cleanup is part of successful capture, before any proof escapes or stage
	// writer runs. A failure leaves the durable receipt and returns no proof.
	if err = liveLease.Close(); err != nil {
		return nil, err
	}
	if err = errors.Join(lease.Check(), ctx.Err()); err != nil {
		return nil, err
	}
	keep = true
	keepLease = true
	return sealed.NewLive(reg, bundle, nativefacts.SettledImportFacts{Chat: selected, GlobalSHA256: hex.EncodeToString(global.Sum(nil))}, digest, owner, b.charged, request, lease, budget), nil
}
