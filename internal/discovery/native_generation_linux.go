//go:build linux

package discovery

import "syscall"

func nativeChangeTime(s *syscall.Stat_t) (int64, int64) { return s.Ctim.Sec, s.Ctim.Nsec }
