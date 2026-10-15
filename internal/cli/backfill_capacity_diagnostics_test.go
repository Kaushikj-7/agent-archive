package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestCapacityDrainReportsOwnerFailuresAndActualProgress(t *testing.T) {
	t.Parallel()
	ownerErr := errors.New("synthetic pending write refusal")
	for _, progress := range []bool{false, true} {
		t.Run(map[bool]string{false: "no progress", true: "partial progress"}[progress], func(t *testing.T) {
			t.Parallel()
			var published []string
			if progress {
				published = []string{"verified"}
			}
			result := collector.Result{Published: published, Errors: map[string]error{"owner": ownerErr}, Deferred: map[string]string{"other": "read budget"}}
			var out bytes.Buffer
			err := reportImportCapacityDrain(&out, result, nil)
			if progress && err != nil {
				t.Fatal(err)
			}
			if !progress && (!errors.Is(err, state.ErrDurableStorageCapacity) || !errors.Is(err, ownerErr)) {
				t.Fatalf("owner failure was lost: %v", err)
			}
			for _, want := range []string{"owner", "synthetic pending write refusal", "deferred", "retained evidence remains pending"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q: %s", want, out.String())
				}
			}
			if strings.Contains(out.String(), "Freed staging capacity") != progress {
				t.Fatal("reported publication without actual progress")
			}
		})
	}
}
