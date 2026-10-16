package collector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A thread written by a current Codex release, captured while Codex runs
// (state_5.sqlite has its WAL beside it) and imported rather than discovered,
// is named from Codex's name index. The earlier resolver required producer
// 0.159.2 and a settled database, so no current thread was ever named.
func TestCurrentCodexThreadIsNamedFromTheIndexWhileCodexRuns(t *testing.T) {
	local, home, reg := codexIndexFixture(t)
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }, LabelEnvironment: agentapi.LabelEnvironment{Homes: []string{home}}}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	if name := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).Name; name != "" {
		t.Fatalf("named before lookup was enabled: %q", name)
	}
	// An already archived session is named on a later pass without any
	// change to its transcript.
	opts.Labels = codexFileLabelLookup{}
	for range 2 {
		now = now.Add(time.Hour)
		if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
			t.Fatal(result, err)
		}
	}
	metadata := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if metadata.Name != "Review book generation quality" || archive.DisplayTitle(metadata) != metadata.Name {
		t.Fatalf("name %q, display title %q", metadata.Name, archive.DisplayTitle(metadata))
	}
}

// A thread's first upload already carries its name: the lookup needs only the
// registration, so it does not wait for a publication to exist, and the
// session is not published a second time just to add the name.
func TestCodexThreadsFirstUploadCarriesItsName(t *testing.T) {
	local, home, reg := codexIndexFixture(t)
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, Labels: codexFileLabelLookup{}, MachineID: "machine", Now: func() time.Time { return now }, LabelEnvironment: agentapi.LabelEnvironment{Homes: []string{home}}}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	if name := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).Name; name != "Review book generation quality" {
		t.Fatalf("first upload name %q", name)
	}
	now = now.Add(time.Hour)
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatal("published again", result, err)
	}
}

func codexIndexFixture(t *testing.T) (*state.Store, string, archive.SessionRegistration) {
	t.Helper()
	local := newTestStore(t)
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	const thread = "01a10e11-4bd0-7041-b71c-58103545883a"
	path := writeTranscript(t, sessions, "rollout.jsonl", `{"type":"session_meta","payload":{"id":"`+thread+`","cli_version":"0.160.1","history_mode":"paginated"}}`+"\n"+codexTranscript)
	reg := registration(t, path)
	reg.NativeSessionID = thread
	reg.Harness.Version = "0.160.1"
	reg.Origin = archive.SessionOriginImport
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state_5.sqlite", "state_5.sqlite-wal", "state_5.sqlite-shm"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	line, err := json.Marshal(map[string]string{"id": thread, "thread_name": "Review book generation quality", "updated_at": "2026-10-05T21:56:20Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return local, home, reg
}

// A source build of Codex reports a version that is not a release number.
// The lookup does not need it, and one such registration does not stop the
// label cache from saving every other observation.
func TestCodexThreadFromADevelopmentBuildIsNamed(t *testing.T) {
	local, home, reg := codexIndexFixture(t)
	reg.Harness.Version = "dev"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, Labels: codexFileLabelLookup{}, MachineID: "machine", Now: func() time.Time { return now }, LabelEnvironment: agentapi.LabelEnvironment{Homes: []string{home}}}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatal(result, err)
	}
	if name := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).Name; name != "Review book generation quality" {
		t.Fatalf("name %q", name)
	}
	if cache, err := local.LoadLabels(); err != nil || len(cache.Entries) != 1 {
		t.Fatalf("label cache not saved: %+v %v", cache, err)
	}
}

// registrationLabels answers every request with one label and needs only the
// registration, as the Codex index provider does.
type registrationLabels struct {
	label archive.SessionLabel
	calls *int
}

type registrationLabelLookup struct{ provider registrationLabels }

func (r registrationLabelLookup) LookupLabels(name string) (agentapi.LabelProvider, bool) {
	return r.provider, name == "codex"
}

func (registrationLabels) RegistrationLabelContext(reg archive.SessionRegistration) (agentapi.LabelContext, bool) {
	return agentapi.LabelContext{NativeID: reg.NativeSessionID, Ordinary: true}, true
}

func (r registrationLabels) LookupLabels(_ context.Context, _ agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	out := map[string]archive.SessionLabel{}
	for _, request := range requests {
		*r.calls++
		out[request.Registration.ArchiveSessionID] = r.label
	}
	return out
}

// A history source set already published without a name is named through
// the pass's own lookup: its revision is readable for naming, and the label
// reaches the published history source.
func TestPublishedHistorySourceSetIsNamedByLookup(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	scan.releaseRetained()
	for range 3 {
		if _, err := Run(t.Context(), scan.local, scan.remote, scan.opts); err != nil {
			t.Fatal(err)
		}
	}
	metadata := fetchMetadata(t, scan.remote, "codex", scan.id())
	if metadata.History == nil || metadata.Name != "" {
		t.Fatalf("fixture: history %v name %q", metadata.History, metadata.Name)
	}
	now := scan.now
	scan.opts.Now = func() time.Time { return now }
	calls := 0
	scan.opts.Labels = registrationLabelLookup{registrationLabels{label: archive.SessionLabel{NativeID: revisionThread, State: archive.SessionLabelPresent, Name: "Native thread name", Source: archive.SessionLabelIndex, Contract: "codex-session-index-v3"}, calls: &calls}}
	for range 3 {
		now = now.Add(time.Hour)
		if _, err := Run(t.Context(), scan.local, scan.remote, scan.opts); err != nil {
			t.Fatal(err)
		}
	}
	if calls == 0 {
		t.Fatal("history source set was never looked up")
	}
	metadata = fetchMetadata(t, scan.remote, "codex", scan.id())
	if metadata.History == nil || metadata.Name != "Native thread name" {
		t.Fatalf("history %v name %q", metadata.History, metadata.Name)
	}
}
