package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type lifecycleIO struct {
	nativeBytes int64
	nativeReads int64
	nativeOpens int64
	remoteBytes int64
	remoteGets  int64
	remotePuts  int64
	remoteStats int64
	putBytes    int64
}

type lifecycleFiles struct {
	transcriptio.Opener
	cost *lifecycleIO
}

func (f lifecycleFiles) OpenRegular(path string) (transcriptio.File, error) {
	file, err := f.Opener.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	f.cost.nativeOpens++
	return lifecycleFile{File: file, cost: f.cost}, nil
}

type lifecycleFile struct {
	transcriptio.File
	cost *lifecycleIO
}

func (f lifecycleFile) ReadAt(data []byte, offset int64) (int, error) {
	n, err := f.File.ReadAt(data, offset)
	f.cost.nativeBytes += int64(n)
	f.cost.nativeReads++
	return n, err
}

func (f lifecycleFile) Stat() (fs.FileInfo, error) { return f.File.Stat() }

type lifecycleProvider struct {
	agentapi.SourceProvider
	cost *lifecycleIO
}

func (p lifecycleProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	base := env.Files
	if base == nil {
		base = transcriptio.OS{}
	}
	env.Files = lifecycleFiles{Opener: base, cost: p.cost}
	return p.SourceProvider.OpenPass(ctx, env)
}

type lifecycleSources struct {
	agentapi.SourcesLookup
	cost *lifecycleIO
}

func (s lifecycleSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := s.SourcesLookup.LookupSources(name)
	if !ok {
		return nil, nil, false
	}
	return lifecycleProvider{SourceProvider: p, cost: s.cost}, f, true
}

type lifecycleLookup struct {
	*reconciliationLookup
	budget *agentapi.NativeReadBudget
}

func (l *lifecycleLookup) NativeReadBudget() *agentapi.NativeReadBudget { return l.budget }

type lifecycleRemote struct {
	*storagetest.MemoryStore
	cost *lifecycleIO
}

func (s lifecycleRemote) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	data, err := s.MemoryStore.GetLimited(ctx, key, limit)
	s.cost.remoteGets++
	s.cost.remoteBytes += int64(len(data))
	return data, err
}

func (s lifecycleRemote) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	data, etag, err := s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
	s.cost.remoteGets++
	s.cost.remoteBytes += int64(len(data))
	return data, etag, err
}

func (s lifecycleRemote) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := s.MemoryStore.Get(ctx, key)
	s.cost.remoteGets++
	s.cost.remoteBytes += int64(len(data))
	return data, err
}

func (s lifecycleRemote) Put(ctx context.Context, key string, data []byte) error {
	s.cost.remotePuts++
	s.cost.putBytes += int64(len(data))
	return s.MemoryStore.Put(ctx, key, data)
}

// This explicit opt-in campaign measures actual provider/collector/store work.
// Each phase is one sample; no percentile or process-memory bound is claimed.
func TestMeasuredCodexLifecycleRecords(t *testing.T) {
	if os.Getenv("AGENT_ARCHIVE_LIFECYCLE_SCALE") != "1" {
		t.Skip("explicit isolated scale campaign")
	}
	for _, n := range []int{1000, 10000, 100000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) { runLifecycleScale(t, n) })
	}
}

