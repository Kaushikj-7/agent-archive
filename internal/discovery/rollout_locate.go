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
// at most once per pass.
func (l *CodexRolloutLookup) locate(ctx context.Context, id string) ([]string, error) {
	var out []string
	for _, root := range l.roots {
		for _, dir := range rolloutDirectories(id) {
			listing, err := l.listing(ctx, root, dir)
			if err != nil {
				return nil, err
			}
			for _, path := range listing[id] {
				if rolloutPresent(path) {
					out = append(out, path)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// listing indexes one directory's rollout files by rollout ID. A missing
// directory lists nothing. Listings are charged to the shared ledger for
// the pass and kept until Close.
func (l *CodexRolloutLookup) listing(ctx context.Context, root, dir string) (map[string][]string, error) {
	key := filepath.Join(root, dir)
	if listing, found := l.listings[key]; found {
		return listing, nil
	}
	listing := map[string][]string{}
	charge := int64(0)
	release := func() { l.readBudget.ReleaseUnscoped(charge) }
	entries := 0
	d := directory{Root: root, Path: dir}
	for {
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		names, next, complete, err := readBatch(d)
		if err != nil {
			release()
			return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("native rollout directory unavailable; retry next scan"))
		}
		for _, name := range names {
			entries++
			if entries > maxListingEntries {
				release()
				return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("native rollout directory too large to search"))
			}
			if !strings.HasPrefix(name, "rollout-") {
				continue
			}
			id := codexmeta.RolloutID(name)
			if id == "" {
				continue
			}
			path := filepath.Join(key, name)
			n := 2*int64(len(path)+len(id)) + 64
			if l.listingBytes+charge+n > maxListingBytes || !l.readBudget.ReserveUnscoped(n) {
				release()
				return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("native rollout listing budget exhausted; retry next scan"))
			}
			charge += n
			listing[id] = append(listing[id], path)
		}
		if complete {
			break
		}
		d.Offset = next
	}
	if l.listings == nil {
		l.listings = map[string]map[string][]string{}
	}
	l.listings[key] = listing
	l.listingBytes += charge
	return listing, nil
}

// rolloutPresent reports a regular file at path. An observation or listing
// can name a rollout Codex has since renamed.
func rolloutPresent(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
