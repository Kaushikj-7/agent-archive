package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	"github.com/wangjohn/agent-archive/internal/testutil/orbifold"
)

func fourthRegistry(t *testing.T, ports *orbifold.Ports) *builtin.Registry {
	t.Helper()
	base := builtin.NewBuiltins()
	descriptors := base.Catalog().All()
	var integrations []builtin.Integration
	for _, d := range descriptors {
		integration, _ := base.Lookup(string(d.ID))
		integration.Descriptor = agentmeta.Descriptor{ID: d.ID}
		integrations = append(integrations, integration)
	}
	descriptors = append(descriptors, agentmeta.Descriptor{ID: orbifold.ID, Aliases: []string{"orbit"}, DisplayName: "Orbifold fixture"})
	catalog, err := agentmeta.New(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	integrations = append(integrations, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: orbifold.ID}, Sources: ports, Filter: ports, Parser: orbifold.Parser{Owner: ports}, Decoder: ports, Imports: ports, Hooks: orbifold.Hooks{}, Skills: orbifold.Skills(), Launcher: orbifold.Launcher{}, Runtime: ports, Version: ports, Evidence: ports, NativeHeaders: orbifold.Discovery{Qualified: true}, Discovery: orbifold.Discovery{Qualified: true}, NativePaths: ports, Preview: ports})
	registry, err := builtin.New(catalog, integrations)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestFourthNormalRegistryFlow(t *testing.T) { fourthNormalRegistryFlow(t, false) }

func TestFourthFrozenPublicationRetry(t *testing.T) { fourthNormalRegistryFlow(t, true) }

