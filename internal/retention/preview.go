package retention

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"time"
)

// ExpiryPreview shares the sweeper's local age and pending-work decision.
// It is read-only: remote evidence may only postpone the predicted expiry,
// and no clock correction, publication, or deletion is performed.
type ExpiryPreview struct{ sweeper sweeper }

// NewExpiryPreview snapshots pending requests for a read-only preview at now.
// Publishable controls whether pending work defers expiry, as in the sweeper.
func NewExpiryPreview(local *state.Store, now time.Time, publishable func(archive.SessionRegistration) bool) (*ExpiryPreview, error) {
	reqs, err := local.LoadRequests()
	if err != nil {
		return nil, err
	}
	return &ExpiryPreview{sweeper: sweeper{
		local: local, now: now, opts: Options{Publishable: publishable}, requested: state.QueuedRequests(reqs),
		// Preview cannot confirm the server clock or correct a future capture.
		// Keep its age unchanged; only the actual sweep may probe and clamp it.
		clock: clockVerdict{checked: true, held: ErrClockUnverified},
	}}, nil
}

// SessionExpired reports local expiry eligibility under maxAge; zero disables it.
// Remote evidence and the sweeper's clock checks may still postpone deletion.
func (p *ExpiryPreview) SessionExpired(reg archive.SessionRegistration, maxAge time.Duration) (bool, error) {
	s := &p.sweeper
	s.opts.SessionMaxAge = maxAge
	summary, found, err := s.local.LoadPublishedSummary(reg.ArchiveSessionID)
	if err != nil {
		return false, err
	}
	age, err := s.sessionAge(reg, summary, found)
	if err != nil {
		return false, err
	}
	expired, _, _, err := s.sessionExpiry(reg, found, age)
	return expired, err
}
