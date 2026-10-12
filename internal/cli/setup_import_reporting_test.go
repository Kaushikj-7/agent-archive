package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Removing a transcript after the immutable plan is committed produces a
// nil-error Gone outcome. Setup must report it even when no session registers.
func TestSetupImportReportsNilErrorSkips(t *testing.T) {
	for _, remaining := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(remaining), func(t *testing.T) {
			f := newScreenFixture(t)
			f.withApps(t, "claude")
			f.inWebApp(t)
			f.pastSession(t, "gone", "src/web-app", screenNow.Add(-2*importDay))
			if remaining > 0 {
				f.pastSession(t, "kept", "src/web-app", screenNow.Add(-3*importDay))
			}
			if remaining < 2 {
				f.env.backfillCheckpoint = func(step string) error {
					if step == "committed" {
						return os.Remove(filepath.Join(f.userHome, ".claude", "projects", "src-web-app", "gone.jsonl"))
					}
					return nil
				}
			}
			out := f.runSetup(t, "", setupYesArgs...)
			ids := importedSessions(t, f.home)
			if len(ids) != remaining {
				t.Fatalf("registered %v, want %d\n%s", ids, remaining, out)
			}
			if remaining < 2 {
				for _, want := range []string{"Not registered: 1 whose transcript is gone.", "Some recent sessions were not imported.", "agent-archive backfill --since 7d"} {
					if !strings.Contains(out, want) {
						t.Fatalf("missing %q\n%s", want, out)
					}
				}
			} else if strings.Contains(out, "Not registered:") || strings.Contains(out, "Some recent sessions were not imported.") {
				t.Fatalf("successful import reported skips\n%s", out)
			}
			if remaining == 0 && strings.Contains(out, "Imported ") {
				t.Fatalf("false import success\n%s", out)
			}
			if remaining > 0 && !strings.Contains(out, fmt.Sprintf("(Claude Code %d)", remaining)) {
				t.Fatalf("wrong successful cohort\n%s", out)
			}
			assertSetupKept(t, f)
		})
	}
}

// Both runs use the same setup filters and local day. The second run resumes
// the original batch, but its success line describes only its new registration.
func TestSetupImportResumeReportsCurrentRunCohort(t *testing.T) {
	f := newImportOfferFixture(t)
	f.env.backfillHoldSteps = 1
	crashed := false
	f.env.backfillCheckpoint = func(step string) error {
		if step == "registered" && !crashed {
			crashed = true
			return errors.New("simulated crash")
		}
		return nil
	}
	first := f.runSetup(t, "", setupYesArgs...)
	if !crashed || !strings.Contains(first, "simulated crash") || len(importedSessions(t, f.home)) != 1 {
		t.Fatalf("missing partial registration\n%s", first)
	}
	before, err := backfill.LoadBatches(f.home)
	must(t, err)
	f.env.backfillCheckpoint = nil
	out := f.runSetup(t, "", setupYesArgs...)
	after, err := backfill.LoadBatches(f.home)
	must(t, err)
	if len(before) != 1 || len(after) != 1 || before[0].ID != after[0].ID || after[0].CompletedAt == nil {
		t.Fatalf("did not resume the same batch: before=%+v after=%+v", before, after)
	}
	if got := importedSessions(t, f.home); !slices.Equal(got, []string{"six", "two"}) {
		t.Fatalf("wrong admission scope: %v", got)
	}
	if !strings.Contains(out, "Imported 1 session from the last 7 days (Claude Code 1).") || strings.Contains(out, "Claude Code 2") {
		t.Fatalf("current count disagrees with app count\n%s", out)
	}
	assertSetupKept(t, f)
}