func fourthNormalRegistryFlow(t *testing.T, failPublication bool) {
	t.Helper()
	t.Parallel()
	ctx := context.Background()
	home, project := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	nativeID := " collision/λ\x00 "
	ports := &orbifold.Ports{Mutation: agentapi.ReplaceableSnapshot, Generation: 1}
	for i, pulse := range []orbifold.NativePulse{{PulseKind: "exchange", Speaker: orbifold.SpeakerPilot, Words: "Map the orbit", NativeIdentity: nativeID, Clock: now.Format(time.RFC3339), Credential: "private-fixture-only"}, {PulseKind: "exchange", Speaker: orbifold.SpeakerOracle, Words: "The orbit is mapped", NativeIdentity: nativeID, Clock: now.Add(time.Second).Format(time.RFC3339)}} {
		raw, err := json.Marshal(pulse)
		if err != nil {
			t.Fatal(err)
		}
		ports.Shards = append(ports.Shards, raw)
		ports.ShardPaths = append(ports.ShardPaths, writeFourthTranscript(t, project, []string{"first.pulse", "second.pulse"}[i], string(raw)))
	}
	registry := fourthRegistry(t, ports)
	cfg := config.Config{MachineID: "fourth-machine", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"}, Archive: archive.Config{SchemaVersion: 1, MachineID: "fourth-machine", Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: now.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	decoder, _ := registry.LookupDecoder(string(orbifold.ID))
	emit := func(pulse string) {
		t.Helper()
		batch, err := decoder.Decode(ctx, agentapi.HookInput{ObservedAt: now, Payload: map[string]any{"opaque": nativeID, "checkout": project, "manifest": project + "/manifest.orbit", "pulse": pulse}})
		if err != nil {
			t.Fatal(err)
		}
		if err := capture.HandleBatch(home, string(orbifold.ID), batch, now); err != nil {
			t.Fatal(err)
		}
	}
	emit("resume")
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("resume admitted: %+v %v", regs, err)
	}
	emit("birth")
	emit("birth")
	emit("reply")
	emit("rest")
	regs, err = local.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("replay admission: %+v %v", regs, err)
	}
	reg := regs[0]
	builtinCollision := reg
	builtinCollision.ArchiveSessionID = "builtin-collision"
	builtinCollision.Harness.Name = "claude"
	builtinCollision.TranscriptPath = ""
	if err := local.SaveRegistration(builtinCollision); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(string(orbifold.ID), nativeID)
	id, found, err := local.ArchiveSessionID(key)
	if err != nil || !found || id != reg.ArchiveSessionID {
		t.Fatalf("qualified identity: %q %v %v", id, found, err)
	}
	failures := 0
	if failPublication {
		failures = 1
	}
	remote := &fourthPublicationStore{MemoryStore: storagetest.NewMemoryStore(), failures: failures}
	opts := collector.Options{Sources: registry, Parsers: registry, Retry: storage.RetryPolicy{MaxAttempts: 1}, MachineID: "fourth-machine", Now: func() time.Time { return now.Add(time.Hour) }}
	ports.TransientReads = 1
	if _, err := collector.Run(ctx, local, remote, opts); err != nil {
		t.Fatal(err)
	}
	var result collector.Result
	if failPublication {
		result, err = collector.Run(ctx, local, remote, opts)
		if err != nil || len(result.Errors) != 1 {
			t.Fatalf("publication failure was lost: %+v %v", result, err)
		}
		pending, found, err := local.LoadPending(reg.ArchiveSessionID)
		if err != nil || !found {
			t.Fatalf("frozen publication missing: %v %v", found, err)
		}
		frozenReads := ports.Reads
		result, err = collector.Run(ctx, local, remote, opts)
		if err != nil || len(result.Errors) > 0 {
			t.Fatalf("publication retry: %+v %v", result, err)
		}
		source, err := remote.Get(ctx, pending.SourceKey)
		if err != nil || !bytes.Equal(source, pending.SourceBytes) {
			t.Fatal("retry changed frozen source key or bytes")
		}
		metadataBytes, err := remote.Get(ctx, pending.MetadataKey)
		if err != nil || !bytes.Equal(metadataBytes, pending.MetadataBytes) {
			t.Fatal("retry changed frozen metadata bytes")
		}
		if ports.Reads != frozenReads {
			t.Fatal("frozen retry reread changed native source")
		}
	} else {
		result, err = collector.Run(ctx, local, remote, opts)
		if err != nil || len(result.Errors) > 0 {
			t.Fatalf("publication: %+v %v", result, err)
		}
	}
	metadata := fetchFourthMetadata(t, remote, string(orbifold.ID), reg.ArchiveSessionID)
	bundle, err := reader.LoadSource(ctx, remote, metadata, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.NativeRecords) != 2 || bundle.Capture.SourceFormat != orbifold.Format {
		t.Fatalf("native record envelope: %+v", bundle)
	}
	encoded, _ := json.Marshal(bundle)
	if bytes.Contains(encoded, []byte("credential")) || bytes.Contains(encoded, []byte("private-fixture-only")) {
		t.Fatal("native privacy field escaped")
	}
	parser, _ := registry.LookupParser(string(orbifold.ID))
	analysis, err := agentapi.Analyze(ctx, parser, bundle)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := archive.BuildHandoffWithAnalysis(bundle, analysis, &metadata, archive.HandoffOptions{})
	if err != nil || len(handoff.Exchanges) != 1 || !handoff.ToolResultsUnavailable {
		t.Fatalf("handoff: %+v %v", handoff, err)
	}
	if metadata.Counts.Compactions != nil || metadata.Counts.ToolErrors != nil {
		t.Fatalf("unavailable metrics fabricated: %+v", metadata.Counts)
	}
	keys, err := reader.NewMetadataFinder(registry.Catalog()).FindMetadataKeys(ctx, remote, "", reg.ArchiveSessionID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("normal discovery: %v %v", keys, err)
	}
	if failPublication {
		return
	} // The successful capture scenario owns parser refresh and replacement checks.
	reads := ports.Reads
	ports.ParserVersion = "orbifold-parser-2"
	if _, err := collector.Run(ctx, local, remote, opts); err != nil {
		t.Fatal(err)
	}
	if ports.Reads != reads {
		t.Fatal("parser upgrade reopened native source")
	}
	now = now.Add(time.Hour)
	ports.Shards = ports.Shards[:1]
	ports.ShardPaths = ports.ShardPaths[:1]
	ports.Generation++
	result, err = collector.Run(ctx, local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("allowed rewrite: %+v %v", result, err)
	}
	rewritten := fetchFourthMetadata(t, remote, string(orbifold.ID), reg.ArchiveSessionID)
	if rewritten.SourceBundle == metadata.SourceBundle || len(rewritten.CaptureGaps) == 0 {
		t.Fatalf("replacement provenance absent: %+v", rewritten)
	}
}

