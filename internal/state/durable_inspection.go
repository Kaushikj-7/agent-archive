package state

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
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
	mu           sync.Mutex
	home         os.FileInfo
	stamps       [3]durableRootStamp
	obligations  []DurableStorageObligation
	anonymous    bool
	valid        bool
	summaryValid bool
	scans        int
	legacy       bool
	configHome   os.FileInfo
	configStamp  os.FileInfo
	configValue  config.Config
	configFound  bool
	configValid  bool
	configLoads  int
	legacyTemps  bool
	entries      int
	budget       *agentapi.NativeReadBudget
	release      func()
	generation   uint64
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

func (s *Store) inspectPendingRoots(ctx context.Context, home *local.RootedHome, legacy, collectNames bool) ([]DurableStorageObligation, bool, int, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, true, 0, 0, err
	}
	cache := s.durableInspection
	if cache == nil {
		return nil, true, 0, 0, ErrDurableStorageRecovery
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	homeInfo, err := home.Root.Stat(".")
	if err != nil {
		return nil, true, 0, 0, err
	}
	roots := [3]string{"pending", generationRecoveryDir, "published"}
	stamps, err := inspectPendingRootStamps(home, roots)
	if err != nil {
		return nil, true, 0, 0, err
	}
	current := cache.summaryValid && cache.legacy == legacy && os.SameFile(cache.home, homeInfo) && sameDurableRootStamps(cache.stamps, stamps)
	if current && !collectNames {
		return nil, cache.anonymous, cache.entries, cache.generation, errors.Join(ctx.Err(), home.Check())
	}
	if current && cache.valid && cache.budget == s.resourceBudget {
		out, err := s.copyDurableObligations(cache.obligations)
		return out, cache.anonymous, cache.entries, cache.generation, errors.Join(err, ctx.Err(), home.Check())
	}
	cache.summaryValid = false
	cache.clearObligations()
	cache.legacyTemps = false
	cache.scans++
	visited := 0
	var out []DurableStorageObligation
	anonymous := false
	for i, name := range roots {
		if stamps[i].info == nil {
			continue
		}
		entries, unknown, e := s.inspectPendingRoot(ctx, home, name, &visited, legacy, collectNames)
		if e != nil {
			return out, true, visited, 0, e
		}
		if collectNames {
			out, e = s.appendDurableObligations(out, entries...)
			if e != nil {
				return out, true, visited, 0, e
			}
		}
		anonymous = anonymous || unknown
	}
	after, err := inspectPendingRootStamps(home, roots)
	if err != nil || !sameDurableRootStamps(stamps, after) {
		return out, true, visited, 0, errors.Join(ErrDurableStorageRecovery, err)
	}
	if err := errors.Join(ctx.Err(), home.Check()); err != nil {
		return out, true, visited, 0, err
	}
	cache.entries = visited
	cache.legacy = legacy
	cache.home = homeInfo
	cache.stamps = stamps
	cache.anonymous = anonymous
	cache.summaryValid = !cache.legacyTemps
	return out, anonymous, cache.entries, cache.generation, nil
}

func inspectPendingRootStamps(home *local.RootedHome, roots [3]string) (stamps [3]durableRootStamp, err error) {
	for i, name := range roots {
		info, err := home.Root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return stamps, errors.Join(ErrDurableStorageRecovery, err)
		}
		stamps[i].info = info
	}
	return stamps, nil
}

func sameDurableRootStamps(before, after [3]durableRootStamp) bool {
	for i := range before {
		if !sameDurableStamp(before[i].info, after[i].info) {
			return false
		}
	}
	return true
}

func (s *Store) inspectPendingRoot(ctx context.Context, home *local.RootedHome, name string, visited *int, legacy, collectNames bool) (out []DurableStorageObligation, unknown bool, err error) {
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
		if err = ctx.Err(); err != nil {
			return out, true, err
		}
		entries, e := readDurableDirectory(ctx, d)
		if e != nil && !errors.Is(e, io.EOF) {
			return out, true, e
		}
		for _, entry := range entries {
			if e := countDurableEntry(visited); e != nil {
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
			if info.Mode().IsRegular() && strings.HasSuffix(entry.Name(), ".json") && safeFileComponent(id) && !strings.HasPrefix(entry.Name(), ".pending-") {
				if collectNames && name != "published" {
					out, err = s.appendDurableObligations(out, DurableStorageObligation{SessionID: id, Namespace: namespace})
					if err != nil {
						return out, true, err
					}
				}
				continue
			}
			unknown = true
		}
		if errors.Is(e, io.EOF) {
			break
		}
	}
	if unknown && collectNames {
		out, err = s.appendDurableObligations(out, DurableStorageObligation{Namespace: namespace})
		if err != nil {
			return out, true, err
		}
	}
	return out, unknown, nil
}

