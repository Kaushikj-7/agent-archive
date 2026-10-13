package backfill

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// An observed old header is not a complete inspection proof. These decisions
// prohibit a header-only date shortcut from hiding body failures or late native
// metadata, even though the valid ordinary case is eventually filtered out.
func TestOldCodexHeadersRequireCompleteInspectionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tail       string
		skip       SkipReason
		knownStart bool
	}{
		{"ordinary", "", SkipFilteredOut, true},
		{"malformed body", `{"type":"session_meta","payload":{"id":42}}` + "\n", SkipUnsafeFormat, false},
		{"late related metadata", `{"type":"session_meta","payload":{"id":"0a9b3c4d-0000-4000-8000-0000000000dd","cwd":"$PROJECT","forked_from_id":"0a9b3c4d-0000-4000-8000-0000000000aa","forked_from_ordinal_exclusive":3,"source":"cli"}}` + "\n", SkipRelatedHistory, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTree(t)
			root := tr.repo("home/project")
			const id = "0a9b3c4d-0000-4000-8000-0000000000dd"
			body := codexTranscript(id, id, root, fixedNow.Add(-30*24*time.Hour))
			tail := tc.tail
			if tc.name == "late related metadata" {
				tail = fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"forked_from_id":"0a9b3c4d-0000-4000-8000-0000000000aa","forked_from_ordinal_exclusive":3,"source":"cli"}}`+"\n", id, root)
			}
			tr.write(filepath.Join("home", codexFile(id)), body+tail)
			c := candidate(t, plan(t, tr.env(), nil, config.Config{}, Filters{Since: "2026-09-16", Harnesses: []string{"codex"}}), id)
			if c.Skip != tc.skip || c.StartedAt.IsZero() == tc.knownStart {
				t.Fatalf("skip %s started %s; want %s known=%v", c.Skip, c.StartedAt, tc.skip, tc.knownStart)
			}
		})
	}
}
