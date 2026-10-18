package backfill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestRecoveryReviewKeepsExactGroupsPrivateAndReportsBound(t *testing.T) {
	tr := newTree(t)
	cwd := tr.path("home/.codex/worktrees/gone/repo")
	for i := range RecoveryReviewLimit + 1 {
		id := fmt.Sprintf("synthetic-%03d", i)
		original := cwd
		if i > 1 {
			original += fmt.Sprintf("-%03d", i)
		}
		tr.write(filepath.Join("home", ".claude", "projects", "synthetic", id+".jsonl"), fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"Synthetic private transcript"}}`+"\n", id, original, fixedNow.Add(-time.Hour).Format(time.RFC3339)))
	}
	env := tr.env()
	cfg := config.Config{Archive: archive.Config{Enabled: true}}
	ordinary := plan(t, env, nil, cfg, Filters{Harnesses: []string{"claude"}})
	if ordinary.RecoveryReview != nil {
		t.Fatal("ordinary plan retained private recovery observations")
	}
	env.ReviewProjects = true
	private := plan(t, env, nil, cfg, Filters{Harnesses: []string{"claude"}})
	if private.RecoveryReview == nil || len(private.RecoveryReview.Groups) != RecoveryReviewLimit || private.RecoveryReview.OmittedCandidates != 0 || private.RecoveryReview.Groups[0].Candidates != 2 {
		t.Fatalf("private groups %+v", private.RecoveryReview)
	}
	tr.write("home/.claude/projects/synthetic/extra.jsonl", fmt.Sprintf(`{"type":"user","sessionId":"extra","cwd":%q,"timestamp":%q,"message":{"role":"user","content":"Synthetic private transcript"}}`+"\n", cwd+"-extra", fixedNow.Add(-time.Hour).Format(time.RFC3339)))
	private = plan(t, env, nil, cfg, Filters{Harnesses: []string{"claude"}})
	if private.RecoveryReview.OmittedCandidates != 1 || len(private.RecoveryReview.Groups) != RecoveryReviewLimit {
		t.Fatalf("silent truncation %+v", private.RecoveryReview)
	}
	var out bytes.Buffer
	if err := RenderJSON(&out, private); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), cwd) || strings.Contains(out.String(), "Synthetic private transcript") || strings.Contains(out.String(), "RecoveryReview") {
		t.Fatalf("private evidence escaped ordinary JSON: %s", out.String())
	}
}

func TestMappingExportRoundTripsCanonicalAssignmentsAndBounds(t *testing.T) {
	const id = "11111111111111111111111111111111"
	input := Filters{ProjectMappings: map[string]string{"/old/z": "/configured"}, HistoricalMappings: map[string]string{"/old/a": id}, HistoricalProjects: []HistoricalProjectDefinition{{ID: id, Label: "Retired project"}}}
	encoded, err := EncodeMappingFile(input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Filters
	if err := ReadMappingFile(bytes.NewReader(encoded), &decoded); err != nil || input.mappingDigest() != decoded.mappingDigest() {
		t.Fatal("export changed semantic assignments", string(encoded), err)
	}
	if bytes.Index(encoded, []byte("/old/a")) > bytes.Index(encoded, []byte("/old/z")) {
		t.Fatal("export is not canonical")
	}
	input.HistoricalMappings["/old/z"] = id
	if _, err := EncodeMappingFile(input); err == nil {
		t.Fatal("conflicting destinations exported")
	}
	input = Filters{ProjectMappings: map[string]string{}}
	for i := range MappingCountLimit + 1 {
		input.ProjectMappings[fmt.Sprintf("/old/%d", i)] = "/configured"
	}
	if _, err := EncodeMappingFile(input); err == nil {
		t.Fatal("oversized assignment count exported")
	}
	input = Filters{ProjectMappings: map[string]string{}}
	for i := range 600 {
		input.ProjectMappings["/"+strings.Repeat("c", 4080)+fmt.Sprintf("%04d", i)] = "/" + strings.Repeat("t", 4095)
	}
	if _, err := EncodeMappingFile(input); err == nil {
		t.Fatal("oversized encoded file exported")
	}
}

func TestRecoveryReviewRetainsBoundedDeterministicGroupsAndChoices(t *testing.T) {
	tr := newTree(t)
	const groupCount = 260
	projects := make([]archive.ProjectActivation, 1300)
	for i := range projects {
		projects[i] = project(tr.path(fmt.Sprintf("home/a-target-%04d", i)), true)
	}
	// The late, unlisted source exclusion must still apply to every group.
	private := tr.path("home/zz-private")
	projects = append(projects, project(private, false))
	env := tr.env()
	r := newResolver(env, config.Config{Archive: archive.Config{Projects: projects}}, Filters{})
	r.mappingRecovery = r.recovery
	var items []*work
	for range 2 {
		for i := range groupCount {
			items = append(items, &work{t: &transcript{cwd: filepath.Join(private, fmt.Sprintf("%04d", i)), repoKey: archive.RepoKey("https://example.test/acme/repo")}, c: Candidate{Skip: SkipExcludedProject}, res: resolution{skip: SkipExcludedProject}})
		}
	}
	first := reviewProjects(t.Context(), r, items)
	slices.Reverse(items)
	second := reviewProjects(t.Context(), r, items)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("source reordering changed retained review or repeated counts")
	}
	if len(first.Groups) != RecoveryReviewLimit || first.OmittedCandidates != 2*(groupCount-RecoveryReviewLimit) || len(first.Witnesses) != RecoveryReviewLimit || first.OmittedTargets != len(projects)-RecoveryReviewLimit || first.OmittedWitnesses != len(projects)-RecoveryReviewLimit {
		t.Fatal("projection exceeded retained window or hid omissions", first)
	}
	for i, g := range first.Groups {
		if g.Candidates != 2 || g.OriginalCwd != filepath.Join(private, fmt.Sprintf("%04d", i)) || len(g.Targets) != RecoveryReviewLimit || g.SourceRestriction != SkipExcludedProject {
			t.Fatal("bounded grouping lost repeated counts, lexical selection or full policy", g)
		}
	}
}

func TestRecoveryReviewPreservesRawCwdWithoutQualifyingCleanedTraversal(t *testing.T) {
	tr := newTree(t)
	private := tr.path("home/private")
	if err := os.MkdirAll(filepath.Join(private, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := tr.path("home/alias")
	if err := os.Symlink(filepath.Join(private, "child"), alias); err != nil {
		t.Fatal(err)
	}
	raw := alias + "/../vanished"
	clean := filepath.Clean(raw)
	actualParent, err := filepath.EvalSymlinks(alias + "/..")
	if err != nil || actualParent != private {
		t.Fatal("synthetic traversal did not reach excluded parent", actualParent, err)
	}
	r := newResolver(tr.env(), config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(private, false)}}}, Filters{})
	items := []*work{{t: &transcript{cwd: raw}, c: Candidate{Skip: SkipWorktreeUnresolved}}, {t: &transcript{cwd: clean}, c: Candidate{Skip: SkipWorktreeUnresolved}}}
	first := reviewProjects(t.Context(), r, items)
	slices.Reverse(items)
	second := reviewProjects(t.Context(), r, items)
	if !reflect.DeepEqual(first, second) || len(first.Groups) != 1 {
		t.Fatal("raw grouping was not deterministic", first, second)
	}
	g := first.Groups[0]
	if g.OriginalCwd != raw || g.AssignmentCwd != clean || g.Candidates != 2 || !g.MixedOriginalSpellings || !g.ParentTraversalOriginal || g.SourceRestriction != SkipWorktreeUnresolved {
		t.Fatal("cleaned spelling was treated as raw traversal evidence", g)
	}
}

func TestRecoveryReviewStopsSourceObservationOnCancellation(t *testing.T) {
	tr := newTree(t)
	r := newResolver(tr.env(), config.Config{}, Filters{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observations := 0
	r.env.Stat = func(string) (os.FileInfo, error) {
		observations++
		cancel()
		return nil, os.ErrNotExist
	}
	items := []*work{{t: &transcript{cwd: tr.path("home/first")}}, {t: &transcript{cwd: tr.path("home/second")}}}
	review := reviewProjects(ctx, r, items)
	if observations != 1 || len(review.Groups) != 0 || len(review.Witnesses) != 0 {
		t.Fatal("cancelled review continued observation or returned partial groups", observations, review)
	}
}

func TestRecoveryReviewSharesOneBoundAcrossFileAndDatabaseWork(t *testing.T) {
	tr := newTree(t)
	r := newResolver(tr.env(), config.Config{}, Filters{})
	var files, database []*work
	for i := range RecoveryReviewLimit + 2 {
		cwd := tr.path(fmt.Sprintf("home/gone/%03d", i))
		files = append(files, &work{t: &transcript{cwd: cwd}, c: Candidate{Skip: SkipWorktreeUnresolved}, res: resolution{skip: SkipWorktreeUnresolved}})
		database = append(database, &work{t: &transcript{cwd: cwd}, c: Candidate{SourceKind: archive.SourceKindCursorSQLite, Skip: SkipWorktreeUnresolved}, res: resolution{skip: SkipWorktreeUnresolved}})
	}
	first := reviewProjects(t.Context(), r, files, database)
	second := reviewProjects(t.Context(), r, database, files)
	if !reflect.DeepEqual(first, second) || len(first.Groups) != RecoveryReviewLimit || first.OmittedCandidates != 4 {
		t.Fatal("candidate batches acquired separate review windows or changed ordering", first, second)
	}
	for _, group := range first.Groups {
		if group.Candidates != 2 || group.DatabaseOnlyCandidates != 1 {
			t.Fatal("shared file/database cwd lost a candidate or widened the group", group)
		}
	}
}

func TestRecoveryReviewDatabaseCleanupFailureDoesNotReturnProjection(t *testing.T) {
	tr := newTree(t)
	env := tr.env()
	env.ReviewProjects = true
	failed := errors.New("synthetic database snapshot cleanup failure")
	closed := 0
	env.CursorDatabase = func(ctx context.Context) (CursorDatabaseResult, error) {
		res, err := fakeCursorDatabase(nil, nil, nil, nil)(ctx)
		res.Close = func() error { closed++; return failed }
		return res, err
	}
	plan, err := BuildPlan(t.Context(), env, states{}, config.Config{}, Filters{Harnesses: []string{"cursor"}})
	if !errors.Is(err, failed) || closed != 1 || plan.RecoveryReview != nil {
		t.Fatal("private database projection concealed snapshot cleanup failure", err, closed, plan.RecoveryReview)
	}
}
