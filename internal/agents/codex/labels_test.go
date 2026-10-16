package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const labelTestID = "01900000-0000-7000-8000-000000000001"

func labelFixture(t *testing.T) (string, agentapi.LabelRequest) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic-label", NativeSessionID: labelTestID, Harness: archive.Harness{Name: "codex", Version: "0.159.2"}, TranscriptPath: path}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: labelTestID, ProjectID: "project", Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: "0.16.0", FilterVersion: archive.FilterVersion, CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, NativeRecords: []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": labelTestID, "cli_version": "0.159.2"}}, {"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "Invented prompt"}}}}
	return root, agentapi.LabelRequest{Registration: reg, Bundle: bundle}
}

func labelIndex(t *testing.T, root string, names ...string) {
	t.Helper()
	out := []byte{}
	for i, name := range names {
		b, _ := json.Marshal(map[string]any{"id": labelTestID, "thread_name": name, "updated_at": time.Date(2030-i, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)})
		out = append(out, b...)
		out = append(out, '\n')
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), out, 0600); err != nil {
		t.Fatal(err)
	}
}

func lookupLabel(root string, request agentapi.LabelRequest) (archive.SessionLabel, bool) {
	m := (LabelProvider{}).LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{root}}, []agentapi.LabelRequest{request})
	l, ok := m[request.Registration.ArchiveSessionID]
	return l, ok
}

func TestLabelsIndexUsesLastNonemptyPhysicalRecord(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Old name", "Latest physical name", "")
	got, ok := lookupLabel(root, request)
	if !ok || got.State != archive.SessionLabelPresent || got.Name != "Latest physical name" || got.Source != archive.SessionLabelIndex || got.Contract != LabelContract {
		t.Fatalf("%+v %v", got, ok)
	}
}

