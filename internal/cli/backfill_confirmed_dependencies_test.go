package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type confirmedFilterMutation struct {
	agentapi.TranscriptFilter
	after  func(agentapi.FilterContext)
	failed func(error)
}

func (f confirmedFilterMutation) Filter(ctx context.Context, input agentapi.NativeInput, facts agentapi.FilterContext) (archive.FilteredTranscript, error) {
	filtered, err := f.TranscriptFilter.Filter(ctx, input, facts)
	if err == nil {
		f.after(facts)
	} else if f.failed != nil {
		f.failed(err)
	}
	return filtered, err
}

func (f confirmedFilterMutation) LeasedFilterFor(adapter archive.Adapter) bool {
	_, ok := adapter.(confirmedFilterMutation)
	return ok
}

func (f confirmedFilterMutation) FilterLeased(ctx context.Context, input agentapi.NativeInput, facts agentapi.FilterContext, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	filtered, release, err := f.TranscriptFilter.(agentapi.LeasedTranscriptFilter).FilterLeased(ctx, input, facts, budget)
	if err == nil {
		f.after(facts)
	} else if f.failed != nil {
		f.failed(err)
	}
	return filtered, release, err
}

func (f confirmedFilterMutation) RefilterBounded(ctx context.Context, bundle archive.SourceBundle, at time.Time, limits agentapi.ReadLimits) (archive.FilteredTranscript, error) {
	return f.TranscriptFilter.(agentapi.BoundedTranscriptRefilter).RefilterBounded(ctx, bundle, at, limits)
}

func (f confirmedFilterMutation) RefilterLeased(ctx context.Context, bundle archive.SourceBundle, at time.Time, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	return f.TranscriptFilter.(agentapi.LeasedTranscriptRefilter).RefilterLeased(ctx, bundle, at, budget)
}

func (f confirmedFilterMutation) MeaningfulRevisionRecord(bundle archive.SourceBundle, index int) bool {
	return f.TranscriptFilter.(agentapi.RevisionEvidence).MeaningfulRevisionRecord(bundle, index)
}

func (f confirmedFilterMutation) NamingOnlyChange(previous, candidate archive.SourceBundle) bool {
	return f.TranscriptFilter.(agentapi.NamingChangeComparator).NamingOnlyChange(previous, candidate)
}

