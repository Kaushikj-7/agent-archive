package state

import (
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
)

// These small fixtures exercise the same admission preflight as the real-size
// backfill regressions, including exact inclusive boundaries in short mode.
func TestImportStageCapacityEncodedBoundary(t *testing.T) {
	t.Parallel()
	for _, content := range []string{strings.Repeat("synthetic ", 128), strings.Repeat("<>&\u2028界", 128)} {
		stage := ImportStage{Bundle: archive.SourceBundle{NativeText: []archive.TextTranscript{{Content: content}}}}
		n, err := jsonwire.Bound(t.Context(), stage, MaxImportStageBytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, delta := range []int64{-1, 0, 1} {
			err := validateImportStageCapacityWithin(t.Context(), stage, n+delta, 1<<20)
			if delta < 0 {
				if !errors.Is(err, ErrDurableStorageCapacity) || !errors.Is(err, jsonwire.ErrLimit) {
					t.Fatalf("encoded bound %d: %v", n+delta, err)
				}
			} else if err != nil {
				t.Fatalf("encoded bound %d: %v", n+delta, err)
			}
		}
	}
	// Equal raw text lengths have different encoded reservations. HTML escaping
	// must count before admission, rather than trusting the raw capture boundary.
	plain := ImportStage{Bundle: archive.SourceBundle{NativeText: []archive.TextTranscript{{Content: strings.Repeat("x", 1024)}}}}
	encoded := plain
	encoded.Bundle.NativeText = []archive.TextTranscript{{Content: strings.Repeat("<", 1024)}}
	limit, err := jsonwire.Bound(t.Context(), plain, MaxImportStageBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateImportStageCapacityWithin(t.Context(), plain, limit, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := validateImportStageCapacityWithin(t.Context(), encoded, limit, 1<<20); !errors.Is(err, jsonwire.ErrLimit) || !errors.Is(err, ErrDurableStorageCapacity) {
		t.Fatalf("encoded expansion admitted: %v", err)
	}
}

func TestImportStageCapacityRefilterBoundary(t *testing.T) {
	t.Parallel()
	for _, preserved := range []bool{false, true} {
		// Sixteen 1 KiB texts need three largest-record owners, the scanner's
		// minimum 64 KiB buffer, and two output owners for all retained text,
		// in addition to the encoded stage itself.
		const recordBytes = 1024
		const retainedBytes = 16 << 10
		bundle := archive.SourceBundle{
			Capture: archive.SourceCapture{Boundary: archive.CaptureBoundary{RetainedBytes: retainedBytes}},
		}
		for range retainedBytes / recordBytes {
			bundle.NativeText = append(bundle.NativeText, archive.TextTranscript{Content: strings.Repeat("s", recordBytes)})
		}
		retained := int64(retainedBytes)
		var preservedBundles []archive.SourceBundle
		if preserved {
			preservedBundles = []archive.SourceBundle{bundle}
			retained *= 2
		}
		stage := ImportStage{Bundle: bundle, Preserved: preservedBundles}
		n, err := jsonwire.Bound(t.Context(), stage, MaxImportStageBytes)
		if err != nil {
			t.Fatal(err)
		}
		allowance := n + 3*recordBytes + (64 << 10) + 2*retained
		for _, delta := range []int64{-1, 0, 1} {
			err := validateImportStageCapacityWithin(t.Context(), stage, n, allowance+delta)
			if delta < 0 {
				if !errors.Is(err, ErrDurableStorageCapacity) || errors.Is(err, jsonwire.ErrLimit) {
					t.Fatalf("preserved=%v refilter allowance %d: %v", preserved, allowance+delta, err)
				}
			} else if err != nil {
				t.Fatalf("preserved=%v refilter allowance %d: %v", preserved, allowance+delta, err)
			}
		}
	}
}
