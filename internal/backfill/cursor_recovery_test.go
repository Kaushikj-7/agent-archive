package backfill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/platform"
)

type cursorUnavailableEvidenceCase string

type cursorChangedEvidenceCase string

type cursorWorkspaceEvidenceCase string

const (
	cursorEvidenceLocked        cursorUnavailableEvidenceCase = "locked"
	cursorEvidenceUnknown       cursorUnavailableEvidenceCase = "unknown"
	cursorEvidenceError         cursorUnavailableEvidenceCase = "error"
	cursorEvidenceMalformed     cursorUnavailableEvidenceCase = "malformed"
	cursorEvidenceUnknownRoot   cursorUnavailableEvidenceCase = "unknown_root"
	cursorEvidenceRows          cursorUnavailableEvidenceCase = "rows"
	cursorEvidenceMembership    cursorChangedEvidenceCase     = "membership"
	cursorEvidenceRoot          cursorChangedEvidenceCase     = "root"
	cursorEvidenceEligibility   cursorChangedEvidenceCase     = "eligibility"
	cursorEvidenceValid         cursorWorkspaceEvidenceCase   = "valid"
	cursorEvidenceOversize      cursorWorkspaceEvidenceCase   = "oversize"
	cursorEvidenceSymlink       cursorWorkspaceEvidenceCase   = "symlink"
	cursorEvidenceUnboundedPort cursorWorkspaceEvidenceCase   = "unbounded_port"
)

func removeFirstRunFileWitness(t *testing.T, tr *tree) {
	t.Helper()
	if err := os.Remove(tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRunRecoveryCursorDatabaseSoleDestinationAndHiddenClone(t *testing.T) {
	for _, clone := range []bool{false, true} {
		t.Run(map[bool]string{false: "sole", true: "hidden_clone"}[clone], func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			removeFirstRunFileWitness(t, tr)
			chats := []CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow.Add(-48 * time.Hour)}}
			composers := map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}
			if clone {
				other := tr.repo("home/clone")
				chats = append(chats, CursorDatabaseChat{ID: "clone", Folder: other, CreatedAt: fixedNow.Add(-48 * time.Hour)})
				composers["clone"] = syntheticChat("clone", nil, "hi")
			}
			calls := 0
			env.CursorDatabase = fakeCursorDatabase(chats, composers, nil, &calls)
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}, Projects: []string{root}})
			c := candidate(t, p, goneID)
			if clone {
				if c.ProjectResolution != nil || c.ProjectRoot != "" {
					t.Fatal(c)
				}
			} else {
				if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
					t.Fatal(c)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := ApplyToConfig(&cfg, p, fixedNow); err != nil {
					t.Fatal(err)
				}
				if len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != root {
					t.Fatal(cfg.Archive.Projects)
				}
			}
			if len(databaseCandidates(p)) != 0 {
				t.Fatal("hidden DB output leaked")
			}
			if calls > 3 {
				t.Fatalf("per-thread catalog observations: %d", calls)
			}
		})
	}
}

func TestFirstRunRecoveryCursorDatabaseUnavailableOrMalformed(t *testing.T) {
	for _, kind := range []cursorUnavailableEvidenceCase{cursorEvidenceLocked, cursorEvidenceUnknown, cursorEvidenceError, cursorEvidenceMalformed, cursorEvidenceUnknownRoot, cursorEvidenceRows} {
		t.Run(string(kind), func(t *testing.T) {
			_, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			chats := []CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow}}
			if kind == cursorEvidenceMalformed {
				chats[0].Malformed = true
			}
			if kind == cursorEvidenceUnknownRoot {
				// A workspace reference that cannot be read could name a clone.
				// (A chat with no folder evidence at all is a non-witness; see
				// TestFirstRunRecoveryFolderlessCursorChatsAreNotWitnesses.)
				chats[0].Folder, chats[0].WorkspaceID = "", "missing"
			}
			if kind == cursorEvidenceRows {
				chats = make([]CursorDatabaseChat, cursorRecoveryRows+1)
			}
			env.CursorDatabase = fakeCursorDatabase(chats, map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}, nil, nil)
			if kind == cursorEvidenceLocked || kind == cursorEvidenceUnknown {
				env.CursorDatabase = func(context.Context) (CursorDatabaseResult, error) {
					return CursorDatabaseResult{Reason: CursorUncheckedLocked}, nil
				}
			}
			if kind == cursorEvidenceError {
				env.CursorDatabase = func(context.Context) (CursorDatabaseResult, error) {
					return CursorDatabaseResult{}, errors.New("unavailable")
				}
			}
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			want := map[cursorUnavailableEvidenceCase]DiagnosticDetail{
				cursorEvidenceLocked:      CauseCursorDatabaseUnavailable,
				cursorEvidenceUnknown:     CauseCursorDatabaseUnavailable,
				cursorEvidenceError:       CauseCursorDatabaseUnavailable,
				cursorEvidenceMalformed:   CauseCursorDatabaseUnavailable,
				cursorEvidenceUnknownRoot: CauseCursorChatFolderUnavailable,
				cursorEvidenceRows:        CauseRecoveryBudget,
			}[kind]
			// A Cursor gap blocks only Cursor's own recoveries. The Codex
			// session recovers into the root a Codex session witnessed, filtered
			// or not, and the plan names the Cursor gap it was recovered without.
			runs := []Filters{{Harnesses: []string{"codex"}}, {}}
			if kind == cursorEvidenceError {
				runs = runs[:1] // An unfiltered plan reads the database for import and fails.
			}
			for _, filters := range runs {
				p := plan(t, env, nil, cfg, filters)
				c := candidate(t, p, goneID)
				if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
					t.Fatalf("Cursor gap blocked Codex recovery: %+v %+v", c, c.Diagnostic)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
				if !recoveredWithout(p) || !hasRecoveryGap(p, "cursor", want) {
					t.Fatalf("Cursor gap not reported: %+v, want %s", p.RecoveryEvidenceGaps, want)
				}
			}
			if kind == cursorEvidenceRows {
				epoch, err := readCursorRecoveryEpoch(t.Context(), env, newResolver(env, cfg, Filters{}), unreadable{})
				if err != nil || !epoch.budget || epoch.complete {
					t.Fatal(epoch, err)
				}
			}
		})
	}
}