// A complete index without the thread is confirmed absence, so the title falls
// back to the first prompt; a missing index is an empty one.
func TestLabelsCompleteIndexWithoutTheThreadIsAbsence(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	got, ok := lookupLabel(root, request)
	if !ok || got.State != archive.SessionLabelAbsent || got.Name != "" {
		t.Fatalf("missing index: %+v %v", got, ok)
	}
	other, err := json.Marshal(map[string]any{"id": "01900000-0000-7000-8000-000000000002", "thread_name": "Another thread", "updated_at": "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), append(other, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := lookupLabel(root, request); !ok || got.State != archive.SessionLabelAbsent {
		t.Fatalf("other thread only: %+v %v", got, ok)
	}
}

// Codex keeps state_5.sqlite in WAL mode while it runs, and threads written
// by Codex after 0.159.2 carry their own producer version. Neither stops the
// index from naming a thread: the earlier resolver required a settled
// database and producer 0.159.2, so it named no current thread at all.
func TestLabelsNameCurrentProducerWhileCodexRuns(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	request.Registration.Harness.Version = "0.160.1"
	labelIndex(t, root, "Review book generation quality")
	for _, side := range []string{"state_5.sqlite", "state_5.sqlite-wal", "state_5.sqlite-shm"} {
		if err := os.WriteFile(filepath.Join(root, side), []byte("live"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := lookupLabel(root, request)
	if !ok || got.Name != "Review book generation quality" {
		t.Fatalf("%+v %v", got, ok)
	}
}

func TestLabelsUnfinishedOrOversizedIndexIsUnavailable(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), []byte(`{"id":"`+labelTestID+`","thread_name":"Half`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := lookupLabel(root, request); ok {
		t.Fatalf("unfinished record proved a name or absence: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), make([]byte, labelIndexBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := lookupLabel(root, request); ok {
		t.Fatalf("oversized index proved a name or absence: %+v", got)
	}
}

// Only an admitted top-level thread whose rollout lies in exactly one
// approved home is looked up.
func TestLabelsRefuseChildFrozenAndUnownedThreads(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*agentapi.LabelRequest, string){
		"child":           func(r *agentapi.LabelRequest, _ string) { r.Registration.NativeChild = true },
		"frozen":          func(r *agentapi.LabelRequest, _ string) { r.Registration.CaptureFrozen = true },
		"outside home":    func(r *agentapi.LabelRequest, _ string) { r.Registration.TranscriptPath = "/outside/rollout.jsonl" },
		"other discovery": func(r *agentapi.LabelRequest, _ string) { r.Registration.DiscoveryRoot = "/other/.codex" },
		"other harness":   func(r *agentapi.LabelRequest, _ string) { r.Registration.Harness.Name = "claude" },
		"context of another thread": func(r *agentapi.LabelRequest, _ string) {
			r.Context = agentapi.LabelContext{NativeID: "01900000-0000-7000-8000-000000000009", Contract: LabelContract}
		},
	} {
		root, request := labelFixture(t)
		labelIndex(t, root, "A name")
		change(&request, root)
		if got, ok := lookupLabel(root, request); ok {
			t.Fatalf("%s: looked up %+v", name, got)
		}
	}
	root, request := labelFixture(t)
	labelIndex(t, root, "A name")
	if got := (LabelProvider{}).LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{root, root}}, []agentapi.LabelRequest{request}); len(got) != 0 {
		t.Fatalf("a rollout in two approved homes was looked up: %+v", got)
	}
}

func TestLabelsRegistrationContextNeedsOnlyTheNativeThread(t *testing.T) {
	t.Parallel()
	_, request := labelFixture(t)
	proof, ok := (LabelProvider{}).RegistrationLabelContext(request.Registration)
	if !ok || proof.NativeID != labelTestID || proof.Contract != LabelContract {
		t.Fatalf("%+v %v", proof, ok)
	}
	request.Registration.NativeSessionID = "not-a-thread"
	if _, ok := (LabelProvider{}).RegistrationLabelContext(request.Registration); ok {
		t.Fatal("a non-UUID native ID got a lookup context")
	}
}

// Names retained under the earlier settled-file contracts still name the
// session; an unknown contract never does.
func TestLabelsEarlierContractsStillName(t *testing.T) {
	t.Parallel()
	for _, contract := range []string{string(legacyLabelInterpretation), string(settledLabelInterpretation), LabelContract} {
		_, request := labelFixture(t)
		label := archive.SessionLabel{NativeID: labelTestID, State: archive.SessionLabelPresent, Name: "Retained name", Source: archive.SessionLabelDatabase, Contract: contract}
		request.Bundle.SupplementalEvidence = []archive.SupplementalEvidence{label.Evidence(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "codex")}
		analysis, err := (Parser{}).Parse(context.Background(), request.Bundle)
		if err != nil {
			t.Fatal(err)
		}
		if analysis.Facts.Name != "Retained name" {
			t.Fatalf("%s: name %q", contract, analysis.Facts.Name)
		}
	}
}

// The index keeps Codex's own reading: a record without updated_at is
// skipped, and thread IDs compare without case.
func TestLabelsIndexMatchesNativeRequiredStringAndUUIDSemantics(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	entries := []map[string]any{
		{"id": labelTestID, "thread_name": "Old name", "updated_at": "unknown"},
		{"id": strings.ToUpper(labelTestID), "thread_name": "Current name", "updated_at": ""},
		{"id": labelTestID, "thread_name": "Missing required stamp"},
	}
	data := []byte{}
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if label, ok := lookupLabel(root, request); !ok || label.Name != "Current name" {
		t.Fatalf("%+v %v", label, ok)
	}
}

func TestLabelsReadDoesNotMutateNativeHome(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Index name")
	type fileState struct {
		Size     int64
		Modified time.Time
		SHA      [32]byte
	}
	snapshot := func() map[string]fileState {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]fileState{}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			var sum [32]byte
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(filepath.Join(root, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				sum = sha256.Sum256(data)
			}
			out[entry.Name()] = fileState{Size: info.Size(), Modified: info.ModTime(), SHA: sum}
		}
		return out
	}
	before := snapshot()
	if label, ok := lookupLabel(root, request); !ok || label.Name != "Index name" {
		t.Fatalf("%+v %v", label, ok)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("read-only resolver changed native contents, metadata or directory entries")
	}
}

func TestLabelsHomeBudgetFollowsPersistentTargetOrder(t *testing.T) {
	t.Parallel()
	firstRoot, first := labelFixture(t)
	secondRoot, second := labelFixture(t)
	second.Registration.ArchiveSessionID = "second"
	env := agentapi.LabelEnvironment{Homes: []string{firstRoot, secondRoot}}
	for _, requests := range [][]agentapi.LabelRequest{{first, second}, {second, first}} {
		ctx, cancel := context.WithCancel(context.Background())
		var homes []string
		provider := LabelProvider{homeLookup: func(_ context.Context, root string, _ []agentapi.LabelRequest) map[string]archive.SessionLabel {
			homes = append(homes, root)
			cancel()
			return nil
		}}
		provider.LookupLabels(ctx, env, requests)
		cancel()
		want := firstRoot
		if requests[0].Registration.ArchiveSessionID == "second" {
			want = secondRoot
		}
		if len(homes) != 1 || homes[0] != want {
			t.Fatalf("budget selected %v, expected first target home %s", homes, want)
		}
	}
}

func TestLabelsCodexInterpreterRejectsNonNativeOwnerAndUnknownContract(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	request.Registration.NativeSessionID = "generic-thread"
	request.Bundle.NativeSessionID = "generic-thread"
	request.Bundle.NativeRecords[0]["payload"].(map[string]any)["id"] = "generic-thread"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("generic ID elevated to native UUID")
	}
	label := archive.SessionLabel{NativeID: labelTestID, State: archive.SessionLabelPresent, Name: "Unsupported native interpretation", Source: archive.SessionLabelDatabase, Contract: "unknown-provider-v1"}
	_, request = labelFixture(t)
	request.Bundle.SupplementalEvidence = []archive.SupplementalEvidence{label.Evidence(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "codex")}
	analysis, err := (Parser{}).Parse(context.Background(), request.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Facts.Name == label.Name {
		t.Fatal("opaque generic contract elevated to known Codex semantics")
	}
}

func TestLabelsGroupPriorityUsesVerifiedHomeWithoutNativeIO(t *testing.T) {
	t.Parallel()
	root, first := labelFixture(t)
	otherRoot, other := labelFixture(t)
	env := agentapi.LabelEnvironment{Homes: []string{root, otherRoot}}
	provider := LabelProvider{}
	firstGroup := provider.LabelRequestGroup(env, first)
	if len(firstGroup) != 64 {
		t.Fatal("verified home supplied no content-free group")
	}
	second := first
	second.Registration.ArchiveSessionID = "other-admitted-id"
	if provider.LabelRequestGroup(env, second) != firstGroup {
		t.Fatal("one home's requests did not share priority")
	}
	if group := provider.LabelRequestGroup(env, other); group == "" || group == firstGroup {
		t.Fatal("separate approved homes shared priority")
	}
	first.Registration.TranscriptPath = "/outside/transcript.jsonl"
	if provider.LabelRequestGroup(env, first) != "" {
		t.Fatal("unverified home supplied native priority")
	}
}

// A record too long to hold a name (here another thread's oversized line) is
// skipped; it does not leave every thread unnamed.
func TestLabelsSkipAnOverlongRecord(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	long, err := json.Marshal(map[string]string{"id": "01900000-0000-7000-8000-000000000002", "thread_name": strings.Repeat("x", 64<<10), "updated_at": "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	named, err := json.Marshal(map[string]string{"id": labelTestID, "thread_name": "After the long record", "updated_at": "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), append(append(append(long, '\n'), named...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := lookupLabel(root, request); !ok || got.Name != "After the long record" {
		t.Fatalf("%+v %v", got, ok)
	}
}
