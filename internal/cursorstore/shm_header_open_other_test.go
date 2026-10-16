//go:build !linux && !darwin

package cursorstore

import "testing"

func TestShmHeaderRejectsFIFOReplacementWithoutBlocking(t *testing.T) {
	t.Skip("nonblocking settled-shm opener is not qualified on this platform")
}
