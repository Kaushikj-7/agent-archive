package state

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// WithReadBudget borrows the pass ledger for a session's local serialized and
// decoded inputs. The returned close ends ownership of that session's data;
// no Published or Pending value read through the handle may outlive it.
func (s *Store) WithReadBudget(ctx context.Context, budget *agentapi.NativeReadBudget) (*Store, func()) {
	scoped := *s
	scoped.pendingWrite = nil
	if scoped.publicationAccounting != nil && scoped.publicationAccounting.budget != budget {
		scoped.publicationAccounting = nil
	}
	if budget != s.resourceBudget {
		scoped.resourceReservation = nil
	}
	scoped.resourceBudget = budget
	scoped.resourceContext = ctx
	scoped.resourceReleases = &[]func(){}
	scoped.summaryBorrow = &publishedSummaryBorrow{}
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

// readBudgeted uses the context captured by WithReadBudget for this Store.
// Explicit-context callers keep using readBudgetedContext without this boundary.
//
//nolint:contextcheck // durableContext supplies the original receiver-owned read scope; the analyzer cannot follow Store fields.
func (s *Store) readBudgeted(path string, value any, retain bool) error {
	return s.readBudgetedContext(s.durableContext(), path, value, retain)
}

func (s *Store) readBudgetedContext(ctx context.Context, path string, value any, retain bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reservation := s.readReservation()
	if s.resourceBudget == nil {
		if _, published := value.(*publishedState); !published {
			return readRegistrationJSON(path, value)
		}
	}
	if s.resourceBudget != nil {
		if err := s.resourceContext.Err(); err != nil {
			return err
		}
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
	if !filepath.IsLocal(rel) {
		return errStateFileUnstable
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
	if !reservation.Reserve(n) {
		return errStateBudget
	}
	defer reservation.Release(n)
	if !reservation.Reserve(n) {
		return errStateBudget
	}
	keep := false
	defer func() {
		if !keep {
			reservation.Release(n)
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
	if s.resourceBudget != nil {
		if err := s.resourceContext.Err(); err != nil {
			return err
		}
	}
	if err := s.decodePublicationOwned(ctx, data, value); err != nil {
		return err
	}
	if retain && s.resourceBudget != nil {
		keep = true
		*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
	}
	return nil
}

func (s *Store) writeCompact(path string, value any) error {
	s.invalidatePublishedAccounting(path)
	reservation := s.readReservation()
	if s.resourceBudget == nil {
		return local.WriteCompact(path, value)
	}
	const scratch = 32 << 10
	if !reservation.Reserve(scratch) {
		return errStateBudget
	}
	available := reservation.Available()
	n, err := agentmeta.JSONWireBound(s.resourceContext, value, available-1)
	if errors.Is(err, agentmeta.ErrJSONWireLimit) {
		reservation.Refuse(available + 1)
	}
	reservation.Release(scratch)
	if err != nil {
		return errors.Join(errStateBudget, err)
	}
	if !reservation.Reserve(n + 1) {
		return errStateBudget
	}
	defer reservation.Release(n + 1)
	return local.WriteCompact(path, value)
}

// unmarshalOwned charges an additional decoded view of bytes already owned by
// this scope. Returned metadata has an independent lifetime from its wire input.
// Its context and ledger come from WithReadBudget.
//
//nolint:contextcheck // durableContext supplies the original receiver-owned read scope; the analyzer cannot follow Store fields.
func (s *Store) unmarshalOwned(data []byte, value any) error {
	reservation := s.readReservation()
	if s.resourceBudget == nil {
		return s.decodePublicationOwned(s.durableContext(), data, value)
	}
	if err := s.resourceContext.Err(); err != nil {
		return err
	}
	n := int64(len(data))
	if !reservation.Reserve(n) {
		return errStateBudget
	}
	if err := s.decodePublicationOwned(s.durableContext(), data, value); err != nil {
		reservation.Release(n)
		return err
	}
	*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
	return nil
}

// summaryBudgeted holds one decoded sidecar and the digest's two encoded byte
// owners only while deriving the compact summary, before persistence starts.
func (s *Store) summaryBudgeted(p publishedState) (PublishedSummary, error) {
	reservation := s.readReservation()
	if s.resourceBudget != nil {
		if err := s.resourceContext.Err(); err != nil {
			return PublishedSummary{}, err
		}
	}
	n := int64(len(p.MetadataBytes))
	if !reservation.Reserve(n) {
		return PublishedSummary{}, errStateBudget
	}
	defer reservation.Release(n)
	var metadata archive.Metadata
	if json.Unmarshal(p.MetadataBytes, &metadata) != nil {
		metadata = archive.Metadata{}
	} // preserve the unreadable legacy fallback
	if s.resourceBudget != nil {
		const scratch = 32 << 10
		if !reservation.Reserve(scratch) {
			return PublishedSummary{}, errStateBudget
		}
		ctx := s.resourceContext
		available := reservation.Available()
		size, err := agentmeta.JSONWireBound(ctx, struct {
			Active     archive.SourceReference  `json:"active"`
			CapturedAt time.Time                `json:"captured_at"`
			History    *archive.RevisionHistory `json:"history,omitempty"`
		}{metadata.SourceBundle, metadata.CapturedAt, metadata.History}, available/2)
		if errors.Is(err, agentmeta.ErrJSONWireLimit) {
			reservation.Refuse(available + 2)
		}
		reservation.Release(scratch)
		if err != nil {
			return PublishedSummary{}, errors.Join(errStateBudget, err)
		}
		// SourceSetDigest's encoder buffer and returned JSON coexist independently.
		if !reservation.Reserve(size + size) {
			return PublishedSummary{}, errStateBudget
		}
		defer reservation.Release(size + size)
	}
	// The final digest and revision ID outlive the ephemeral metadata. Reserve
	// their independently owned bytes before deriving/cloning them.
	output := int64(64)
	if metadata.History != nil {
		output += int64(len(metadata.History.CurrentRevision))
	}
	if !reservation.Reserve(output) {
		return PublishedSummary{}, errStateBudget
	}
	summary := p.summaryFromMetadata(metadata)
	summary.CurrentRevision = strings.Clone(summary.CurrentRevision)
	actual := int64(len(summary.SourceSetDigest) + len(summary.CurrentRevision))
	reservation.Release(output - actual)
	if s.resourceBudget != nil {
		*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(actual) })
	}
	return summary, nil
}

// nextBudgeted borrows the sole temporary metadata decode used to preserve an
// existing clock clamp. The result keeps only the compact clamp, never that view.
func (p *Published) nextBudgeted(bundle archive.SourceBundle, at time.Time, status CacheStatus, reason BlockedReason, metadata []byte, held []archive.SupplementalEvidence, source *archive.SourceReference) (publishedState, error) {
	reservation := p.store.readReservation()
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
	if !reservation.Reserve(n) {
		return publishedState{}, errStateBudget
	}
	defer reservation.Release(n)
	return p.state.next(bundle, at, status, reason, metadata, held, source), nil
}

type stateReservation interface {
	Reserve(int64) bool
	Release(int64)
	Available() int64
	Refuse(int64)
}

func (s *Store) readReservation() stateReservation {
	if s.resourceReservation != nil {
		return s.resourceReservation()
	}
	return s.resourceBudget.Reservation()
}

func (s *Store) loadConfig() (cfg config.Config, found bool, err error) {
	if s.resourceReservation == nil {
		return config.Load(s.home)
	}
	held, err := local.OpenRootedHome(s.home)
	if err != nil {
		return cfg, false, err
	}
	defer func() { err = errors.Join(err, held.Close()) }()
	cfg, found, closeCfg, err := config.LoadRootedOwned(s.resourceContext, held, s.readReservation())
	if err == nil {
		*s.resourceReleases = append(*s.resourceReleases, closeCfg)
	}
	return cfg, found, err
}

func (s *Store) withImportStaging(ctx context.Context, write func(config.DurableStorageGuard) error) error {
	if s.resourceReservation == nil {
		return config.WithImportStaging(s.home, write)
	}
	return config.WithImportStagingOwned(ctx, s.home, func() config.ReadReservation { return s.readReservation() }, write)
}

// ownStageValidationWire holds the sequential canonical encoder view through
// checked capture validation and its write; default staging is unchanged.
func (s *Store) ownStageValidationWire(ctx context.Context, stage ImportStage) (func(), error) {
	if s.resourceReservation == nil {
		return func() {}, nil
	}
	owner := s.readReservation()
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return nil, errors.Join(ErrDurableStorageCapacity, errStateBudget)
	}
	n, err := agentmeta.JSONWireBound(ctx, stage, MaxImportStageBytes)
	if err != nil {
		owner.Release(scratch)
		return nil, errors.Join(ErrDurableStorageCapacity, err)
	}
	if !owner.Reserve(n) {
		owner.Release(scratch)
		return nil, errors.Join(ErrDurableStorageCapacity, errStateBudget)
	}
	var once sync.Once
	return func() { once.Do(func() { owner.Release(n + scratch) }) }, nil
}

// decodePublicationOwned supplies only the inherited ledger to closed authority
// decoding, while malformed JSON retains its existing corruption classification.
func (s *Store) decodePublicationOwned(ctx context.Context, data []byte, value any) error {
	var err error
	switch p := value.(type) {
	case *publishedState:
		err = decodePublishedState(data, p, ctx, s.resourceBudget, s.readReservation())
	case *PendingPublication:
		err = decodePendingPublication(data, p, ctx, s.resourceBudget, s.readReservation())
	default:
		// Protected state types took the explicit-context branches above.
		return decodeRegistrationJSON(data, value)
	}
	if err != nil && !json.Valid(data) {
		return decodeRegistrationJSON(data, value)
	}
	if _, published := value.(*publishedState); published && err == nil && s.publishedReadDigest != nil {
		*s.publishedReadDigest = sha256.Sum256(data)
	}
	return err
}
