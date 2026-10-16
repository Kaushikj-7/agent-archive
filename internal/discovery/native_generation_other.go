//go:build !darwin && !linux

package discovery

import "syscall"

func nativeChangeTime(*syscall.Stat_t) (int64, int64) { return 0, 0 }
