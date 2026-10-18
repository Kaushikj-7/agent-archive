//go:build linux

package local

import (
	"os"
	"syscall"
)

func atomicChangeIdentity(info os.FileInfo) (int64, int64, bool) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return s.Ctim.Sec, s.Ctim.Nsec, true
}
