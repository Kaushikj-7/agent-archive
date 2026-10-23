package state

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/trace"
)

var errImportStageMembershipChanged = errors.New("import stage directory membership changed; retry recovery")

var errImportAdmissionsPaused = errors.New("import admissions paused")

type importStageNames struct {
	home    *local.RootedHome
	dir     *os.Root
	stamp   os.FileInfo
	names   []string
	owner   stateReservation
	budget  *agentapi.NativeReadBudget
	charged int64
	reads   int
	entries int
}

// Count at the actual ReadDir call, after readDurableDirectory's cancellation
// check. Counts belong to one acquisition, including its partial error work.
type importStageNameReader struct {
	source durableDirectoryReader
	owner  *importStageNames
}

func (r importStageNameReader) ReadDir(limit int) ([]os.DirEntry, error) {
	if err := countDurableEntry(&r.owner.reads); err != nil {
		return nil, err
	}
	entries, err := r.source.ReadDir(limit)
	if len(entries) > int(^uint(0)>>1)-r.owner.entries {
		return nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	r.owner.entries += len(entries)
	return entries, err
}

func (n *importStageNames) Close() error {
	var err error
	if n.dir != nil {
		err = n.dir.Close()
	}
	if n.home != nil {
		err = errors.Join(err, n.home.Close())
	}
	n.names = nil
	n.owner.Release(n.charged)
	n.charged = 0
	return err
}

func (n *importStageNames) reserve(size int64) error {
	if !n.owner.Reserve(size) {
		return errStateBudget
	}
	n.charged += size
	return nil
}

func (s *Store) openImportStageNames(ctx context.Context) (_ *importStageNames, err error) {
	budget := s.resourceBudget
	if budget == nil {
		budget = agentapi.NewNativeReadBudget(128 << 20)
	}
	owner := s.readReservation()
	if s.resourceBudget == nil {
		owner = budget.Reservation()
	}
	n := &importStageNames{owner: owner, budget: budget}
	measurement := trace.Start("import stage names")
	defer func() {
		measurement.Count("directory reads", n.reads)
		measurement.Count("acquired entries", n.entries)
		measurement.End()
	}()
	defer func() {
		if err != nil {
			err = errors.Join(err, n.Close())
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// Directory entries, their maximum filesystem names, and sorting's stack
	// coexist with the retained index. Reserve before ReadDir or allocation.
	const scratch int64 = durableDirectoryBatch*1024 + 4096
	if err = n.reserve(scratch); err != nil {
		return nil, err
	}
	defer func() { n.owner.Release(scratch); n.charged -= scratch }()
	n.home, err = local.OpenRootedHome(s.home)
	if err != nil {
		return nil, err
	}
	n.dir, err = privateDirectory(n.home.Root, "sessions", false)
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return nil, err
	}
	n.stamp, err = n.dir.Stat(".")
	if err != nil {
		return nil, err
	}
	dir, err := n.dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	reader := importStageNameReader{source: dir, owner: n}
	for {
		entries, e := readDurableDirectory(ctx, reader)
		if e != nil && !errors.Is(e, io.EOF) {
			return nil, e
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !safeFileComponent(name) {
				return nil, ErrDurableStorageRecovery
			}
			if len(n.names) == cap(n.names) {
				capacity := max(128, 2*cap(n.names))
				// The new and old arrays coexist while copying. Strings are
				// independently charged and sorting rearranges only headers.
				if err = n.reserve(int64(capacity) * 16); err != nil {
					return nil, err
				}
				names := make([]string, len(n.names), capacity)
				copy(names, n.names)
				n.owner.Release(int64(cap(n.names)) * 16)
				n.charged -= int64(cap(n.names)) * 16
				n.names = names
			}
			if err = n.reserve(int64(len(name))); err != nil {
				return nil, err
			}
			n.names = append(n.names, strings.Clone(name))
		}
		if errors.Is(e, io.EOF) {
			break
		}
	}
	slices.Sort(n.names)
	if err = n.current(ctx); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *importStageNames) current(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	named, err := n.home.Root.Lstat("sessions")
	if n.dir == nil && errors.Is(err, os.ErrNotExist) {
		return n.home.Check()
	}
	if err != nil || n.dir == nil {
		return errors.Join(errImportStageMembershipChanged, err)
	}
	held, e := n.dir.Stat(".")
	if e != nil || !sameDurableStamp(n.stamp, named) || !sameDurableStamp(n.stamp, held) {
		return errors.Join(errImportStageMembershipChanged, e)
	}
	return n.home.Check()
}

func (n *importStageNames) page(after string, limit int) (start, end int, next string) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	start = sort.Search(len(n.names), func(i int) bool { return n.names[i] > after })
	end = min(len(n.names), start+limit)
	if end < len(n.names) {
		next = n.names[end-1]
	}
	return start, end, next
}

// VisitImportStagePages acquires one charged, content-free lexical name index
// for an explicit full traversal. Each callback borrows at most 50 freshly read
// descriptors until it returns. The index is progress only: current stage and
// control reads still determine ownership, cancellation, and release. Observed
// membership changes return an incomplete traversal, requiring a fresh retry.
func (s *Store) VisitImportStagePages(ctx context.Context, after string, visit func([]ImportStage, string) error) (err error) {
	read, ctx, closeRead := s.withImportResultScope(ctx)
	defer closeRead()
	names, err := read.openImportStageNames(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, names.Close()) }()
	for {
		if err = names.current(ctx); err != nil {
			return err
		}
		start, end, next := names.page(after, 50)
		page, closePage := read.WithReadBudget(ctx, read.resourceBudget)
		stages, e := page.loadImportStagePage(ctx, names.home, names.names[start:end], names.budget)
		if e == nil {
			e = visit(stages, next)
		}
		closePage()
		if e != nil {
			return e
		}
		if err = names.current(ctx); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		after = next
	}
}
