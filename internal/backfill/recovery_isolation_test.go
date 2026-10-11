package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// recoveredWithout reports Codex sessions recovered without Cursor's evidence.
func recoveredWithout(p Plan) bool {
	for _, g := range p.RecoveryEvidenceGaps {
		if g.App == string(harnessCursor) && g.RecoveredWithout[string(harnessCodex)] > 0 {
			return true
		}
	}
	return false
}

// resolveEvidence resolves a session of unknown app: every gap applies.
func (r *resolver) resolveEvidence(ctx context.Context, cwd, key string) resolution {
	return r.resolveSessionEvidence(ctx, "", cwd, key)
}

type appendDuringDiscovery string

const (
	appendSameRoot  appendDuringDiscovery = "append"
	appendRewrite   appendDuringDiscovery = "rewrite"
	appendCloneRoot appendDuringDiscovery = "append_clone"
)

type sourceChange string

const (
	sourceAppended  sourceChange = "append"
	sourceTruncated sourceChange = "truncate"
	sourceReplaced  sourceChange = "replace"
)

func hasRecoveryGap(p Plan, app string, cause DiagnosticDetail) bool {
	for _, g := range p.RecoveryEvidenceGaps {
		if g.App == app && g.Cause == cause {
			return true
		}
	}
	return false
}

