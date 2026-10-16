//go:build darwin

package discovery

import "syscall"

func nativeChangeTime(s *syscall.Stat_t) (int64, int64) { return s.Ctimespec.Sec, s.Ctimespec.Nsec }
