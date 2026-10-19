//go:build unix

package evidence

import (
	"os"
	"syscall"
)

func skillEntryOwned(info os.FileInfo) bool {
	value, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(value.Uid) == os.Getuid() && (!info.Mode().IsRegular() || value.Nlink == 1)
}

// Open without following the final link or waiting on a substituted FIFO.
// os.Root confines all ancestor resolution to its existing anchored directory.
func openConfinedSkillFile(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