func TestSetupAllProjectsHistoryRemedyPreservesScope(t *testing.T) {
	for _, paused := range []bool{false, true} {
		cfg := config.Config{Harnesses: []string{"codex"}}
		must(t, config.SetCodexCaptureScope(&cfg, config.CodexAllProjects))
		var out bytes.Buffer
		printNextSteps(newPrompter(strings.NewReader(""), &out), cfg, paused)
		if paused {
			if !strings.Contains(out.String(), "agent-archive resume") || strings.Contains(out.String(), "backfill") {
				t.Fatalf("paused guidance: %s", &out)
			}
		} else {
			for _, want := range []string{"Setup's recent import only covers explicitly included projects.", "Review earlier Codex sessions in other projects with agent-archive backfill --since 7d.", "Codex hooks capture supported tasks in any non-excluded project."} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q\n%s", want, &out)
				}
			}
		}
		if len(cfg.Archive.Projects) != 0 || cfg.EffectiveCodexCaptureScope() != config.CodexAllProjects {
			t.Fatal("guidance changed consent")
		}
	}
}

// A proven native child is independently admitted, even without its parent.
// Setup must include it in both the total and per-app counts. A mixed cohort
// checks that the total does not use only RegistrationResult.Sessions.
func TestSetupImportReportsIndependentNativeChildren(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(strconv.FormatBool(mixed), func(t *testing.T) {
			canonical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
			home, userHome, project := canonical(), canonical(), canonical()
			at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
			const child = "11111111-1111-4111-8111-111111111111"
			const parent = "22222222-2222-4222-8222-222222222222"
			native := filepath.Join(userHome, ".codex", "sessions")
			must(t, os.MkdirAll(native, 0700))
			payload := map[string]any{"id": child, "session_id": parent, "parent_thread_id": parent, "cwd": project, "timestamp": at.Format(time.RFC3339Nano), "cli_version": "dev", "originator": "codex_cli_rs", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}, "subagent_history_start_ordinal": 1025}
			meta, err := json.Marshal(map[string]any{"type": "session_meta", "payload": payload})
			must(t, err)
			var raw strings.Builder
			raw.Write(meta)
			raw.WriteByte('\n')
			for range 1024 {
				raw.WriteString(`{"type":"turn_context","payload":{"model":"synthetic inherited","padding":"` + strings.Repeat("x", 512) + `"}}` + "\n")
			}
			raw.Write(fmt.Appendf(nil, `{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n", at.Format(time.RFC3339Nano), child, at.Format(time.RFC3339Nano)))
			raw.WriteString(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic child own prompt"}]}}` + "\n")
			must(t, os.WriteFile(filepath.Join(native, "rollout-"+child+".jsonl"), []byte(raw.String()), 0600))
			if mixed {
				meta, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": parent, "timestamp": at.Add(-time.Hour).Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}})
				must(t, err)
				body := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic parent own prompt"}]}}`+"\n", parent, at.Add(-time.Hour).Format(time.RFC3339Nano))
				must(t, os.WriteFile(filepath.Join(native, "rollout-"+parent+".jsonl"), append(append(meta, '\n'), []byte(body)...), 0600))
			}
			cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
			must(t, config.Save(home, cfg))
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Hour))
			release, err := local.NamedLock(home, "setup.lock")
			must(t, err)
			defer release()
			var out bytes.Buffer
			importRecentSessions(newPrompter(strings.NewReader(""), &out), &out, home, userHome, env)
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			must(t, err)
			want := 1
			if mixed {
				want = 2
			}
			if len(regs) != want {
				t.Fatalf("native admission fixture: %+v\n%s", regs, &out)
			}
			found := false
			for _, reg := range regs {
				if reg.NativeSessionID == child {
					found = reg.NativeChild && reg.CodexBinding != nil && reg.CodexBinding.OwnStart != nil && *reg.CodexBinding.OwnStart == 1025 && !reg.ImportBatch.IsZero()
				}
			}
			if !found {
				t.Fatal("child lost authoritative independent provenance")
			}
			if !strings.Contains(out.String(), fmt.Sprintf("(Codex %d)", want)) || !strings.Contains(out.String(), fmt.Sprintf("Imported %d session", want)) {
				t.Fatalf("native total/app reporting: %s", &out)
			}
		})
	}
}
