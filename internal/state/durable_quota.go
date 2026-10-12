package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/local"
)

const durableStorageQuota int64 = 1 << 30

const durableControlBytes int64 = 64 << 10

const durableEntryLimit = 65536

// ErrDurableStorageCapacity leaves durable evidence intact until capacity is freed.
var ErrDurableStorageCapacity = errors.New("durable publication capacity exhausted; finish pending publication or recover retained evidence before retrying")

// ErrDurableStorageRecovery preserves unknown or damaged obligations without native substitution.
var ErrDurableStorageRecovery = errors.New("durable publication evidence requires recovery; native substitution and automatic cleanup are forbidden")

type durableRootPath string

const (
	pendingDurableRoot     durableRootPath = "pending"
	generationDurableRoot  durableRootPath = generationRecoveryDir
	admissionDurableRoot   durableRootPath = "admission-stages"
	reservationDurableRoot durableRootPath = "temporary-reservations"
	scratchDurableRoot     durableRootPath = "temporary-scratch"
)

func protectedDurableRoot(path string) bool {
	return durableRootPath(path) == pendingDurableRoot || durableRootPath(path) == generationDurableRoot
}

type durableUsage struct {
	physical int64
	charged  int64
	recovery bool
}

type durableQuota struct {
	guard  config.DurableStorageGuard
	path   string
	home   *local.RootedHome
	unlock func()
}

func (s *Store) openDurableQuota(g config.DurableStorageGuard) (*durableQuota, error) {
	home, err := g.RootedHome(s.home)
	if err != nil {
		return nil, err
	}
	if s.onLockWait != nil {
		s.onLockWait("temporary-quota")
	}
	unlock, err := local.RootedLockWait(home, "temporary-quota", time.Second)
	if err != nil {
		return nil, err
	}
	return &durableQuota{home: home, unlock: unlock, guard: g, path: s.home}, nil
}

func (q *durableQuota) Close() error { q.unlock(); return q.guard.CheckHome(q.path) }

func privateDirectory(root *os.Root, path string, create bool) (*os.Root, error) {
	return privateDirectoryWithParentSync(root, path, create, syncDurableParent)
}

// syncDurableParent persists directory entries through the held parent capability.
func syncDurableParent(parent *os.Root) error {
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// The dependency is local to this traversal; production always uses the held
// parent barrier above. Tests can observe ordering and inject a barrier failure.
func privateDirectoryWithParentSync(root *os.Root, path string, create bool, syncParent func(*os.Root) error) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for part := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		if !safeFileComponent(part) {
			return nil, errors.Join(ErrDurableStorageRecovery, current.Close())
		}
		info, e := current.Lstat(part)
		if errors.Is(e, os.ErrNotExist) && create {
			e = current.Mkdir(part, 0700)
			if e == nil || errors.Is(e, os.ErrExist) {
				info, e = current.Lstat(part)
			}
		}
		if e != nil {
			return nil, errors.Join(e, current.Close())
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.Join(ErrDurableStorageRecovery, current.Close())
		}
		next, e := current.OpenRoot(part)
		if e == nil {
			opened, x := next.Stat(".")
			named, n := current.Lstat(part)
			if x != nil || n != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, named) || !os.SameFile(info, opened) {
				e = errors.Join(ErrDurableStorageRecovery, next.Close())
				next = nil
			}
		}
		if e == nil && create {
			// Persist every required parent-child link, including existing links
			// encountered when retrying after an earlier failed durability barrier.
			e = syncParent(current)
		}
		e = errors.Join(e, current.Close())
		if e != nil {
			if next != nil {
				e = errors.Join(e, next.Close())
			}
			return nil, e
		}
		current = next
	}
	return current, nil
}

