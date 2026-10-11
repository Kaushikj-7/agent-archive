package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// A workspace reference that fails to read keeps the epoch incomplete even when
// a single message folder would otherwise name the chat's root.
func TestCursorRecoveryUnreadableWorkspaceStaysGapDespiteMessageFolder(t *testing.T) {
	_, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	env.ReadFile = os.ReadFile // recovery refuses the unbounded port
	works := []*work{{t: &transcript{harness: harnessCursor}, chat: CursorDatabaseChat{ID: "c", WorkspaceID: "current"}, messageFolders: []string{root}}}
	complete, gaps, err := observeCursorRecoveryWorks(t.Context(), env, newResolver(env, cfg, Filters{}), works, &cursorRecoveryReadBudget{remaining: 1 << 20, rows: 16})
	if err != nil || complete || !gaps[recoveryGap{Agent: string(harnessCursor), Cause: CauseCursorChatFolderUnavailable}] {
		t.Fatalf("complete=%v gaps=%+v err=%v", complete, gaps, err)
	}
	if works[0].res.root != root {
		t.Fatalf("message folder not used: %+v", works[0].res)
	}
}

// noGapCause is a recovery failure that no witness gap explains.
const noGapCause DiagnosticDetail = ""

// Chats with no folder evidence (subagent composers, a chat opened without a
// workspace) name no root, so they cannot hide a clone. A workspace reference
// whose workspace.json is missing still could, and so could one this release
// cannot resolve (a remote or unknown workspaceIdentifier), so both stay gaps.
func TestFirstRunRecoveryFolderlessCursorChatsAreNotWitnesses(t *testing.T) {
	for _, kind := range []string{"folderless", "unreadable_workspace", "unresolvable_workspace"} {
		t.Run(kind, func(t *testing.T) {
			_, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			chats := []CursorDatabaseChat{
				{ID: "live", Folder: root, CreatedAt: fixedNow.Add(-48 * time.Hour)},
				{ID: "task-call-00000000-0000-0000-0000-000000000001-1", CreatedAt: fixedNow.Add(-48 * time.Hour)},
				{ID: "top-level", CreatedAt: fixedNow.Add(-48 * time.Hour)},
			}
			if kind == "unreadable_workspace" {
				chats[2].WorkspaceID = "missing"
			}
			if kind == "unresolvable_workspace" {
				chats[2].WorkspaceIdentifier = true
			}
			composers := map[string]cursorstore.Composer{}
			for _, chat := range chats {
				composers[chat.ID] = syntheticChat(chat.ID, nil, "hi")
			}
			env.CursorDatabase = fakeCursorDatabase(chats, composers, nil, nil)
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			p := plan(t, env, nil, cfg, Filters{})
			c := candidate(t, p, goneID)
			if kind == "folderless" {
				if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
					t.Fatalf("folderless chat blocked recovery: %+v %+v", c, c.Diagnostic)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
				return
			}
			// The unreadable workspace is a Cursor gap: the Codex session still
			// recovers, and the plan names the gap it was recovered without.
			if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
				t.Fatalf("Cursor gap blocked Codex recovery: %+v %+v", c, c.Diagnostic)
			}
			if !recoveredWithout(p) || !hasRecoveryGap(p, "cursor", CauseCursorChatFolderUnavailable) {
				t.Fatalf("%+v", p.RecoveryEvidenceGaps)
			}
		})
	}
}

func TestCursorChatFolderlessRequiresNoEvidence(t *testing.T) {
	for _, tc := range []struct {
		chat    CursorDatabaseChat
		folders []string
		want    bool
	}{
		{CursorDatabaseChat{}, nil, true},
		{CursorDatabaseChat{Folder: "/a"}, nil, false},
		{CursorDatabaseChat{WorkspaceID: "w"}, nil, false},
		{CursorDatabaseChat{WorkspaceIdentifier: true}, nil, false},
		{CursorDatabaseChat{}, []string{"/a"}, false},
		{CursorDatabaseChat{}, []string{"/a", "/b"}, false},
	} {
		if got := cursorChatFolderless(tc.chat, tc.folders); got != tc.want {
			t.Errorf("%+v %v: got %v", tc.chat, tc.folders, got)
		}
	}
}

// The epoch digest covers a present but unresolvable workspace reference, so
// an admission renewal notices it appearing or disappearing.
func TestCursorRecoveryDigestCoversWorkspaceIdentifier(t *testing.T) {
	works := func(present bool) []*work {
		return []*work{{t: &transcript{harness: harnessCursor}, chat: CursorDatabaseChat{ID: "c", KeyID: "c", WorkspaceIdentifier: present}}}
	}
	if cursorRecoveryDigest(works(false)) == cursorRecoveryDigest(works(true)) {
		t.Fatal("digest ignores the workspace reference")
	}
}

