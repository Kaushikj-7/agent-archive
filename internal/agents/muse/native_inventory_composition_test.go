package muse

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// This test-only producer bypasses ONLY public OpenPass dispatch. Snapshots,
// provisional facts, complete Filter and proofs are the actual private Muse
// implementation. Production capture stays fenced; this is composition evidence.
type inventoryBridge struct {
	reads      *int
	rawReads   *bridgeRawCounts
	beforeOpen func()
}

func (b inventoryBridge) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	if name != "muse" {
		return nil, nil, false
	}
	return b, Filter{}, true
}

func (inventoryBridge) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	return Parser{}, name == "muse"
}

func (inventoryBridge) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return (SourceProvider{}).Describe(ref)
}

func (b inventoryBridge) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.beforeOpen != nil {
		b.beforeOpen()
	}
	if b.rawReads != nil {
		env.Files = bridgeRawOpener{Opener: env.Files, counts: b.rawReads}
	}
	return &inventoryBridgePass{sourcePass: &sourcePass{env: env, live: map[*sourceSnapshot]bool{}}, reads: b.reads}, nil
}

type inventoryBridgePass struct {
	*sourcePass
	reads *int
}

func (p *inventoryBridgePass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	*p.reads++
	return p.sourcePass.Read(ctx, ref, limits)
}

type inventoryBridgeMode string

const (
	inventorySuccess        inventoryBridgeMode = "success"
	inventoryObservations   inventoryBridgeMode = "observations"
	inventorySuffixFailure  inventoryBridgeMode = "suffix-failure"
	inventoryStaleSelection inventoryBridgeMode = "stale-selection"
	inventoryPaused         inventoryBridgeMode = "paused"
)

