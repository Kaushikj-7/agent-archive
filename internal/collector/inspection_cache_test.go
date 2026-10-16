package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/inspectioncache"
)

func TestContentVerifiedWarmInspectionSkipsAllScansAndChangedFileScansOnce(t *testing.T) {
	// Serial: the collector filtering counter is process-wide.
	dir := t.TempDir()
	cache := inspectioncache.New(filepath.Join(t.TempDir(), "cache"))
	var sources []agentapi.SourceRef
	for i := range 3 {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		path := filepath.Join(dir, "rollout-2026-10-01T12-00-00-"+id+".jsonl")
		raw := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q,\"timestamp\":\"2026-10-01T12:00:00Z\",\"source\":\"cli\",\"originator\":\"codex_cli_rs\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"synthetic safe text\"}]}}\n", id, dir)
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: id})
	}
	inspect := func() {
		t.Helper()
		for _, ref := range sources {
			_, summary, observed, err := InspectSourceCached(t.Context(), "codex", ref, time.Time{}, testSources, cache)
			if err != nil || summary == nil || !summary.Conversation {
				t.Fatalf("inspection: %+v, %v", summary, err)
			}
			if observed != nil {
				if err := cache.StoreBatch(t.Context(), []inspectioncache.Observation{*observed}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	before := transcriptFilters.Load()
	inspect()
	if scans := transcriptFilters.Load() - before; scans != 3 {
		t.Fatalf("cold scans=%d", scans)
	}
	before = transcriptFilters.Load()
	inspect()
	if scans := transcriptFilters.Load() - before; scans != 0 {
		t.Fatalf("unchanged scans=%d", scans)
	}
	file, err := os.OpenFile(sources[1].Path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"type\":\"response_item\",\"role\":\"user\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before = transcriptFilters.Load()
	inspect()
	if scans := transcriptFilters.Load() - before; scans != 1 {
		t.Fatalf("one changed source scans=%d", scans)
	}
}

type cacheFaultSources struct {
	base          agentapi.SourcesLookup
	verify        func() error
	snapshotClose error
	passClose     error
	readFailure   error
	proofFailure  error
}

func (s cacheFaultSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, filter, ok := s.base.LookupSources(name)
	return cacheFaultProvider{provider, s}, filter, ok
}

type cacheFaultProvider struct {
	agentapi.SourceProvider
	faults cacheFaultSources
}

func (p cacheFaultProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return cacheFaultPass{pass, p.faults}, nil
}

type cacheFaultPass struct {
	agentapi.SourcePass
	faults cacheFaultSources
}

func (p cacheFaultPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.faults.readFailure != nil {
		return nil, p.faults.readFailure
	}
	snapshot, err := p.SourcePass.Read(ctx, ref, limits)
	if err != nil {
		return nil, err
	}
	proof, ok := snapshot.(agentapi.VerifiedContentSnapshot)
	if !ok {
		return snapshot, nil
	}
	return cacheFaultSnapshot{snapshot, proof, p.faults}, nil
}

func (p cacheFaultPass) Close() error { return errors.Join(p.SourcePass.Close(), p.faults.passClose) }

type cacheFaultSnapshot struct {
	agentapi.SourceSnapshot
	proof  agentapi.VerifiedContentSnapshot
	faults cacheFaultSources
}

// ContentProofFor explicitly retains the delegated input for fault injection.
func (s cacheFaultSnapshot) ContentProofFor(snapshot agentapi.SourceSnapshot, in agentapi.NativeInput) bool {
	actual, ok := snapshot.(cacheFaultSnapshot)
	return ok && actual.SourceSnapshot == s.SourceSnapshot && s.proof.ContentProofFor(s.SourceSnapshot, in)
}

func (s cacheFaultSnapshot) ContentProof(ctx context.Context) (agentapi.SourceContentProof, error) {
	if s.faults.proofFailure != nil {
		return agentapi.SourceContentProof{}, s.faults.proofFailure
	}
	return s.proof.ContentProof(ctx)
}

func (s cacheFaultSnapshot) VerifyContent(ctx context.Context) error {
	if s.faults.verify != nil {
		if err := s.faults.verify(); err != nil {
			return err
		}
	}
	return s.proof.VerifyContent(ctx)
}

func (s cacheFaultSnapshot) Close() error {
	return errors.Join(s.SourceSnapshot.Close(), s.faults.snapshotClose)
}

type cacheFaultKind string

const (
	cacheSnapshotClose       cacheFaultKind = "snapshot-close"
	cachePassClose           cacheFaultKind = "pass-close"
	cacheChangedAfterHit     cacheFaultKind = "change-after-hit"
	cacheCancelledAfterHit   cacheFaultKind = "cancel-after-hit"
	cachePermissionBeforeHit cacheFaultKind = "permission-before-hit"
	cacheMissingBeforeHit    cacheFaultKind = "missing-before-hit"
	cacheReplacedAfterHit    cacheFaultKind = "replace-after-hit"
	cachePermissionAfterHit  cacheFaultKind = "permission-after-hit"
	cacheProofFailure        cacheFaultKind = "proof-failure"
)

func TestCacheNeverEmitsAfterProviderCleanupFailureAndRechecksHitContent(t *testing.T) {
	for _, fault := range []cacheFaultKind{cacheSnapshotClose, cachePassClose, cacheChangedAfterHit, cacheCancelledAfterHit, cachePermissionBeforeHit, cacheMissingBeforeHit, cacheReplacedAfterHit, cachePermissionAfterHit, cacheProofFailure} {
		t.Run(string(fault), func(t *testing.T) {
			dir := t.TempDir()
			id := "00000000-0000-4000-8000-000000000001"
			ref := agentapi.SourceRef{Kind: archive.SourceKindFile, Path: filepath.Join(dir, "rollout-"+id+".jsonl"), Key: id}
			raw := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q,\"source\":\"cli\",\"originator\":\"codex_cli_rs\",\"timestamp\":\"2026-10-01T12:00:00Z\"}}\n{\"type\":\"response_item\",\"role\":\"user\"}\n", id, dir)
			if err := os.WriteFile(ref.Path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cache := inspectioncache.New(filepath.Join(t.TempDir(), "cache"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			faults := cacheFaultSources{base: testSources}
			switch fault {
			case cacheProofFailure:
				faults.proofFailure = agentapi.Wrap(agentapi.Unavailable, errors.New("synthetic transient proof failure"))
			case cacheSnapshotClose:
				faults.snapshotClose = agentapi.Wrap(agentapi.Cleanup, errors.New("synthetic snapshot close"))
			case cachePassClose:
				faults.passClose = agentapi.Wrap(agentapi.Cleanup, errors.New("synthetic pass close"))
			case cacheChangedAfterHit, cacheCancelledAfterHit, cachePermissionBeforeHit, cacheMissingBeforeHit, cacheReplacedAfterHit, cachePermissionAfterHit:
				_, _, observed, err := InspectSourceCached(ctx, "codex", ref, time.Time{}, testSources, cache)
				if err != nil || observed == nil {
					t.Fatalf("prime: %+v %v", observed, err)
				}
				if err := cache.StoreBatch(ctx, []inspectioncache.Observation{*observed}); err != nil {
					t.Fatal(err)
				}
				switch fault {
				case cachePermissionAfterHit:
					faults.verify = func() error { return os.Chmod(ref.Path, 0000) }
				case cacheReplacedAfterHit:
					faults.verify = func() error {
						temp := ref.Path + ".replacement"
						if err := os.WriteFile(temp, []byte(raw), 0600); err != nil {
							return err
						}
						return os.Rename(temp, ref.Path)
					}
				case cachePermissionBeforeHit:
					faults.readFailure = os.ErrPermission
				case cacheMissingBeforeHit:
					if err := os.Remove(ref.Path); err != nil {
						t.Fatal(err)
					}
				case cacheCancelledAfterHit:
					faults.verify = func() error { cancel(); return ctx.Err() }
				case cacheSnapshotClose, cachePassClose, cacheProofFailure:
					t.Fatal("cleanup and proof failures must not prime the cache")
				case cacheChangedAfterHit:
					faults.verify = func() error {
						info, err := os.Stat(ref.Path)
						if err != nil {
							return err
						}
						rewritten := strings.Replace(raw, "response_item", "unknown_type_", 1)
						if err := os.WriteFile(ref.Path, []byte(rewritten), 0600); err != nil {
							return err
						}
						return os.Chtimes(ref.Path, info.ModTime(), info.ModTime())
					}
				}
			}
			_, summary, observed, err := InspectSourceCached(ctx, "codex", ref, time.Time{}, faults, cache)
			if err == nil || summary != nil || observed != nil {
				t.Fatalf("failed provider exposed reusable summary: %+v %+v %v", summary, observed, err)
			}
			if (fault == cacheChangedAfterHit || fault == cacheReplacedAfterHit) && !agentapi.HasFailure(err, agentapi.Changed) {
				t.Fatalf("content change classification: %v", err)
			}
		})
	}
}
