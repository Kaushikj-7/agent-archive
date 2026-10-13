package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/inspectioncache"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type ownedReadFiles struct {
	transcriptio.OS
	bytes  *int64
	before func([]byte, int64)
}

type ownedReadFile struct {
	transcriptio.File
	bytes  *int64
	before func([]byte, int64)
}

func (f ownedReadFiles) OpenRegular(path string) (transcriptio.File, error) {
	file, err := f.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return ownedReadFile{file, f.bytes, f.before}, nil
}

func (f ownedReadFile) ReadAt(p []byte, off int64) (int, error) {
	if f.before != nil {
		f.before(p, off)
	}
	n, err := f.File.ReadAt(p, off)
	*f.bytes += int64(n)
	return n, err
}

type ownedReadSources struct {
	agentapi.SourcesLookup
	bytes  *int64
	before func([]byte, int64)
}

type ownedReadProvider struct {
	agentapi.SourceProvider
	bytes  *int64
	before func([]byte, int64)
}

func (s ownedReadSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, filter, ok := s.SourcesLookup.LookupSources(name)
	return ownedReadProvider{provider, s.bytes, s.before}, filter, ok
}

func (p ownedReadProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	env.Files = ownedReadFiles{bytes: p.bytes, before: p.before}
	return p.SourceProvider.OpenPass(ctx, env)
}

func TestOwnedInspectionKeepsBaselineIndependentReadsAndVerifiesWarmHit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := "00000000-0000-4000-8000-000000000001"
	ref := agentapi.SourceRef{Kind: archive.SourceKindFile, Path: filepath.Join(dir, "rollout-"+id+".jsonl"), Key: id}
	header := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q,\"source\":\"cli\",\"timestamp\":\"2026-10-01T12:00:00Z\"}}\n", id, dir)
	row := `{"type":"response_item","payload":{"type":"message","role":"user","content":"` + strings.Repeat("synthetic text ", 1000) + `"}}` + "\n"
	raw := header + strings.Repeat(row, 16)
	if err := os.WriteFile(ref.Path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var readBytes int64
	sources := ownedReadSources{SourcesLookup: testSources, bytes: &readBytes}
	cache := inspectioncache.New(filepath.Join(t.TempDir(), "cache"))
	_, baseline, _, err := InspectSourceCached(t.Context(), "codex", ref, time.Time{}, sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	baselineBytes := readBytes
	readBytes = 0
	_, cold, observation, err := InspectSourceCached(t.Context(), "codex", ref, time.Time{}, sources, cache)
	if err != nil || observation == nil || baseline == nil || cold == nil || *baseline != *cold {
		t.Fatalf("cold summary/observation changed: %v", err)
	}
	coldBytes := readBytes
	if coldBytes != baselineBytes {
		t.Fatalf("owned miss bytes=%d baseline=%d", coldBytes, baselineBytes)
	}
	readBytes = 0
	// A wrapper hiding ownership attestation keeps the fully verified fallback.
	_, fallback, _, err := InspectSourceCached(t.Context(), "codex", ref, time.Time{}, cacheFaultSources{base: sources}, cache)
	if err != nil || fallback == nil || *fallback != *cold {
		t.Fatalf("fallback: %v", err)
	}
	if readBytes-coldBytes != 2*int64(len(raw)+len(header)) {
		t.Fatalf("removed verification bytes=%d want=%d", readBytes-coldBytes, 2*(len(raw)+len(header)))
	}
	if err := cache.StoreBatch(t.Context(), []inspectioncache.Observation{*observation}); err != nil {
		t.Fatal(err)
	}
	readBytes = 0
	_, warm, observed, err := InspectSourceCached(t.Context(), "codex", ref, time.Time{}, sources, cache)
	if err != nil || warm == nil || *warm != *cold || observed != nil {
		t.Fatalf("warm hit: %v", err)
	}
	// A warm hit still hashes current bytes independently after lookup, removing
	// only the filter stream compared with the ordinary independently checked miss.
	if coldBytes-readBytes != int64(len(raw))+1 {
		t.Fatalf("warm verification bytes: cold=%d warm=%d", coldBytes, readBytes)
	}

	info, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	bulkStarts := 0
	sources.before = func(p []byte, off int64) {
		if off != 0 || len(p) < 64<<10 {
			return
		}
		bulkStarts++
		if bulkStarts != 2 {
			return
		}
		// First start is captured hashing; second is mandatory postlookup
		// verification of the matching cached hint. Preserve size and timestamps.
		changed := strings.ReplaceAll(raw, "synthetic text", "different text")

		if len(changed) != len(raw) {
			t.Fatal("rewrite changed length")
		}
		if err := os.WriteFile(ref.Path, []byte(changed), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(ref.Path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	_, rejected, observed, err := InspectSourceCached(t.Context(), "codex", ref, time.Time{}, sources, cache)
	if err == nil || rejected != nil || observed != nil {
		t.Fatalf("postlookup rewrite escaped live verification: summary=%+v observed=%+v err=%v", rejected, observed, err)
	}
}
