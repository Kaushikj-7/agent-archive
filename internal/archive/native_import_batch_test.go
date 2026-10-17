package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func nativeImportBatchFixture(t *testing.T) NativeImportBatchConfirmation {
	t.Helper()
	scope := NativeImportBatchScope{BatchID: "2026-10-17-1", StartedAtUTC: "2026-10-17T12:00:00Z", DestinationID: "destination", Filters: ImportBatchFilters{Harnesses: []string{"muse"}, ProjectIDs: []string{}}, ProjectIDs: []string{"project"}, Selected: []NativeImportBatchSelection{{Kind: "ordinary-root", NativeID: "session", OriginalTUTC: "2026-10-16T12:00:00Z", ProjectID: "project", RootFingerprint: strings.Repeat("a", 64), RootRevision: "one", BindingSHA256: strings.Repeat("b", 64), Source: nativeImportFixture().Confirmation.SelectedSource}}}
	digest, err := NativeImportBatchScopeDigest(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: strings.Repeat("c", 32), ScopeSHA256: digest, Scope: scope}
}

func TestNativeImportBatchClosedConfirmation(t *testing.T) {
	t.Parallel()
	value := nativeImportBatchFixture(t)
	// Frozen pre-repair kind encoding and domain-separated scope digest.
	const expectedScopeDigest = "a232d6eeea3d387e95a24a3de38841bef80dcc46d18444b1a3f16394168b4fa9"
	if value.ScopeSHA256 != expectedScopeDigest {
		t.Fatalf("scope wire digest changed: %s", value.ScopeSHA256)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded NativeImportBatchConfirmation
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"future":            bytes.Replace(raw, []byte("muse-historical-import-v1"), []byte("muse-historical-import-v2"), 1),
		"duplicate":         bytes.Replace(raw, []byte(`"instance":`), []byte(`"instance":"`+value.Instance+`","instance":`), 1),
		"alias":             bytes.Replace(raw, []byte(`"scope":`), []byte(`"Scope":`), 1),
		"unknown":           bytes.Replace(raw, []byte(`"selected":[`), []byte(`"future":true,"selected":[`), 1),
		"null":              bytes.Replace(raw, []byte(`"project_ids":[]`), []byte(`"project_ids":null`), 1),
		"filter-alias":      bytes.Replace(raw, []byte(`"include_home":`), []byte(`"Include_Home":`), 1),
		"filter-duplicate":  bytes.Replace(raw, []byte(`"include_home":`), []byte(`"include_home":true,"include_home":`), 1),
		"changed-proof":     bytes.Replace(raw, []byte(`"boundary":1234`), []byte(`"boundary":1235`), 1),
		"unqualified-child": bytes.Replace(raw, []byte("ordinary-root"), []byte("child"), 1),
		"offset-time":       bytes.Replace(raw, []byte("2026-10-16T12:00:00Z"), []byte("2026-10-16T12:00:00+00:00"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var decoded NativeImportBatchConfirmation
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(decoded)
			if err := decoded.UnmarshalJSON(bad); !errors.Is(err, ErrUnsupportedNativeImport) {
				t.Fatalf("invalid confirmation accepted: %v", err)
			}
			after, _ := json.Marshal(decoded)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal replaced confirmation")
			}
		})
	}
}

func TestNativeImportBatchBoundsAndCancellation(t *testing.T) {
	t.Parallel()
	value := nativeImportBatchFixture(t)
	for i := 1; i < MaxNativeImportBatchSelections; i++ {
		entry := value.Scope.Selected[0]
		entry.NativeID = fmt.Sprintf("session%03d", i)
		value.Scope.Selected = append(value.Scope.Selected, entry)
	}
	if _, err := NativeImportBatchScopeDigest(t.Context(), value.Scope); err != nil {
		t.Fatal("exact count limit refused", err)
	}
	value.Scope.Selected = append(value.Scope.Selected, value.Scope.Selected[0])
	if _, err := NativeImportBatchScopeDigest(t.Context(), value.Scope); err == nil {
		t.Fatal("count overflow accepted")
	}
	value = nativeImportBatchFixture(t)
	// Valid raw strings can expand sixfold under default JSON escaping. Refuse
	// before canonical Marshal, including when subsection input itself was small.
	for i := 1; i < 100; i++ {
		entry := value.Scope.Selected[0]
		entry.NativeID = fmt.Sprintf("session%03d", i)
		entry.ProjectID = fmt.Sprintf("%03d", i) + strings.Repeat("<", 4000)
		value.Scope.Selected = append(value.Scope.Selected, entry)
		value.Scope.ProjectIDs = append([]string{entry.ProjectID}, value.Scope.ProjectIDs...)
	}
	// Project IDs must be sorted for a valid size-only refusal.
	slices.Sort(value.Scope.ProjectIDs)
	if _, err := NativeImportBatchScopeDigest(t.Context(), value.Scope); err == nil {
		t.Fatal("escaped byte overflow accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NativeImportBatchScopeDigest(ctx, nativeImportBatchFixture(t).Scope); !errors.Is(err, context.Canceled) {
		t.Fatal("lost caller cancellation", err)
	}
}

func TestNativeImportBatchControlPreservesCallerCancellation(t *testing.T) {
	t.Parallel()
	n := nativeImportBatchFixture(t)
	started, err := time.Parse(time.RFC3339Nano, n.Scope.StartedAtUTC)
	if err != nil {
		t.Fatal(err)
	}
	b := NativeImportBatchControl{ID: n.Scope.BatchID, StartedAt: started, Filters: n.Scope.Filters, DestinationID: n.Scope.DestinationID, NativeImport: &n}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := b.Validate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("control lost caller cancellation", err)
	}
}