func TestFirstRunRecoveryCursorDatabaseEpochRenewsMembershipRootAndEligibility(t *testing.T) {
	for _, kind := range []cursorChangedEvidenceCase{cursorEvidenceMembership, cursorEvidenceRoot, cursorEvidenceEligibility} {
		t.Run(string(kind), func(t *testing.T) {
			tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
			removeFirstRunFileWitness(t, tr)
			chats := []CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow}}
			composers := map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}
			env.CursorDatabase = func(ctx context.Context) (CursorDatabaseResult, error) {
				return fakeCursorDatabase(chats, composers, nil, nil)(ctx)
			}
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			if err := p.CheckRecovery(t.Context()); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case cursorEvidenceMembership:
				chats = append(chats, CursorDatabaseChat{ID: "clone", Folder: tr.repo("home/clone"), CreatedAt: fixedNow})
				composers["clone"] = syntheticChat("clone", nil, "hi")
			case cursorEvidenceRoot:
				chats[0].Folder = tr.repo("home/other")
			case cursorEvidenceEligibility:
				composers["live"] = syntheticChat("live", nil)
			}
			if err := p.CheckRecovery(t.Context()); err == nil {
				t.Fatal("changed DB evidence admitted")
			}
		})
	}
}

func TestFirstRunRecoveryCursorDatabaseSettledObservationAvoidsChatRereads(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	removeFirstRunFileWitness(t, tr)
	path := env.cursorStateDatabase()
	writeCursorDB(t, path, true, chatRows("live", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + root}}, "hi"))
	reader := CursorRecoveryDatabaseReaderFor(env)
	catalogs, reads := 0, 0
	env.CursorRecoveryDatabase = func(ctx context.Context, rows int, bytes int64) (CursorDatabaseResult, error) {
		catalogs++
		res, err := reader(ctx, rows, bytes)
		read := res.ReadRecoverySnapshot
		if read != nil {
			res.ReadRecoverySnapshot = func(ctx context.Context, id string, budget agentapi.RecoveryReadBudget) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
				reads++
				return read(ctx, id, budget)
			}
		}
		return res, err
	}
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	for range 4 {
		if err := p.CheckRecovery(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if catalogs != 1 || reads != 1 {
		t.Fatalf("settled DB reread: catalogs=%d chats=%d", catalogs, reads)
	}
	db := openCursorWriter(t, path, true)
	insertCursorRows(t, db, chatRows("other", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + tr.repo("home/clone")}}, "hi"))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("changed settled DB admitted")
	}
	if catalogs != 1 || reads != 1 {
		t.Fatal("changed settled DB must require a new reviewed plan")
	}
}

