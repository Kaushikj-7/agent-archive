//go:build darwin || linux

package cli

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// browserLineReady polls cooked terminal/pipe input without owning another
// reader goroutine. The command retains ownership of stdin throughout.
func browserLineReady(source io.Reader) (bool, error) {
	file, ok := source.(*os.File)
	if !ok {
		return true, nil
	}
	fds := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
	_, err := unix.Poll(fds, 50)
	return fds[0].Revents != 0, err
}