func (q *durableQuota) usage() (u durableUsage, err error) {
	remaining := durableEntryLimit
	for _, dir := range []string{"pending", generationRecoveryDir, "publication-evidence", "temporary-reservations", "temporary-scratch", "admission-stages"} {
		recovery := durableRootPath(dir) == admissionDurableRoot || durableRootPath(dir) == reservationDurableRoot || durableRootPath(dir) == scratchDurableRoot
		e := q.scan(dir, 0, &remaining, &u)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return u, errors.Join(ErrDurableStorageRecovery, e)
		}
		if recovery { // Unavailable owners never provide reservation credit.
			// Presence, rather than aggregate usage from preceding roots, determines refusal.
			present, e := rootHasEntries(q.home.Root, dir)
			if e != nil {
				return u, e
			}
			u.recovery = u.recovery || present
			if present {
				u.charged = durableStorageQuota
			}
		}
	}
	sessions, e := privateDirectory(q.home.Root, "sessions", false)
	if errors.Is(e, os.ErrNotExist) {
		return u, q.finishUsage(u)
	}
	if e != nil {
		return u, errors.Join(ErrDurableStorageRecovery, e)
	}
	defer func() { err = errors.Join(err, sessions.Close()) }()
	d, e := sessions.Open(".")
	if e != nil {
		return u, e
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	for {
		entries, e := d.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return u, e
		}
		for _, entry := range entries {
			remaining--
			if remaining < 0 {
				return u, ErrDurableStorageRecovery
			}
			// sessions also holds native-index JSON files; only directory evidence is scanned.
			info, e := sessions.Lstat(entry.Name())
			if e != nil {
				return u, e
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return u, ErrDurableStorageRecovery
			}
			if !info.IsDir() {
				continue
			}
			if !safeFileComponent(entry.Name()) {
				return u, ErrDurableStorageRecovery
			}
			e = q.scan(filepath.Join("sessions", entry.Name(), "import-stage"), 0, &remaining, &u)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return u, e
			}
			e = q.scan(filepath.Join("sessions", entry.Name(), "pending-sources"), 0, &remaining, &u)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return u, e
			}
		}
		if errors.Is(e, io.EOF) {
			break
		}
	}
	return u, q.finishUsage(u)
}

func (q *durableQuota) finishUsage(u durableUsage) error {
	if u.recovery {
		return ErrDurableStorageRecovery
	}
	return q.home.Check()
}

