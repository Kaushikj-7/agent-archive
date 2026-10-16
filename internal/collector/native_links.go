package collector

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Native links are derived from this pass's registration inventory. They do
// not enumerate native files or read published transcript state. Missing links
// never prevent independently admitted children from publishing.
type nativeLinkKey struct {
	native      string
	project     string
	root        string
	destination string
	home        string
}

func nativeRegistrationHome(reg archive.SessionRegistration) string {
	if reg.CodexBinding != nil && reg.CodexBinding.Home != "" {
		return reg.CodexBinding.Home
	}
	if reg.NativeSourceHome != "" {
		return reg.NativeSourceHome
	}
	return reg.DiscoveryRoot
}

func nativeParentKey(reg archive.SessionRegistration, native string) nativeLinkKey {
	return nativeLinkKey{native, reg.ProjectID, reg.ProjectRoot, reg.DestinationID, nativeRegistrationHome(reg)}
}

func (p *pass) reconcileNativeLinks() {
	parents := make(map[nativeLinkKey]archive.SessionRegistration, len(p.registrations))
	ambiguous := map[nativeLinkKey]bool{}
	for _, reg := range p.registrations {
		if reg.Harness.Name != "codex" || nativeRegistrationHome(reg) == "" || p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		key := nativeParentKey(reg, reg.NativeSessionID)
		if prior, ok := parents[key]; ok && prior.ArchiveSessionID != reg.ArchiveSessionID {
			ambiguous[key] = true
		}
		parents[key] = reg
	}
	for i, reg := range p.registrations {
		p.reconcileNativeLink(i, reg, parents, ambiguous)
	}

}

func (p *pass) nativeLinkPending(reason string) {
	p.result.PendingNativeLinks++
	if p.result.NativeLinkPendingReasons == nil {
		p.result.NativeLinkPendingReasons = map[string]int{}
	}
	p.result.NativeLinkPendingReasons[reason]++
}

func (p *pass) markLegacyNativeComposite(parent, child archive.SessionRegistration) bool {
	oldID, known, err := p.local.LegacyCodexCompositeReservation(parent.NativeSessionID, child.NativeSessionID)
	if err != nil {
		p.nativeLinkPending("legacy_link_lookup_pending")
		return false
	}
	if !known || oldID == child.ArchiveSessionID {
		return true
	}

	marker, err := archive.NewLinkedSessionEvidence(oldID, archive.LinkedSessionUnavailable, p.now)
	if err != nil {
		p.nativeLinkPending("legacy_link_lookup_pending")
		return false
	}
	marker.Provenance = archive.NativeLegacyUnverifiedLinkProvenance
	gap, err := archive.NewCaptureGapEvidence("native_child_link_unverified", "Legacy composite link lacked independent native child admission", archive.NativeLegacyUnverifiedLinkProvenance, p.now)
	if err != nil {
		p.nativeLinkPending("legacy_link_lookup_pending")
		return false
	}
	if err := p.local.SaveRequest(parent.ArchiveSessionID, "native-legacy-link-reconciled", p.now, marker, gap); err != nil {
		p.nativeLinkPending("legacy_link_lookup_pending")
		return false
	}
	return true
}

func (p *pass) reconcileNativeLink(i int, reg archive.SessionRegistration, parents map[nativeLinkKey]archive.SessionRegistration, ambiguous map[nativeLinkKey]bool) {

	if !reg.NativeChild || reg.ParentSessionID != "" && reg.NativeLinkVersion >= 1 {
		return
	}
	// An admitted deletion freezes the source and relationship owner until
	// exact restoration/cleanup removes its intent. Linking first would make the
	// current registration unable to resume its own removal.
	if _, found, err := p.local.LoadSessionDeletion(reg); found || err != nil {
		p.nativeLinkPending("session_removal_pending")
		return
	}
	if reg.ParentNativeSessionID == "" {
		p.nativeLinkPending("native_parent_not_recorded")
		return
	}
	if nativeRegistrationHome(reg) == "" {
		p.nativeLinkPending("source_home_pending")
		return
	}
	key := nativeParentKey(reg, reg.ParentNativeSessionID)
	parent, found := parents[key]
	if !found {
		p.nativeLinkPending("parent_registration_pending")
		return
	}
	if ambiguous[key] || parent.ArchiveSessionID == reg.ArchiveSessionID {
		p.nativeLinkPending("parent_identity_conflict")
		return
	}
	if reg.ParentSessionID != "" && reg.ParentSessionID != parent.ArchiveSessionID {
		p.nativeLinkPending("parent_identity_conflict")
		return
	}
	// Removal records are authoritative even during a partial retention/undo.
	if _, removed, err := p.local.Removal("codex", parent.NativeSessionID); err != nil {
		p.nativeLinkPending("relationship_state_retry")
		return
	} else if removed {
		p.nativeLinkPending("parent_removed")
		return
	}
	legacyChecked := p.markLegacyNativeComposite(parent, reg)
	if reg.ParentSessionID == parent.ArchiveSessionID && !legacyChecked {
		return
	}
	// Queue before marking reconciliation complete. A crash or failed request
	// write must leave a retryable relationship, even when the child is settled.
	if err := p.local.SaveRequest(reg.ArchiveSessionID, "native-parent-linked", p.now); err != nil {
		p.nativeLinkPending("relationship_state_retry")
		return
	}
	found, err := p.local.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
		if !current.NativeChild || current.ParentNativeSessionID != reg.ParentNativeSessionID || nativeParentKey(*current, current.ParentNativeSessionID) != key {
			return errors.New("native relationship changed")
		}
		if current.ParentSessionID != "" && current.ParentSessionID != parent.ArchiveSessionID {
			return errors.New("native parent link conflicts")
		}
		current.ParentSessionID = parent.ArchiveSessionID
		if legacyChecked {
			current.NativeLinkVersion = 1
		}
		return nil
	})
	if err != nil {
		p.nativeLinkPending("relationship_state_retry")
		return
	}
	if !found {
		return
	}
	reg.ParentSessionID = parent.ArchiveSessionID
	if legacyChecked {
		reg.NativeLinkVersion = 1
	}
	p.registrations[i] = reg
}
