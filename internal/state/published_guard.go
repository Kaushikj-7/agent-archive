package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

type legacyPublishedDirectory struct {
	root    *os.Root
	check   func() error
	lock    func() (func(), error)
	close   func() error
	private bool
}

// Every legacy cache mutation shares the generation transition's hooks lock.
// Its view must still describe the exact file decoded before acquiring the lock;
// a stale handle cannot overwrite a newer selecting protocol or source set.
func (p *Published) writeLegacyPublished(next publishedState) (sum [32]byte, err error) {
	home, err := p.openLegacyPublishedDirectory()
	if err != nil {
		return sum, err
	}
	defer func() {
		err = errors.Join(err, home.check(), home.close())
		if err != nil {
			sum = [32]byte{}
		}
	}()
	unlock, err := lockLegacyPublished(p.store.durableContext(), home.lock)
	if err != nil {
		return sum, err
	}
	defer unlock()
	if err := p.checkLegacyGenerationWriter(home.private); err != nil {
		return sum, err
	}
	path := filepath.Join("published", p.id+".json")
	before, err := home.root.Lstat(path)
	if p.found {
		if err != nil {
			return sum, errors.Join(ErrDurableStorageRecovery, err)
		}
		current, e := p.store.hashPublishedFile(home.root, path, before, home.check)
		if e != nil {
			return sum, e
		}
		if current != p.observedWireSHA256 {
			return sum, ErrDurableStorageRecovery
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return sum, errors.Join(ErrDurableStorageRecovery, err)
	}
	if err = errors.Join(home.check(), p.store.durableContext().Err()); err != nil {
		return sum, err
	}
	if err = p.store.writeCompactAt(home.root, path, next); err != nil {
		return sum, err
	}
	after, err := home.root.Lstat(path)
	if err != nil {
		return sum, err
	}
	return p.store.hashPublishedFile(home.root, path, after, home.check)
}

func (p *Published) openLegacyPublishedDirectory() (legacyPublishedDirectory, error) {
	return p.openLegacyPublishedDirectoryUsing(local.OpenRootedHome)
}

func (p *Published) openLegacyPublishedDirectoryUsing(openPrivate func(string) (*local.RootedHome, error)) (out legacyPublishedDirectory, err error) {
	identity, err := os.Stat(p.store.home)
	if err != nil || p.observedHome == nil || !os.SameFile(p.observedHome, identity) {
		return out, errors.Join(ErrDurableStorageRecovery, err)
	}
	named, namedErr := os.Lstat(p.store.home)
	if namedErr != nil {
		return out, namedErr
	}
	if identity.Mode().Perm()&0077 == 0 && named.Mode()&os.ModeSymlink == 0 {
		home, err := openPrivate(p.store.home)
		if err != nil {
			return out, err
		}
		opened, e := home.Root.Stat(".")
		if e != nil || !os.SameFile(p.observedHome, opened) {
			return out, errors.Join(ErrDurableStorageRecovery, e, home.Close())
		}
		return legacyPublishedDirectory{root: home.Root, check: home.Check, close: home.Close, private: true, lock: func() (func(), error) { return local.RootedLockWait(home, "hooks.lock", 0) }}, nil
	}
	// Compatibility for existing unprotected homes retains their permissions.
	// This directory is never represented by a protected RootedHome witness.
	if err := p.checkLegacyGenerationWriter(false); err != nil {
		return out, err
	}
	root, err := os.OpenRoot(p.store.home)
	if err != nil {
		return out, err
	}
	check := func() error {
		named, e := os.Stat(p.store.home)
		opened, f := root.Stat(".")
		if e != nil || f != nil || !os.SameFile(identity, named) || !os.SameFile(identity, opened) {
			return errors.Join(ErrDurableStorageRecovery, e, f)
		}
		return p.store.durableContext().Err()
	}
	if err := check(); err != nil {
		return out, errors.Join(err, root.Close())
	}
	return legacyPublishedDirectory{root: root, check: check, close: root.Close, lock: func() (func(), error) {
		if err := check(); err != nil {
			return nil, err
		}
		return local.NamedLock(p.store.home, "hooks.lock")
	}}, nil
}

// A bounded projection checks the actual affected generation namespaces, without
// allocating an unfinished journal's payload or weakening a protected version.
func (p *Published) checkLegacyGenerationWriter(private bool) error {
	for _, id := range []string{p.id, p.state.Bundle.PreviousGenerationID} {
		if id == "" {
			continue
		}
		if !safeFileComponent(id) {
			return ErrDurableStorageRecovery
		}
		var marker generationWriterMarker
		err := p.store.readProjected(p.store.generationRecoveryPath(id), jsonProjection{"version": nil, "prior_metadata_sha256": nil, "prior_source_set_sha256": nil}, &marker)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if marker.Version != 1 && (marker.Version != 2 || !private) {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

func lockLegacyPublished(ctx context.Context, lock func() (func(), error)) (func(), error) {
	deadline := time.Now().Add(time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := lock()
		if !errors.Is(err, local.ErrBusy) {
			return unlock, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, local.ErrBusy
		}
		timer := time.NewTimer(min(10*time.Millisecond, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Projected selectors remain strict even though unrelated payloads are skipped.
// In particular, duplicate or case-aliased versions cannot downgrade a fence.
type generationWriterMarker struct {
	Version              int    `json:"version"`
	PriorMetadataSHA256  string `json:"prior_metadata_sha256,omitempty"`
	PriorSourceSetSHA256 string `json:"prior_source_set_sha256,omitempty"`
}

func (m *generationWriterMarker) UnmarshalJSON(raw []byte) error {
	type plain generationWriterMarker
	var marker plain
	if err := json.Unmarshal(raw, &marker); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	keys := []string{"version"}
	if marker.Version == 2 {
		keys = append(keys, "prior_metadata_sha256", "prior_source_set_sha256")
	}
	if err := strictStagedKeys(raw, keys); err != nil {
		return err
	}
	if !validGenerationRecoveryVersion(generationRecovery{Version: marker.Version, PriorMetadataSHA256: marker.PriorMetadataSHA256, PriorSourceSetSHA256: marker.PriorSourceSetSHA256}) {
		return ErrDurableStorageRecovery
	}
	*m = generationWriterMarker(marker)
	return nil
}
