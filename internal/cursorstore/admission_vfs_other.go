//go:build !darwin && !linux

package cursorstore

import (
	"context"
	"errors"
	"os"
)

type admissionVFS struct{ name string }

func newAdmissionVFS(map[string]os.FileInfo, bool, int64) (*admissionVFS, error) {
	return nil, errors.New("bounded live admission VFS unavailable on this platform")
}

func (*admissionVFS) Close() error { return nil }

func (*admissionVFS) copyStats(*AdmissionCopyStats) {}

func newAdmissionDestinationVFS(context.Context, string, *os.File, int64) (*admissionVFS, error) {
	return nil, errors.New("bounded descriptor destination unavailable")
}

func (*admissionVFS) setSourceRoot(*os.Root, string) {}

func lockAdmissionRegistry() func() { return func() {} }
