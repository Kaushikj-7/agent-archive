package backfill

import (
	"context"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// A Cursor database epoch that was complete at planning and then changed
// blocks only recoveries whose proof rests on Cursor database evidence. A
// Codex recovery into a root that a Codex session proposes is admitted:
// Cursor's chats were only possible clones, the per-app gap residual.
func TestRecoveryCursorEpochChangeBlocksOnlyCursorDependentProofs(t *testing.T) {
	for _, kind := range []cursorChangedEvidenceCase{cursorEvidenceMembership, cursorEvidenceRoot, cursorEvidenceEligibility} {
		for _, filters := range []Filters{{Harnesses: []string{"codex"}}, {}} {
			t.Run(string(kind)+"/"+map[bool]string{true: "codex", false: "all"}[len(filters.Harnesses) > 0], func(t *testing.T) {
				tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
				// The Codex witness stays: it proposes root on its own.
				chats := []CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow.Add(-48 * time.Hour)}}
				composers := map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}
				env.CursorDatabase = func(ctx context.Context) (CursorDatabaseResult, error) {
					return fakeCursorDatabase(chats, composers, nil, nil)(ctx)
				}
				env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
				p := plan(t, env, nil, cfg, filters)
				c := candidate(t, p, goneID)
				if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil || c.ProjectResolution.Method != "recorded_repository" {
					t.Fatalf("%+v %+v", c, c.Diagnostic)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case cursorEvidenceMembership:
					// A new clone of the same repository known only to Cursor:
					// the documented residual of admitting past Cursor evidence.
					chats = append(chats, CursorDatabaseChat{ID: "clone", Folder: tr.repo("home/clone"), CreatedAt: fixedNow})
					composers["clone"] = syntheticChat("clone", nil, "hi")
				case cursorEvidenceRoot:
					chats[0].Folder = tr.repo("home/other")
				case cursorEvidenceEligibility:
					composers["live"] = syntheticChat("live", nil)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatalf("Cursor epoch change blocked a Codex-proposed recovery: %v", err)
				}
			})
		}
	}
}

// recoveryAdmissionResolver plans one recorded-key recovery into root with
// the given witnesses and a Cursor epoch whose currency is *databaseCurrent.
func recoveryAdmissionResolver(t *testing.T, witnesses []*work, databaseCurrent *bool) (*resolver, string, string, string) {
	t.Helper()
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	gone := tr.path("home/.codex/worktrees/deleted/repo")
	for _, w := range witnesses {
		w.res = resolution{root: root, kind: ProjectKindRepository}
	}
	r := newResolver(env, cfg, Filters{})
	r.databaseRecoveryCurrent = func(context.Context) bool { return *databaseCurrent }
	prepareRecoveryInventory(t.Context(), r, witnesses, unreadable{}, nil)
	return r, root, gone, archive.RepoKey("https://example.test/acme/repo")
}

func recoveryWitness(h harness, kind archive.SourceKind) *work {
	return &work{t: &transcript{harness: h}, c: Candidate{SourceKind: kind}, validated: true}
}

func TestRecoveryCursorEpochChangeByAppAndDestinationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		// witnesses at the destination root
		codex             bool
		codexEvidenceOnly bool
		cursorDB          bool
		// admitted after the Cursor epoch changed, by session app
		codexAdmitted   bool
		cursorAdmitted  bool
		unknownAdmitted bool
	}{
		{name: "codex_and_cursor_witnesses", codex: true, cursorDB: true, codexAdmitted: true},
		{name: "codex_witness_only", codex: true, codexAdmitted: true},
		{name: "cursor_witness_only", cursorDB: true},
		// A Codex witness that cannot propose the root on its own leaves the
		// destination's eligibility resting on Cursor's chat.
		{name: "codex_cannot_propose", codex: true, codexEvidenceOnly: true, cursorDB: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var witnesses []*work
			if tc.codex {
				w := recoveryWitness(harnessCodex, archive.SourceKindFile)
				w.validated = !tc.codexEvidenceOnly
				witnesses = append(witnesses, w)
			}
			if tc.cursorDB {
				witnesses = append(witnesses, recoveryWitness(harnessCursor, archive.SourceKindCursorSQLite))
			}
			databaseCurrent := true
			r, root, gone, key := recoveryAdmissionResolver(t, witnesses, &databaseCurrent)
			for _, h := range []harness{harnessCodex, harnessCursor, ""} {
				res := r.resolveSessionEvidence(t.Context(), h, gone, key)
				if res.proof == nil || res.root != root || res.current == nil {
					t.Fatalf("%s: not recovered: %+v", h, res)
				}
				res.current.reset(t.Context())
				if !res.current.valid() {
					t.Fatalf("%s: unchanged epoch rejected", h)
				}
				databaseCurrent = false
				res.current.reset(t.Context())
				want := map[harness]bool{harnessCodex: tc.codexAdmitted, harnessCursor: tc.cursorAdmitted, "": tc.unknownAdmitted}[h]
				if got := res.current.valid(); got != want {
					t.Fatalf("%s after epoch change: admitted=%v, want %v", h, got, want)
				}
				databaseCurrent = true
			}
		})
	}
}

func TestCursorEvidenceRequired(t *testing.T) {
	for _, tc := range []struct {
		h         harness
		dependent bool
		want      bool
	}{
		{h: harnessCodex, dependent: false, want: false},
		{h: harnessCodex, dependent: true, want: true},
		{h: harnessClaude, dependent: false, want: false},
		{h: harnessCursor, dependent: false, want: true},
		{h: "", dependent: false, want: true},
	} {
		if got := cursorEvidenceRequired(tc.h, tc.dependent); got != tc.want {
			t.Errorf("cursorEvidenceRequired(%q, %v) = %v, want %v", tc.h, tc.dependent, got, tc.want)
		}
	}
}