// unreadableCursorStore makes Cursor's transcript store unlistable.
func unreadableCursorStore(t *testing.T, tr *tree) {
	t.Helper()
	dir := tr.mkdir("home/.cursor/projects")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// The owner's case: Cursor's store is unavailable and Codex sessions in
// deleted worktrees must still recover, with --harness codex or without it.
func TestRecoveryCursorStoreGapDoesNotBlockCodex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can list an unreadable folder")
	}
	for _, filters := range []Filters{{Harnesses: []string{"codex"}}, {}} {
		t.Run(strings.Join(filters.Harnesses, ","), func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			unreadableCursorStore(t, tr)
			p := plan(t, env, nil, cfg, filters)
			c := candidate(t, p, goneID)
			if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil || c.ProjectResolution.Method != "recorded_repository" {
				t.Fatalf("Cursor store gap blocked Codex recovery: %+v %+v", c, c.Diagnostic)
			}
			if err := p.CheckRecovery(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !recoveredWithout(p) || !hasRecoveryGap(p, "cursor", CauseNativeStoreUnreadable) {
				t.Fatalf("gap not reported: %+v", p.RecoveryEvidenceGaps)
			}
			var text bytes.Buffer
			RenderText(&text, p)
			if !strings.Contains(text.String(), "recovered without Cursor's evidence (native_store_unreadable)") {
				t.Fatalf("text does not name the gap:\n%s", text.String())
			}
			var structured bytes.Buffer
			if err := RenderJSON(&structured, p); err != nil {
				t.Fatal(err)
			}
			var got struct {
				Gaps []RecoveryEvidenceGap `json:"recovery_evidence_gaps"`
			}
			if err := json.Unmarshal(structured.Bytes(), &got); err != nil || len(got.Gaps) == 0 || got.Gaps[0].App != "cursor" {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

// A gap in Codex's own evidence still blocks Codex recoveries, and the plan
// says which app and cause blocked them.
func TestRecoveryOwnAppGapBlocksAndIsNamed(t *testing.T) {
	tr, env, cfg, _, goneID := firstRunRecoveryFixture(t, false)
	unknown := "00000000-0000-0000-0000-000000000033"
	tr.write(filepath.Join("home", codexFile(unknown)), codexTranscript(unknown, unknown, "", fixedNow))
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	c := candidate(t, p, goneID)
	if c.ProjectResolution != nil || c.Skip != SkipWorktreeUnresolved || c.Diagnostic == nil || c.Diagnostic.Detail != CauseSessionFolderUnknown {
		t.Fatalf("own gap did not block: %+v %+v", c, c.Diagnostic)
	}
	if len(p.RecoveryEvidenceGaps) != 1 || p.RecoveryEvidenceGaps[0].App != "codex" || p.RecoveryEvidenceGaps[0].Blocked["codex"] != 1 {
		t.Fatalf("%+v", p.RecoveryEvidenceGaps)
	}
	var text bytes.Buffer
	RenderText(&text, p)
	if !strings.Contains(text.String(), "Codex session not recovered: Codex's evidence is incomplete (session_folder_unknown)") {
		t.Fatalf("text does not name the blocking gap:\n%s", text.String())
	}
}

// Gaps attribute per app; unattributed gaps and budget keep their outcomes.
func TestRecoveryGapsBlockOnlyAffectedApps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unread  unreadable
		db      recoveryGaps
		changed bool
		codex   sourcefacts.RecoveryOutcome
		cursor  sourcefacts.RecoveryOutcome
	}{
		{"claude store", unreadable{stores: map[string]bool{"claude": true}}, nil, false, "", ""},
		{"cursor database", unreadable{}, recoveryGaps{{Agent: "cursor", Cause: CauseCursorDatabaseUnavailable}: true}, false, "", sourcefacts.RecoveryInventoryUnavailable},
		{"cursor budget", unreadable{}, recoveryGaps{{Agent: "cursor", Cause: CauseRecoveryBudget}: true}, false, "", sourcefacts.RecoveryBudgetExhausted},
		{"codex folders", unreadable{folders: 1, folderAgents: map[string]bool{"codex": true}}, nil, false, sourcefacts.RecoveryInventoryUnavailable, ""},
		{"unattributed folders", unreadable{folders: 1}, nil, false, sourcefacts.RecoveryInventoryUnavailable, sourcefacts.RecoveryInventoryUnavailable},
		{"inventory changed", unreadable{}, nil, true, sourcefacts.RecoveryInventoryUnavailable, sourcefacts.RecoveryInventoryUnavailable},
		{"root limit", unreadable{}, recoveryGaps{{Cause: CauseWitnessLimit}: true}, false, sourcefacts.RecoveryInventoryUnavailable, sourcefacts.RecoveryInventoryUnavailable},
		{"unattributed budget", unreadable{}, recoveryGaps{{Cause: CauseRecoveryBudget}: true}, false, sourcefacts.RecoveryBudgetExhausted, sourcefacts.RecoveryBudgetExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
			gone := tr.path("home/.codex/worktrees/deleted/repo")
			key := archive.RepoKey("https://example.test/acme/repo")
			cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
			r := newResolver(env, cfg, Filters{})
			r.inventoryAdded = func(context.Context) (addedSources, bool) { return addedSources{}, !tc.changed }
			prepareRecoveryInventory(t.Context(), r, nil, tc.unread, tc.db)
			for h, want := range map[harness]sourcefacts.RecoveryOutcome{harnessCodex: tc.codex, harnessCursor: tc.cursor} {
				res := r.resolveSessionEvidence(t.Context(), h, gone, key)
				if res.outcome != want {
					t.Fatalf("%s: got %q, want %q", h, res.outcome, want)
				}
				if want == "" && (res.proof == nil || res.proof.Root != root || res.gap != nil) {
					t.Fatalf("%s: %+v", h, res)
				}
				if want != "" && res.gap == nil {
					t.Fatalf("%s: blocking gap not recorded", h)
				}
			}
		})
	}
}

// A clone that another app observed keeps recovery ambiguous: as a complete
// witness, and as evidence from an incomplete Cursor database.
func TestRecoveryCompetingCloneInOtherAppStillBlocks(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "incomplete_database"}[degraded], func(t *testing.T) {
			tr, env, cfg, _, goneID := firstRunRecoveryFixture(t, false)
			clone := tr.repo("home/clone")
			chats := []CursorDatabaseChat{{ID: "clone", Folder: clone, CreatedAt: fixedNow.Add(-48 * time.Hour)}}
			composers := map[string]cursorstore.Composer{"clone": syntheticChat("clone", nil, "hi")}
			if degraded {
				chats = append(chats, CursorDatabaseChat{ID: "unknown", WorkspaceID: "missing", CreatedAt: fixedNow.Add(-48 * time.Hour)})
				composers["unknown"] = syntheticChat("unknown", nil, "hi")
			}
			env.CursorDatabase = fakeCursorDatabase(chats, composers, nil, nil)
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			for _, filters := range []Filters{{Harnesses: []string{"codex"}}, {}} {
				p := plan(t, env, nil, cfg, filters)
				c := candidate(t, p, goneID)
				if c.ProjectResolution != nil || c.Skip != SkipWorktreeUnresolved || c.Diagnostic == nil || c.Diagnostic.Detail != DetailRecoveryAmbiguous {
					t.Fatalf("Cursor-observed clone did not keep recovery ambiguous: %+v %+v", c, c.Diagnostic)
				}
			}
		})
	}
}