func writeFourthTranscript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fetchFourthMetadata(t *testing.T, store storage.ObjectStore, name, id string) archive.Metadata {
	t.Helper()
	key, err := archive.MetadataObjectKey(name, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFourthHistoricalQualifiedLocatorFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ports := &orbifold.Ports{Generation: 1}
	registry := fourthRegistry(t, ports)
	root := filepath.Join(userHome, ".orbifold", "constellations")
	must(t, os.MkdirAll(root, 0700))
	pulse := orbifold.NativePulse{PulseKind: "exchange", Speaker: orbifold.SpeakerPilot, Words: "Historical orbit", NativeIdentity: "historical", Clock: at.Add(-time.Hour).Format(time.RFC3339), Landing: project}
	raw, err := json.Marshal(pulse)
	must(t, err)
	ports.Shards = [][]byte{raw}
	manifest := writeFourthTranscript(t, root, "orbit-historical.orbit", string(raw)+"\n")
	cfg := config.Config{MachineID: "fourth", Harnesses: []string{"claude"}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "fourth", Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: at.Add(-2 * time.Hour)}}}}
	must(t, config.Save(home, cfg))
	local, err := state.Open(home)
	must(t, err)
	cliEnv := Env{Agents: registry}
	env := backfill.Environment{Home: userHome, Discovery: selectedDiscoveryApps{DiscoveryLookup: registry, apps: cliEnv.backfillDiscoveryAgents()}, Sources: registry, Imports: registry, NativePaths: registry, Now: func() time.Time { return at }, CursorDatabase: func(context.Context) (backfill.CursorDatabaseResult, error) {
		return backfill.CursorDatabaseResult{Checked: true}, nil
	}}
	plan, err := backfill.BuildPlan(ctx, env, newArchiveState(home, cfg), cfg, backfill.Filters{Harnesses: []string{string(orbifold.ID)}})
	must(t, err)
	imported := plan.Imported()
	if len(imported) != 1 || imported[0].SourceKind != orbifold.Kind || imported[0].SourceKey != "historical" || imported[0].TranscriptPath != manifest {
		t.Fatalf("qualified discovery lost: %+v", plan.Candidates)
	}
	_, err = backfill.ApplyToConfig(&cfg, plan, at)
	must(t, err)
	must(t, config.Save(home, cfg))
	registration := backfill.Registration{Home: home, Store: local, Batch: "2026-09-01-1", AdmittedAt: at, DestinationID: cfg.DestinationID()}
	result, err := registration.Run(imported)
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("historical admission: %+v %v", result, err)
	}
	remote := storagetest.NewMemoryStore()
	collected, err := collector.Run(ctx, local, remote, collector.Options{Sources: registry, Parsers: registry, MachineID: "fourth", Now: func() time.Time { return at.Add(time.Hour) }})
	if err != nil || len(collected.Published) != 1 || len(collected.Errors) != 0 {
		t.Fatalf("historical publication: %+v %v", collected, err)
	}
	metadata := fetchFourthMetadata(t, remote, string(orbifold.ID), result.Sessions[0])
	bundle, err := reader.LoadSource(ctx, remote, metadata, reader.Limits{})
	must(t, err)
	if bundle.NativeSessionID != "historical" || bundle.Capture.SourceFormat != orbifold.Format {
		t.Fatalf("historical readback: %+v", bundle)
	}
	replay, err := backfill.BuildPlan(ctx, env, newArchiveState(home, cfg), cfg, backfill.Filters{Harnesses: []string{string(orbifold.ID)}})
	must(t, err)
	if len(replay.Imported()) != 0 || replay.Candidates[0].Skip != backfill.SkipAlreadyArchived {
		t.Fatalf("historical replay: %+v", replay.Candidates)
	}
}

