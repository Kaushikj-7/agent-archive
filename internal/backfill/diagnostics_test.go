package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestRecoveryOutcomesSurvivePlanningAndPrecedence(t *testing.T) {
	outcomes := []sourcefacts.RecoveryOutcome{sourcefacts.RecoveryBudgetExhausted, sourcefacts.RecoveryInventoryUnavailable, sourcefacts.RecoveryExcluded, sourcefacts.RecoveryAmbiguous, sourcefacts.RecoveryRepositoryUnavailable, sourcefacts.RecoveryMappingConflict, sourcefacts.RecoverySubtreeUnavailable, sourcefacts.RecoveryUnavailable}
	for _, outcome := range outcomes {
		t.Run(string(outcome), func(t *testing.T) {
			w := &work{t: &transcript{}, c: Candidate{StartedAt: fixedNow.Add(-time.Hour)}, res: resolution{skip: SkipWorktreeUnresolved, outcome: outcome}}
			decidePlanCandidates([]*work{w}, time.Time{}, time.Time{}, fixedNow)
			if w.c.Skip != SkipWorktreeUnresolved || w.c.Diagnostic == nil || w.c.Diagnostic.Detail != DiagnosticDetail(outcome) {
				t.Fatalf("lost outcome: %+v", w.c)
			}
			w.state = SkipAlreadyArchived
			decidePlanCandidates([]*work{w}, time.Time{}, time.Time{}, fixedNow)
			if w.c.Skip != SkipAlreadyArchived || w.c.Diagnostic != nil {
				t.Fatalf("diagnostic overrode precedence: %+v", w.c)
			}
		})
	}
}

func TestDeletedWorktreeResolverRetainsUnavailableAndAmbiguousEvidence(t *testing.T) {
	tr := newTree(t)
	a, b := tr.repo("home/a"), tr.repo("home/b")
	gone := tr.path("home/.codex/worktrees/gone/repo")
	key := archive.RepoKey("https://example.test/acme/repo")
	for _, known := range []bool{false, true} {
		env := tr.env()
		env.RepositoryIdentity = func(_ context.Context, root string) sourcefacts.RepositoryIdentity {
			return sourcefacts.RepositoryIdentity{Root: root, Key: key, Known: known}
		}
		cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(a, true), project(b, true)}}}
		r := newResolver(env, cfg, Filters{})
		want := sourcefacts.RecoveryInventoryUnavailable
		if known {
			want = sourcefacts.RecoveryAmbiguous
		}
		for range 2 {
			got := r.resolveEvidence(t.Context(), gone, key)
			if got.skip != SkipWorktreeUnresolved || got.outcome != want {
				t.Fatalf("got %+v, want %s", got, want)
			}
		}
	}
}

// untrustedDetail stands in for source text that must never be rendered.
const untrustedDetail DiagnosticDetail = "untrusted transcript content"

func TestDiagnosticsAreBoundedContentFreeAndJSONOptional(t *testing.T) {
	p := Plan{Filters: Filters{Harnesses: []string{"codex"}}}
	for range 5000 {
		p.Candidates = append(p.Candidates, Candidate{Harness: "codex", NativeSessionID: "private-native-id", TranscriptPath: "/private/native/locator.jsonl", SourceKey: "credential-secret", Skip: SkipWorktreeUnresolved, Diagnostic: candidateDiagnostic(SkipWorktreeUnresolved, sourcefacts.RecoveryAmbiguous, noGapCause)})
	}
	p.Candidates = append(p.Candidates, Candidate{Skip: SkipWorktreeUnresolved, Diagnostic: &Diagnostic{Detail: untrustedDetail}})
	var text, structured bytes.Buffer
	RenderText(&text, p)
	if err := RenderJSON(&structured, p); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{text.String(), structured.String()} {
		for _, secret := range []string{"private-native-id", "locator.jsonl", "credential-secret", "untrusted transcript content"} {
			if strings.Contains(output, secret) {
				t.Fatalf("leaked %s", secret)
			}
		}
		if len(output) > 4000 {
			t.Fatalf("unbounded summary: %d", len(output))
		}
	}
	if !strings.Contains(text.String(), "multiple configured repositories") {
		t.Fatal(text.String())
	}
	var got struct {
		Diagnostics []DiagnosticSummary  `json:"diagnostics"`
		Inventory   *InventoryAccounting `json:"inventory"`
	}
	if err := json.Unmarshal(structured.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Count != 5000 || got.Inventory == nil || !got.Inventory.LogicalHistoryPending {
		t.Fatalf("%+v", got)
	}
	structured.Reset()
	if err := RenderJSON(&structured, Plan{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(structured.String(), `"diagnostics"`) || strings.Contains(structured.String(), `"inventory"`) {
		t.Fatal("ordinary compatibility fields changed")
	}
}

func TestInventoryCountsPhysicalCandidatesWithoutOverlappingRoles(t *testing.T) {
	p := Plan{Candidates: []Candidate{
		{TranscriptPath: "/synthetic/a"}, {TranscriptPath: "/synthetic/a"},
		{TranscriptPath: "/synthetic/b", Skip: SkipDuplicateSession},
		{TranscriptPath: "/synthetic/c", Skip: SkipRelatedHistory},
		{TranscriptPath: "/synthetic/d", Skip: SkipExcludedProject},
		{TranscriptPath: "/synthetic/e", Skip: SkipAlreadyArchived}, {},
	}}
	a := p.InventoryAccounting()
	total := 0
	for _, n := range a.Dispositions {
		total += n
	}
	if total != 5 || a.UniqueCandidateFiles != 5 || a.DatabaseCandidates != 1 || a.Dispositions["duplicate_candidate"] != 1 || a.Dispositions["unresolved"] != 1 || !a.LogicalHistoryPending {
		t.Fatalf("%+v", a)
	}
	if d := candidateDiagnostic(SkipRelatedHistory, "", noGapCause); d.Detail != DetailHistoryLookupPending || d.Action != ActionAwaitSupport {
		t.Fatal(d)
	}
}

// Database-only candidates use the same subordinate diagnostics as file candidates.
func TestDatabaseCandidateDiagnosticsFollowWinningSkip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		skip            SkipReason
		unsafe          bool
		tooLarge        bool
		missingWorktree bool
	}{
		{skip: SkipUnsafeFormat, unsafe: true},
		{skip: SkipTooLarge, tooLarge: true},
		{skip: SkipWorktreeUnresolved, missingWorktree: true},
	} {
		t.Run(string(tc.skip), func(t *testing.T) {
			t.Parallel()
			tr := newTree(t)
			env := tr.env()
			r := newResolver(env, config.Config{}, Filters{})
			folder := tr.repo("home/repo")
			if tc.missingWorktree {
				folder = tr.path("home/.cursor/worktrees/gone")
			}
			w := &work{t: &transcript{harness: harnessCursor}, c: Candidate{StartedAt: fixedNow.Add(-time.Hour)}, chat: CursorDatabaseChat{Folder: folder}, unsafe: tc.unsafe, tooLarge: tc.tooLarge}

			p := Plan{GeneratedAt: fixedNow, Filters: Filters{Harnesses: []string{"cursor"}}, CursorDatabaseChecked: true}
			appendCursorDatabaseChats(env, r, nil, []*work{w}, &p)
			if len(p.Candidates) != 1 || p.Candidates[0].Skip != tc.skip || p.Candidates[0].Diagnostic == nil || len(p.Diagnostics()) != 1 {
				t.Fatalf("missing database diagnostic: %+v", p.Candidates)
			}
			var output bytes.Buffer
			if err := RenderJSON(&output, p); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), `"database_candidates": 1`) || !strings.Contains(output.String(), `"unique_candidate_files": 0`) {
				t.Fatal(output.String())
			}
			w.state = SkipAlreadyArchived
			p.Candidates = nil
			appendCursorDatabaseChats(env, r, nil, []*work{w}, &p)
			if p.Candidates[0].Skip != SkipAlreadyArchived || p.Candidates[0].Diagnostic != nil {
				t.Fatalf("precedence changed: %+v", p.Candidates)
			}
		})
	}
}

