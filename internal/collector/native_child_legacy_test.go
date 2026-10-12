package collector

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
)

func TestLegacyNativeChildOwnershipNeedsExactPersistedOwner(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	reg := scan.reg
	binding := *reg.CodexBinding
	binding.Child, binding.RootID = true, ""
	binding.ParentID = "44444444-4444-4444-8444-444444444444"
	reg.CodexBinding, reg.NativeChild = &binding, true
	reg.ParentNativeSessionID, reg.NativeSourceHome = binding.ParentID, binding.Home
	bundle := p.Bundle
	bundle.NativeChild, bundle.ParentSessionID = false, ""
	reg.ParentSessionID = "qualified-parent"
	if !nativeChildMarkerPending(reg, bundle) || !nativeParentResolved(reg, bundle) {
		t.Fatal("matching persisted legacy owner rejected")
	}
	for _, name := range []legacyOwnerCase{legacyUnbound, legacyOrdinary, legacyVersion, legacyNative, legacyArchive, legacyProject, legacyHarness, legacyHome, legacyParent, legacyRoot} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			candidate, source, facts := reg, bundle, binding
			candidate.CodexBinding = &facts
			switch name {
			case legacyUnbound:
				candidate.CodexBinding = nil
			case legacyOrdinary:
				facts.Child = false
			case legacyVersion:
				facts.Version = 2
			case legacyNative:
				source.NativeSessionID = "55555555-5555-4555-8555-555555555555"
			case legacyArchive:
				source.ArchiveSessionID = "different-owner"
			case legacyProject:
				source.ProjectID = "different-project"
			case legacyHarness:
				source.Capture.Harness.Name = "claude"
			case legacyHome:
				candidate.NativeSourceHome = filepath.Join(binding.Home, "other")
			case legacyParent:
				candidate.ParentNativeSessionID = "55555555-5555-4555-8555-555555555555"
			case legacyRoot:
				facts.RootID = "55555555-5555-4555-8555-555555555555"
			}
			if nativeChildMarkerPending(candidate, source) || retainedParentMatches(candidate, source) {
				t.Fatal("unproven legacy owner relaxed source parent", name)
			}
		})
	}
	bundle.ParentSessionID = "conflicting-known-parent"
	if retainedParentMatches(reg, bundle) {
		t.Fatal("binding overwrote a different known source parent")
	}
}

func TestLegacyNativeChildMigrationRefusesChangedAuthorityWithoutWrites(t *testing.T) {
	t.Parallel()
	for _, name := range []legacyAuthorityCase{legacyAuthorityProject, legacyAuthorityDestination, legacyAuthorityHome, legacyAuthorityVersion} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			scan, p := privacyJournal(t)
			if outcome, err := scan.publishPending(scan.ctx, p); err != nil || outcome != outcomePublished {
				t.Fatal(outcome, err)
			}
			facts := *scan.reg.CodexBinding
			facts.Child, facts.RootID = true, ""
			scan.reg.CodexBinding = &facts
			if err := scan.local.SaveRegistration(scan.reg); err != nil {
				t.Fatal(err)
			}
			switch name {
			case legacyAuthorityProject:
				scan.reg.ProjectID = "another-project"
			case legacyAuthorityDestination:
				scan.reg.DestinationID = "another-destination"
			case legacyAuthorityHome:
				scan.reg.NativeSourceHome = filepath.Join(facts.Home, "other")
			case legacyAuthorityVersion:
				facts.Version = 2
			}
			before := snapshotMtimes(t, scan.local.Home())
			if _, err := scan.run(scan.ctx); err == nil {
				t.Fatal("changed authority allowed legacy migration", name)
			}
			if !reflect.DeepEqual(before, snapshotMtimes(t, scan.local.Home())) {
				t.Fatal("refusal changed local authority or journal", name)
			}
		})
	}
}

// A pre-marker pending bundle keeps its own target across a later parent link.
// Retained preparation uses source bytes alone even when native inputs vanish.
func TestLegacyNativeChildPreparationFreezesParentAndDoesNotReadNative(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	facts := *scan.reg.CodexBinding
	facts.Child, facts.RootID = true, ""
	scan.reg.CodexBinding, scan.reg.NativeChild, scan.reg.NativeSourceHome = &facts, true, facts.Home
	scan.reg.ParentSessionID = "later-parent"
	p.History.Preparing, p.History.PrivacyCursor = true, 0
	p.Bundle.NativeChild, p.Bundle.ParentSessionID = false, ""
	for i := range p.History.Inputs {
		p.History.Inputs[i].ParentSessionID = nil
	}
	paths, err := filepath.Glob(filepath.Join(facts.Home, "rollout-*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatal("native fixture inputs missing", err)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.local.SavePending(scan.id(), p); err != nil {
		t.Fatal(err)
	}
	scan.local, err = state.Open(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	p, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found {
		t.Fatal(err)
	}
	scan.opts.CodexRollouts = nil
	for p.History.Preparing {
		if err := scan.advanceHistoryPreparation(&p); err != nil {
			t.Fatal("legacy preparation target changed", err)
		}
	}
	if !p.Bundle.NativeChild || p.Bundle.ParentSessionID != "" {
		t.Fatal("legacy output adopted later parent", p.Bundle.ParentSessionID)
	}
}

type legacyOwnerCase string

const (
	legacyUnbound  legacyOwnerCase = "unbound"
	legacyOrdinary legacyOwnerCase = "ordinary"
	legacyVersion  legacyOwnerCase = "version"
	legacyNative   legacyOwnerCase = "native"
	legacyArchive  legacyOwnerCase = "archive"
	legacyProject  legacyOwnerCase = "project"
	legacyHarness  legacyOwnerCase = "harness"
	legacyHome     legacyOwnerCase = "home"
	legacyParent   legacyOwnerCase = "parent"
	legacyRoot     legacyOwnerCase = "root"
)

type legacyAuthorityCase string

const (
	legacyAuthorityProject     legacyAuthorityCase = "project"
	legacyAuthorityDestination legacyAuthorityCase = "destination"
	legacyAuthorityHome        legacyAuthorityCase = "home"
	legacyAuthorityVersion     legacyAuthorityCase = "unsupported_binding"
)