func TestRecoveryWitnessGapsNameTheirCause(t *testing.T) {
	file := func(edit func(*work)) *work {
		w := &work{t: &transcript{harness: harnessCodex}, res: resolution{root: "/r", kind: ProjectKindRepository}}
		edit(w)
		return w
	}
	database := func(edit func(*work)) *work {
		w := &work{t: &transcript{harness: harnessCursor}, c: Candidate{SourceKind: archive.SourceKindCursorSQLite}, res: resolution{root: "/r", kind: ProjectKindRepository}}
		edit(w)
		return w
	}
	for name, tc := range map[string]struct {
		w    *work
		want DiagnosticDetail
		gap  bool
	}{
		"usable":                {file(func(*work) {}), "", false},
		"changed":               {file(func(w *work) { w.sourceChanged, w.sourceRewritten = true, true }), CauseNativeInventoryChanged, true},
		"appended":              {file(func(w *work) { w.sourceChanged = true }), "", false},
		"appended unknown":      {file(func(w *work) { w.sourceChanged, w.res = true, resolution{skip: SkipProjectUnknown} }), CauseNativeInventoryChanged, true},
		"appended mismatch":     {file(func(w *work) { w.sourceChanged, w.t.identityMismatch = true, true }), CauseNativeInventoryChanged, true},
		"database appended":     {database(func(w *work) { w.sourceChanged = true }), CauseCursorDatabaseUnavailable, true},
		"vanished":              {file(func(w *work) { w.vanished = true }), CauseNativeInventoryChanged, true},
		"unsafe":                {file(func(w *work) { w.unsafe = true }), CauseSessionFolderUnknown, true},
		"mismatch":              {file(func(w *work) { w.t.identityMismatch = true }), CauseSessionFolderUnknown, true},
		"file unknown":          {file(func(w *work) { w.res = resolution{skip: SkipProjectUnknown} }), CauseSessionFolderUnknown, true},
		"database vanished":     {database(func(w *work) { w.vanished = true }), CauseCursorDatabaseUnavailable, true},
		"database unsafe":       {database(func(w *work) { w.unsafe = true }), CauseCursorDatabaseUnavailable, true},
		"database folderless":   {database(func(w *work) { w.res = resolution{skip: SkipProjectUnknown} }), "", false},
		"database workspace":    {database(func(w *work) { w.res, w.chat.WorkspaceID = resolution{skip: SkipProjectUnknown}, "w" }), CauseCursorChatFolderUnavailable, true},
		"database folders":      {database(func(w *work) { w.res, w.messageFolders = resolution{skip: SkipProjectUnknown}, []string{"/a", "/b"} }), CauseCursorChatFolderUnavailable, true},
		"database named folder": {database(func(w *work) { w.res, w.chat.Folder = resolution{skip: SkipProjectUnknown}, "relative" }), CauseCursorChatFolderUnavailable, true},
	} {
		if cause, gap := witnessGap(tc.w); cause != tc.want || gap != tc.gap {
			t.Errorf("%s: got %q %v", name, cause, gap)
		}
	}

	var gaps recoveryGaps
	if gaps.cause() != "" {
		t.Fatal("empty gaps named a cause")
	}
	gaps.addUnreadable(unreadable{folders: 2, folderAgents: map[string]bool{"claude": true}})
	gaps.add(string(harnessCursor), CauseCursorChatFolderUnavailable)
	if !gaps[recoveryGap{Agent: "claude", Cause: CauseNativeStoreUnreadable}] || gaps.cause() != CauseNativeStoreUnreadable {
		t.Fatalf("%+v", gaps)
	}
	gaps.add("", CauseRecoveryBudget)
	if gaps.cause() != CauseRecoveryBudget {
		t.Fatal("budget must select the budget outcome")
	}
	var stores recoveryGaps
	stores.addUnreadable(unreadable{stores: map[string]bool{"codex": true, "claude": false}})
	if len(stores) != 1 || !stores[recoveryGap{Agent: "codex", Cause: CauseNativeStoreUnreadable}] {
		t.Fatalf("%+v", stores)
	}
	var unattributed recoveryGaps
	unattributed.addUnreadable(unreadable{folders: 1})
	if unattributed.cause() != CauseNativeStoreUnreadable {
		t.Fatalf("%+v", unattributed)
	}
}

