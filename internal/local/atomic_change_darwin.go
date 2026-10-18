//go:build darwin

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
	return s.Ctimespec.Sec, s.Ctimespec.Nsec, true
}
