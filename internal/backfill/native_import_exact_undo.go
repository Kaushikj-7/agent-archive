package backfill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// NativeImportUndoSession describes a logical membership, never a deletion.
type NativeImportUndoSession struct {
	NativeID  string
	ArchiveID string
	ProjectID string
	Detached  bool
}

// NativeImportUndoPlan retains one bounded actual control and owner snapshot.
// Close releases its read loan; callers cannot construct an executable plan.
type nativeImportUndoLifetime struct{ mu sync.Mutex }

type NativeImportUndoPlan struct {
	lifetime    *nativeImportUndoLifetime
	self        *NativeImportUndoPlan
	store       *state.Store
	batch       Batch
	controlSHA  string
	owners      []archive.SessionRegistration
	closeRead   func()
	closed      bool
	absent      int
	independent int
}

// Close ends the bounded plan lifetime. It is safe to call more than once.
func (p *NativeImportUndoPlan) Close() {
	if p == nil || p.self != p || p.lifetime == nil {
		return
	}
	p.lifetime.mu.Lock()
	defer p.lifetime.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.owners = nil
	p.batch = Batch{}
	p.store = nil
	if p.closeRead != nil {
		p.closeRead()
		p.closeRead = nil
	}
}

// Sessions returns display values without exposing the exact receipt capability.
func (p *NativeImportUndoPlan) Sessions() []NativeImportUndoSession {
	if p == nil || p.self != p || p.lifetime == nil {
		return nil
	}
	p.lifetime.mu.Lock()
	defer p.lifetime.mu.Unlock()
	if p.closed {
		return nil
	}
	out := make([]NativeImportUndoSession, 0, len(p.owners))
	for _, reg := range p.owners {
		out = append(out, NativeImportUndoSession{NativeID: reg.NativeSessionID, ArchiveID: reg.ArchiveSessionID, ProjectID: reg.ProjectID, Detached: reg.NativeImport.Membership.State == archive.NativeImportDetached})
	}
	return out
}

// Unowned reports selected entries with no admitted owner and independent owners
// which the creating batch cannot claim. Neither category is detached.
func (p *NativeImportUndoPlan) Unowned() (absent, independent int) {
	if p == nil || p.self != p || p.lifetime == nil {
		return 0, 0
	}
	p.lifetime.mu.Lock()
	defer p.lifetime.mu.Unlock()
	if p.closed {
		return 0, 0
	}
	return p.absent, p.independent
}

// PlanNativeImportUndo reads only the confirmed selection's bounded point
// lookups. project is an exact stored project root, including a removed worktree.
func PlanNativeImportUndo(ctx context.Context, store *state.Store, id, project string) (_ *NativeImportUndoPlan, err error) {
	if store == nil || !archive.ValidImportBatchID(id) {
		return nil, archive.ErrUnsupportedNativeImport
	}
	read, closeRead := store.WithImportResultScope(ctx)
	p := &NativeImportUndoPlan{store: read, closeRead: closeRead, lifetime: &nativeImportUndoLifetime{}}
	p.self = p
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	raw, batch, err := readNativeImportUndoBatch(ctx, store.Home(), id)
	if err != nil {
		return nil, err
	}
	if len(batch.Filters.Harnesses) != 1 || batch.Filters.Harnesses[0] != "muse" {
		return nil, errors.New("historical Muse undo requires a Muse-only batch; mixed-harness batch effects were preserved")
	}
	p.batch = batch
	sum := sha256.Sum256(raw)
	p.controlSHA = hex.EncodeToString(sum[:])
	if project != "" {
		project, err = filepath.Abs(project)
		if err != nil {
			return nil, err
		}
	}
	for _, selected := range batch.NativeImport.Scope.Selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		archiveID, found, err := read.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: selected.NativeID})
		if err != nil {
			return nil, err
		}
		if !found {
			p.absent++
			continue
		}
		reg, found, err := read.LoadRegistration(archiveID)
		if err != nil || !found {
			return nil, errors.Join(state.ErrSessionNotRegistered, err)
		}
		if project != "" && filepath.Clean(reg.ProjectRoot) != project {
			continue
		}
		if reg.NativeImport == nil || reg.NativeImport.Confirmation.BatchID != id || reg.NativeImport.Confirmation.BatchInstance != batch.NativeImport.Instance {
			p.independent++
			continue
		}
		if err := nativeImportUndoOwnerMatches(ctx, batch, reg); err != nil {
			return nil, err
		}
		p.owners = append(p.owners, reg)
	}
	return p, nil
}

func nativeImportUndoOwnerMatches(ctx context.Context, batch Batch, reg archive.SessionRegistration) error {
	if reg.ValidateNativeImport() != nil {
		return archive.ErrUnsupportedNativeImport
	}
	receipt := *reg.NativeImport
	receipt.Membership.State = archive.NativeImportAttached
	reg.NativeImport = &receipt
	control := batch.nativeImportControl()
	control.UndoneAt = nil
	return control.MatchesRegistration(ctx, reg)
}

func readNativeImportUndoBatch(ctx context.Context, home, id string) (raw []byte, batch Batch, err error) {
	err = state.WithImportBatchLock(ctx, home, id, func(held *local.RootedHome) (err error) {
		raw, err = state.ReadImportBatchControlBytes(held, id)
		if err != nil {
			return err
		}
		if err := decodeBatch(raw, &batch); err != nil {
			return err
		}
		if batch.ID != id || batch.NativeImport == nil {
			return archive.ErrUnsupportedNativeImport
		}
		return batch.nativeImportControl().Validate(ctx)
	})
	return raw, batch, err
}

// Apply durably stops this batch first, then detaches only its exact receipts.
// An interruption is restartable; registrations, stages, requests, remote objects,
// native files and all configuration survive every completed detach.
func (p *NativeImportUndoPlan) Apply(ctx context.Context, at time.Time) (detached int, err error) {
	if p == nil || p.self != p || p.lifetime == nil {
		return 0, archive.ErrUnsupportedNativeImport
	}
	p.lifetime.mu.Lock()
	defer p.lifetime.mu.Unlock()
	if p.closed || p.store == nil || at.IsZero() {
		return 0, archive.ErrUnsupportedNativeImport
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if p.batch.UndoneAt == nil {
		batch := p.batch
		at = at.UTC()
		batch.UndoneAt = &at
		payload, err := json.Marshal(batch)
		if err != nil {
			return 0, err
		}
		if len(payload) > archive.MaxImportBatchControlBytes {
			return 0, archive.ErrUnsupportedNativeImport
		}
		err = state.ReplaceImportBatchControl(ctx, p.store.Home(), batch.ID, func(w io.Writer) error {
			n, err := w.Write(payload)
			if err == nil && n != len(payload) {
				err = io.ErrShortWrite
			}
			return err
		}, func(raw []byte, found bool) error {
			sum := sha256.Sum256(raw)
			if !found || hex.EncodeToString(sum[:]) != p.controlSHA {
				return archive.ErrUnsupportedNativeImport
			}
			var current Batch
			if err := decodeBatch(raw, &current); err != nil {
				return err
			}
			return errors.Join(ctx.Err(), validateBatchReplacement(ctx, &current, batch))
		})
		if err != nil {
			return 0, err
		}
		p.batch = batch
	}
	for _, reg := range p.owners {
		changed, err := p.store.DetachNativeImportBatchMembership(ctx, p.batch.ID, reg.ArchiveSessionID, reg.NativeImport.Membership.ReceiptID)
		if err != nil {
			return detached, err
		}
		if changed {
			detached++
		}
	}
	return detached, nil
}
