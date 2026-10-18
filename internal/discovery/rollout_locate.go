package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

// maxListingEntries bounds one directory listing a rollout search reads.
const maxListingEntries = 65536

// maxListingBytes bounds the rollout listings one pass keeps, so each
// directory is listed at most once per pass however many lineages it serves.
const maxListingBytes = 4 << 20

// maxObservationBytes bounds the observed rollout facts one pass keeps.
const maxObservationBytes = 8 << 20

// rolloutDayWindow is how many days either side of a rollout's UUIDv7
// creation time are searched: Codex names the day directory by local date,
// which can differ from this process's local date when the time zone changed.
const rolloutDayWindow = 1

// uuidV7Time is the creation time a version 7 UUID carries in its first 48
// bits, in Unix milliseconds.
func uuidV7Time(id string) (time.Time, bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[14] != '7' {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(id[:8]+id[9:13], 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// rolloutDirectories are the native-home-relative directories that can hold
// rollout id: the day directories around its creation time when the ID is a
// UUIDv7, and archived_sessions/, where Codex moves archived rollouts.
func rolloutDirectories(id string) []string {
	var dirs []string
	if created, ok := uuidV7Time(id); ok {
		created = created.Local()
		for delta := -rolloutDayWindow; delta <= rolloutDayWindow; delta++ {
			dirs = append(dirs, filepath.Join("sessions", created.AddDate(0, 0, delta).Format("2006/01/02")))
		}
	}
	return append(dirs, "archived_sessions")
}

// locate finds rollout id by its file name, rollout-<local time>-<id>.jsonl,
// in each approved native home. Only a few directories are listed, and each
// at most once per pass while the pass's listing allowance lasts; past it, a
// directory is searched for id alone without keeping its listing. A directory
// that cannot be searched leaves uniqueness unresolved even when another
// directory holds the rollout. Cached listings remain a bounded pass view;
// they do not establish absence of copies created after a listing was read.
func (l *CodexRolloutLookup) locate(ctx context.Context, id string) ([]string, error) {
	var out []string
	var failed error
	for _, root := range l.roots {
		for _, dir := range rolloutDirectories(id) {
			listing, err := l.listing(ctx, root, dir)
			var paths []string
			switch {
			case errors.Is(err, errListingAllowance):
				paths, err = searchRollouts(ctx, root, dir, id)
			case err == nil:
				paths = listing[id]
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if err != nil {
				if agentapi.HasFailure(err, agentapi.Limit) {
					return nil, err
				}
				if failed == nil {
					failed = err
				}
				continue
			}
			for _, path := range paths {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				present, err := physicalLocatorPresent(path)
				if err != nil {
					return nil, err
				}
				if present && !slices.Contains(out, path) {
					if len(out) >= archive.MaxHistorySpans {
						return nil, agentapi.Wrap(agentapi.Limit, errors.New("physical locator limit"))
					}
					out = append(out, path)
				}
			}
		}
	}
	if failed != nil {
		return nil, failed
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// errListingAllowance reports a directory listing the pass cannot keep.
var errListingAllowance = errors.New("native rollout listing allowance exhausted")

// listing indexes one directory's rollout files by rollout ID. A missing
// directory lists nothing. Listings are charged to the shared ledger for
// the pass and kept until Close; one that does not fit is not kept
// (errListingAllowance).
func (l *CodexRolloutLookup) listing(ctx context.Context, root, dir string) (map[string][]string, error) {
	key := filepath.Join(root, dir)
	if listing, found := l.listings[key]; found {
		return listing, nil
	}
	listing := map[string][]string{}
	charge := int64(0)
	err := walkRollouts(ctx, root, dir, func(id, path string) error {
		n := 2*int64(len(path)+len(id)) + 64
		if l.listingBytes+charge+n > maxListingBytes || !l.readBudget.ReserveUnscoped(n) {
			return errListingAllowance
		}
		charge += n
		listing[id] = append(listing[id], path)
		return nil
	})
	if err != nil {
		l.readBudget.ReleaseUnscoped(charge)
		return nil, err
	}
	if l.listings == nil {
		l.listings = map[string]map[string][]string{}
	}
	l.listings[key] = listing
	l.listingBytes += charge
	return listing, nil
}

// searchRollouts lists one directory for rollout id alone, keeping nothing
// else, for a directory whose whole listing the pass cannot keep.
func searchRollouts(ctx context.Context, root, dir, want string) ([]string, error) {
	var out []string
	err := walkRollouts(ctx, root, dir, func(id, path string) error {
		if id == want {
			if len(out) >= archive.MaxHistorySpans {
				return agentapi.Wrap(agentapi.Limit, errors.New("physical locator limit"))
			}
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// walkRollouts calls visit with each rollout file in one directory, at most
// maxListingEntries entries. A missing directory has none.
func walkRollouts(ctx context.Context, root, dir string, visit func(id, path string) error) error {
	key := filepath.Join(root, dir)
	entries := 0
	d := directory{Root: root, Path: dir}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var names []string
		var next int64
		var complete bool
		err := nativeUnit(ctx, func(context.Context) error {
			var readErr error
			names, next, complete, readErr = readBatch(d)
			return readErr
		})
		if err != nil {
			if nativeInterrupted(err) {
				return err
			}
			return agentapi.Wrap(agentapi.Unavailable, errors.New("native rollout directory unavailable; retry next scan"))
		}
		for _, name := range names {
			entries++
			if entries > maxListingEntries {
				return agentapi.Wrap(agentapi.Unavailable, errors.New("native rollout directory too large to search"))
			}
			if !strings.HasPrefix(name, "rollout-") {
				continue
			}
			id := codexmeta.RolloutID(name)
			if id == "" {
				continue
			}
			if err := visit(id, filepath.Join(key, name)); err != nil {
				return err
			}
		}
		if complete {
			return nil
		}
		if next == d.Offset {
			return agentapi.Wrap(agentapi.Unavailable, errors.New("native rollout directory made no progress"))
		}
		d.Offset = next
	}
}

// rolloutPresent reports a regular file at path. An observation or listing
// can name a rollout Codex has since renamed.
func rolloutPresent(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// physicalLocatorPresent distinguishes a stale renamed locator from a path
// whose current state cannot safely exclude another physical candidate.
func physicalLocatorPresent(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return false, agentapi.Wrap(agentapi.Unavailable, errors.New("native rollout locator unavailable"))
	}
	return true, nil
}
