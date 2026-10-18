package collector

import "github.com/wangjohn/agent-archive/internal/archive"

// build assembles the candidate bundle, and returns the supplemental
// evidence it was built with. A recoverable block the readable
// source has ended is cleared first, since a source that comes back carrying
// exactly what was already captured produces no change to act on.
func (s *sessionScan) build(read sourceRead) (archive.SourceBundle, []archive.SupplementalEvidence, error) {
	return s.buildRead(&read)
}