func TestBackfillConfirmedRenewsRequiredMiddleDependencyAfterFiltering(t *testing.T) {
	for _, retarget := range []bool{false, true} {
		t.Run(strconv.FormatBool(retarget), func(t *testing.T) {
			f, bucket, _ := newHistoricalOnlyFixture(t)
			// This timing control uses canonical trusted homes. Raw index paths
			// through a platform home alias have a separate, reproduced lookup
			// ownership refusal and cannot reach this post-filter boundary.
			var err error
			f.data, err = filepath.EvalSymlinks(f.data)
			must(t, err)
			f.userHome, err = filepath.EvalSymlinks(f.userHome)
			must(t, err)
			f.env = setupTestEnv(t, f.data, f.userHome, newFakeKeychain(), time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
			f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
			f.env.repoKey = func(string) string { t.Fatal("historical import invoked Git"); return "" }
			must(t, os.RemoveAll(filepath.Join(f.userHome, ".claude")))
			original := filepath.Join(f.userHome, "vanished-service")
			mapping := writeHistoricalMapping(t, original)
			allowed, excluded := filepath.Join(f.userHome, "allowed"), filepath.Join(f.userHome, "excluded")
			must(t, os.MkdirAll(allowed, 0700))
			must(t, os.MkdirAll(excluded, 0700))
			alias := filepath.Join(f.userHome, "middle-alias")
			must(t, os.Symlink(allowed, alias))
			cfg := mustLoadConfig(t, f.data)
			cfg.Harnesses = []string{"codex"}
			cfg.Archive.Projects = []archive.ProjectActivation{{Root: excluded, ProjectID: archive.ProjectID(excluded), Included: false}}
			must(t, config.Save(f.data, cfg))
			root := filepath.Join(f.userHome, ".codex")
			must(t, os.MkdirAll(filepath.Join(root, "sessions"), 0700))
			const thread = "11111111-1111-4111-8111-111111111111"
			const middle = "22222222-2222-4222-8222-222222222222"
			const tip = "33333333-3333-4333-8333-333333333333"
			header := func(cwd string, ordinal int, base any) []byte {
				payload := map[string]any{"id": thread, "cwd": cwd, "timestamp": "2026-10-11T12:00:00Z", "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
				if base != nil {
					payload["history_base"] = base
				}
				raw, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "payload": payload})
				must(t, err)
				return append(raw, '\n')
			}
			message := func(ordinal int) []byte {
				return []byte(fmt.Sprintf(`{"type":"response_item","ordinal":%d,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic required history %d"}]}}`+"\n", ordinal, ordinal))
			}
			first := append(header(original, 0, nil), []byte(`{"type":"event_msg","ordinal":1,"payload":{"type":"task_started","turn_id":"synthetic-task","root_turn_id":"synthetic-task","started_at":"2026-10-11T12:00:00Z"}}`+"\n")...)
			mid := append(header(alias, 2, map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(first)}), message(3)...)
			last := append(header(original, 4, map[string]any{"thread_id": middle, "end_ordinal_exclusive": 4, "end_byte_offset": len(mid)}), message(5)...)
			var selected string
			for id, raw := range map[string][]byte{thread: first, middle: mid, tip: last} {
				path := filepath.Join(root, "sessions", "rollout-"+id+".jsonl")
				must(t, os.WriteFile(path, raw, 0600))
				if id == tip {
					selected = path
				}
			}
			db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
			must(t, err)
			defer func() { must(t, db.Close()) }()
			_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
			must(t, err)
			_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, selected)
			must(t, err)
			preview, previewErr, previewCode := f.importRun(t, nil, false, "--dry-run", "--json", "--harness", "codex", "--mapping-file", mapping)
			t.Logf("undecorated supported-history preview: code=%d stderr=%s plan=%s", previewCode, previewErr, preview)
			confirmed, mutated := false, false
			resources := &foregroundResources{}
			var filterFailures []error
			f.env.backfillCheckpoint = func(step string) error { confirmed = confirmed || step == "committed"; return nil }
			var bindings []builtin.Integration
			for _, descriptor := range productionAgents.Catalog().All() {
				binding, ok := productionAgents.Lookup(string(descriptor.ID))
				if !ok {
					t.Fatal("missing builtin")
				}
				binding.Descriptor = agentmeta.Descriptor{ID: descriptor.ID}
				if descriptor.ID == agentmeta.Codex {
					binding.Sources = foregroundProvider{SourceProvider: binding.Sources, resources: resources}
					binding.Filter = confirmedFilterMutation{TranscriptFilter: binding.Filter, after: func(facts agentapi.FilterContext) {
						if confirmed && !mutated && filepath.Base(facts.Filename) == filepath.Base(selected) {
							mutated = true
							if retarget {
								must(t, os.Remove(alias))
								must(t, os.Symlink(excluded, alias))
							}
						}
					}, failed: func(err error) { filterFailures = append(filterFailures, err) }}
				}
				bindings = append(bindings, binding)
			}
			f.env.Agents, err = builtin.New(agentmeta.Builtins(), bindings)
			must(t, err)
			out, stderr, code := f.importRun(t, nil, false, "--yes", "--harness", "codex", "--mapping-file", mapping)
			if !mutated {
				for _, failure := range append(resources.failures, filterFailures...) {
					t.Logf("supported history refusal: %T %#v %v", failure, failure, failure)
					var typed *agentapi.SourceError
					if errors.As(failure, &typed) {
						t.Logf("source refusal detail: %#v underlying=%v", typed, typed.Unwrap())
					}
				}
				t.Fatal("confirmed selected filter did not run", code, stderr, out)
			}
			local := state.OpenReadOnly(f.data)
			regs, err := local.LoadRegistrations()
			must(t, err)
			if retarget {
				if len(regs) != 0 {
					t.Fatal("retargeted required middle dependency gained admission")
				}
				stages, _, err := local.ImportStages(t.Context(), "", 50)
				must(t, err)
				if len(stages) != 0 {
					t.Fatal("changed dependency became a frozen prepared selection")
				}
				objects, err := bucket.List(t.Context(), "")
				must(t, err)
				if len(objects) != 0 {
					t.Fatal("changed dependency reached remote storage")
				}
				return
			}
			if code != 0 || len(regs) != 1 {
				if len(regs) == 1 {
					result, retryErr := runPass(f.env, false, passOptions{importBatch: regs[0].ImportBatch.Recorded()})
					t.Logf("selected publication diagnostic: err=%v errors=%v deferred=%v retry=%v", retryErr, result.Errors, result.Deferred, result.RetryLater)
					result.CloseStagedResults()
				}
				t.Fatal("unchanged required history failed", code, len(regs), stderr, out)
			}
			published, err := local.LoadPublishedState(regs[0].ArchiveSessionID)
			must(t, err)
			metadata, found, err := published.LastPublishedMetadata()
			must(t, err)
			if !found {
				t.Fatal("unchanged history not published")
			}
			_, err = reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
			must(t, err)
		})
	}
}
