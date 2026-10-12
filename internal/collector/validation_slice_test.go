package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestCollectorCachesFailedSweepUntilSliceExpires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := &collectorCatalog{failure: agentapi.Wrap(agentapi.Limit, errors.New("synthetic failed sweep"))}
		set := &sourcePassSet{env: agentapi.SourceEnvironment{CodexRollouts: c}, passes: map[sourcePassKey]agentapi.SourcePass{}}
		provider := &sliceLifetimeProvider{}
		reader := providerReader{passes: set}
		for range 20 {
			_, release, err := reader.pass(t.Context(), provider, "codex")
			if release != nil {
				_ = release()
			}
			if agentapi.Failure(err) != agentapi.Limit {
				t.Fatalf("failed sweep did not stop reader: %v", err)
			}
		}
		if provider.opens != 0 || c.sweeps != 1 {
			t.Fatalf("failed sweep repeated or opened provider: opens=%d counters=%#v", provider.opens, c.sweeps)
		}
		time.Sleep(30 * time.Second)
		if _, _, err := reader.pass(t.Context(), provider, "codex"); agentapi.Failure(err) != agentapi.Limit {
			t.Fatal(err)
		}
		if c.sweeps != 2 {
			t.Fatal("expired failed slice did not receive a fresh sweep budget")
		}
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if _, _, err := reader.pass(cancelled, provider, "codex"); !errors.Is(err, context.Canceled) {
			t.Fatal("cached failure bypassed cancellation", err)
		}
		if err := set.slice.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCollectorRenewsExpiredSliceOnlyAfterReaderCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &collectorCatalog{}
		set := &sourcePassSet{env: agentapi.SourceEnvironment{CodexRollouts: c}, passes: map[sourcePassKey]agentapi.SourcePass{}}
		provider := &sliceLifetimeProvider{}
		reader := providerReader{passes: set}
		pass, release, err := reader.pass(t.Context(), provider, "codex")
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{}, agentapi.ReadLimits{})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Second)
		if _, _, err := reader.pass(t.Context(), provider, "codex"); agentapi.Failure(err) != agentapi.Limit {
			t.Fatal("renewed active snapshot", err)
		}
		if provider.closes != 0 || c.sweeps != 1 {
			t.Fatal("active reader closed or catalog reswept")
		}
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		_, release, err = reader.pass(t.Context(), provider, "codex")
		if err != nil {
			t.Fatal(err)
		}
		if provider.closes != 1 || c.sweeps != 2 || provider.opens != 2 {
			t.Fatal("closed reader did not renew fairly")
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		for _, pass := range set.passes {
			if err := pass.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := set.slice.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOrdinaryCollectorPassDoesNotCreateValidationSlice(t *testing.T) {
	t.Parallel()
	provider := &sliceLifetimeProvider{}
	set := &sourcePassSet{env: agentapi.SourceEnvironment{}, passes: map[sourcePassKey]agentapi.SourcePass{}}
	pass, err := set.get(t.Context(), sourcePassKey{name: "codex"}, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	if set.slice != nil || provider.bound != nil {
		t.Fatal("ordinary capture acquired history evidence")
	}
}

type sliceLifetimeProvider struct {
	observationProvider
	opens  int
	closes int
	bound  agentapi.CodexRolloutLookup
}

func (p *sliceLifetimeProvider) OpenPass(_ context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p.opens++
	p.bound = env.CodexRollouts
	return sliceLifetimePass{observationPass: observationPass{}, owner: p}, nil
}

type sliceLifetimePass struct {
	observationPass
	owner *sliceLifetimeProvider
}

func (p sliceLifetimePass) Close() error { p.owner.closes++; return nil }

func TestExplicitCatalogReadsHaveBoundedOverallContext(t *testing.T) {
	t.Parallel()
	c := &collectorCatalog{}
	reader := providerReader{rollouts: c}
	bounded, cancel := reader.validationContext(context.Background())
	defer cancel()
	deadline, ok := bounded.Deadline()
	if !ok || time.Until(deadline) > 30*time.Second {
		t.Fatal("explicit read lacks overall bound")
	}
	ordinary := providerReader{}
	ctx, closeCtx := ordinary.validationContext(context.Background())
	defer closeCtx()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("ordinary read context changed")
	}
}

func TestExplicitCatalogDefaultReadDeadlineFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &collectorCatalog{}
		reader := providerReader{rollouts: c}
		bounded, cancel := reader.validationContext(context.Background())
		defer cancel()
		slice, err := c.BeginValidationSlice(bounded, agentapi.CodexValidationLimits{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := slice.Close(); err != nil {
				t.Error(err)
			}
		}()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if err := slice.Valid(bounded); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("default context ignored deadline", err)
		}
		if c.sweeps != 1 {
			t.Fatal("expiry enumerated or admitted content")
		}
	})
}

