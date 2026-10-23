package state

import (
	"context"
	"errors"
	"io"
	"os"
)

// The directory batch bounds resident enumeration work, not the complete
// inventory. Only actual EOF and successful closes complete a census.
const durableDirectoryBatch = 128

type durableDirectoryReader interface {
	ReadDir(int) ([]os.DirEntry, error)
}

func readDurableDirectory(ctx context.Context, dir durableDirectoryReader) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(durableDirectoryBatch)
	if len(entries) == 0 && err == nil {
		return nil, errors.Join(ErrDurableStorageRecovery, io.ErrNoProgress)
	}
	return entries, err
}

// Count complete traversal work without turning an allocation window into an
// inventory ceiling. Overflow leaves the census incomplete.
func countDurableEntry(visited *int) error {
	if *visited == int(^uint(0)>>1) {
		return ErrDurableStorageRecovery
	}
	*visited++
	return nil
}
