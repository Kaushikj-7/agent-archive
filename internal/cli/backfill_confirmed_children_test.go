package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type confirmedPlainFilterMutation struct {
	agentapi.TranscriptFilter
	after func(agentapi.FilterContext) error
}

func TestBackfillPartialChildrenPreserveCompletedGroups(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(map[bool]string{false: "I/O before group manifest", true: "policy between children"}[policy], func(t *testing.T) {
			f, bucket := newImportFixture(t)
			project := filepath.Join(f.userHome, "agent-archive")
			out, stderr, code := f.importRun(t, nil, false, "--yes", "--background", "--harness", "claude", "--project", project, "--until", "2026-09-20")
			if code != 0 {
				t.Fatal("earlier completed preparation", code, stderr, out)
			}
			store := state.OpenReadOnly(f.data)
			stages, _, err := store.ImportStages(t.Context(), "", 50)
			must(t, err)
			if len(stages) != 1 {
				t.Fatal("missing earlier completed group", len(stages))
			}
			earlier := stages[0].Registration
			before := snapshotTree(t, filepath.Join(f.data, "sessions", earlier.ArchiveSessionID))
			confirmed, interrupted := false, false
			partialID := ""
			f.env.backfillCheckpoint = func(step string) error { confirmed = confirmed || step == "committed"; return nil }
			decorateConfirmedClaudeFilter(t, f, func(facts agentapi.FilterContext) error {
				if !confirmed || filepath.Base(facts.Filename) != "agent-a2.jsonl" {
					return nil
				}
				interrupted = true
				stages, _, err := store.ImportStages(t.Context(), "", 50)
				must(t, err)
				for _, stage := range stages {
					if stage.Registration.NativeSessionID == "c-aa-2:subagent:a1" {
						partialID = stage.Registration.ArchiveSessionID
					}
				}
				if partialID == "" {
					t.Fatal("first child was not durable before the second child finished filtering")
				}
				if !policy {
					return errors.New("synthetic interruption before complete group preparation")
				}
				cfg := mustLoadConfig(t, f.data)
				cfg.Archive.Projects[0].Included = false
				must(t, config.Save(f.data, cfg))
				return nil
			})
			args := []string{"--yes", "--harness", "claude", "--project", project, "--since", "2026-09-21"}
			out, stderr, code = f.importRun(t, nil, false, append(args, "--background")...)
			if !interrupted || code != 1 {
				t.Fatal("between-child boundary not reached", code, stderr, out)
			}
			batches, err := backfill.LoadBatches(f.data)
			must(t, err)
			batch := batches[len(batches)-1].ID
			regs, err := store.LoadRegistrations()
			must(t, err)
			for _, reg := range regs {
				if reg.InBatch(batch) {
					t.Fatal("partial child payload became logical admission")
				}
			}
			if before != snapshotTree(t, filepath.Join(f.data, "sessions", earlier.ArchiveSessionID)) {
				t.Fatal("partial group changed earlier completed preparation")
			}
			stages, _, err = store.ImportStages(t.Context(), "", 50)
			must(t, err)
			partial := 0
			for _, stage := range stages {
				if stage.Registration.InBatch(batch) {
					partial++
				}
			}
			want := 1
			if policy {
				want = 0
			}
			if partial != want {
				t.Fatal("partial payload cleanup changed", partial, want)
			}
			if policy {
				_, err = os.Stat(filepath.Join(f.data, "sessions", partialID, "import-stage", "cancel.json"))
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("completed cancellation left its cleanup journal", err)
				}
				writable, err := state.Open(f.data)
				must(t, err)
				err = writable.ResumeAllImportAdmissions(t.Context())
				must(t, err)
				if _, found, err := store.LoadRegistration(partialID); err != nil || found {
					t.Fatal("cancelled partial child was admitted on recovery", found, err)
				}
				return
			}
			// Partial payloads alone promise retry with native input, not
			// native-loss recovery of an unstaged parent's content.
			f.env.Agents, f.env.backfillCheckpoint = productionAgents, nil
			out, stderr, code = f.importRun(t, nil, false, args...)
			if code != 0 {
				status, statusErr := store.LoadStatus()
				t.Logf("partial retry status: %+v err=%v", status, statusErr)
				regs, err := store.LoadRegistrations()
				must(t, err)
				for _, reg := range regs {
					if !reg.InBatch(batch) {
						continue
					}
					request, found, err := store.LoadRequest(reg.ArchiveSessionID)
					must(t, err)
					owed, err := store.Outstanding(reg, found)
					must(t, err)
					t.Logf("partial retry owner: native=%s parent=%s request=%+v owed=%+v", reg.NativeSessionID, reg.ParentSessionID, request, owed)
				}
				result, retryErr := runPass(f.env, false, passOptions{importBatch: batch})
				t.Logf("partial retry next pass: err=%v errors=%v published=%v deferred=%v retry=%v", retryErr, result.Errors, result.Published, result.Deferred, result.RetryLater)
				result.CloseStagedResults()
				t.Fatal("partial group retry", code, stderr, out)
			}
			regs, err = store.LoadRegistrations()
			must(t, err)
			owned := 0
			for _, reg := range regs {
				if !reg.InBatch(batch) {
					continue
				}
				owned++
				published, err := store.LoadPublishedState(reg.ArchiveSessionID)
				must(t, err)
				metadata, found, err := published.LastPublishedMetadata()
				must(t, err)
				if !found {
					t.Fatal("retried group not published")
				}
				_, err = reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
				must(t, err)
			}
			if owned != 3 {
				t.Fatal("partial retry duplicated or lost logical owners", owned)
			}
			key, err := archive.MetadataObjectKey(earlier.Harness.Name, earlier.ArchiveSessionID)
			must(t, err)
			if _, err := bucket.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("retry published earlier unrelated group", err)
			}
		})
	}
}