func TestFourthSetupJournalSkillsAndRemoval(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ports := &orbifold.Ports{}
	registry := fourthRegistry(t, ports)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.Agents = registry
	executable, err := env.executable()
	must(t, err)
	location := (orbifold.Hooks{}).Location(agentapi.HookLocations{UserHome: userHome})
	must(t, os.MkdirAll(filepath.Dir(location), 0700))
	foreign := []byte(`{"color":"ultraviolet","orbits":{"/other/owner":{"birth":"foreign"}}}`)
	must(t, os.WriteFile(location, foreign, 0600))
	next := config.Config{MachineID: "fourth", Harnesses: []string{string(orbifold.ID)}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test"}, Archive: archive.Config{SchemaVersion: 1, MachineID: "fourth", Enabled: true, Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
	must(t, applySetup(home, userHome, executable, config.Config{}, &next, nil, env))
	if next.HookFiles[string(orbifold.ID)] != location {
		t.Fatalf("native setup path: %v", next.HookFiles)
	}
	installed := agentskills.Installed(registry, userHome, "", home)
	if len(installed) == 0 {
		t.Fatal("shared templates were not installed")
	}
	health := statusView{Apps: []appStatus{{Name: string(orbifold.ID)}}}
	readInstalledApps(&health, next, home, userHome, env)
	if health.Apps[0].Hooks != "installed" {
		t.Fatalf("native health inspector: %+v", health.Apps)
	}
	changes, found, err := hooks.PlanRemovalOf(env.installedHookFiles(userHome, next), env.installation(home, userHome).owner(), string(orbifold.ID))
	must(t, err)
	if !found {
		t.Fatal("native hook removal absent")
	}
	must(t, hooks.Apply([]hooks.Change{changes}))
	raw, err := os.ReadFile(location)
	must(t, err)
	if !bytes.Contains(raw, []byte("ultraviolet")) || !bytes.Contains(raw, []byte("foreign")) || bytes.Contains(raw, []byte(executable)) {
		t.Fatalf("foreign hook ownership changed: %s", raw)
	}
	removal, kept, err := agentskills.PlanRemoval(registry, userHome, "", home)
	must(t, err)
	if len(kept) != 0 || len(removal) == 0 {
		t.Fatalf("native skill removal: %v %v", removal, kept)
	}
	must(t, hooks.Apply(removal))
	if installed := agentskills.Installed(registry, userHome, "", home); len(installed) != 0 {
		t.Fatalf("owned skill files remain: %v", installed)
	}
}

func TestFourthRecordOwnershipConformance(t *testing.T) {
	t.Parallel()
	ports := &orbifold.Ports{Generation: 1, Shards: [][]byte{[]byte(`{"pulseKind":"exchange"}`), []byte(`{"pulseKind":"exchange"}`)}}
	agenttest.RecordSource(t, ports, agentapi.SourceRef{Kind: orbifold.Kind, Key: "fixture"}, agentapi.SourceEnvironment{})
}

func TestFourthOrdinarySetupInventory(t *testing.T) {
	t.Parallel()
	registry := fourthRegistry(t, &orbifold.Ports{})
	env := Env{Agents: registry}
	cfg := config.Config{}
	if problems := setupApps(env.setupNames(), &cfg, string(orbifold.ID), nil, false); len(problems) > 0 {
		t.Fatal(problems)
	}
	if len(cfg.Harnesses) != 1 || cfg.Harnesses[0] != string(orbifold.ID) {
		t.Fatalf("selection discarded injected app: %v", cfg.Harnesses)
	}
	if !slices.Contains(installedApps(config.Config{}, true, env.agentRegistry()), string(orbifold.ID)) {
		t.Fatal("legacy installed inventory discarded injected app")
	}
	userHome := t.TempDir()
	files := env.hookFiles(userHome)
	if err := os.MkdirAll(filepath.Dir(files[string(orbifold.ID)]), 0700); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env.detectHarnesses(userHome), string(orbifold.ID)) {
		t.Fatal("detection discarded injected hook location")
	}
	if apps := preflightApps(env.setupNames(), nil, cfg.Harnesses, nil, nil); !slices.Equal(apps, cfg.Harnesses) {
		t.Fatalf("preflight discarded selected app: %v", apps)
	}
}

