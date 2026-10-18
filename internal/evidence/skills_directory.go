package evidence

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
)

var errSkillDirectoryAcquired = errors.New("acquired skill directory inventory unavailable")

type skillDirectory interface {
	ReadDir(int) ([]os.DirEntry, error)
	Close() error
}

// readSkillEntries closes the acquired directory before inspecting any entry.
// Overflow proves only that a complete bounded inventory is unavailable.
func readSkillEntries(dir skillDirectory, ctx context.Context) ([]os.DirEntry, error) {
	var entries []os.DirEntry
	var readErr error
	if ctx != nil {
		readErr = ctx.Err()
	}
	if readErr == nil {
		entries, readErr = dir.ReadDir(maxSkillsPerRoot + 1)
	}
	//lint:ignore errorlint Only bare EOF is completion; wrapped or joined EOF remains a failure.
	if readErr == io.EOF { //nolint:errorlint // Only bare EOF completes the directory; wrapped or joined EOF must survive.
		readErr = nil
	}
	closeErr := dir.Close()
	var contextErr error
	if ctx != nil {
		contextErr = ctx.Err()
	}
	if err := errors.Join(readErr, closeErr, contextErr); err != nil {
		return nil, errors.Join(errSkillDirectoryAcquired, err)
	}
	if len(entries) > maxSkillsPerRoot {
		return nil, errSkillDirectoryAcquired
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func skillRootAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) && !errors.Is(err, errSkillDirectoryAcquired)
}
