//go:build !darwin && !linux

package local

import "os"

func atomicChangeIdentity(os.FileInfo) (int64, int64, bool) { return 0, 0, false }