func TestCollectorRealCodexProviderRenewsAcrossManyThreads(t *testing.T) {
	t.Parallel()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const threads = 260
	paths := make([]string, 0, threads)
	for i := range threads {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
		raw := `{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/synthetic","timestamp":"2026-10-01T12:00:00Z","source":"cli","cli_version":"0.160.0","originator":"codex_cli_rs"}}` + "\n"
		paths = append(paths, writeTranscript(t, dir, "rollout-"+id+".jsonl", raw))
	}
	c := newCollectorCatalog(t, []string{home})
	opts := Options{Sources: testSources, CodexRollouts: c, ConfiguredCodexHomes: []string{home}}
	closePasses := openCursorPass(nil, &opts)
	defer func() {
		if err := closePasses(); err != nil {
			t.Error(err)
		}
	}()
	for i, path := range paths {
		reg := archive.SessionRegistration{NativeSessionID: fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1), Harness: archive.Harness{Name: "codex"}, TranscriptPath: path}
		reader, ok := newSourceReader(reg, opts)
		if !ok {
			t.Fatal("missing source reader")
		}
		if _, err := reader.Signature(t.Context()); err != nil {
			old := opts.sourcePasses.slice
			if agentapi.Failure(err) != agentapi.Limit || old == nil || agentapi.Failure(old.Valid(t.Context())) != agentapi.Limit || opts.sourcePasses.active != 0 {
				t.Fatalf("read %d unexpected refusal: %v", i, err)
			}
			before := c.sweeps
			if _, retry := reader.Signature(t.Context()); retry != nil {
				t.Fatal(retry)
			}
			if opts.sourcePasses.slice == old || c.sweeps != before+1 {
				t.Fatal("exhausted slice was not renewed exactly once")
			}
		}

	}
	if c.sweeps != 4 || opts.sourcePasses.active != 0 {
		t.Fatalf("real caller did not share and renew sweeps: %d", c.sweeps)
	}
}

// collectorCatalog scripts only lifetime failures; non-nil lookups use the
// actual production owner for metadata/current/physical validation.
type collectorCatalog struct {
	agentapi.CodexRolloutLookup
	sweeps  int
	owner   *discovery.CodexRolloutLookup
	failure error
}

func newCollectorCatalog(t *testing.T, homes []string) *collectorCatalog {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := discovery.NewCodexRolloutLookup(t.Context(), store, homes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.CloseReadOnly(); err != nil {
			t.Error(err)
		}
	})
	return &collectorCatalog{CodexRolloutLookup: owner.MetadataInventory(), owner: owner}
}

func (c *collectorCatalog) NativeReadBudget() *agentapi.NativeReadBudget {
	if owner, ok := c.CodexRolloutLookup.(interface {
		NativeReadBudget() *agentapi.NativeReadBudget
	}); ok {
		return owner.NativeReadBudget()
	}
	return nil
}

func (c *collectorCatalog) BeginValidationSlice(ctx context.Context, limits agentapi.CodexValidationLimits) (agentapi.CodexRolloutSlice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.sweeps++
	if c.CodexRolloutLookup != nil {
		return c.CodexRolloutLookup.(agentapi.CodexRolloutSliceProvider).BeginValidationSlice(ctx, limits)
	}
	return &collectorScriptSlice{failure: c.failure, expires: time.Now().Add(30 * time.Second)}, nil
}

type collectorScriptSlice struct {
	failure error
	expires time.Time
	closed  bool
}

func (s *collectorScriptSlice) Valid(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return agentapi.ErrClosed
	}
	if !time.Now().Before(s.expires) {
		return agentapi.Wrap(agentapi.Limit, errors.New("scripted slice expired"))
	}
	return s.failure
}

func (s *collectorScriptSlice) Thread(ctx context.Context, _ string) (agentapi.CodexRolloutSet, error) {
	return agentapi.CodexRolloutSet{}, s.Valid(ctx)
}

func (s *collectorScriptSlice) Rollout(ctx context.Context, _ string) ([]agentapi.SourceRef, error) {
	return nil, s.Valid(ctx)
}

func (s *collectorScriptSlice) Check(ctx context.Context, _, _ string) error { return s.Valid(ctx) }

func (s *collectorScriptSlice) Close() error { s.closed = true; return nil }

func TestCollectorActualInventoryLimitCachesFailureAndCloses(t *testing.T) {
	home := t.TempDir()
	c := newCollectorCatalog(t, []string{home})
	budget := c.NativeReadBudget()
	held := budget.Available()
	if !budget.Reserve(held) {
		t.Fatal("fixture ledger reservation failed")
	}
	opts := Options{Sources: testSources, CodexRollouts: c}
	closePasses := openCursorPass(nil, &opts)
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: "codex"}, TranscriptPath: filepath.Join(home, "rollout-11111111-1111-4111-8111-111111111111.jsonl")}
	reader, ok := newSourceReader(reg, opts)
	if !ok {
		t.Fatal("missing reader")
	}
	for range 20 {
		if _, err := reader.Signature(t.Context()); agentapi.Failure(err) != agentapi.Limit {
			t.Fatal("actual inventory limit not retained", err)
		}
	}
	if c.sweeps != 1 || opts.sourcePasses.active != 0 || len(opts.sourcePasses.passes) != 0 {
		t.Fatal("failed inventory opened provider or renewed", c.sweeps)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.Signature(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cached failure hid cancellation", err)
	}
	if err := closePasses(); err != nil {
		t.Fatal(err)
	}
	budget.Release(held)
	if err := c.owner.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("actual failed inventory leaked", used)
	}
}