func TestFourthRuntimeLauncherAndEvidence(t *testing.T) {
	t.Parallel()
	ports := &orbifold.Ports{}
	registry := fourthRegistry(t, ports)
	env := Env{Agents: registry, LookupEnv: func(key string) (string, bool) { return "same opaque/λ", key == "ORBIT_NATIVE_KEY" }, LookPath: func(name string) (string, error) { return "/synthetic/bin/" + name, nil }, Environ: func() []string { return []string{"ORBIT_NATIVE_KEY=caller", "KEEP=1"} }}
	key, err := agentmeta.NewSessionKey(string(orbifold.ID), "same opaque/λ")
	if err != nil {
		t.Fatal(err)
	}
	if !currentSessions(env)[key] {
		t.Fatal("runtime inventory did not qualify injected opaque identity")
	}
	spec, err := buildLaunchSpec(fourthDestination, "follow the orbit", "/synthetic/handoff", "/synthetic/project", []string{"--verbose"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.Args, []string{"--landing", "/synthetic/project", "--carry", "follow the orbit", "--verbose"}) || !slices.Equal(spec.Env, []string{"KEEP=1"}) {
		t.Fatalf("native launcher contract was lost: %+v", spec)
	}
	evidence := captureCapabilityProfile(registry, string(orbifold.ID))
	if evidence.FreshStart.State != agentapi.CapabilityFixtureValidated || evidence.SubagentLinkage.State != agentapi.CapabilityUnavailable {
		t.Fatalf("fixture capability evidence overstated: %+v", evidence)
	}
	inspector, ok := registry.LookupVersionInspector(string(orbifold.ID))
	if !ok {
		t.Fatal("version capability missing")
	}
	observed := inspector.ObserveVersion(agentapi.VersionEnvironment{Host: fourthVersionHost{}})
	if !observed.Installed || observed.Version != "" || observed.VersionState != "unavailable" {
		t.Fatalf("unavailable version probe became installed verification: %+v", observed)
	}
	preview, ok := registry.LookupPreview(string(orbifold.ID))
	if !ok {
		t.Fatal("native preview missing")
	}
	raw, _ := json.Marshal(orbifold.NativePulse{PulseKind: "exchange", Speaker: orbifold.SpeakerPilot, Words: "Orbital question"})
	view, err := preview.PreviewRecord(t.Context(), raw)
	if err != nil || view.Title != "Orbital question" {
		t.Fatalf("native preview unavailable: %+v %v", view, err)
	}
	discovery, _ := registry.LookupDiscovery(string(orbifold.ID))
	_, err = discovery.Discover(t.Context(), agentapi.DiscoveryRequest{Purpose: agentapi.DiscoveryHandoff}, func(agentapi.DiscoveryCandidate) error { return errors.New("unexpected native handoff candidate") })
	if err == nil {
		t.Fatal("fixture unexpectedly advertises native file handoff")
	}

}

type fourthVersionHost struct{}

func (fourthVersionHost) Exists(string) (bool, bool) { return false, false }

func (fourthVersionHost) ResolveExecutable(name string) (string, bool) {
	return "/synthetic/bin/" + name, true
}

func (fourthVersionHost) Directories(string) []agentapi.VersionDirectory { return nil }

func (fourthVersionHost) ProbeVersion(agentapi.VersionProbe) (string, bool) { return "", false }

// A source write succeeds before one transient metadata failure. The next pass
// must publish the frozen key and bytes without another native read.

type fourthPublicationStore struct {
	*storagetest.MemoryStore
	failures int
}

func (s *fourthPublicationStore) Put(ctx context.Context, key string, data []byte) error {
	if strings.HasSuffix(key, "/metadata.json") && s.failures > 0 {
		s.failures--
		return io.ErrUnexpectedEOF
	}
	return s.MemoryStore.Put(ctx, key, data)
}

const fourthDestination handoffDestination = handoffDestination(orbifold.ID)

// Exercise the real command entry point, storage check and setup transaction with
// an injected integration; inventory helper tests alone cannot prove selection.
func TestFourthOrdinarySetupStatusAndUninstallCommands(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.Agents = fourthRegistry(t, &orbifold.Ports{})
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "fixture", Region: "us-east-1"}}, nil }
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "fixture-bucket", "--aws-profile", "fixture", "--project", project, "--apps", string(orbifold.ID))
	cfg, found, err := config.Load(home)
	if err != nil || !found || !slices.Equal(cfg.Harnesses, []string{string(orbifold.ID)}) {
		t.Fatalf("command selection: %+v %v", cfg, err)
	}
	location := env.hookFiles(userHome)[string(orbifold.ID)]
	if _, err := os.Stat(location); err != nil {
		t.Fatal(err)
	}
	if got := agentskills.Installed(env.Agents, userHome, "", home); len(got) != len(agentskills.Registry) {
		t.Fatalf("command installed skills: %v", got)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--verbose"}, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "orbifold: waiting for first session") {
		t.Fatalf("status exit=%d output=%s errors=%s", code, &out, &errOut)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall exit=%d output=%s errors=%s", code, &out, &errOut)
	}
	if got := agentskills.Installed(env.Agents, userHome, "", home); len(got) != 0 {
		t.Fatalf("command left owned skills: %v", got)
	}
	raw, err := os.ReadFile(location)
	if err == nil && bytes.Contains(raw, []byte(home)) {
		t.Fatalf("command left owned hook: %s", raw)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestFourthAliasOrdinaryBackfill(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ports := &orbifold.Ports{Generation: 1}
	registry := fourthRegistry(t, ports)
	root := filepath.Join(userHome, ".orbifold", "constellations")
	must(t, os.MkdirAll(root, 0700))
	raw, err := json.Marshal(orbifold.NativePulse{PulseKind: "exchange", Speaker: orbifold.SpeakerPilot, Words: "Historical orbit", NativeIdentity: "history", Clock: at.Add(-time.Hour).Format(time.RFC3339), Landing: project})
	must(t, err)
	ports.Shards = [][]byte{raw}
	writeFourthTranscript(t, root, "orbit-history.orbit", string(raw)+"\n")
	cfg := config.Config{MachineID: "fourth", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "fourth", Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: at.Add(-2 * time.Hour)}}}}
	must(t, config.Save(home, cfg))
	env := testEnv(t, home, at)
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	env.Agents = registry
	env.Now = func() time.Time { return at }
	for _, spelling := range []string{"orbifold", "orbit", " ORBIT "} {
		var out, stderr bytes.Buffer
		code := Run([]string{"backfill", "--dry-run", "--json", "--harness", spelling}, strings.NewReader(""), &out, &stderr, env)
		var summary struct {
			Filters struct {
				Harnesses []string `json:"harnesses"`
			} `json:"filters"`
			Projects []struct {
				Sessions map[string]int `json:"sessions"`
			} `json:"projects"`
			Skipped map[string]int `json:"skipped"`
		}
		must(t, json.Unmarshal(out.Bytes(), &summary))
		total := 0
		for _, p := range summary.Projects {
			for _, n := range p.Sessions {
				total += n
			}
		}
		if code != 0 || total != 1 || strings.Join(summary.Filters.Harnesses, ",") != "orbifold" || summary.Skipped["filtered_out"] != 0 {
			t.Fatalf("--harness %q: code=%d summary=%+v stderr=%s", spelling, code, summary, stderr.String())
		}
	}
}