func TestFirstRunRecoveryCursorDatabaseBoundedRenewalsAndAggregateBytes(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	removeFirstRunFileWitness(t, tr)
	calls := 0
	env.CursorDatabase = fakeCursorDatabase([]CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow}}, map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}, nil, &calls)
	env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	for range cursorRecoveryRenewals {
		if err := p.CheckRecovery(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("unbounded renewed DB scans")
	}
	if calls != cursorRecoveryRenewals+1 {
		t.Fatal(calls)
	}
	b := cursorRecoveryReadBudget{remaining: 12}
	c := cursorstore.Composer{Composer: []byte("1234"), Bubbles: []cursorstore.Bubble{{Value: []byte("5678")}}}
	if err := b.Charge(0, int64(len(c.Composer)+len(c.Bubbles[0].Value))); err != nil || b.remaining != 4 {
		t.Fatal(b, err)
	}
	if err := b.Charge(0, int64(len(c.Composer)+len(c.Bubbles[0].Value))); err == nil || !b.exhausted {
		t.Fatal("aggregate byte cap ignored")
	}
}

// The synthetic catalog is already in memory; this test port enforces membership
// and value allowances before returning borrowed composer bytes.
func boundedTestRecoveryReader(reader func(context.Context) (CursorDatabaseResult, error)) func(context.Context, int, int64) (CursorDatabaseResult, error) {
	return func(ctx context.Context, rows int, bytes int64) (CursorDatabaseResult, error) {
		res, err := reader(ctx)
		if err != nil || !res.Checked {
			return res, err
		}
		if len(res.Chats) > rows {
			return CursorDatabaseResult{RecoveryBudgetExhausted: true}, nil
		}
		res.ReadRecoverySnapshot = func(ctx context.Context, id string, budget agentapi.RecoveryReadBudget) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
			c, err := res.ReadChat(ctx, id)
			if err != nil {
				return c, nil, err
			}
			b := budget
			size := int64(len(c.Composer))
			for _, bubble := range c.Bubbles {
				size += int64(len(bubble.Value))
			}
			if b.Charge(1, size) != nil {
				return cursorstore.Composer{}, nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
			}
			return c, nil, nil
		}
		return res, nil
	}
}

func TestFirstRunRecoveryCursorDatabaseCatalogBoundBeforeAllocation(t *testing.T) {
	tr := newTree(t)
	root := tr.repo("home/repo")
	env := tr.env()
	path := env.cursorStateDatabase()
	writeCursorDB(t, path, true, mergeRows(chatRows("a", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + root}}, "hi"), chatRows("b", nil, "hi")))
	read := CursorRecoveryDatabaseReaderFor(env)
	for _, tc := range []struct {
		name  string
		rows  int
		bytes int64
	}{{"oversize_row", 4, 10}, {"many_rows", 1, 128 << 20}} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := read(t.Context(), tc.rows, tc.bytes)
			if err != nil || res.Checked || !res.RecoveryBudgetExhausted || res.RecoveryRows > 4 || res.RecoveryBytes > tc.bytes {
				t.Fatal(res, err)
			}
		})
	}
	res, err := read(t.Context(), 5, 128<<20)
	if err != nil || !res.Checked || res.RecoveryRows != 5 || res.RecoveryBytes <= 0 {
		t.Fatal(res, err)
	}
	if _, snap, err := res.ReadRecoverySnapshot(t.Context(), "a", &cursorRecoveryReadBudget{remaining: 1, rows: cursorRecoveryRecords}); err == nil || snap != nil || !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatal("snapshot exceeded allowance", err)
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRunRecoveryCursorDatabaseMissingBoundedCapabilityDoesNotFallback(t *testing.T) {
	_, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	calls := 0
	env.CursorDatabase = fakeCursorDatabase([]CursorDatabaseChat{{ID: "live", Folder: root}}, map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}, nil, &calls)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	// Without the bounded reader the database is a Cursor gap: it does not
	// block the Codex session, whose destination a Codex session witnessed.
	if c := candidate(t, p, goneID); c.Skip != "" || c.ProjectRoot != root || !recoveredWithout(p) {
		t.Fatal(c, p.RecoveryEvidenceGaps)
	}
	if calls != 0 {
		t.Fatal("unbounded evidence fallback", calls)
	}
	cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
	p = plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}, ProjectMappings: map[string]string{filepath.Join(env.Home, ".codex", "worktrees", "deleted", "repo"): root}})
	if c := candidate(t, p, goneID); c.Skip != "" || c.ProjectResolution == nil || c.ProjectResolution.Method != "explicit_mapping" {
		t.Fatal(c)
	}
	if err := p.CheckRecovery(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRunRecoveryCursorDatabaseSettledOwnershipCannotChange(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	removeFirstRunFileWitness(t, tr)
	folder := tr.mkdir("home/repo/inner")
	path := env.cursorStateDatabase()
	writeCursorDB(t, path, false, chatRows("live", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + folder}}, "hi"))
	env.CursorRecoveryDatabase = CursorRecoveryDatabaseReaderFor(env)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if len(p.Imported()) != 1 || p.Imported()[0].ProjectRoot != root {
		t.Fatal(p.Candidates)
	}
	tr.mkdir("home/repo/inner/.git")
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("changed live ownership retained old destination")
	}
}

