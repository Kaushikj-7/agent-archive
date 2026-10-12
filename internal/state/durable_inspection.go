package state

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

type durableRootStamp struct{ info os.FileInfo }

type durableInspectionCache struct {
	mu          sync.Mutex
	home        os.FileInfo
	stamps      [2]durableRootStamp
	obligations []DurableStorageObligation
	anonymous   bool
	valid       bool
	scans       int
	legacy      bool
	configHome  os.FileInfo
	configStamp os.FileInfo
	configValue config.Config
	configFound bool
	configValid bool
	configLoads int
	legacyTemps bool
	entries     int
}

func sameDurableStamp(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (s *Store) durableContext() context.Context {
	if s.resourceContext != nil {
		return s.resourceContext
	}
	return context.Background()
}

func (s *Store) inspectPendingRoots(home *local.RootedHome, legacy bool) ([]DurableStorageObligation, bool, int, error) {
	ctx := s.durableContext()
	if err := ctx.Err(); err != nil {
		return nil, true, 0, err
	}
	cache := s.durableInspection
	if cache == nil {
		return nil, true, 0, ErrDurableStorageRecovery
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	homeInfo, err := home.Root.Stat(".")
	if err != nil {
		return nil, true, 0, err
	}
	roots := [2]string{"pending", generationRecoveryDir}
	var stamps [2]durableRootStamp
	for i, name := range roots {
		info, e := home.Root.Lstat(name)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, true, 0, errors.Join(ErrDurableStorageRecovery, e)
		}
		stamps[i].info = info
	}
	if cache.valid && cache.legacy == legacy && os.SameFile(cache.home, homeInfo) && sameDurableStamp(cache.stamps[0].info, stamps[0].info) && sameDurableStamp(cache.stamps[1].info, stamps[1].info) {
		return append([]DurableStorageObligation(nil), cache.obligations...), cache.anonymous, cache.entries, home.Check()
	}
	cache.valid = false
	cache.legacyTemps = false
	cache.scans++
	remaining := durableEntryLimit
	var out []DurableStorageObligation
	anonymous := false
	for i, name := range roots {
		if stamps[i].info == nil {
			continue
		}
		entries, unknown, e := s.inspectPendingRoot(home, name, &remaining, legacy)
		if e != nil {
			return out, true, durableEntryLimit - remaining, e
		}
		out = append(out, entries...)
		anonymous = anonymous || unknown
	}
	for i, name := range roots {
		info, e := home.Root.Lstat(name)
		if errors.Is(e, os.ErrNotExist) {
			info = nil
			e = nil
		}
		if e != nil || !sameDurableStamp(stamps[i].info, info) {
			return out, true, durableEntryLimit - remaining, errors.Join(ErrDurableStorageRecovery, e)
		}
	}
	if err := errors.Join(ctx.Err(), home.Check()); err != nil {
		return out, true, durableEntryLimit - remaining, err
	}
	cache.entries = durableEntryLimit - remaining
	cache.legacy = legacy
	cache.home = homeInfo
	cache.stamps = stamps
	cache.obligations = out
	cache.anonymous = anonymous
	cache.valid = !cache.legacyTemps
	return append([]DurableStorageObligation(nil), out...), anonymous, cache.entries, nil
}

func (s *Store) inspectPendingRoot(home *local.RootedHome, name string, remaining *int, legacy bool) (out []DurableStorageObligation, unknown bool, err error) {
	root, err := privateDirectory(home.Root, name, false)
	if err != nil {
		return nil, true, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	d, err := root.Open(".")
	if err != nil {
		return nil, true, err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	namespace := PendingPublicationStorage
	if name == generationRecoveryDir {
		namespace = GenerationRecoveryStorage
	}
	for {
		if err = s.durableContext().Err(); err != nil {
			return out, true, err
		}
		entries, e := d.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return out, true, e
		}
		for _, entry := range entries {
			*remaining--
			if *remaining < 0 {
				return out, true, ErrDurableStorageRecovery
			}
			info, e := root.Lstat(entry.Name())
			if e != nil {
				return out, true, e
			}
			if legacy && strings.HasPrefix(entry.Name(), ".pending-") {
				s.durableInspection.legacyTemps = true
			}
			if legacy && info.Mode().IsRegular() && (acknowledgedLegacyQuarantine(entry.Name()) || strings.HasPrefix(entry.Name(), ".pending-") && info.ModTime().Before(time.Now().Add(-staleTempAge))) {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if info.Mode().IsRegular() && strings.HasSuffix(entry.Name(), ".json") && safeFileComponent(id) {
				out = append(out, DurableStorageObligation{SessionID: id, Namespace: namespace})
				continue
			}
			unknown = true
		}
		if errors.Is(e, io.EOF) {
			break
		}
	}
	if unknown {
		out = append(out, DurableStorageObligation{Namespace: namespace})
	}
	return out, unknown, nil
}

func (s *Store) loadDurableInspectionConfig(home *local.RootedHome) (cfg config.Config, found bool, err error) {
	cache := s.durableInspection
	if cache == nil {
		return cfg, false, ErrDurableStorageRecovery
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if err = s.durableContext().Err(); err != nil {
		return cfg, false, err
	}
	homeInfo, err := home.Root.Stat(".")
	if err != nil {
		return cfg, false, err
	}
	info, err := home.Root.Lstat("config.json")
	if errors.Is(err, os.ErrNotExist) {
		info = nil
		err = nil
	}
	if err != nil {
		return cfg, false, err
	}
	if cache.configValid && os.SameFile(cache.configHome, homeInfo) && sameDurableStamp(cache.configStamp, info) {
		return cache.configValue, cache.configFound, home.Check()
	}
	cache.configValid = false
	cache.configLoads++
	cfg, found, err = config.LoadRooted(home)
	if err != nil {
		return cfg, false, err
	}
	after, err := home.Root.Lstat("config.json")
	if errors.Is(err, os.ErrNotExist) {
		after = nil
		err = nil
	}
	if err != nil || !sameDurableStamp(info, after) {
		return cfg, false, errors.Join(ErrDurableStorageRecovery, err)
	}
	if err = errors.Join(s.durableContext().Err(), home.Check()); err != nil {
		return cfg, false, err
	}
	cache.configHome = homeInfo
	cache.configStamp = info
	cache.configValue = cfg
	cache.configFound = found
	cache.configValid = true
	return cfg, found, nil
}

func (s *Store) inspectDurableReadRoots() (cfg config.Config, err error) {
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	cfg, _, err = s.inspectHeldDurableReadRoots(home)
	return cfg, err
}

func (s *Store) inspectHeldDurableReadRoots(home *local.RootedHome) (cfg config.Config, found bool, err error) {
	cfg, found, err = s.loadDurableInspectionConfig(home)
	if err != nil {
		return cfg, found, errors.Join(ErrDurableStorageRecovery, err)
	}
	_, anonymous, entries, err := s.inspectPendingRoots(home, found && !cfg.DurableStorageProtection)
	if anonymous || err != nil || !found && entries > 0 {
		return cfg, found, errors.Join(ErrDurableStorageRecovery, err)
	}
	return cfg, found, nil
}

func acknowledgedLegacyQuarantine(name string) bool {
	const layout = "20060102T150405.000000000Z"
	stem, ok := strings.CutSuffix(name, quarantineSuffix)
	if !ok || len(stem) <= len(layout)+len(".json.") {
		return false
	}
	stamp := stem[len(stem)-len(layout):]
	id, ok := strings.CutSuffix(stem[:len(stem)-len(layout)], ".json.")
	if !ok || !safeFileComponent(id) {
		return false
	}
	parsed, err := time.Parse(layout, stamp)
	return err == nil && parsed.UTC().Format(layout) == stamp
}

// CheckDurableSessionRead refuses unknown session obligations before preview
// opens a provider. It shares the root/config cache and bounded local probes.
func (s *Store) CheckDurableSessionRead(id string) (err error) {
	if !safeFileComponent(id) || len(id) > 250 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return ErrDurableStorageRecovery
	}
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return s.durableContext().Err()
	}
	if err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() {
		cleanup := errors.Join(home.Check(), home.Close())
		// A read the memory ledger refused names no recovery: it is retried.
		refused := cleanup == nil && errors.Is(err, agentapi.ErrReadBudget)
		err = errors.Join(err, cleanup)
		if err != nil && !refused {
			err = errors.Join(ErrDurableStorageRecovery, err)
		}
	}()
	_, found, err := s.inspectHeldDurableReadRoots(home)
	if err != nil {
		return err
	}
	info, receiptErr := home.Root.Lstat(filepath.Join(generationRecoveryDir, id+".json"))
	if receiptErr == nil {
		if !found || !info.Mode().IsRegular() {
			return ErrDurableStorageRecovery
		}
		complete, err := s.completedGenerationReceipt(home, id)
		if err != nil || !complete {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
	} else if !errors.Is(receiptErr, os.ErrNotExist) {
		return errors.Join(ErrDurableStorageRecovery, receiptErr)
	}
	evidence, err := rootHasEntries(home.Root, filepath.Join("publication-evidence", id))
	if err != nil || evidence {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	history, err := rootHasEntries(home.Root, filepath.Join("sessions", id, "pending-sources"))
	if err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	info, pendingErr := home.Root.Lstat(filepath.Join("pending", id+".json"))
	present := pendingErr == nil
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) || present && !info.Mode().IsRegular() {
		return errors.Join(ErrDurableStorageRecovery, pendingErr)
	}
	if (history || present) && !found {
		return ErrDurableStorageRecovery
	}
	if history || present {
		if err := s.checkPendingReadable(id, history); err != nil {
			return err
		}
	}
	return s.durableContext().Err()
}

// checkPendingReadable reads the pending publication CheckDurableSessionRead
// found. This guard discards the validated transaction; its decoded inputs
// must not accumulate on the caller's existing ledger.
func (s *Store) checkPendingReadable(id string, history bool) error {
	scratch, closeScratch := s.WithReadBudget(s.durableContext(), s.resourceBudget)
	pending, readable, readErr := scratch.LoadPending(id)
	missingHistory := history && pending.History == nil
	closeScratch()
	if errors.Is(readErr, agentapi.ErrReadBudget) {
		// Memory for reading it, not the evidence, is what is missing.
		return readErr
	}
	if readErr != nil || !readable || missingHistory {
		return errors.Join(ErrDurableStorageRecovery, readErr)
	}
	return nil
}

// CheckDurableReadRoots refuses anonymous recovery work before native content is read.
// It reuses bounded observations, never ownership or cleanup authority.
func (s *Store) CheckDurableReadRoots() error { _, err := s.inspectDurableReadRoots(); return err }
