//go:build linux || darwin

package cursorstore

import (
	"os"
	"syscall"
)

// openShmHeaderFile refuses a replaced FIFO without blocking at open, and
// refuses final-component symlinks. The caller verifies regular-file identity.
func openShmHeaderFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
