package collector

import (
	"errors"

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
