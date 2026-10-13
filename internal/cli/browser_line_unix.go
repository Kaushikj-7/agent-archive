//go:build darwin || linux

package cli

import (
	"errors"
	"io"
	"math"
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
	fd := file.Fd()
	if fd > math.MaxInt32 {
		return false, errors.New("browser input descriptor exceeds supported poll range")
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	_, err := unix.Poll(fds, 50)
	return fds[0].Revents != 0, err
}
