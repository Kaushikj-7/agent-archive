package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
)

// collectorLockRecordName is the file in the data directory that says who
// holds collector.lock and since when. flock itself records neither, so
// without it status could only guess from the age of the last scan, which
// cannot tell a pass that started a moment ago (after the machine woke, say)
// from one that has hung.
const collectorLockRecordName = "collector-lock.json"

// collectorLockRecord is written by the holder right after taking
// collector.lock and removed just before releasing it. A holder that dies
// leaves it behind, but then nothing holds the lock, and status only reads
// the record while the lock is held.
type collectorLockRecord struct {
	// Holder names the command: "collect", "sync", "backfill import", ...
	Holder string    `json:"holder"`
	PID    int       `json:"pid"`
	Since  time.Time `json:"since"`
}

// collectLockStuckAfter is how long collector.lock can be held before
// status calls collection stuck: twice the hard deadline a collection pass
// runs under (collectHardDeadline), so a pass that is slow but working, a
// large upload over a slow link, is never reported.
var collectLockStuckAfter = 2 * collectHardDeadline

// lockCollector takes collector.lock without waiting and records holder.
func lockCollector(home, holder string, now time.Time) (func(), error) {
	return recordCollectorLock(home, holder, now, func() (func(), error) { return local.Lock(home) })
}

func lockCollectorGuard(home, holder string, now time.Time, format destination.ArchiveFormat) (*local.CollectorGuard, func(), error) {
	if format != destination.FormatCatalogV4 {
		unlock, err := lockCollector(home, holder, now)
		return nil, unlock, err
	}
	var guard *local.CollectorGuard
	unlock, err := recordCollectorLock(home, holder, now, func() (func(), error) {
		var e error
		guard, e = local.LockCollectorGuard(home)
		if e != nil {
			return nil, e
		}
		return guard.Release, nil
	})
	return guard, unlock, err
}

// lockCollectorWait is lockCollector, waiting up to wait for the lock.
func lockCollectorWait(home, holder string, now time.Time, wait time.Duration) (func(), error) {
	return recordCollectorLock(home, holder, now, func() (func(), error) {
		return local.NamedLockWait(home, "collector.lock", wait)
	})
}

func recordCollectorLock(home, holder string, now time.Time, lock func() (func(), error)) (func(), error) {
	unlock, err := lock()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, collectorLockRecordName)
	// A record a dead holder left behind names a process that no longer
	// holds the lock (its PID may even be reused), so it goes first: if the
	// write below fails, no record is better than a wrong one.
	_ = os.Remove(path)
	// Best effort: without the record status cannot call a hold stuck, which
	// is the safe direction to fail in.
	_ = local.Write(path, collectorLockRecord{Holder: holder, PID: os.Getpid(), Since: now.UTC()})
	return func() {
		_ = os.Remove(path) // a record left behind is overwritten by the next holder
		unlock()
	}, nil
}

// durationAgo renders a positive duration as "3h 5m ago" (or "5m ago").
func durationAgo(d time.Duration) string {
	d = d.Round(time.Minute)
	hours, minutes := int(d.Hours()), int(d.Minutes())%60
	if hours == 0 {
		return fmt.Sprintf("%dm ago", minutes)
	}
	return fmt.Sprintf("%dh %dm ago", hours, minutes)
}

// readCollectorLockRecord returns the current record, if any.
func readCollectorLockRecord(home string) (collectorLockRecord, bool) {
	var record collectorLockRecord
	if err := local.Read(filepath.Join(home, collectorLockRecordName), &record); err != nil || record.Since.IsZero() {
		return collectorLockRecord{}, false
	}
	return record, true
}

// processAlive reports whether pid names a running process. EPERM means it
// exists but belongs to someone else, which still counts as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