// The resolver reports which witness evidence was missing instead of the
// generic configured-repository text, and keeps the budget outcome distinct.
func TestRecoveryInventoryGapCauseReachesResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unread  unreadable
		db      recoveryGaps
		changed bool
		outcome sourcefacts.RecoveryOutcome
		cause   DiagnosticDetail
	}{
		{"store", unreadable{stores: map[string]bool{"claude": true}}, nil, false, sourcefacts.RecoveryInventoryUnavailable, CauseNativeStoreUnreadable},
		{"inventory", unreadable{}, nil, true, sourcefacts.RecoveryInventoryUnavailable, CauseNativeInventoryChanged},
		{"database", unreadable{}, recoveryGaps{{Agent: "cursor", Cause: CauseCursorDatabaseUnavailable}: true}, false, sourcefacts.RecoveryInventoryUnavailable, CauseCursorDatabaseUnavailable},
		{"budget", unreadable{}, recoveryGaps{{Agent: "cursor", Cause: CauseRecoveryBudget}: true}, false, sourcefacts.RecoveryBudgetExhausted, ""},
		{"complete", unreadable{}, nil, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
			gone := tr.path("home/.codex/worktrees/deleted/repo")
			key := archive.RepoKey("https://example.test/acme/repo")
			cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
			r := newResolver(env, cfg, Filters{})
			r.inventoryAdded = func(context.Context) (addedSources, bool) { return addedSources{}, !tc.changed }
			prepareRecoveryInventory(t.Context(), r, nil, tc.unread, tc.db)
			res := r.resolveEvidence(t.Context(), gone, key)
			if res.outcome != tc.outcome || res.cause != tc.cause {
				t.Fatalf("got %q/%q, want %q/%q", res.outcome, res.cause, tc.outcome, tc.cause)
			}
			if d := candidateDiagnostic(res.skip, res.outcome, res.cause); tc.cause != "" && (d == nil || d.Detail != tc.cause) {
				t.Fatalf("diagnostic %+v", d)
			}
		})
	}
}

func TestRecoveryGapDiagnosticsRenderInTextAndJSON(t *testing.T) {
	for _, cause := range recoveryGapCauses {
		t.Run(string(cause), func(t *testing.T) {
			d := candidateDiagnostic(SkipWorktreeUnresolved, sourcefacts.RecoveryInventoryUnavailable, cause)
			if d == nil || d.Detail != cause {
				t.Fatalf("%+v", d)
			}
			p := Plan{Candidates: []Candidate{{Harness: "codex", NativeSessionID: "n", Skip: SkipWorktreeUnresolved, Diagnostic: d}}}
			var text, structured bytes.Buffer
			RenderText(&text, p)
			if err := RenderJSON(&structured, p); err != nil {
				t.Fatal(err)
			}
			if want := diagnosticDescriptions[cause].text; want == "" || !strings.Contains(text.String(), want) {
				t.Fatalf("text missing %q:\n%s", want, text.String())
			}
			var got struct {
				Diagnostics []DiagnosticSummary `json:"diagnostics"`
			}
			if err := json.Unmarshal(structured.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Diagnostics) != 1 || got.Diagnostics[0].Detail != cause || got.Diagnostics[0].Count != 1 {
				t.Fatalf("%+v", got)
			}
		})
	}
	// A cause never replaces a decided outcome other than inventory.
	if d := candidateDiagnostic(SkipWorktreeUnresolved, sourcefacts.RecoveryAmbiguous, CauseNativeStoreUnreadable); string(d.Detail) != string(sourcefacts.RecoveryAmbiguous) {
		t.Fatal(d)
	}
}

// More distinct witness roots than the inventory compares is a fixed limit,
// not an exhausted budget: the outcome stays inventory-unavailable, as before
// gaps were named, and the diagnostic does not advise a retry.
func TestRecoveryWitnessRootLimitIsNotABudget(t *testing.T) {
	tr, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	gone := tr.path("home/.codex/worktrees/deleted/repo")
	key := archive.RepoKey("https://example.test/acme/repo")
	r := newResolver(env, cfg, Filters{})
	prepareRecoveryInventory(t.Context(), r, witnessLimitItems(t), unreadable{}, nil)
	res := r.resolveEvidence(t.Context(), gone, key)
	if res.outcome != sourcefacts.RecoveryInventoryUnavailable || res.cause != CauseWitnessLimit {
		t.Fatalf("got %q/%q", res.outcome, res.cause)
	}
	d := candidateDiagnostic(res.skip, res.outcome, res.cause)
	if d == nil || d.Detail != CauseWitnessLimit || d.Action == ActionRetry {
		t.Fatalf("diagnostic %+v", d)
	}
}