func TestFirstRunRecoveryCatalogLengthAndValuesShareTransaction(t *testing.T) {
	tr := newTree(t)
	path := tr.env().cursorStateDatabase()
	db := openCursorWriter(t, path, true)
	defer closeAtEnd(t, db)
	insertCursorRows(t, db, map[string]any{"composerData:a": "a", "composerData:b": "b"})
	writer := openCursorWriter(t, path, true)
	defer closeAtEnd(t, writer)
	h := &recoveryCatalogHost{db: db, maxRows: 2, budget: &cursorRecoveryReadBudget{remaining: 4096, rows: 100}}
	seen := 0
	const query = `SELECT key,value FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;'`
	if err := h.Query(t.Context(), query, func(agentapi.DatabaseRecord) error {
		seen++
		if seen == 1 {
			_, err := writer.ExecContext(t.Context(), `INSERT INTO cursorDiskKV(key,value)VALUES('composerData:c','c')`)
			return err
		}
		return nil
	}); err != nil || seen != 2 {
		t.Fatal("catalog snapshot changed during value allocation", seen, err)
	}
	h = &recoveryCatalogHost{db: db, maxRows: 2, budget: &cursorRecoveryReadBudget{remaining: 4096, rows: 100}}
	seen = 0
	if err := h.Query(t.Context(), query, func(agentapi.DatabaseRecord) error { seen++; return nil }); !agentapi.HasFailure(err, agentapi.Limit) || seen != 0 {
		t.Fatal("oversized membership allocated values", seen, err)
	}
}

func TestFirstRunRecoveryWorkspaceEvidenceIsBoundedAndRooted(t *testing.T) {
	for _, kind := range []cursorWorkspaceEvidenceCase{cursorEvidenceValid, cursorEvidenceOversize, cursorEvidenceSymlink, cursorEvidenceUnboundedPort} {
		t.Run(string(kind), func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			removeFirstRunFileWitness(t, tr)
			rel := "home/Library/Application Support/Cursor/User/workspaceStorage/current/workspace.json"
			path := tr.write(rel, fmt.Sprintf(`{"folder":%q}`, "file://"+root))
			switch kind {
			case cursorEvidenceOversize:
				tr.write(rel, strings.Repeat("x", (1<<20)+1))
			case cursorEvidenceSymlink:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := tr.write("home/outside.json", fmt.Sprintf(`{"folder":%q}`, "file://"+root))
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case cursorEvidenceUnboundedPort:
				env.ReadFile = os.ReadFile
			case cursorEvidenceValid:
				// Keep the supported bounded workspace fixture unchanged.
			}
			writeCursorDB(t, env.cursorStateDatabase(), false, chatRows("live", map[string]any{"workspaceIdentifier": map[string]any{"id": "current"}}, "hi"))
			env.CursorRecoveryDatabase = CursorRecoveryDatabaseReaderFor(env)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			c := candidate(t, p, goneID)
			if kind == cursorEvidenceValid {
				if c.Skip != "" || c.ProjectRoot != root {
					t.Fatal(c)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if c.ProjectResolution != nil || c.Skip == "" {
				t.Fatal("unsafe metadata invented destination", c)
			}
		})
	}
}

func TestFirstRunRecoveryCursorDatabaseUsesInjectedLocations(t *testing.T) {
	for _, operatingSystem := range []platform.OS{platform.Darwin, platform.Linux} {
		t.Run(string(operatingSystem), func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			env.OS = operatingSystem
			removeFirstRunFileWitness(t, tr)
			writeCursorDB(t, env.cursorStateDatabase(), false, chatRows("live", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + root}}, "hi"))
			env.CursorRecoveryDatabase = CursorRecoveryDatabaseReaderFor(env)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			if c := candidate(t, p, goneID); c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
				t.Fatal(c)
			}
			if err := p.CheckRecovery(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryCatalogExhaustionPrecedesSQL(t *testing.T) {
	tr := newTree(t)
	db := openCursorWriter(t, tr.env().cursorStateDatabase(), false)
	defer closeAtEnd(t, db)
	for _, budget := range []*cursorRecoveryReadBudget{{rows: 0, remaining: 4096}, {rows: 100, remaining: 15}} {
		host := &recoveryCatalogHost{db: db, maxRows: 1024, budget: budget}
		// An invalid static selector proves the gate runs before native SQL.
		err := host.Query(t.Context(), "SELECT key,value FROM unavailable_table", func(agentapi.DatabaseRecord) error { t.Fatal("allocated catalog payload after exhaustion"); return nil })
		if !agentapi.HasFailure(err, agentapi.Limit) {
			t.Fatal("catalog query preceded budget gate", err)
		}
	}
}
