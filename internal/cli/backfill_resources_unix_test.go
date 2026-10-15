//go:build darwin || linux

package cli

import (
	"runtime"
	"syscall"
)

// backfillProcessResources observes this synthetic process at command return,
// before artifact verification can raise its lifetime RSS high-water mark.
func backfillProcessResources() (float64, int64, error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, 0, err
	}
	cpu := float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	rss := usage.Maxrss
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	return cpu, rss, nil
}
