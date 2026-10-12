package collector

import (
	"context"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type labelBudgetSources struct {
	agentapi.SourcesLookup
	inspect func(*agentapi.NativeReadBudget)
}

func (s labelBudgetSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := s.SourcesLookup.LookupSources(name)
	return labelBudgetProvider{SourceProvider: p, inspect: s.inspect}, f, ok
}

type labelBudgetProvider struct {
	agentapi.SourceProvider
	inspect func(*agentapi.NativeReadBudget)
}

func (p labelBudgetProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p.inspect(env.ReadBudget)
	return p.SourceProvider.OpenPass(ctx, env)
}

// Generic naming providers do not supply a Codex rollout ledger. Their borrowed
// retained publications must still consume the same capacity as native work.
func TestLabelContextAndNativeSourceShareDefaultPassBudget(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatal(result, err)
	}
	var decoded int64
	var ledger *agentapi.NativeReadBudget
	opts.Labels = mutableLabelLookup{&mutableLabels{}}
	opts.labelReadObserver = func(n int64) { decoded += n }
	opts.Sources = labelBudgetSources{SourcesLookup: testSources, inspect: func(b *agentapi.NativeReadBudget) {
		ledger = b
		if decoded <= 0 {
			t.Fatal("native signature preceded retained label context")
		}
		if b.Available() > agentapi.PassReadBudget-decoded {
			t.Errorf("borrowed label context bypassed native ledger: decoded=%d available=%d", decoded, b.Available())
		}
	}}
	now = now.Add(time.Hour)
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatal(result, err)
	}
	if ledger == nil || ledger.Available() != agentapi.PassReadBudget {
		t.Fatal("pass did not release all borrowed label/native owners")
	}
}