func runLifecycleScale(t *testing.T, n int) {
	t.Helper()
	nativeHome := t.TempDir()
	dir := filepath.Join(nativeHome, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	prompts := make([]string, n)
	for i := range prompts {
		prompts[i] = fmt.Sprintf("synthetic task %06d", i)
	}
	a := reconciliationNative(t, dir, revisionThread, 0, false, prompts...)
	local := newTestStore(t)
	reg := registration(t, a.Path)
	reg.NativeSessionID = revisionThread
	reg.ProjectRoot = dir
	reg.Origin = archive.SessionOriginHook
	reg.SessionStartedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reg.RegisteredAt = reg.SessionStartedAt.Add(time.Second)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Discovery: &config.DiscoveryConfig{Enabled: false, CodexHomes: []string{nativeHome}}, MachineID: "machine", Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: dir, ProjectID: reg.ProjectID, Included: true, ActivatedAt: reg.SessionStartedAt}}}}
	if err := config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cost := &lifecycleIO{}
	lookup := &lifecycleLookup{reconciliationLookup: &reconciliationLookup{set: agentapi.CodexRolloutSet{Current: &a, Candidates: []agentapi.SourceRef{a}, Revision: "initial"}, refs: map[string][]agentapi.SourceRef{revisionThread: {a}}}, budget: agentapi.NewNativeReadBudget(128 << 20)}
	remote := lifecycleRemote{MemoryStore: storagetest.NewMemoryStore(), cost: cost}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "machine", Sources: lifecycleSources{SourcesLookup: testSources, cost: cost}, Parsers: testParsers, CodexRollouts: lookup, ConfiguredCodexHomes: []string{nativeHome}, Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	phase := func(name string, advance bool) {
		t.Helper()
		if used, _ := lookup.budget.Charged(); used != 0 {
			t.Fatalf("prior phase leaked %d", used)
		}
		lookup.budget = agentapi.NewNativeReadBudget(128 << 20)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		ioBefore := *cost
		loads := state.PublishedStateLoads()
		mtimes := snapshotMtimes(t, local.Home())
		var peak atomic.Uint64
		peak.Store(before.HeapAlloc)
		done := make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
						if peak.CompareAndSwap(old, m.HeapAlloc) {
							break
						}
					}
				}
			}
		}()
		passes := 0
		published := false
		observedStageMax := int64(0)
		for ; passes < 8; passes++ {
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, issue := range result.Errors {
				if !errors.Is(issue, archive.ErrHistoryMutationPending) {
					t.Fatalf("%s pending/error: %v", name, issue)
				}
			}
			if err := filepath.Walk(filepath.Join(local.Home(), "sessions"), func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.Mode().IsRegular() && strings.Contains(path, string(filepath.Separator)+"pending-sources"+string(filepath.Separator)) {
					observedStageMax += info.Size()
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(result.Published) > 0 {
				published = true
				passes++
				break
			}
			if !advance {
				passes++
				break
			}
		}
		close(done)
		<-stopped
		runtime.ReadMemStats(&after)
		if advance && !published {
			t.Fatalf("%s failed to converge in %d passes", name, passes)
		}
		writes := 0
		for path, mtime := range snapshotMtimes(t, local.Home()) {
			if old, ok := mtimes[path]; (!ok || !old.Equal(mtime)) && filepath.Base(path) != "status.json" {
				writes++
			}
		}
		localBytes, stagedBytes := int64(0), int64(0)
		if err := filepath.Walk(local.Home(), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				localBytes += info.Size()
				if strings.Contains(path, string(filepath.Separator)+"pending-sources"+string(filepath.Separator)) {
					stagedBytes += info.Size()
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		used, charged := lookup.budget.Charged()
		if used != 0 {
			t.Fatalf("%s leaked %d logical bytes", name, used)
		}
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			t.Fatal(err)
		}
		rss := usage.Maxrss
		if runtime.GOOS != "darwin" {
			rss *= 1024
		}
		t.Logf("phase=%s records=%d passes=%d seconds=%.6f native_bytes=%d native_reads=%d native_opens=%d remote_gets=%d remote_bytes=%d puts=%d put_bytes=%d full_decodes=%d changed_paths=%d charged_peak=%d allocations=%d sampled_heap_peak=%d process_highwater_rss=%d remote_stats=%d local_disk_bytes=%d live_stage_end_bytes=%d sum_stage_pass_end_bytes=%d", name, n, passes, time.Since(start).Seconds(), cost.nativeBytes-ioBefore.nativeBytes, cost.nativeReads-ioBefore.nativeReads, cost.nativeOpens-ioBefore.nativeOpens, cost.remoteGets-ioBefore.remoteGets, cost.remoteBytes-ioBefore.remoteBytes, cost.remotePuts-ioBefore.remotePuts, cost.putBytes-ioBefore.putBytes, state.PublishedStateLoads()-loads, writes, charged, after.TotalAlloc-before.TotalAlloc, peak.Load(), rss, cost.remoteStats-ioBefore.remoteStats, localBytes, stagedBytes, observedStageMax)
		if !advance && (state.PublishedStateLoads() != loads || writes != 0) {
			t.Fatalf("%s unchanged full decode/write", name)
		}
	}
	phase("ordinary_initial", true)
	reopened, err := openTestStore(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	local = reopened
	phase("ordinary_restart_settled", false)
	b := reconciliationNative(t, dir, revisionB, 2, true, "new physical revision")
	lookup.set.Current = &b
	lookup.set.Candidates = []agentapi.SourceRef{a, b}
	lookup.set.Revision = "revision"
	lookup.refs[revisionB] = []agentapi.SourceRef{b}
	now = now.Add(time.Hour)
	phase("related_revision_preservation", true)
	// Emulate a supported older privacy capture in every live reference. All
	// post-transition preparation/publication then uses actual current ports.
	m := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	refs, err := m.SourceReferences()
	if err != nil {
		t.Fatal(err)
	}
	for i, ref := range refs {
		raw, err := remote.Get(t.Context(), ref.Key)
		if err != nil {
			t.Fatal(err)
		}
		var bundle archive.SourceBundle
		if i == 0 {
			bundle, err = reader.DecodeReferencedSource(t.Context(), m, raw, reader.Limits{})
		} else {
			bundle, err = reader.DecodeRevisionSource(t.Context(), m, m.History.Preserved[i-1].RevisionID, raw, reader.Limits{})
		}
		if err != nil {
			t.Fatal(err)
		}
		bundle.Capture.FilterVersion = "14"
		packed, err := archive.BuildCompressedSource(bundle)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.SourceObjectKey(bundle, packed.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Put(t.Context(), key, packed.Bytes); err != nil {
			t.Fatal(err)
		}
		next := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
		if i == 0 {
			m.SourceBundle = next
			m.FilterVersion = "14"
			p, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.SavePublication(bundle, now, next, encoded); err != nil {
				t.Fatal(err)
			}
		} else {
			m.History.Preserved[i-1].Source = next
			m.History.Preserved[i-1].FilterVersion = "14"
		}
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(t.Context(), key, encoded); err != nil {
		t.Fatal(err)
	}
	p, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, _ := p.LastPublished()
	if err := p.SavePublication(bundle, now, m.SourceBundle, encoded); err != nil {
		t.Fatal(err)
	}
	if err := local.RemoveScanSignature(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	phase("all_reference_privacy", true)
	// Settle live-native policy after retained maintenance, then reopen again.
	for range 2 {
		if _, err := Run(t.Context(), local, remote, opts); err != nil {
			t.Fatal(err)
		}
	}
	local, err = openTestStore(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	phase("related_restart_settled", false)
	currentReg, found, err := local.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal("current registration", err)
	}
	currentState, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	currentBundle, _, _ := currentState.LastPublished()
	if err := currentState.SaveBlocked(currentBundle, now, state.BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	recoveryOpts := opts
	recoveryOpts.RepoKey = func(string) string { return "" }
	build, closePreview, err := PrepareGenerationRecovery(t.Context(), currentReg, now.Add(time.Hour), recoveryOpts)
	defer closePreview()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.BeginGenerationRecovery(reg.ArchiveSessionID, now.Add(time.Hour), build); err != nil {
		t.Fatal(err)
	}
	closePreview()
	if err := os.RemoveAll(nativeHome); err != nil {
		t.Fatal(err)
	}
	opts.AcceptSession = func(candidate archive.SessionRegistration) bool {
		return candidate.ArchiveSessionID == reg.ArchiveSessionID
	}
	opts.ParserVersion = "synthetic-frozen-derivation"
	opts.RepoKey = func(string) string { t.Fatal("frozen phase read current Git"); return "" }
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("frozen phase read current inventory")
		return nil, nil
	}
	now = now.Add(time.Hour)
	phase("frozen_parser_maintenance", true)
	phase("frozen_settled", false)

}

func (s lifecycleRemote) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	s.cost.remoteStats++
	return s.MemoryStore.Stat(ctx, key)
}