// A root named only by an incomplete Cursor database cannot be renewed, so it
// cannot become a proposed destination.
func TestRecoveryIncompleteDatabaseCannotProposeDestination(t *testing.T) {
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	removeFirstRunFileWitness(t, tr)
	chats := []CursorDatabaseChat{
		{ID: "live", Folder: root, CreatedAt: fixedNow.Add(-48 * time.Hour)},
		{ID: "unknown", WorkspaceID: "missing", CreatedAt: fixedNow.Add(-48 * time.Hour)},
	}
	composers := map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi"), "unknown": syntheticChat("unknown", nil, "hi")}
	env.CursorDatabase = fakeCursorDatabase(chats, composers, nil, nil)
	env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if c := candidate(t, p, goneID); c.ProjectResolution != nil || c.Skip == "" {
		t.Fatalf("incomplete database proposed a destination: %+v", c)
	}
	if len(p.Imported()) != 0 {
		t.Fatal(p.Imported())
	}
}

// An append to a session file during discovery (after its header was read)
// keeps that session source_changed but no longer hides every recovery; a
// rewrite still does. An appended clone stays clone evidence.
func TestRecoveryAppendDuringDiscoveryKeepsOtherRecoveries(t *testing.T) {
	for _, kind := range []appendDuringDiscovery{appendSameRoot, appendRewrite, appendCloneRoot} {
		t.Run(string(kind), func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			id := "00000000-0000-0000-0000-000000000022"
			cwd := root
			if kind == appendCloneRoot {
				cwd = tr.repo("home/clone")
			}
			rel := filepath.Join("home", codexFile(id))
			source := tr.write(rel, codexTranscript(id, id, cwd, fixedNow.Add(-48*time.Hour)))
			changed := false
			mutate := func() {
				if changed {
					return
				}
				changed = true
				if kind == appendRewrite {
					// A truncating rewrite: the file is not merely grown.
					tr.write(rel, strings.SplitN(codexTranscript(id, id, cwd, fixedNow.Add(-47*time.Hour)), "\n", 2)[0]+"\n")
					return
				}
				f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				line := `{"type":"response_item","timestamp":"2026-09-20T10:05:00Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"more"}]}}` + "\n"
				_, err = f.WriteString(line)
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			env.Open = func(path string) (io.ReadCloser, error) {
				f, err := os.Open(path)
				if err != nil || path != source {
					return f, err
				}
				return &mutationReader{ReadCloser: f, afterClose: mutate}, nil
			}
			p := plan(t, env, nil, cfg, Filters{})
			if !changed {
				t.Fatal("mutation hook was not exercised")
			}
			if c := candidate(t, p, id); c.Skip != SkipSourceChanged || c.ProjectRoot != "" {
				t.Fatalf("changed session: %+v", c)
			}
			// An append is neither a witness gap of its own app nor, since the
			// native inventory lets owned transcripts grow (PR #394), a change
			// to the unattributed inventory.
			if kind != appendRewrite && (hasRecoveryGap(p, "codex", CauseNativeInventoryChanged) || hasRecoveryGap(p, "", CauseNativeInventoryChanged)) {
				t.Fatalf("append recorded as a witness gap: %+v", p.RecoveryEvidenceGaps)
			}
			c := candidate(t, p, goneID)
			switch kind {
			case appendSameRoot:
				if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
					t.Fatalf("append hid recovery: %+v %+v", c, c.Diagnostic)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
				mutate2 := func() {
					f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.WriteString("{}\n")
					if closeErr := f.Close(); err == nil {
						err = closeErr
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				mutate2()
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal("further append invalidated recovery:", err)
				}
			case appendRewrite:
				if c.ProjectResolution != nil || c.Diagnostic == nil || c.Diagnostic.Detail != CauseNativeInventoryChanged {
					t.Fatalf("rewrite certified uniqueness: %+v %+v", c, c.Diagnostic)
				}
			case appendCloneRoot:
				if c.ProjectResolution != nil || c.Diagnostic == nil || c.Diagnostic.Detail != DetailRecoveryAmbiguous {
					t.Fatalf("appended clone lost: %+v %+v", c, c.Diagnostic)
				}
			}
		})
	}
}

func TestAppendedSourceRequiresSameGrownFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) os.FileInfo {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	before := write("a", "x")
	grown := write("a", "xy")
	same := write("a", "x")
	other := write("b", "xyz")
	if !appendedSource(before, grown, nil) {
		t.Fatal("grown same file is an append")
	}
	if appendedSource(before, same, nil) || appendedSource(before, other, nil) || appendedSource(before, grown, os.ErrPermission) || appendedSource(nil, grown, nil) {
		t.Fatal("non-append accepted")
	}
	if err := os.Chmod(filepath.Join(dir, "a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	moded, err := os.Lstat(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if appendedSource(before, moded, nil) {
		t.Fatal("mode change accepted as append")
	}
}

// An appended file is clone evidence that cannot propose a destination, and
// it renews only while it keeps growing in place.
func TestAppendedWitnessIsNonEligibleAndRenewsOnlyAppends(t *testing.T) {
	tr := newTree(t)
	root := tr.repo("home/repo")
	path := tr.write("home/source.jsonl", "a\n")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	env := tr.env()
	w := &work{t: &transcript{harness: harnessCodex, path: path, sourceInfo: info, cwd: root}, res: resolution{root: root, kind: ProjectKindRepository}, sourceChanged: true}
	projects, witnesses, gaps := recoveryWitnessInventory(newResolver(env, config.Config{}, Filters{}), []*work{w})
	if len(gaps) != 0 || len(witnesses[root]) != 1 || len(projects) != 1 || projects[0].Included {
		t.Fatalf("appended witness: %+v %+v %+v", projects, witnesses, gaps)
	}
	if w.stillAppendedOnly(env) {
		t.Fatal("unchanged file renewed as appended")
	}
	tr.write("home/source.jsonl", "a\nb\n")
	if !w.stillAppendedOnly(env) {
		t.Fatal("grown file did not renew")
	}
	tr.write("home/source.jsonl", "")
	if w.stillAppendedOnly(env) {
		t.Fatal("truncated file renewed")
	}
	w.sourceRewritten = true
	tr.write("home/source.jsonl", "a\nb\nc\n")
	if w.stillAppendedOnly(env) {
		t.Fatal("rewritten source renewed")
	}
}

// An incomplete database's chat cannot make a root proposable even when a
// header-only file witness keeps the root eligible in the inventory.
func TestRecoveryIncompleteDatabaseCannotCompleteEligibility(t *testing.T) {
	tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	id := "00000000-0000-0000-0000-000000000011"
	body := codexTranscript(id, id, root, fixedNow.Add(-48*time.Hour))
	tr.write(filepath.Join("home", codexFile(id)), strings.SplitN(body, "\n", 2)[0]+"\n")
	chats := []CursorDatabaseChat{
		{ID: "live", Folder: root, CreatedAt: fixedNow.Add(-48 * time.Hour)},
		{ID: "unknown", WorkspaceID: "missing", CreatedAt: fixedNow.Add(-48 * time.Hour)},
	}
	composers := map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi"), "unknown": syntheticChat("unknown", nil, "hi")}
	env.CursorDatabase = fakeCursorDatabase(chats, composers, nil, nil)
	env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
	p := plan(t, env, states{id: SkipAlreadyArchived}, cfg, Filters{Harnesses: []string{"codex"}})
	if c := candidate(t, p, goneID); c.ProjectResolution != nil || c.Skip == "" {
		t.Fatalf("incomplete database completed eligibility: %+v", c)
	}
}

// checkSource tells a pure append from a truncation or replacement.
func TestCheckSourceSeparatesAppendsFromRewrites(t *testing.T) {
	for _, kind := range []sourceChange{sourceAppended, sourceTruncated, sourceReplaced} {
		t.Run(string(kind), func(t *testing.T) {
			tr := newTree(t)
			path := tr.write("home/source.jsonl", "a\nb\n")
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			w := &work{t: &transcript{path: path, sourceInfo: info}}
			switch kind {
			case sourceAppended:
				tr.write("home/source.jsonl", "a\nb\nc\n")
			case sourceTruncated:
				tr.write("home/source.jsonl", "a\n")
			case sourceReplaced:
				tr.write("home/other.jsonl", "a\nb\nc\n")
				if err := os.Rename(tr.path("home/other.jsonl"), path); err != nil {
					t.Fatal(err)
				}
			}
			w.checkSource(tr.env())
			if !w.sourceChanged || w.appendedOnly() != (kind == sourceAppended) {
				t.Fatalf("changed=%v appended=%v", w.sourceChanged, w.appendedOnly())
			}
			if kind == sourceAppended {
				tr.write("home/source.jsonl", "x\n")
				w.checkSource(tr.env())
				if w.appendedOnly() {
					t.Fatal("a later truncation kept the append-only mark")
				}
			}
		})
	}
}

// Once a check observed a rewrite, regrowing past the header observation does
// not make the source an append again.
func TestCheckSourceRewriteStaysRewrite(t *testing.T) {
	tr := newTree(t)
	path := tr.write("home/source.jsonl", "a\nb\n")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &work{t: &transcript{path: path, sourceInfo: info}}
	tr.write("home/source.jsonl", "x\n")
	w.checkSource(tr.env())
	tr.write("home/source.jsonl", "x\ny\nz\n")
	w.checkSource(tr.env())
	if w.appendedOnly() {
		t.Fatal("regrown rewrite classified as an append")
	}
}

// An appended witness that is a root's only evidence renews at admission while
// it keeps growing in place, and not after a truncation.
func TestAppendedWitnessRenewsAtAdmission(t *testing.T) {
	tr := newTree(t)
	root := tr.repo("home/other")
	path := tr.write("home/source.jsonl", "a\n")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	env := tr.env()
	r := newResolver(env, config.Config{}, Filters{})
	w := &work{t: &transcript{harness: harnessCodex, path: path, sourceInfo: info, cwd: root}, res: r.resolve(root)}
	tr.write("home/source.jsonl", "a\nb\n")
	w.checkSource(env)
	if !w.appendedOnly() {
		t.Fatal("append not observed")
	}
	witnesses := map[string][]*work{w.res.root: {w}}
	tr.write("home/source.jsonl", "a\nb\nc\n")
	if current, _, _ := recoveryWitnessesCurrent(t.Context(), r, witnesses, map[string]bool{}, true); !current {
		t.Fatal("further append invalidated the appended witness")
	}
	tr.write("home/source.jsonl", "a\n")
	if current, _, _ := recoveryWitnessesCurrent(t.Context(), r, witnesses, map[string]bool{}, true); current {
		t.Fatal("truncated witness renewed")
	}
}

// A blocking gap fails closed even before the inventory prepared its resolvers.
func TestRecoveryForFailsClosedWithoutPreparedInventory(t *testing.T) {
	r := newResolver(newTree(t).env(), config.Config{}, Filters{})
	r.recoveryGaps = recoveryGaps{{Agent: string(harnessCodex), Cause: CauseNativeStoreUnreadable}: true}
	recovery, gap := r.recoveryFor(t.Context(), harnessCodex)
	if gap == nil || recovery == r.recovery {
		t.Fatalf("blocked app got the shared resolver: %+v", gap)
	}
	if recovery, gap := r.recoveryFor(t.Context(), harnessClaude); gap != nil || recovery != r.recovery {
		t.Fatalf("unblocked app: %+v", gap)
	}
}
