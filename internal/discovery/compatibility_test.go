package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func compatibilityRollout(t *testing.T, root, project, fixtureName, version string, n int, alter func(map[string]any, map[string]any)) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "sourcefacts", "testdata", fixtureName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var meta, task map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &task); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
	m, start := meta["payload"].(map[string]any), task["payload"].(map[string]any)
	m["id"], m["cwd"] = id, project
	if _, ok := m["session_id"]; ok {
		m["session_id"] = id
	}
	if version != "" {
		m["cli_version"] = version
	}
	if alter != nil {
		alter(m, start)
	}
	for i, record := range []map[string]any{meta, task} {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = string(encoded)
	}
	path := filepath.Join(root, "sessions", "rollout-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return id, path
}

func TestCompatibleFormatsPublishAndReadBackAmongRejectedRecords(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	versions := []string{"0.150.0", "0.155.0", "0.155.0-alpha.9.2", "0.159.3", "0.160.0", "0.999.0-alpha.1"}
	fixtures := []string{"codex-150-absent-history.jsonl", "codex-155-legacy.jsonl", "codex-155-alpha-paginated.jsonl"}
	wanted := map[agentmeta.SessionKey]string{}
	for i, version := range versions {
		id, _ := compatibilityRollout(t, root, cfg.Archive.Projects[0].Root, fixtures[i%len(fixtures)], version, i+1, nil)
		wanted[sessionKey("codex", id)] = version
	}
	rejected := map[string]string{}
	rejectedKeys := []agentmeta.SessionKey{}
	rejectionCodes := []string{"unsupported_history", "unsupported_history", "unsupported_producer", "invalid_relationship", "unsupported_execution", "inherited_history", "inherited_history"}
	for i, alter := range []func(map[string]any, map[string]any){
		func(m, _ map[string]any) { m["history_mode"] = "compressed" },
		func(m, _ map[string]any) { m["history_mode"] = "referenced" },
		func(m, _ map[string]any) { delete(m, "originator") },
		func(m, _ map[string]any) { m["forked_from_id"] = "parent" },
		func(m, _ map[string]any) { m["thread_source"] = "guardian_review" },
		func(_, task map[string]any) { task["turn_id"] = "external-import-turn-1" },
		func(_, task map[string]any) { delete(task, "started_at") },
	} {
		id, path := compatibilityRollout(t, root, cfg.Archive.Projects[0].Root, fixtures[2], "0.999.0", 100+i, alter)
		rejected[path] = rejectionCodes[i]
		rejectedKeys = append(rejectedKeys, sessionKey("codex", id))
		if _, found, err := store.ArchiveSessionID(sessionKey("codex", id)); err != nil || found {
			t.Fatal("rejected fixture already owns an identity", err)
		}
	}
	// Health counts observations per pass, including revisits. Check durable
	// identities and classifications instead of summing those repeated counts.
	var h Health
	var checkpoint catalog
	formats := map[string]bool{}
	supported := false
	for pass := range 12 {
		var err error
		h, err = runWithAdapters(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, registeredAdapters())
		if err != nil {
			t.Fatalf("mixed scan pass %d: %+v %v", pass, h, err)
		}
		if h.Outcomes["admission_retry"] != 0 {
			if err := recoverytest.Exhaust(t.Context(), store, state.SessionIndexRecoverySlice, false); err != nil {
				t.Fatal(err)
			}
		}
		supported = supported || h.Supported
		for _, format := range h.Formats {
			formats[format.Version] = true
		}
		if err := local.Read(filepath.Join(store.Home(), "discovery-catalog.json"), &checkpoint); err != nil {
			t.Fatal(err)
		}
		regs, err := store.LoadRegistrations()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("mixed scan pass %d: pending=%t registrations=%d cached=%d", pass+1, h.Pending, len(regs), len(checkpoint.Cache))
		if !h.Pending && len(regs) == len(wanted) && len(checkpoint.Cache) == len(wanted)+len(rejected) {
			break
		}
		if pass == 11 {
			t.Fatalf("mixed scan did not converge: %+v registrations=%d cached=%d", h, len(regs), len(checkpoint.Cache))
		}
	}
	if !supported || len(formats) != len(versions) {
		t.Fatalf("lost session version diagnostics: %+v %+v", h, formats)
	}
	for _, version := range versions {
		if !formats[version] {
			t.Fatalf("missing version diagnostic %q", version)
		}
	}
	for path, expected := range rejected {
		entry, found := checkpoint.Cache[path]
		actual := string(entry.Observation.Outcome)
		if entry.Observation.Outcome == outcomeUsable && !registeredAdapters()[0].Supported(entry.Observation.Candidate) {
			actual = "unsupported_producer"
		}
		if !found || actual != expected {
			t.Fatalf("rejection %s: got %q, want %q", filepath.Base(path), actual, expected)
		}
	}
	for _, key := range rejectedKeys {
		if _, found, err := store.ArchiveSessionID(key); err != nil || found {
			t.Fatalf("rejected session reserved identity %+v: %v", key, err)
		}
	}
	objectStore := storagetest.NewMemoryStore()
	result, err := collector.Run(context.Background(), store, objectStore, collector.Options{Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(3 * time.Minute) }})
	if err != nil || len(result.Published) != len(versions) {
		t.Fatalf("publication: %+v %v", result, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != len(versions) {
		t.Fatalf("registrations: %d %v", len(regs), err)
	}
	seen := map[agentmeta.SessionKey]bool{}
	for _, reg := range regs {
		key := sessionKey(reg.Harness.Name, reg.NativeSessionID)
		if version, found := wanted[key]; !found || seen[key] || version != reg.Harness.Version {
			t.Fatalf("unexpected or duplicate admission: %+v", reg)
		}
		seen[key] = true
		if reg.Origin != archive.SessionOriginDiscovery || reg.Harness.Version == "" || reg.DestinationID != cfg.DestinationID() {
			t.Fatal("admission provenance lost")
		}
		_, _, _, found, err := store.LoadPublished(reg.ArchiveSessionID)
		if err != nil || !found {
			t.Fatal("read-back publication missing", err)
		}
		metadata, err := store.PublishedMetadata(reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
		var meta archive.Metadata
		if err := json.Unmarshal(metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.SessionID != reg.ArchiveSessionID || meta.NativeSessionID != reg.NativeSessionID || meta.Harness.Name != reg.Harness.Name || meta.Harness.Version != reg.Harness.Version {
			t.Fatalf("publication identity mismatch: %+v", meta)
		}
		source, err := objectStore.Get(context.Background(), meta.SourceBundle.Key)
		if err != nil {
			t.Fatal(err)
		}
		if len(source) != meta.SourceBundle.CompressedBytes || fmt.Sprintf("%x", sha256.Sum256(source)) != meta.SourceBundle.SHA256 {
			t.Fatal("published source does not match its verified reference")
		}
		validatePublishedDiscovery(t, metadata, source)
		reader, err := gzip.NewReader(bytes.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil || !bytes.Contains(decoded, []byte("SYNTHETIC_COMPATIBLE_PROMPT")) || bytes.Contains(decoded, []byte("synthetic-private-password")) || !bytes.Contains(decoded, []byte(reg.Harness.Version)) {
			t.Fatalf("filtering/version preservation failed: %v", err)
		}
	}
	// Repeated observations retain native ownership and never allocate duplicates.
	h, err = runWithCensus(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(4 * time.Minute) }}, registeredAdapters())
	if err != nil || h.Registered != 0 {
		t.Fatalf("duplicate admission: %+v %v", h, err)
	}
	regs, err = store.LoadRegistrations()
	if err != nil || len(regs) != len(wanted) {
		t.Fatalf("repeated scan changed registrations: %d %v", len(regs), err)
	}
	for _, name := range []string{"discovery-catalog.json", "discovery-health.json"} {
		raw, err := os.ReadFile(filepath.Join(store.Home(), name))
		if err != nil || bytes.Contains(raw, []byte("SYNTHETIC_COMPATIBLE_PROMPT")) || bytes.Contains(raw, []byte("synthetic-private-password")) {
			t.Fatal("diagnostics retained conversation", err)
		}
	}
}

type creationConsentCase string

const (
	consentOldCreation        creationConsentCase = "old-creation"
	consentUnapprovedProject  creationConsentCase = "unapproved-project"
	consentUnapprovedSource   creationConsentCase = "unapproved-source"
	consentPausedCreation     creationConsentCase = "paused-creation"
	consentDestinationChanged creationConsentCase = "destination-changed"
	consentExcluded           creationConsentCase = "excluded"
)

func TestUnknownCompatibleVersionCannotBypassCreationConsent(t *testing.T) {
	t.Parallel()
	for _, mode := range []creationConsentCase{consentOldCreation, consentUnapprovedProject, consentUnapprovedSource, consentPausedCreation, consentDestinationChanged, consentExcluded} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			project := cfg.Archive.Projects[0].Root
			if mode == consentUnapprovedProject {
				project = t.TempDir()
			}
			id, _ := compatibilityRollout(t, root, project, "codex-155-alpha-paginated.jsonl", "0.999.0-alpha.1", 1, func(m, _ map[string]any) {
				if mode == consentOldCreation {
					m["timestamp"] = at.Add(-time.Hour).Format(time.RFC3339Nano)
				}
			})
			prior := cfg
			switch mode {
			case consentOldCreation, consentUnapprovedProject:
				// These cases are applied while constructing the rollout above.
			case consentUnapprovedSource:
				cfg.Discovery.CodexHomes = []string{t.TempDir()}
			case consentExcluded:
				cfg.Archive.Projects = append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
				cfg.Archive.Projects[0].Included = false
			case consentDestinationChanged:
				cfg.Storage.Bucket = "different-synthetic-bucket"
			case consentPausedCreation:
				var err error
				if _, err = config.SetPaused(store.Home(), true, at.Add(30*time.Second)); err != nil {
					t.Fatal(err)
				}
				cfg, err = config.SetPaused(store.Home(), false, at.Add(90*time.Second))
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode != consentPausedCreation {
				if err := config.ReconcileDiscovery(&cfg, prior, at.Add(90*time.Second)); err != nil {
					t.Fatal(err)
				}
				if err := config.Save(store.Home(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			h, err := runWithCensus(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, registeredAdapters())
			if err != nil || h.Registered != 0 {
				t.Fatalf("consent bypass: %+v %v", h, err)
			}
			if _, found, err := store.ArchiveSessionID(sessionKey("codex", id)); err != nil || found {
				t.Fatal("rejected session reserved identity", err)
			}
		})
	}
}

func TestFormatDiagnosticsAreBoundedAndValidated(t *testing.T) {
	t.Parallel()
	h := Health{Outcomes: map[string]int{}}
	for i := range 30 {
		h.observeFormat(Candidate{FormatProfile: sourcefacts.CodexLegacyJSONL, HarnessVersion: fmt.Sprintf("0.%d.0", i), ProducerSource: "cli", ProducerOriginator: "codex-tui"})
	}
	if len(h.Formats) != maxObservedFormats || h.Outcomes["format_summary_overflow"] != 14 {
		t.Fatal("unbounded format summary")
	}
	home := t.TempDir()
	if err := writeHealth(home, h); err != nil {
		t.Fatal(err)
	}
	saved, found, err := ReadHealth(home)
	if err != nil || !found || len(saved.Formats) != maxObservedFormats {
		t.Fatal("format evidence did not round trip", err)
	}
	h.Formats[0].Version = "/private/session\nsecret"
	if writeHealth(home, h) == nil {
		t.Fatal("unsafe version diagnostic accepted")
	}
}

func TestVersionTwoCatalogReprobesForFormatEvidence(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	_, path := compatibilityRollout(t, root, cfg.Archive.Projects[0].Root, "codex-155-alpha-paginated.jsonl", "", 1, nil)
	adapter := codexAdapter{}
	entry := adapter.Describe(root, "sessions", filepath.Base(path))
	observation := adapter.Inspect(context.Background(), entry.Source)
	observation.Candidate.FormatProfile = "" // v2 cached no profile.
	now := at.Add(2 * time.Minute)
	old := catalog{Version: 2, Roots: []string{root}, Cache: map[string]cached{path: {Size: entry.Fingerprint.Size, Mtime: entry.Fingerprint.Mtime, Checked: now, Observation: observation}}}
	if err := local.Write(filepath.Join(store.Home(), "discovery-catalog.json"), old); err != nil {
		t.Fatal(err)
	}
	h, err := Run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return now }})
	if err != nil || h.Probes != 1 || len(h.Formats) != 1 || h.Formats[0].Profile != sourcefacts.CodexPaginatedJSONL || !h.Supported {
		t.Fatalf("stale cache blocked compatible producer: %+v %v", h, err)
	}
}