func (s *Store) loadDurableInspectionConfig(ctx context.Context, home *local.RootedHome) (cfg config.Config, found bool, err error) {
	cache := s.durableInspection
	if cache == nil {
		return cfg, false, ErrDurableStorageRecovery
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if err = ctx.Err(); err != nil {
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
	if err = errors.Join(ctx.Err(), home.Check()); err != nil {
		return cfg, false, err
	}
	cache.configHome = homeInfo
	cache.configStamp = info
	cache.configValue = cfg
	cache.configFound = found
	cache.configValid = true
	return cfg, found, nil
}

func (s *Store) inspectDurableReadRoots(ctx context.Context) (cfg config.Config, err error) {
	s, ctx, closeContext := s.withDurableContext(ctx)
	defer closeContext()
	if err := ctx.Err(); err != nil {
		return cfg, err
	}
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, ctx.Err()
	}
	if err != nil {
		return cfg, errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	cfg, _, err = s.inspectHeldDurableReadRoots(ctx, home)
	return cfg, err
}

func (s *Store) inspectHeldDurableReadRoots(ctx context.Context, home *local.RootedHome) (cfg config.Config, found bool, err error) {
	cfg, found, err = s.loadDurableInspectionConfig(ctx, home)
	if err != nil {
		return cfg, found, errors.Join(ErrDurableStorageRecovery, err)
	}
	if present, e := rootHasEntries(home.Root, deletionDirectory); e != nil || present && (!found || !cfg.DeletionProtection) {
		return cfg, found, errors.Join(ErrDurableStorageRecovery, e)
	}
	_, anonymous, entries, _, err := s.inspectPendingRoots(ctx, home, found && !cfg.DurableStorageProtection, false)
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
func (s *Store) CheckDurableSessionRead(id string) error {
	return s.checkDurableSessionRead(id, nil)
}

type publicationReadOwner struct {
	registration archive.SessionRegistration
	machine      string
	admission    string
}

// CheckPublicationSessionRead validates owned publication replay only. Generic
// preview, naming and fresh native eligibility retain CheckDurableSessionRead.
func (s *Store) CheckPublicationSessionRead(reg archive.SessionRegistration, machine, admission string) error {
	return s.checkDurableSessionRead(reg.ArchiveSessionID, &publicationReadOwner{reg, machine, admission})
}

func (s *Store) checkDurableSessionRead(id string, owner *publicationReadOwner) (err error) {
	if unsafeDurableSessionID(id) {
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
	_, found, err := s.inspectHeldDurableReadRoots(s.durableContext(), home)
	if err != nil {
		return err
	}
	if err := s.validateDurableReadReceipt(home, id, found); err != nil {
		return err
	}
	evidence, err := rootHasEntries(home.Root, filepath.Join("publication-evidence", id))
	if err != nil || evidence && owner == nil {
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
	if (history || present || evidence) && !found {
		return ErrDurableStorageRecovery
	}
	if evidence && owner != nil {
		if err := s.validatePublicationEvidenceDirectory(home.Root, id); err != nil {
			return err
		}
	}

	if history || present || evidence {
		if err := s.validateDurableReadPending(id, history, owner); err != nil {
			return err
		}
	}
	return s.durableContext().Err()
}

// CheckDurableReadRoots refuses anonymous recovery work before native content is read.
// It reuses bounded observations, never ownership or cleanup authority.
func (s *Store) CheckDurableReadRoots() error {
	_, err := s.inspectDurableReadRoots(s.durableContext())
	return err
}

func publicationReadOwnerChanged(metadata archive.Metadata, reg archive.SessionRegistration, origin archive.SessionOrigin, owner *publicationReadOwner, pending PendingPublication) bool {
	return (metadata.SessionID != reg.ArchiveSessionID || metadata.NativeSessionID != reg.NativeSessionID || metadata.ProjectID != reg.ProjectID || metadata.Harness.Name != reg.Harness.Name || metadata.PreviousGenerationID != reg.PreviousGenerationID || !metadata.StartedAt.Equal(reg.SessionStartedAt) || metadata.Origin != origin || owner.machine != "" && metadata.MachineID != owner.machine || pending.Preparation == nil || pending.Preparation.DestinationID != reg.DestinationID || pending.Preparation.AdmissionContext != owner.admission)
}

func (s *Store) validateDurableReadPending(id string, history bool, owner *publicationReadOwner) error {
	// This guard discards the validated transaction; its decoded inputs
	// must not accumulate on the caller's existing ledger.
	scratch, closeScratch := s.WithReadBudget(s.durableContext(), s.resourceBudget)
	var pending PendingPublication
	var readable bool
	var readErr error
	if owner != nil {
		pending, readable, readErr = scratch.LoadPublicationPending(id)
		if readErr == nil && readable && pending.JournalVersion == 5 {
			var metadata archive.Metadata
			readErr = scratch.unmarshalOwned(pending.MetadataBytes, &metadata)
			reg := owner.registration
			origin := reg.Origin
			if origin == archive.SessionOriginHook {
				origin = ""
			}
			if readErr == nil && publicationReadOwnerChanged(metadata, reg, origin, owner, pending) {
				readErr = ErrDurableStorageRecovery
			}
		}
	} else {
		pending, readable, readErr = scratch.LoadPending(id)
	}
	missingHistory := history && pending.History == nil
	closeScratch()
	// A capacity refusal has not inspected the publication. Keep it owed and
	// retryable; it proves neither missing history nor damaged authority.
	if errors.Is(readErr, agentapi.ErrReadBudget) && !errors.Is(readErr, ErrDurableStorageRecovery) {
		return readErr
	}
	if readErr != nil || !readable || missingHistory {
		return errors.Join(ErrDurableStorageRecovery, readErr)
	}
	return nil
}

func unsafeDurableSessionID(id string) bool {
	return !safeFileComponent(id) || len(id) > 250 || strings.IndexFunc(id, unicode.IsControl) >= 0
}

func (s *Store) validateDurableReadReceipt(home *local.RootedHome, id string, found bool) error {
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
	return s.durableContext().Err()
}
