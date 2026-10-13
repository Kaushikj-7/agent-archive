//go:build !darwin && !linux

package cli

import "io"

func browserLineReady(io.Reader) (bool, error) { return true, nil }