func TestCompatibleProducerVersionIsImmutableAtPublication(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	compatibilityRollout(t, root, cfg.Archive.Projects[0].Root, "codex-155-alpha-paginated.jsonl", "0.999.0-alpha.1", 1, nil)
	h, err := runWithCensus(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, registeredAdapters())
	if err != nil || h.Registered != 1 {
		t.Fatalf("admission failed: %+v %v", h, err)
	}
	compatibilityRollout(t, root, cfg.Archive.Projects[0].Root, "codex-155-alpha-paginated.jsonl", "0.999.0-alpha.2", 1, nil)
	result, err := collector.Run(context.Background(), store, storagetest.NewMemoryStore(), collector.Options{Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(3 * time.Minute) }})
	if err != nil || len(result.Published) != 0 {
		t.Fatalf("changed producer identity published: %+v %v", result, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].Harness.Version != "0.999.0-alpha.1" {
		t.Fatal("original producer evidence rewritten", err)
	}
}

func TestSelfContainedChildrenAndForksAdmitIndependentlyWhileDependenciesRemainPending(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	const ancestor = "00000000-0000-0000-0000-000000000001"
	const parent = "00000000-0000-0000-0000-000000000002"
	for i, alter := range []func(map[string]any, map[string]any){
		func(m, _ map[string]any) { m["session_id"] = ancestor; m["parent_thread_id"] = ancestor },
		func(m, _ map[string]any) { m["session_id"] = ancestor; m["parent_thread_id"] = parent },
		func(m, _ map[string]any) { m["forked_from_id"] = ancestor; m["forked_from_ordinal_exclusive"] = 0 },
		func(m, _ map[string]any) {
			m["history_base"] = map[string]any{"thread_id": ancestor, "end_ordinal_exclusive": 0, "end_byte_offset": 0}
		},
	} {
		compatibilityRollout(t, root, cfg.Archive.Projects[0].Root, "codex-155-alpha-paginated.jsonl", "0.999.0", 20+i, alter)
	}
	h, err := runWithCensus(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, registeredAdapters())
	if err != nil || h.Registered != 3 || h.Outcomes["related_history_pending"] < 1 || h.Outcomes["invalid_identity"] != 0 {
		t.Fatalf("related history: %+v %v", h, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 3 {
		t.Fatalf("independent histories missing: %v %v", regs, err)
	}
}