func TestFourthHistoryRepositoryMatch(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	project := filepath.Join(userHome, "relocated")
	must(t, os.MkdirAll(filepath.Join(project, ".git"), 0700))
	registry := fourthRegistry(t, &orbifold.Ports{})
	nativeRoot := filepath.Join(userHome, ".orbifold", "constellations")
	must(t, os.MkdirAll(nativeRoot, 0700))
	raw, err := json.Marshal(orbifold.NativePulse{PulseKind: "exchange", NativeIdentity: "history-only", Landing: project})
	must(t, err)
	writeFourthTranscript(t, nativeRoot, "orbit-history-only.orbit", string(raw)+"\n")
	key := archive.RepoKey("https://example.test/acme/relocated.git")
	env := Env{Agents: registry, BackfillTempDirs: []string{}, LookupEnv: func(string) (string, bool) { return "", false }, WorkingDir: func() (string, error) { return "", nil }, repoKeyContext: func(context.Context, string) string { return key }}
	got := matchProjects(t.Context(), env, userHome, config.Config{}, []projectMatchRequest{{RepoKey: key}})
	if got.Incomplete || len(got.Roots) != 1 || len(got.Roots[0]) != 1 || got.Roots[0][0] != local.CanonicalPath(project) {
		t.Fatalf("history-only fourth clone missed: %+v", got)
	}
}
