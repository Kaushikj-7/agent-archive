package agentapi

import (
	"context"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// NativeLocatorLookup supplies hints for already admitted owners. It cannot
// register owners or alter consent. Nil means unavailable. Check revalidates an
// exact token scoped to agent, root/revision, native ID, identities and coverage,
// both after opened reads and before relying on signature equality.
type NativeLocatorLookup interface {
	Locate(context.Context, LocatorRequest) (LocatorResult, error)
	Check(context.Context, string, string) error
}

// LocatorRequest scopes lookup to the committed root. Observed only locates
// candidates; it is never original creation evidence. Parent must be validated.
type LocatorRequest struct {
	NativeID string
	Root     archive.NativeRoot
	Parent   *SourceRef
	Observed time.Time
}

// LocatorStatus distinguishes complete uniqueness from incomplete observations.
type LocatorStatus string

const (
	// LocatorFound reports completed uniqueness or qualified current authority.
	LocatorFound LocatorStatus = "found"
	// LocatorPending reports incomplete bounded evidence.
	LocatorPending LocatorStatus = "pending"
	// LocatorAmbiguous reports conflicting candidate identities.
	LocatorAmbiguous LocatorStatus = "ambiguous"
	// LocatorRefused reports evidence that cannot safely supply a locator.
	LocatorRefused LocatorStatus = "refused"
)

// LocatorResult contains untrusted hints. Found requires completed scoped
// uniqueness or a separately qualified native current pointer. Zero is unavailable.
// An unqualified current pointer remains nil. Hints need same-handle admission.
type LocatorResult struct {
	Status     LocatorStatus
	Current    *SourceRef
	Candidates []SourceRef
	Revision   string
}

// SourceBinding is a lossless in-memory tagged union. Exactly one member is
// populated; the registration agent selects it. Persisted Codex shape is unchanged.
type SourceBinding struct {
	Codex  *archive.CodexSourceBinding
	Native *archive.NativeSourceBinding
}

// ValidateFor rejects ambiguous and cross-agent source authority.
func (b *SourceBinding) ValidateFor(agent string) error {
	if b == nil {
		return nil
	}
	if b.Codex != nil && b.Native == nil && agent == "codex" {
		return b.Codex.Validate()
	}
	if b.Native != nil && b.Codex == nil && agent == b.Native.Agent {
		return b.Native.Validate()
	}
	return ErrInvalidSourceBinding
}

// ErrInvalidSourceBinding refuses absent, ambiguous or cross-agent tagged facts.
var ErrInvalidSourceBinding = errors.New("invalid source binding tag")

// CodexBinding returns the Codex variant, or nil for an absent carrier. Callers
// accepting external authority must ValidateFor before interpreting absence.
func (b *SourceBinding) CodexBinding() *archive.CodexSourceBinding {
	if b == nil {
		return nil
	}
	return b.Codex
}

// BindCodex retains all Codex facts without flattening or serializing them.
func BindCodex(b *archive.CodexSourceBinding) *SourceBinding {
	if b == nil {
		return nil
	}
	return &SourceBinding{Codex: b}
}
