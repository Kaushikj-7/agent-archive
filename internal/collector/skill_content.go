package collector

import (
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func (s *sessionScan) replaceSkillTranscript(read *sourceRead, out archive.FilteredTranscript) {
	read.filtered = out
	if read.filterLease == nil {
		return
	}
	if s.filtered != nil && s.filtered.filterLease != nil && *s.filtered.filterLease == *read.filterLease {
		s.filtered.transcript, s.filtered.observed = out, read.observed
	}
	if s.lastRead != nil && s.lastRead != read && s.lastRead.filterLease != nil && *s.lastRead.filterLease == *read.filterLease {
		s.lastRead.filtered, s.lastRead.observed = out, read.observed
	}
}

func (s *sessionScan) restoreSkillContent(read *sourceRead, prior archive.SourceBundle) error {
	if read.observed.skillContent == nil || read.observed.skillContentRestored {
		return nil
	}
	if prior.SchemaVersion != 0 && (prior.ArchiveSessionID != s.reg.ArchiveSessionID || prior.ProjectID != s.reg.ProjectID || prior.NativeSessionID != s.reg.NativeSessionID) {
		return errors.New("skill content baseline owner differs")
	}
	out, err := read.observed.skillContent.Restore(s.ctx, prior)
	if err != nil {
		return err
	}
	read.observed.skillContentRestored = true
	s.replaceSkillTranscript(read, out)
	return nil
}

func (s *sessionScan) finalizeSkillContent(read *sourceRead, digests []agentapi.SkillContentDigest) error {
	if read.observed.skillContent == nil || read.observed.skillContentFinalized {
		return nil
	}
	out, err := read.observed.skillContent.Finalize(s.ctx, digests)
	if err != nil {
		return err
	}
	read.observed.skillContentFinalized = true
	s.replaceSkillTranscript(read, out)
	return nil
}

func (s *sessionScan) observeSkillContent() (agentapi.SkillContentObservation, func(), error) {
	if s.opts.SupplementalSkillContent != nil && !s.reg.Imported() && s.opts.skillEvidence() != config.SkillEvidenceNone {
		return s.opts.SupplementalSkillContent(s.ctx, s.reg, s.now, s.readBudget())
	}
	if s.opts.SupplementalEvidence == nil {
		return agentapi.SkillContentObservation{}, nil, nil
	}
	items, err := s.opts.SupplementalEvidence(s.reg, s.now)
	return agentapi.SkillContentObservation{Evidence: items}, nil, err
}

// skillActivityProbe owns its decoded candidate and index loan. Only an
// inactive funded candidate may escape; active probes end before observation.
func (s *sessionScan) skillActivityProbe(read *sourceRead, cached archive.SourceBundle, haveCached bool, supplemental []archive.SupplementalEvidence) (archive.SourceBundle, bool, error) {
	// now is a placeholder here; bundleEvidenceEqual ignores CapturedAt, so
	// it has no effect on the comparison. The real value is assigned once it
	// is known whether this is genuinely new evidence.
	candidateOwner := len(s.retainedReleases)
	candidate, err := s.newSourceBundle(s.reg, read.adapter, read.filtered, s.now, supplemental)
	if err != nil {
		return archive.SourceBundle{}, false, fmt.Errorf("build source bundle: %w", err)
	}

	// Observe the filesystem only with session activity. An unrelated skill edit
	// must not refresh every historical session or extend its retention lifetime.
	nativeChanged := !nativeEvidenceExtends(read.adapter, cached, candidate) || !nativeEvidenceExtends(read.adapter, candidate, cached)
	active := !haveCached || s.req.Token != "" || (nativeChanged && !bundleChangeIsNamingOnly(read.adapter, cached, candidate))
	if signature, found, _ := s.local.LoadScanSignature(s.id()); found && pendingSkillMode(signature.SkillEvidence) != s.opts.skillEvidence() {
		active = true
	}
	if (s.opts.SupplementalEvidence == nil && s.opts.SupplementalSkillContent == nil) || !active {
		return candidate, false, nil
	}
	// The filtered input still owns history/ordinal/text aliases required by
	// the final build; this decoded probe and its independent loan end here.
	s.releaseRetainedIndex(candidateOwner)
	return archive.SourceBundle{}, true, nil
}
