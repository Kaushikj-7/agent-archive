package collector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type labelOwnershipMutation string

const (
	labelMachineChange      labelOwnershipMutation = "machine"
	labelOwnerChange        labelOwnershipMutation = "owner"
	labelOwnerBackoffChange labelOwnershipMutation = "owner-backoff"
	labelArchiveChange      labelOwnershipMutation = "archive"
	labelNativeChange       labelOwnershipMutation = "native"
	labelMissingMetadata    labelOwnershipMutation = "missing"
	labelMalformedMetadata  labelOwnershipMutation = "malformed"
)

func TestExternalRenameRequiresCurrentPublishedOwnerBeforeLookup(t *testing.T) {
	t.Run("legacy", func(t *testing.T) { testExternalRenameRequiresCurrentPublishedOwner(t, false) })
	t.Run("source-set", func(t *testing.T) { testExternalRenameRequiresCurrentPublishedOwner(t, true) })
}

func testExternalRenameRequiresCurrentPublishedOwner(t *testing.T, selecting bool) {
	for _, tc := range []struct {
		name   string
		warm   bool
		mutate labelOwnershipMutation
	}{
		{"current machine changed", false, labelMachineChange},
		{"current machine changed with cached context", true, labelMachineChange},
		{"published owner changed with same source", true, labelOwnerChange},
		{"owed cached name during owner-change backoff", true, labelOwnerBackoffChange},
		{"published archive identity changed", true, labelArchiveChange},
		{"published native identity changed", true, labelNativeChange},
		{"published metadata missing", true, labelMissingMetadata},
		{"published metadata malformed", true, labelMalformedMetadata},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
			reg := registration(t, path)
			reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
			now := reg.RegisteredAt.Add(time.Hour)
			provider := &mutableLabels{}
			filter := &operationFilter{}
			bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
			opts := Options{Sources: bindings, Parsers: bindings, Labels: mutableLabelLookup{provider}, MachineID: "old-owner", Now: func() time.Time { return now }}
			run := func() {
				t.Helper()
				if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
					t.Fatalf("%+v %v", result, err)
				}
			}
			run()
			// Exercise the legacy ownership check independently of v5's
			// selecting-body checksum, which rejects an edited body earlier.
			// These files still carry the original source revision after an
			// owner edit, so that revision cannot authorize native naming.
			if !selecting {
				editPublishedState(t, local, func(map[string]any) {})
			}
			if tc.warm {
				now = now.Add(time.Hour)
				run()
			}
			before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			if tc.mutate == labelOwnerBackoffChange {
				cache, err := local.LoadLabels()
				if err != nil {
					t.Fatal(err)
				}
				entry := cache.Entries[reg.ArchiveSessionID]
				entry.Label = archive.SessionLabel{NativeID: reg.NativeSessionID, State: archive.SessionLabelPresent, Name: "Unpublished cached name", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
				entry.ObservedAt, entry.NextAt = now, now.Add(time.Hour)
				cache.Entries[reg.ArchiveSessionID] = entry
				if err := local.SaveLabels(cache); err != nil {
					t.Fatal(err)
				}
			}
			originalChecksum, _, err := local.LabelRevision(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate == labelMachineChange {
				opts.MachineID = "new-owner"
			} else {
				published, err := local.LoadPublishedState(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				metadata := before
				switch tc.mutate {
				case labelMachineChange, labelMissingMetadata, labelMalformedMetadata:
					// These cases change the current machine or encoded bytes outside this projection.
				case labelOwnerChange, labelOwnerBackoffChange:
					metadata.MachineID = "other-owner"
				case labelArchiveChange:
					metadata.SessionID = "other-session"
				case labelNativeChange:
					metadata.NativeSessionID = "other-native"
				}
				encoded, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if tc.mutate == labelMissingMetadata {
					encoded = nil
				}
				if tc.mutate == labelMalformedMetadata {
					encoded = []byte(labelMalformedMetadata)
				}
				injectPublishedMetadata(t, local, reg.ArchiveSessionID, published.Metadata(), encoded)
				checksum, _, err := local.LabelRevision(reg.ArchiveSessionID)
				if selecting {
					if !errors.Is(err, state.ErrDurableStorageRecovery) || checksum != "" {
						t.Fatalf("edited selecting body authorized a label revision: %q %v", checksum, err)
					}
				} else if err != nil || checksum != originalChecksum {
					t.Fatalf("owner edit changed source revision: %s %s %v", originalChecksum, checksum, err)
				}
			}
			if tc.mutate == labelOwnerBackoffChange {
				now = now.Add(30 * time.Second)
			} else {
				now = now.Add(time.Hour)
			}
			provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "New native name", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
			calls := provider.calls
			filter.calls = 0
			remote.keys = nil
			result, runErr := Run(context.Background(), local, remote, opts)
			if runErr != nil {
				t.Fatal(runErr)
			}
			if !selecting || tc.mutate == labelMachineChange {
				if len(result.Errors) != 0 {
					t.Fatal(result)
				}
			} else if !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrDurableStorageRecovery) || len(result.Published) != 0 {
				t.Fatalf("invalid selecting metadata must refuse before native work: %+v", result)
			}
			after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			if provider.calls != calls || filter.calls != 0 || len(remote.keys) != 0 || after.MachineID != before.MachineID || after.Name != before.Name || after.SourceBundle != before.SourceBundle {
				t.Fatalf("foreign label caused native or publication work: calls=%d/%d filters=%d before=%+v after=%+v keys=%v", calls, provider.calls, filter.calls, before, after, remote.keys)
			}
		})
	}
}