func (f confirmedPlainFilterMutation) Filter(ctx context.Context, input agentapi.NativeInput, facts agentapi.FilterContext) (archive.FilteredTranscript, error) {
	filtered, err := f.TranscriptFilter.Filter(ctx, input, facts)
	if err == nil {
		err = f.after(facts)
	}
	return filtered, err
}

func TestBackfillRenewsAuthorityBeforeFirstChildStage(t *testing.T) {
	f, _ := newImportFixture(t)
	confirmed, changed := false, false
	f.env.backfillCheckpoint = func(step string) error { confirmed = confirmed || step == "committed"; return nil }
	decorateConfirmedClaudeFilter(t, f, func(facts agentapi.FilterContext) error {
		if !confirmed {
			return nil
		}
		switch filepath.Base(facts.Filename) {
		case "c-aa-2.jsonl":
			cfg := mustLoadConfig(t, f.data)
			cfg.Archive.Projects[0].Included = false
			must(t, config.Save(f.data, cfg))
			changed = true
		case "agent-a2.jsonl":
			// A later I/O interruption exposes any first child stage saved
			// before the changed parent authority was renewed.
			return errors.New("synthetic next-child I/O interruption")
		}
		return nil
	})
	out, stderr, code := f.importRun(t, nil, false, "--yes", "--background", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive"), "--since", "2026-09-21")
	if !changed {
		t.Fatal("confirmed parent filtering not reached", code, stderr, out)
	}
	stages, _, err := state.OpenReadOnly(f.data).ImportStages(t.Context(), "", 50)
	must(t, err)
	if len(stages) != 0 {
		t.Fatal("child evidence became durable before current parent authority", len(stages), code, stderr, out)
	}
}

func decorateConfirmedClaudeFilter(t *testing.T, f *backfillFixture, after func(agentapi.FilterContext) error) {
	t.Helper()
	var bindings []builtin.Integration
	for _, descriptor := range productionAgents.Catalog().All() {
		binding, ok := productionAgents.Lookup(string(descriptor.ID))
		if !ok {
			t.Fatal("missing builtin")
		}
		binding.Descriptor = agentmeta.Descriptor{ID: descriptor.ID}
		if descriptor.ID == agentmeta.Claude {
			binding.Filter = confirmedPlainFilterMutation{binding.Filter, after}
		}
		bindings = append(bindings, binding)
	}
	var err error
	f.env.Agents, err = builtin.New(agentmeta.Builtins(), bindings)
	must(t, err)
}
