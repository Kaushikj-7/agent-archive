package collector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestHistoricalRetainedPrivacyParserAndLocalReadNeverObserveNative(t *testing.T) {
	store, cloud := newTestStore(t), storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript)
	reg := registration(t, path)
	const id = "11111111111111111111111111111111"
	cfg := config.Config{MachineID: "m", RetentionDays: 90, Archive: archive.Config{Enabled: true}, ImportedHarnesses: []string{"codex"}, HistoricalProjectProtection: true, HistoricalProjects: []config.HistoricalProject{{ID: id, Label: "Retired", Permissions: []config.HistoricalProjectPermission{{DestinationID: config.Config{}.DestinationID(), Enabled: true}}}}}
	if err := config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	reg.ProjectID, reg.ProjectRoot = id, ""
	reg.DestinationID = cfg.DestinationID()
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("2026-01-01-1")
	reg.AdmittedAt = at
	reg.RegisteredAt = at
	reg.StagedImportSHA256 = strings.Repeat("a", 64)
	reg.HistoricalProject = &archive.HistoricalProjectProof{Version: 1, ProjectID: id, DestinationID: reg.DestinationID, OriginalCwd: "/vanished", CanonicalCwd: "/vanished", Label: "Retired"}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	filtered, adapter, err := FilterSource(t.Context(), "codex", agentapi.SourceRef{Path: path}, at, testSources)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }, AcceptSession: cfg.AcceptSession, HistoricalProjectLabel: cfg.HistoricalProjectLabel}
	rendered, err := renderPublication(t.Context(), opts.parserFor("codex"), opts.parserVersionFor("codex"), bundle, reg, at, opts, func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSourceThenMetadataIndexed(t.Context(), cloud, rendered.source.Key, rendered.metadataKey, rendered.sourceBytes, rendered.metadata, storage.RetryPolicy{}, nil); err != nil {
		t.Fatal(err)
	}
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SavePublication(bundle, at, rendered.source, rendered.metadata); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), store, cloud, opts); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not native JSON and synthetic future text"), 0600); err != nil {
		t.Fatal(err)
	}
	local, err := ReadLocalBundle(t.Context(), store.Home(), reg, at.Add(time.Hour), "", testSources)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recordsText(local), "synthetic future text") || !local.Capture.CapturedAt.Equal(at) {
		t.Fatal("local handoff read new native evidence")
	}
	if result, cost := measurePass(t, store, cloud, opts); len(result.Errors) != 0 || len(result.Published) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatalf("settled historical pass decoded retained data or wrote state: %+v %+v", result, cost)
	}
	cfg.ImportedHarnesses = nil
	opts.AcceptSession = cfg.AcceptSession
	opts.ParserVersion = "would-publish-if-app-still-authorized"
	if result, cost := measurePass(t, store, cloud, opts); len(result.Errors) != 0 || len(result.Published) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatalf("removed historical app retained publication authority: %+v %+v", result, cost)
	}
	cfg.ImportedHarnesses = []string{"codex"}
	opts.AcceptSession = cfg.AcceptSession
	opts.ParserVersion = ""
	editRetainedRecords(t, store, plantSecret)
	simulateFilterUpgrade(t, store)
	opts.Now = func() time.Time { return at.Add(time.Hour) }
	result, err := Run(context.Background(), store, cloud, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("retained privacy maintenance: %+v %v", result, err)
	}
	assertRefilteredSnapshot(t, cloud, at)
	opts.ParserVersion = "historical-parser-upgrade"
	result, err = Run(context.Background(), store, cloud, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("retained parser maintenance: %+v %v", result, err)
	}
	metadata := fetchMetadata(t, cloud, "codex", reg.ArchiveSessionID)
	if metadata.Parser.Version != opts.ParserVersion || !metadata.CapturedAt.Equal(at) {
		t.Fatal("parser maintenance advanced capture age")
	}
	if _, _, err := PrepareGenerationRecovery(t.Context(), reg, at.Add(time.Hour), opts); err == nil {
		t.Fatal("historical import authorized native recovery")
	}
	cfg.HistoricalProjects[0].Permissions[0].Enabled = false
	if err := config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	future := at.Add(100 * 24 * time.Hour)
	sweep := retention.Options{Now: func() time.Time { return future }, ServerClock: func(context.Context) (time.Time, error) { return future, nil }, CurrentDestination: cfg.InCurrentDestination, Publishable: cfg.AcceptSession}
	forever, err := retention.Sweep(t.Context(), store, cloud, sweep)
	if err != nil || len(forever.DeletedSessions) != 0 || len(forever.PrunedSessions) != 0 {
		t.Fatalf("forever lost disabled historical evidence: %+v %v", forever, err)
	}
	sweep.SessionMaxAge = 90 * 24 * time.Hour
	expired, err := retention.Sweep(t.Context(), store, cloud, sweep)
	if err != nil || len(expired.Errors) > 0 || len(expired.DeletedSessions) != 1 {
		t.Fatalf("disabled historical expiry: %+v %v", expired, err)
	}
	if _, found, err := store.LoadRegistration(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("expired ownership retained: %v", err)
	}
	saved, _, err := config.Load(store.Home())
	if err != nil || !saved.HistoricalProjectProtection || len(saved.HistoricalProjects) != 1 {
		t.Fatal("expiry lost sticky identity/fence", err)
	}

}