// Recorded exclusion evidence must be reflected without changing the primary skip.
func TestInventoryRetainsRecoveryExclusion(t *testing.T) {
	t.Parallel()
	c := Candidate{TranscriptPath: "/synthetic/excluded", Skip: SkipWorktreeUnresolved, Diagnostic: candidateDiagnostic(SkipWorktreeUnresolved, sourcefacts.RecoveryExcluded, noGapCause)}
	p := Plan{Candidates: []Candidate{c}}
	a := p.InventoryAccounting()
	if a.UniqueCandidateFiles != 1 || a.Dispositions["excluded"] != 1 || a.Dispositions["unresolved"] != 0 || p.Candidates[0].Skip != SkipWorktreeUnresolved {
		t.Fatalf("%+v", a)
	}
}

// Observing a selected file more than once does not make that file a discarded copy.
func TestInventoryRepeatedPathUsesSelectedDisposition(t *testing.T) {
	t.Parallel()
	for _, selected := range []SkipReason{"", SkipAlreadyArchived, SkipExcludedProject, SkipRelatedHistory} {
		for _, reverse := range []bool{false, true} {
			rows := []Candidate{{TranscriptPath: "/synthetic/repeated", Skip: SkipDuplicateSession}, {TranscriptPath: "/synthetic/repeated", Skip: selected}}
			if reverse {
				rows[0], rows[1] = rows[1], rows[0]
			}
			a := (Plan{Candidates: rows}).InventoryAccounting()
			want := (Plan{Candidates: []Candidate{rows[0]}}).InventoryAccounting()
			if !reverse {
				want = (Plan{Candidates: []Candidate{rows[1]}}).InventoryAccounting()
			}
			if a.UniqueCandidateFiles != 1 || a.Dispositions["duplicate_candidate"] != 0 || len(a.Dispositions) != 1 {
				t.Fatalf("selected %q reverse %v: %+v", selected, reverse, a)
			}
			for key, count := range want.Dispositions {
				if a.Dispositions[key] != count {
					t.Fatalf("got %+v want %+v", a, want)
				}
			}
		}
	}
}

// An unavailable inspector is not evidence that adding format support is required.
func TestUnsafeDiagnosticDoesNotInventUnsupportedFormat(t *testing.T) {
	t.Parallel()
	w := &work{t: &transcript{}, c: Candidate{StartedAt: fixedNow.Add(-time.Hour)}}
	runAdapter(t.Context(), Environment{}, w)
	decidePlanCandidates([]*work{w}, time.Time{}, time.Time{}, fixedNow)
	if w.c.Skip != SkipUnsafeFormat || w.c.Diagnostic == nil || w.c.Diagnostic.Detail != DetailSourceInspectionUnavailable || w.c.Diagnostic.Action != ActionReviewSource {
		t.Fatalf("%+v", w.c)
	}
	var output bytes.Buffer
	RenderText(&output, Plan{Candidates: []Candidate{w.c}, Filters: Filters{Harnesses: []string{"codex"}}})
	if strings.Contains(output.String(), "support is required") || !strings.Contains(output.String(), "check source access and format support") {
		t.Fatal(output.String())
	}
}
