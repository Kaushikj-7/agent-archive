package cli

import (
	"strings"
	"testing"
)

// Tracing is process-wide, so this test is deliberately not parallel.
func TestBackfillTraceUsesPrivateAggregateMetrics(t *testing.T) {
	f, _ := newImportFixture(t)
	f.env = withTrace(f.env, "1")
	out, diagnostics, code := f.importRun(t, nil, false, "--dry-run", "--harness", "codex", "--since", "7d")
	if code != 0 {
		t.Fatal(code, out, diagnostics)
	}
	for _, want := range []string{"backfill planning", "backfill inventory and recovery", "backfill checking", "native totals:", "header_bytes", "hash_bytes", "filter_input_bytes", "verification_bytes", "adapter_invocations 1", "peak_workers 1", "selected_sessions 1"} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("trace lacks %q: %s", want, diagnostics)
		}
	}
	f.checkPrivate(t, "trace", diagnostics)
	if strings.Contains(diagnostics, f.root) {
		t.Fatal("trace exposes source path")
	}
}

// Command tracing is process-wide; this pins real cold and warm queue coverage.
func TestBackfillTraceReportsVerifiedCacheCoverage(t *testing.T) {
	f, _ := newImportFixture(t)
	f.env = withTrace(f.env, "1")
	_, cold, code := f.importRun(t, nil, false, "--dry-run", "--harness", "codex")
	if code != 0 {
		t.Fatal(code, cold)
	}
	for _, want := range []string{"adapter_invocations 1", "inspection_cache_lookups 1", "inspection_cache_queued 1", "inspection_cache_stored 1"} {
		if !strings.Contains(cold, want) {
			t.Errorf("cold trace missing %q: %s", want, cold)
		}
	}
	_, warm, code := f.importRun(t, nil, false, "--dry-run", "--harness", "codex")
	if code != 0 {
		t.Fatal(code, warm)
	}
	if strings.Contains(warm, "adapter_invocations 1") || !strings.Contains(warm, "inspection_cache_hits 1") || strings.Contains(warm, "inspection_cache_queued 1") {
		t.Fatalf("warm cache trace: %s", warm)
	}
	f.checkPrivate(t, "cold trace", cold)
	f.checkPrivate(t, "warm trace", warm)
}
