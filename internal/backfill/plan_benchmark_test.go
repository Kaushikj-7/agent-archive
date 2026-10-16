package backfill

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type planScenarioKind string

const (
	planScenarioRecent   planScenarioKind = "recent"
	planScenarioOld      planScenarioKind = "old"
	planScenarioLarge    planScenarioKind = "large"
	planScenarioRetained planScenarioKind = "retained"
	planScenarioTool     planScenarioKind = "tool"
)

type planReadCounts struct {
	bytes atomic.Int64
	reads atomic.Int64
}

type planCountedFiles struct {
	transcriptio.OS
	counts *planReadCounts
}

type planCountedFile struct {
	transcriptio.File
	counts *planReadCounts
}

func (f planCountedFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	f.counts.bytes.Add(int64(n))
	f.counts.reads.Add(1)
	return n, err
}

func (f planCountedFiles) OpenRegular(path string) (transcriptio.File, error) {
	file, err := f.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return planCountedFile{file, f.counts}, nil
}

type planCountedSources struct {
	agentapi.SourcesLookup
	files planCountedFiles
}

type planCountedProvider struct {
	agentapi.SourceProvider
	files planCountedFiles
}

func (s planCountedSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := s.SourcesLookup.LookupSources(name)
	if !ok {
		return p, f, false
	}
	return planCountedProvider{p, s.files}, f, true
}

func (p planCountedProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	env.Files = p.files
	return p.SourceProvider.OpenPass(ctx, env)
}

type planCountedHeader struct {
	io.ReadCloser
	counts *planReadCounts
}

func (r planCountedHeader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.counts.bytes.Add(int64(n))
	r.counts.reads.Add(1)
	return n, err
}

// BenchmarkBuildPlan measures full discovery, recovery and inspection with actual
// provider file reads counted. Whole-command subprocess RSS/CPU and completion
// are measured separately by scripts/measure_backfill.py. Large corpus creation
// is explicit, outside the timer, and never occurs in ordinary test runs.
func BenchmarkBuildPlan(b *testing.B) {
	for _, scenario := range []planScenarioKind{planScenarioRecent, planScenarioOld, planScenarioLarge, planScenarioRetained, planScenarioTool} {
		b.Run(string(scenario), func(b *testing.B) {
			home := b.TempDir()
			root := filepath.Join(home, "project")
			if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
				b.Fatal(err)
			}
			dir := filepath.Join(home, ".codex", "sessions")
			if err := os.MkdirAll(dir, 0700); err != nil {
				b.Fatal(err)
			}
			count := 1500
			if scenario == planScenarioLarge || scenario == planScenarioRetained || scenario == planScenarioTool {
				count = 32
			}
			expected := count
			var total int64
			for i := range count {
				id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
				at := fixedNow.Add(-48 * time.Hour)
				if scenario == planScenarioOld && i%10 != 0 {
					at = fixedNow.Add(-30 * 24 * time.Hour)
					expected--
				}
				file, err := os.Create(filepath.Join(dir, "rollout-"+id+".jsonl"))
				if err != nil {
					b.Fatal(err)
				}
				n, err := file.WriteString(codexTranscript(id, id, root, at))
				if err != nil {
					b.Fatal(err)
				}
				total += int64(n)
				target := int64(256<<10) * (1 + int64(i%8))
				if scenario == planScenarioLarge {
					target = 1024
					if i == 0 {
						target = 210 << 20
					}
				}
				if scenario == planScenarioRetained || scenario == planScenarioTool {
					target = 4 << 20
				}
				row := `{"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("synthetic stdout ", 1024) + `"}}` + "\n"
				if scenario == planScenarioLarge && i == 0 {
					// Preserve ordinary production output limits while inspecting
					// the complete large raw source.
					row = `{"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 1<<20) + `"}}` + "\n"
				}
				if scenario == planScenarioRetained || i%3 == 0 && scenario != planScenarioLarge {
					row = `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("Synthetic explanation. ", 700) + `"}]}}` + "\n"
				}
				for wrote := int64(0); wrote < target; {
					n, err = file.WriteString(row)
					if err != nil {
						b.Fatal(err)
					}
					wrote += int64(n)
					total += int64(n)
				}
				if err := file.Close(); err != nil {
					b.Fatal(err)
				}
			}
			counts := &planReadCounts{}
			registry := builtin.NewBuiltins()
			env := Environment{Home: home, Now: func() time.Time { return fixedNow }, Discovery: importFixtureDiscovery{registry}, Imports: registry, NativePaths: registry, Worktrees: registry, Workspaces: registry, Children: registry, DatabaseCatalogs: registry, Sources: planCountedSources{registry, planCountedFiles{counts: counts}}, Workers: 1,
				Open: func(path string) (io.ReadCloser, error) {
					file, err := os.Open(path)
					if err != nil {
						return nil, err
					}
					return planCountedHeader{file, counts}, nil
				},
			}
			cfg := config.Config{}
			b.ReportAllocs()
			b.SetBytes(total)
			b.ResetTimer()
			for range b.N {
				plan, err := BuildPlan(b.Context(), env, states{}, cfg, Filters{Harnesses: []string{"codex"}, Since: "2026-09-16", IncludeTemp: true})
				if err != nil {
					b.Fatal(err)
				}
				if plan.Found() != count || len(plan.Imported()) != expected {
					b.Fatalf("found %d imported%d, want%d/%d", plan.Found(), len(plan.Imported()), count, expected)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(counts.bytes.Load())/float64(b.N), "read_bytes/op")
			b.ReportMetric(float64(counts.reads.Load())/float64(b.N), "reads/op")
		})
	}
}