func (q *durableQuota) scan(path string, depth int, remaining *int, u *durableUsage) (err error) {
	if depth > 64 {
		return ErrDurableStorageRecovery
	}
	root, err := privateDirectory(q.home.Root, path, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	controlled := strings.HasPrefix(path, "publication-evidence/") || strings.HasSuffix(path, "/import-stage")
	first := true
	for {
		entries, e := d.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		if controlled && first && len(entries) > 0 {
			if durableControlBytes > durableStorageQuota-u.charged {
				return ErrDurableStorageCapacity
			}
			u.charged += durableControlBytes
		}
		first = false
		for _, entry := range entries {
			*remaining--
			if *remaining < 0 {
				return ErrDurableStorageRecovery
			}
			info, e := root.Lstat(entry.Name())
			if e != nil {
				return e
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return ErrDurableStorageRecovery
			}
			if info.IsDir() {
				if protectedDurableRoot(path) || strings.HasSuffix(path, "pending-sources") {
					u.recovery = true
				}
				if e = q.scan(filepath.Join(path, entry.Name()), depth+1, remaining, u); e != nil {
					return e
				}
				continue
			}
			if !info.Mode().IsRegular() || info.Size() < 0 {
				return ErrDurableStorageRecovery
			}
			if err := chargeDurableFile(path, entry.Name(), info.Size(), u); err != nil {
				return err
			}
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
	}
}

func (q *durableQuota) write(path string, n int64, write func(io.Writer) error) (err error) {
	if n < 0 || n > durableStorageQuota/2 {
		return ErrDurableStorageCapacity
	}
	u, err := q.usage()
	if err != nil {
		return err
	}
	old := int64(0)
	info, e := q.home.Root.Lstat(path)
	if e == nil {
		if !info.Mode().IsRegular() {
			return ErrDurableStorageRecovery
		}
		old = info.Size()
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	additional := max(int64(0), 2*n-2*old)
	if strings.HasSuffix(filepath.Dir(path), "/import-stage") {
		present, e := rootHasEntries(q.home.Root, filepath.Dir(path))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if !present {
			additional += durableControlBytes
		}
		if filepath.Base(path) == "cancel.json" && present {
			if n > durableControlBytes/2 {
				return ErrDurableStorageCapacity
			}
			additional = 0
		}
	}
	if additional > durableStorageQuota-u.charged {
		return ErrDurableStorageCapacity
	}
	if err = q.guard.CheckHome(q.path); err != nil {
		return err
	}
	dir, err := privateDirectory(q.home.Root, filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	return local.RootedAtomicWrite(q.home.Root, path, func(w io.Writer) error {
		limited := &boundedDurableWriter{writer: w, remaining: n}
		if e := write(limited); e != nil {
			return e
		}
		return q.guard.CheckHome(q.path)
	})
}

type boundedDurableWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *boundedDurableWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrDurableStorageCapacity
	}
	n, e := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, e
}

func (s *Store) savePendingGuard(ctx context.Context, g config.DurableStorageGuard, id string, p PendingPublication) error {
	if len(p.SourceBytes) > maxPendingHistoryBytes {
		return ErrDurableStorageCapacity
	}
	return s.writeDurableGuard(ctx, g, filepath.Join("pending", id+".json"), p)
}

func (s *Store) writeDurableGuard(ctx context.Context, g config.DurableStorageGuard, path string, p any) (err error) {
	if err = g.CheckHome(s.home); err != nil {
		return err
	}
	// Keep the existing independent encoder lease; disk quota never substitutes for memory.
	const scratch = 32 << 10
	limit := durableStorageQuota/2 - 1
	if s.resourceBudget != nil {
		if !s.resourceBudget.Reserve(scratch) {
			return errStateBudget
		}
		limit = min(limit, s.resourceBudget.Available()-1)
	}
	n, err := jsonwire.Bound(ctx, p, limit)
	if s.resourceBudget != nil {
		if errors.Is(err, jsonwire.ErrLimit) && limit < durableStorageQuota/2-1 {
			// Memory, not the disk quota, refused the encoding.
			s.resourceBudget.Refuse(limit + 2)
		}
		s.resourceBudget.Release(scratch)
	}
	if err != nil {
		return errors.Join(errStateBudget, err)
	}
	n++
	if s.resourceBudget != nil {
		if !s.resourceBudget.Reserve(n) {
			return errStateBudget
		}
		defer s.resourceBudget.Release(n)
	}
	q, err := s.openDurableQuota(g)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, q.Close()) }()
	return q.write(path, n, func(w io.Writer) error { return json.NewEncoder(w).Encode(p) })
}

func (s *Store) stagePendingSourceGuard(g config.DurableStorageGuard, id string, ref archive.SourceReference, data []byte) (err error) {
	q, err := s.openDurableQuota(g)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, q.Close()) }()
	return q.write(filepath.Join("sessions", id, "pending-sources", ref.SHA256+".gz"), int64(len(data)), func(w io.Writer) error { _, e := w.Write(data); return e })
}

func chargeDurableFile(path, name string, size int64, u *durableUsage) error {
	if strings.HasSuffix(path, "/import-stage") && name == "cancel.json" {
		if size > durableControlBytes/2 {
			return ErrDurableStorageRecovery
		}
		u.physical += size
		return nil // Charged by the import directory's reserved control allowance.
	}
	if size > (durableStorageQuota-u.charged)/2 {
		return ErrDurableStorageCapacity
	}
	if (protectedDurableRoot(path)) && !strings.HasSuffix(name, ".json") {
		u.recovery = true
	}
	u.physical += size
	u.charged += 2 * size
	if strings.HasSuffix(path, "pending-sources") && !stagedSourceName.MatchString(name) && !stagedTempName.MatchString(name) {
		u.recovery = true
	}
	return nil
}
