package state

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
)

// PublicationNativeTarget freezes an admitted child's intended retained header.
// A target marker alone is never evidence that an original source is a child.
type PublicationNativeTarget struct {
	ParentSessionID       string                      `json:"parent_session_id"`
	NativeChild           bool                        `json:"native_child"`
	Binding               *archive.CodexSourceBinding `json:"binding,omitempty"`
	ParentNativeSessionID string                      `json:"parent_native_session_id,omitempty"`
	NativeRootSessionID   string                      `json:"native_root_session_id,omitempty"`
	NativeSourceHome      string                      `json:"native_source_home,omitempty"`
}

// FreezePublicationNativeTarget snapshots only the existing admitted registration.
// Its facts are checked against decoded retained headers by the transform factory.
// A legacy absent marker may be upgraded only through those admitted-owner checks;
// the original parent target stays frozen even if the registration later resolves it.
func FreezePublicationNativeTarget(ctx context.Context, p *PendingPublication, reg archive.SessionRegistration, budget *agentapi.NativeReadBudget) (func(), error) {
	owner := budget.Reservation()
	if p.Preparation != nil || p.Commit != nil || p.History == nil || !reg.NativeChild {
		return func() {}, nil
	}
	if budget == nil || p.Bundle.ArchiveSessionID != reg.ArchiveSessionID || p.Bundle.NativeSessionID != reg.NativeSessionID || p.Bundle.ProjectID != reg.ProjectID || p.Bundle.Capture.Harness.Name != reg.Harness.Name {
		return nil, ErrDurableStorageRecovery
	}
	target := PublicationNativeTarget{ParentSessionID: p.Bundle.ParentSessionID, NativeChild: reg.NativeChild, Binding: reg.CodexBinding, ParentNativeSessionID: reg.ParentNativeSessionID, NativeRootSessionID: reg.NativeRootSessionID, NativeSourceHome: reg.NativeSourceHome}
	if target.Binding != nil && target.Binding.Validate() != nil {
		target.Binding = nil
	}
	if target.validate() != nil {
		return nil, ErrDurableStorageRecovery
	}
	const scratch = 32 << 10
	if !owner.Reserve(scratch) {
		return nil, errStateBudget
	}
	n, err := jsonwire.Bound(ctx, target, min(int64(64<<10), budget.Available()/2))
	owner.Release(scratch)
	if err != nil {
		return nil, err
	}
	if !owner.Reserve(2 * n) {
		return nil, errStateBudget
	}
	raw, err := json.Marshal(target)
	if err == nil {
		err = ctx.Err()
	}
	var owned PublicationNativeTarget
	if err == nil {
		err = closedPublicationDecode(raw, &owned)
	}
	owner.Release(n)
	if err != nil {
		owner.Release(n)
		return nil, err
	}
	p.nativeTarget = &owned
	var once sync.Once
	return func() { once.Do(func() { owner.Release(n) }) }, nil
}

func (t PublicationNativeTarget) validate() error {
	if len(t.ParentSessionID) > 4096 || len(t.ParentNativeSessionID) > 4096 || len(t.NativeRootSessionID) > 4096 || len(t.NativeSourceHome) > 4096 || t.Binding != nil && t.Binding.Validate() != nil {
		return ErrDurableStorageRecovery
	}
	return nil
}

func (t PublicationNativeTarget) registration(reg archive.SessionRegistration) archive.SessionRegistration {
	reg.ParentSessionID = t.ParentSessionID
	reg.NativeChild = t.NativeChild
	reg.CodexBinding = t.Binding
	reg.ParentNativeSessionID = t.ParentNativeSessionID
	reg.NativeRootSessionID = t.NativeRootSessionID
	reg.NativeSourceHome = t.NativeSourceHome
	return reg
}

func nativeTargetDigest(target *PublicationNativeTarget) string {
	if target == nil {
		return ""
	}
	raw, _ := json.Marshal(target)
	return publicationSHA256(append([]byte("publication-native-target/v1\x00"), raw...))
}

func nativeTargetSHA(ctx context.Context, target *PublicationNativeTarget, budget *agentapi.NativeReadBudget) (string, error) {
	owner := budget.Reservation()
	if target == nil {
		return "", nil
	}
	const scratch = 32 << 10
	if !owner.Reserve(scratch) {
		return "", errStateBudget
	}
	n, err := jsonwire.Bound(ctx, target, min(int64(64<<10), budget.Available()/2))
	owner.Release(scratch)
	if err != nil {
		return "", err
	}
	if !owner.Reserve(2 * n) {
		return "", errStateBudget
	}
	defer owner.Release(2 * n)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return nativeTargetDigest(target), nil
}
