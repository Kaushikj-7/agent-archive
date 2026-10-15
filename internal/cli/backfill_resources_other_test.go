//go:build !darwin && !linux

package cli

// The benchmark resource observer supports the project's Linux/macOS hosts.
func backfillProcessResources() (float64, int64, error) { return 0, 0, nil }
