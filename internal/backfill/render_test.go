package backfill

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func TestRenderJSONKeepsCaptureColumnsAndRepresentedExtensions(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"codex", "injected-agent"} {
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			plan := Plan{Candidates: []Candidate{{Harness: harness, ProjectRoot: "/synthetic/project", ProjectExists: true}}}
			var out bytes.Buffer
			if err := RenderJSON(&out, plan); err != nil {
				t.Fatal(err)
			}
			var rendered planJSON
			if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
				t.Fatal(err)
			}
			if len(rendered.Projects) != 1 {
				t.Fatalf("projects: %+v", rendered.Projects)
			}
			sessions := rendered.Projects[0].Sessions
			if _, found := sessions["muse"]; found {
				t.Fatalf("runtime-only identity became a capture count: %v", sessions)
			}
			for _, capture := range []string{"claude", "codex", "cursor"} {
				if _, found := sessions[capture]; !found {
					t.Fatalf("established capture column missing: %v", sessions)
				}
			}
			if sessions[harness] != 1 {
				t.Fatalf("represented integration lost: %v", sessions)
			}
		})
	}
}

// Every skip reason renders with its wording and override, in the spec's
// order. Regenerate with `go test ./internal/backfill -run
// TestSkipReasonsGolden -update` and review the diff.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestSkipReasonsGolden(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	p := Plan{
		GeneratedAt: fixedNow, Home: "/Users/p", RetentionDays: 90, Harnesses: []string{"claude"},
		Destination: Destination{Provider: "s3", Bucket: "bucket", Prefix: "agent-archive"},
		Candidates: []Candidate{
			{Harness: "claude", ProjectRoot: "/Users/p/repo", ProjectKind: ProjectKindRepository, ProjectExists: true, StartedAt: start, Bytes: 2000,
				Subagents: []Subagent{{AgentID: "a1", Bytes: 1000}}, SubagentsSkipped: 1},
			{Harness: "claude", ProjectRoot: "/Users/p", ProjectKind: ProjectKindHome, ProjectIncluded: true, ProjectExists: true, StartedAt: start, Bytes: 500},
		},
		CursorSubagentsNotImported: 2, CursorDatabaseChecked: true, UnreadableFolders: 2, UnreadableStores: []string{"claude", "codex"},
	}
	for _, reason := range skipOrder {
		harness := "claude"
		if reason == SkipProjectUnknown {
			harness = "cursor"
		}
		p.Candidates = append(p.Candidates, Candidate{Harness: harness, Skip: reason})
	}
	var out bytes.Buffer
	RenderText(&out, p)
	out.WriteString("\n--- cursor database not checked, --include-home set ---\n")
	p.CursorSubagentsNotImported, p.CursorDatabaseChecked, p.CursorDatabaseUnchecked, p.Filters.IncludeHome, p.UnreadableFolders, p.UnreadableStores = 0, false, CursorUncheckedLocked, true, 1, nil
	p.Candidates = p.Candidates[:2]
	RenderText(&out, p)
	golden.Check(t, filepath.Join("testdata", "skip-reasons.txt"), out.Bytes())
	for _, reason := range skipOrder {
		if skipLabels[reason] == "" {
			t.Errorf("%s has no label", reason)
		}
	}
}

// These plans exercise the text-only decisions around empty results, an
// included project, and mixed repository/folder rows with missing hooks.
func TestRenderTextPresentationGolden(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 20, 23, 0, 0, 0, time.UTC)
	base := Plan{GeneratedAt: fixedNow, Home: "/Users/p", Destination: Destination{Provider: "s3", Bucket: "bucket"}, CursorDatabaseChecked: true}
	cases := []struct {
		name string
		plan Plan
	}{
		{"empty", base},
		{"included, retention off", func() Plan {
			p := base
			p.Harnesses = []string{"claude"}
			p.Candidates = []Candidate{{Harness: "claude", ProjectRoot: "/Users/p/repo", ProjectKind: ProjectKindRepository, ProjectIncluded: true, ProjectExists: true, StartedAt: start, Bytes: 1024}}
			return p
		}()},
		{"mixed, filtered, retention", func() Plan {
			p := base
			p.Filters.Since = "2026-09-20"
			p.Harnesses = []string{"claude"}
			p.RetentionDays = 30
			p.Candidates = []Candidate{
				{Harness: "claude", ProjectRoot: "/Users/p/日本語-project", ProjectKind: ProjectKindRepository, ProjectExists: true, StartedAt: start, Bytes: 2048, Subagents: []Subagent{{Bytes: 512}}},
				{Harness: "codex", ProjectRoot: "/Users/p/notes", ProjectKind: ProjectKindDirectory, ProjectExists: true, StartedAt: start.Add(48 * time.Hour), Bytes: 4096},
			}
			return p
		}()},
	}
	var out bytes.Buffer
	for i, tc := range cases {
		out.WriteString("--- " + tc.name + " ---\n")
		RenderText(&out, tc.plan)
		if i < len(cases)-1 {
			out.WriteByte('\n')
		}
	}
	golden.Check(t, filepath.Join("testdata", "render-presentation.txt"), out.Bytes())
}

func TestRenderTextPresentationDecisions(t *testing.T) {
	t.Parallel()
	projects := []ProjectSummary{{Included: true}, {Included: false}}
	if got := addedProjectMessage(projects, nil); got != "1 project is added, and new sessions in it are captured." {
		t.Errorf("singular project message: %q", got)
	}
	if got := addedProjectMessage(projects, []string{"cursor", "claude"}); !strings.Contains(got, "new Claude Code and Cursor sessions") {
		t.Errorf("hook order: %q", got)
	}
	if got := addedProjectMessage(projects[:1], []string{"claude"}); got != "" {
		t.Errorf("included-only project message: %q", got)
	}
	if got := missingSetupMessage([]string{"codex", "cursor"}); got != "New Codex and Cursor sessions need those apps added in setup." {
		t.Errorf("multiple missing hooks: %q", got)
	}
	if got := missingSetupMessage(nil); got != "" {
		t.Errorf("no missing hooks: %q", got)
	}
}
