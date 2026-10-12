//go:build !linux && !darwin

package cursorstore

import (
	"errors"
	"os"
)

// Without a qualified nonblocking opener, settled -shm evidence is unavailable.
// Ordinary database reading keeps its existing independent path.
func openShmHeaderFile(string) (*os.File, error) {
	return nil, errors.ErrUnsupported
}