// witnessLimitItems names one more distinct repository root than the witness
// inventory compares.
func witnessLimitItems(t *testing.T) []*work {
	t.Helper()
	base := t.TempDir()
	items := make([]*work, 0, 1025)
	for i := range 1025 {
		root := filepath.Join(base, strconv.Itoa(i))
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		items = append(items, &work{t: &transcript{harness: harnessCodex}, res: resolution{root: root, kind: ProjectKindRepository}})
	}
	return items
}

// The witness-limit text points to --map-project, so an exact mapping to a
// configured repository must still recover, and stay valid, past the limit.
func TestRecoveryWitnessLimitLeavesExactMappingAvailable(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	gone := tr.path("home/.codex/worktrees/deleted/repo")
	cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
	r := newResolver(env, cfg, Filters{ProjectMappings: map[string]string{gone: root}})
	prepareRecoveryInventory(t.Context(), r, witnessLimitItems(t), unreadable{}, nil)
	if !r.recoveryGaps[recoveryGap{Cause: CauseWitnessLimit}] {
		t.Fatalf("gaps %+v", r.recoveryGaps)
	}
	res := r.resolveEvidence(t.Context(), gone, archive.RepoKey("https://example.test/acme/repo"))
	if res.outcome != "" || res.root != root || res.proof == nil || res.proof.Method != "explicit_mapping" || res.current == nil {
		t.Fatalf("%+v", res)
	}
	res.current.reset(t.Context())
	if !res.current.valid() {
		t.Fatal("mapped recovery did not stay valid past the witness limit")
	}
}

// When the Cursor budget and the witness limit both leave gaps, the budget
// outcome wins, as on main, where only the budget selected it.
func TestRecoveryBudgetOutranksWitnessLimit(t *testing.T) {
	tr, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	gone := tr.path("home/.codex/worktrees/deleted/repo")
	var db recoveryGaps
	db.add(string(harnessCursor), CauseRecoveryBudget)
	r := newResolver(env, cfg, Filters{})
	prepareRecoveryInventory(t.Context(), r, witnessLimitItems(t), unreadable{}, db)
	res := r.resolveEvidence(t.Context(), gone, archive.RepoKey("https://example.test/acme/repo"))
	if res.outcome != sourcefacts.RecoveryBudgetExhausted {
		t.Fatalf("got %q/%q", res.outcome, res.cause)
	}
}

// An exact mapping does not use the witness inventory, so a witness gap must
// not rename an inventory outcome from the mapped target's own lookup.
func TestRecoveryMappingOutcomeCarriesNoWitnessGap(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	gone := tr.path("home/.codex/worktrees/deleted/repo")
	env.RepositoryIdentity = func(context.Context, string) sourcefacts.RepositoryIdentity {
		return sourcefacts.RepositoryIdentity{}
	}
	cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
	r := newResolver(env, cfg, Filters{ProjectMappings: map[string]string{gone: root}})
	prepareRecoveryInventory(t.Context(), r, nil, unreadable{stores: map[string]bool{"claude": true}}, nil)
	res := r.resolveEvidence(t.Context(), gone, archive.RepoKey("https://example.test/acme/repo"))
	if res.outcome != sourcefacts.RecoveryInventoryUnavailable || res.cause != noGapCause {
		t.Fatalf("got %q/%q", res.outcome, res.cause)
	}
}

// A workspace reference whose workspace.json does not exist resolves no
// folder; the epoch itself, not only the witness inventory, stays incomplete.
func TestCursorRecoveryMissingWorkspaceLeavesEpochIncomplete(t *testing.T) {
	_, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	works := []*work{{t: &transcript{harness: harnessCursor}, chat: CursorDatabaseChat{ID: "c", WorkspaceID: "missing"}}}
	complete, gaps, err := observeCursorRecoveryWorks(t.Context(), env, newResolver(env, cfg, Filters{}), works, &cursorRecoveryReadBudget{remaining: 1 << 20, rows: 16})
	if err != nil || complete || !gaps[recoveryGap{Agent: string(harnessCursor), Cause: CauseCursorChatFolderUnavailable}] {
		t.Fatalf("complete=%v gaps=%+v err=%v", complete, gaps, err)
	}
	folderless := []*work{{t: &transcript{harness: harnessCursor}, chat: CursorDatabaseChat{ID: "f"}}}
	complete, gaps, err = observeCursorRecoveryWorks(t.Context(), env, newResolver(env, cfg, Filters{}), folderless, &cursorRecoveryReadBudget{remaining: 1 << 20, rows: 16})
	if err != nil || !complete || len(gaps) != 0 {
		t.Fatalf("folderless: complete=%v gaps=%+v err=%v", complete, gaps, err)
	}
}