func TestInventoryPendingNoRequestThroughActualMuseProofAndGuardedCommit(t *testing.T) {
	t.Parallel()
	for _, mode := range []inventoryBridgeMode{inventorySuccess, inventoryObservations, inventorySuffixFailure, inventoryStaleSelection, inventoryPaused} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			project, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rootPath, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			fixture := "synthetic-exec-conversation.jsonl"
			if mode == inventoryObservations {
				fixture = "synthetic-own-observations.jsonl"
			}
			raw := bytes.ReplaceAll(readSynthetic(t, fixture), []byte("/work/synthetic-muse"), []byte(project))
			id := "00000000-0000-7000-8000-000000000001"
			path := filepath.Join(rootPath, "2026", "10", "15", id, "session.jsonl")
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if mode == inventorySuffixFailure {
				raw = append(raw, []byte("invalid suffix\n")...)
			}
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			if err := os.Chmod(home, 0700); err != nil {
				t.Fatal(err)
			}
			local, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			root := archive.NativeRoot{Agent: "muse", Configured: rootPath, Canonical: rootPath, Revision: "one"}
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			cfg := config.Config{MachineID: "synthetic", Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-inventory", Prefix: "synthetic", AWSProfile: "synthetic"}, Harnesses: []string{"muse"}, NativeRoots: []archive.NativeRoot{root}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: start}}}}
			reg := archive.SessionRegistration{ArchiveSessionID: "native-inventory-session", NativeSessionID: id, Harness: archive.Harness{Name: "muse"}, ProjectRoot: project, ProjectID: archive.ProjectID(project), SessionStartedAt: start, RegisteredAt: start.Add(time.Second), DestinationID: cfg.DestinationID()}
			if err = local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if err = config.Save(local.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			// There is deliberately no request, scan signature or journal: this exercises
			// the real pre-read owesNothing skip that otherwise hid pending owners forever.
			reads := 0
			var beforeOpen func()
			if mode == inventoryPaused {
				beforeOpen = func() {
					changed := cfg
					changed.Paused = true
					if err := config.Save(local.Home(), changed); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == inventoryStaleSelection {
				beforeOpen = func() {
					duplicate := filepath.Join(rootPath, "2026", "10", "14", id, "session.jsonl")
					if err := os.MkdirAll(filepath.Dir(duplicate), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(duplicate, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			bridge := inventoryBridge{reads: &reads, beforeOpen: beforeOpen}
			var inventories []*discovery.NativeInventory
			var budget *agentapi.NativeReadBudget
			opts := collector.Options{Sources: bridge, Parsers: bridge, MachineID: "synthetic", NativeRoots: cfg.NativeRoots, RequireSkillUse: true, RepoKey: func(string) string { return "" }, AcceptSession: cfg.AcceptSession, NativeLocatorFactory: func(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
				budget = pass.ReadBudget
				l, err := discovery.NewNativeInventory(ctx, pass.Store.Home(), pass.Roots, pass.Queries, pass.ReadBudget, ValidateNativeLocator)
				if err != nil {
					return nil, nil, err
				}
				inventories = append(inventories, l)
				return l, l.Close, nil
			}}
			remote := storagetest.NewMemoryStore()
			result, runErr := collector.Run(t.Context(), local, remote, opts)
			defer result.CloseStagedResults()
			saved, found, err := local.LoadRegistration(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			if reads != 1 {
				t.Fatalf("no-request pending owner did not reach exactly one real source Filter read: %d, result=%+v, err=%v", reads, result, runErr)
			}
			if mode != inventorySuccess && mode != inventoryObservations {
				if saved.TranscriptPath != "" || saved.NativeBinding != nil {
					t.Fatal("failed source/policy/selection committed authority")
				}
				return
			}
			if runErr != nil || len(result.Errors) > 0 {
				t.Fatal(runErr, result.Errors)
			}
			if saved.TranscriptPath != path || saved.NativeBinding == nil || saved.NativeBinding.Checkpoint == nil || saved.NativeBinding.Checkpoint.Boundary != int64(len(raw)) || !saved.SessionStartedAt.Equal(reg.SessionStartedAt) || !saved.RegisteredAt.Equal(reg.RegisteredAt) {
				t.Fatalf("first bind not atomic/original owner lost: %+v", saved)
			}
			if mode == inventoryObservations {
				bundle, _, _, found, err := local.LoadPublished(reg.ArchiveSessionID)
				if err != nil || !found {
					t.Fatal("missing observation-bearing publication", found, err)
				}
				analysis, err := (Parser{}).Parse(t.Context(), bundle)
				if err != nil {
					t.Fatal(err)
				}
				summary := analysis.TokenObservations
				if !summary.HasObservations() || *summary.Input != 202 || *summary.Output != 46 || summary.Coverage.Available() || summary.Composition.Available() || summary.Pricing.Available() {
					t.Fatal("first binding changed observation accounting", summary)
				}
			}
			if charged, _ := budget.Charged(); charged != 0 {
				t.Fatal("pass leaked", charged)
			}
			// The bound owner follows its immutable path; no second inventory acquisition,
			// and the settled strong signature skips the actual full Filter and any PUT.
			bridge.beforeOpen = nil
			counts := &bridgeRawCounts{}
			bridge.rawReads = counts
			opts.Sources = bridge
			result2, err := collector.Run(t.Context(), local, remote, opts)
			defer result2.CloseStagedResults()
			if err != nil || len(result2.Errors) > 0 {
				t.Fatal(err, result2.Errors)
			}
			t.Logf("warm direct raw: opens=%d calls=%d bytes=%d boundary=%d", counts.opens, counts.calls, counts.bytes, len(raw))
			if counts.bytes < int64(len(raw)) || counts.opens == 0 {
				t.Fatal("warm raw verification missing", counts)
			}
			if reads != 1 || len(inventories) != 2 || inventories[1].Counts() != (discovery.NativeInventoryCounts{}) || len(result2.Published) != 0 {
				t.Fatal("warm bound inventory/read/publication", reads, result2, inventories[1].Counts())
			}
		})
	}
}

func TestFirstBindingEvidenceNeedsIndependentRootAndExactOrdinaryOwner(t *testing.T) {
	t.Parallel()
	p, s, _, _ := proofFixture(t, "synthetic-exec-empty.jsonl")
	facts, err := s.startupFacts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmissionEvidence(t.Context(), agentapi.SourceAdmission{NativeID: facts.NativeID}); err == nil {
		t.Fatal("explicit file minted first bind")
	}
	// A malformed union cannot fall through to the nil-previous branch.
	for _, binding := range []*agentapi.SourceBinding{{}, {Codex: &archive.CodexSourceBinding{}}, {Native: &archive.NativeSourceBinding{Version: 1}}} {
		if _, err = s.AdmissionEvidence(t.Context(), agentapi.SourceAdmission{NativeID: facts.NativeID, Binding: binding}); err == nil {
			t.Fatal("malformed/lost prior reconstructed")
		}
	}
	if err = p.Close(); err != nil && !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal(err)
	}
}

type bridgeRawCounts struct {
	opens int64
	calls int64
	bytes int64
}

type bridgeRawOpener struct {
	transcriptio.Opener
	counts *bridgeRawCounts
}

func (o bridgeRawOpener) OpenRegular(path string) (transcriptio.File, error) {
	f, e := o.Opener.OpenRegular(path)
	if e != nil {
		return nil, e
	}
	o.counts.opens++
	return bridgeRawFile{File: f, counts: o.counts}, nil
}

type bridgeRawFile struct {
	transcriptio.File
	counts *bridgeRawCounts
}

func (f bridgeRawFile) ReadAt(b []byte, off int64) (int, error) {
	n, e := f.File.ReadAt(b, off)
	f.counts.calls++
	f.counts.bytes += int64(n)
	return n, e
}
