package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/local"
)

// WithReadBudget borrows the pass ledger for a session's local serialized and
// decoded inputs. The returned close ends ownership of that session's data;
// no Published or Pending value read through the handle may outlive it.
func (s *Store) WithReadBudget(ctx context.Context, budget *agentapi.NativeReadBudget) (*Store, func()) {
	scoped := *s
	scoped.resourceBudget = budget
	scoped.resourceContext = ctx
	scoped.resourceReleases = &[]func(){}
	var once sync.Once
	return &scoped, func() {
		once.Do(func() {
			for _, release := range *scoped.resourceReleases {
				release()
			}
			*scoped.resourceReleases = nil
		})
	}
}

var errStateBudget = agentapi.ReadBudgetLimit(errors.New("local state exceeds shared data budget"))

// errStateFileUnstable refuses a local state file that is not a regular file
// or grew while it was read. Unlike errStateBudget it is no memory refusal,
// so a pending publication that meets it still requires recovery.
var errStateFileUnstable = errors.New("local state file is not a stable regular file")

func (s *Store) readBudgeted(path string, value any, retain bool) error {
	if s.resourceBudget == nil {
		return local.Read(path, value)
	}
	if err := s.resourceContext.Err(); err != nil {
		return err
	}
	owner, err := os.OpenRoot(s.home)
	if err != nil {
		return err
	}
	defer func() { _ = owner.Close() }()
	rel, err := filepath.Rel(s.home, path)
	if err != nil {
		return err
	}
	f, err := owner.Open(rel)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	n := info.Size()
	if !info.Mode().IsRegular() || n < 0 {
		return errStateFileUnstable
	}
	// Serialized input and decoded logical data coexist during Unmarshal.
	if !s.resourceBudget.Reserve(n) {
		return errStateBudget
	}
	defer s.resourceBudget.Release(n)
	if !s.resourceBudget.Reserve(n) {
		return errStateBudget
	}
	keep := false
	defer func() {
		if !keep {
			s.resourceBudget.Release(n)
		}
	}()
	data := make([]byte, n)
	if _, err := io.ReadFull(f, data); err != nil {
		return err
	}
	var extra [1]byte
	if count, err := f.Read(extra[:]); count != 0 || !errors.Is(err, io.EOF) {
		return errStateFileUnstable
	}
	if err := s.resourceContext.Err(); err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return err
	}
	if retain {
		keep = true
		*s.resourceReleases = append(*s.resourceReleases, func() { s.resourceBudget.Release(n) })
	}
	return nil
}

func (s *Store) writeCompact(path string, value any) error {
	if s.resourceBudget == nil {
		return local.WriteCompact(path, value)
	}
	const scratch = 32 << 10
	if !s.resourceBudget.Reserve(scratch) {
		return errStateBudget
	}
	available := s.resourceBudget.Available()
	n, err := jsonwire.Bound(s.resourceContext, value, available-1)
	if errors.Is(err, jsonwire.ErrLimit) {
		s.resourceBudget.Refuse(available + 1)
	}
	s.resourceBudget.Release(scratch)
	if err != nil {
		return errors.Join(errStateBudget, err)
	}
	if !s.resourceBudget.Reserve(n + 1) {
		return errStateBudget
	}
	defer s.resourceBudget.Release(n + 1)
	return local.WriteCompact(path, value)
}

// unmarshalOwned charges an additional decoded view of bytes already owned by
// this scope. Returned metadata has an independent lifetime from its wire input.
func (s *Store) unmarshalOwned(data []byte, value any) error {
	if s.resourceBudget == nil {
		return json.Unmarshal(data, value)
	}
	if err := s.resourceContext.Err(); err != nil {
		return err
	}
	n := int64(len(data))
	if !s.resourceBudget.Reserve(n) {
		return errStateBudget
	}
	if err := json.Unmarshal(data, value); err != nil {
		s.resourceBudget.Release(n)
		return err
	}
	*s.resourceReleases = append(*s.resourceReleases, func() { s.resourceBudget.Release(n) })
	return nil
}

// summaryBudgeted holds one decoded sidecar and the digest's two encoded byte
// owners only while deriving the compact summary, before persistence starts.
func (s *Store) summaryBudgeted(p publishedState) (PublishedSummary, error) {
	if s.resourceBudget != nil {
		if err := s.resourceContext.Err(); err != nil {
			return PublishedSummary{}, err
		}
	}
	n := int64(len(p.MetadataBytes))
	if !s.resourceBudget.Reserve(n) {
		return PublishedSummary{}, errStateBudget
	}
	defer s.resourceBudget.Release(n)
	var metadata archive.Metadata
	if json.Unmarshal(p.MetadataBytes, &metadata) != nil {
		metadata = archive.Metadata{}
	} // preserve the unreadable legacy fallback
	if s.resourceBudget != nil {
		const scratch = 32 << 10
		if !s.resourceBudget.Reserve(scratch) {
			return PublishedSummary{}, errStateBudget
		}
		ctx := s.resourceContext
		available := s.resourceBudget.Available()
		size, err := jsonwire.Bound(ctx, struct {
			Active     archive.SourceReference  `json:"active"`
			CapturedAt time.Time                `json:"captured_at"`
			History    *archive.RevisionHistory `json:"history,omitempty"`
		}{metadata.SourceBundle, metadata.CapturedAt, metadata.History}, available/2)
		if errors.Is(err, jsonwire.ErrLimit) {
			s.resourceBudget.Refuse(available + 2)
		}
		s.resourceBudget.Release(scratch)
		if err != nil {
			return PublishedSummary{}, errors.Join(errStateBudget, err)
		}
		// SourceSetDigest's encoder buffer and returned JSON coexist independently.
		if !s.resourceBudget.Reserve(size + size) {
			return PublishedSummary{}, errStateBudget
		}
		defer s.resourceBudget.Release(size + size)
	}
	// The final digest and revision ID outlive the ephemeral metadata. Reserve
	// their independently owned bytes before deriving/cloning them.
	output := int64(64)
	if metadata.History != nil {
		output += int64(len(metadata.History.CurrentRevision))
	}
	if !s.resourceBudget.Reserve(output) {
		return PublishedSummary{}, errStateBudget
	}
	summary := p.summaryFromMetadata(metadata)
	summary.CurrentRevision = strings.Clone(summary.CurrentRevision)
	actual := int64(len(summary.SourceSetDigest) + len(summary.CurrentRevision))
	s.resourceBudget.Release(output - actual)
	if s.resourceBudget != nil {
		*s.resourceReleases = append(*s.resourceReleases, func() { s.resourceBudget.Release(actual) })
	}
	return summary, nil
}

// nextBudgeted borrows the sole temporary metadata decode used to preserve an
// existing clock clamp. The result keeps only the compact clamp, never that view.
func (p *Published) nextBudgeted(bundle archive.SourceBundle, at time.Time, status CacheStatus, reason BlockedReason, metadata []byte, held []archive.SupplementalEvidence, source *archive.SourceReference) (publishedState, error) {
	if p.store.resourceBudget != nil {
		if err := p.store.resourceContext.Err(); err != nil {
			return publishedState{}, err
		}
	}
	n := int64(0)
	if p.state.AgeFrom != nil {
		raw := metadata
		if len(raw) == 0 {
			raw = p.state.MetadataBytes
		}
		n = int64(len(raw))
	}
	if !p.store.resourceBudget.Reserve(n) {
		return publishedState{}, errStateBudget
	}
	defer p.store.resourceBudget.Release(n)
	return p.state.next(bundle, at, status, reason, metadata, held, source), nil
}
