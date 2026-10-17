package state

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// TestOutstandingFacets pins each facet of Outstanding to the file that
// records it, and each question to the facets it is made of.
func TestOutstandingFacets(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	bundle := func(reg archive.SessionRegistration) archive.SourceBundle {
		return archive.SourceBundle{ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, CapturedAt: at}}
	}
	for _, tc := range []struct {
		name          string
		reg           func(archive.SessionRegistration) archive.SessionRegistration
		setup         func(t *testing.T, s *Store, reg archive.SessionRegistration)
		requested     bool
		want          Outstanding
		pending       bool
		owed          bool
		syncCanFinish bool
		defersExpiry  bool
		rechecking    bool
	}{
		{
			name: "never captured", want: Outstanding{NeverCaptured: true},
			pending: true, syncCanFinish: true,
		},
		{
			name: "published and settled",
			setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				savePublished(t, s, reg, bundle(reg), CacheStatusPublished)
			},
			want: Outstanding{Published: true},
		},
		{
			name: "published with a request queued", requested: true,
			setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				savePublished(t, s, reg, bundle(reg), CacheStatusPublished)
			},
			want:    Outstanding{Requested: true, Published: true},
			pending: true, owed: true, syncCanFinish: true, defersExpiry: true,
		},
		{
			name: "published with an interrupted scan",
			setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				savePublished(t, s, reg, bundle(reg), CacheStatusPublished)
				if err := s.SetScanPending(reg.ArchiveSessionID, true); err != nil {
					t.Fatal(err)
				}
			},
			want:    Outstanding{Scan: true, Published: true},
			pending: true, owed: true, syncCanFinish: true, rechecking: true,
		},
		{
			// Not in the archive yet: waiting, not rechecking.
			name: "never captured with an interrupted scan",
			setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				if err := s.SetScanPending(reg.ArchiveSessionID, true); err != nil {
					t.Fatal(err)
				}
			},
			want:    Outstanding{Scan: true, NeverCaptured: true},
			pending: true, owed: true, syncCanFinish: true,
		},
		{
			// Newer work to upload as well: more than a re-scan.
			name: "published with an interrupted scan and a request", requested: true,
			setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				savePublished(t, s, reg, bundle(reg), CacheStatusPublished)
				if err := s.SetScanPending(reg.ArchiveSessionID, true); err != nil {
					t.Fatal(err)
				}
			},
			want:    Outstanding{Requested: true, Scan: true, Published: true},
			pending: true, owed: true, syncCanFinish: true, defersExpiry: true,
		},
		{
			// The review's case: a published session whose update waits for
			// the upload interval is pending everywhere.
			name: "published with a rate-limited update",
			setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				savePublished(t, s, reg, bundle(reg), CacheStatusPublished)
				savePublished(t, s, reg, bundle(reg), CacheStatusRateLimited)
				if err := s.SavePending(reg.ArchiveSessionID, pendingPublication(bundle(reg), at.Add(time.Hour))); err != nil {
					t.Fatal(err)
				}
			},
			want:    Outstanding{Upload: true, RateLimited: true, Published: true},
			pending: true, owed: true, syncCanFinish: true, defersExpiry: true,
		},
		{
			// A rate-limited candidate whose pending file was lost: the
			// collector rebuilds it on its next pass.
			name: "rate-limited cache without its upload",
			setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				savePublished(t, s, reg, bundle(reg), CacheStatusRateLimited)
			},
			want:    Outstanding{RateLimited: true},
			pending: true, owed: true, syncCanFinish: true,
		},
		{
			name: "blocked", setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				p, err := s.LoadPublishedState(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if err := p.SaveBlocked(bundle(reg), at, BlockedReasonTranscriptMissing); err != nil {
					t.Fatal(err)
				}
			},
			want: Outstanding{Blocked: true},
		},
		{
			name: "declined", setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				savePublished(t, s, reg, bundle(reg), CacheStatusDeclined)
			},
			want: Outstanding{},
		},
		{
			name: "waiting for its transcript", requested: true,
			reg: func(r archive.SessionRegistration) archive.SessionRegistration {
				r.Harness.Name, r.TranscriptPath = "cursor", ""
				return r
			},
			want:    Outstanding{Requested: true, NeverCaptured: true, WaitingForTranscript: true},
			pending: true, owed: true, defersExpiry: true,
		},
		{
			name: "no transcript path but an upload in flight", requested: true,
			reg: func(r archive.SessionRegistration) archive.SessionRegistration {
				r.Harness.Name, r.TranscriptPath = "cursor", ""
				return r
			},
			setup: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				if err := s.SavePending(reg.ArchiveSessionID, pendingPublication(bundle(reg), time.Time{})); err != nil {
					t.Fatal(err)
				}
			},
			want:    Outstanding{Requested: true, Upload: true, NeverCaptured: true},
			pending: true, owed: true, syncCanFinish: true, defersExpiry: true,
		},
		{
			// A Cursor database chat has no path by design and never waits.
			name: "cursor database chat", requested: true,
			reg: func(r archive.SessionRegistration) archive.SessionRegistration {
				r.Harness.Name, r.TranscriptPath, r.SourceKind = "cursor", "", archive.SourceKindCursorSQLite
				return r
			},
			want:    Outstanding{Requested: true, NeverCaptured: true},
			pending: true, owed: true, syncCanFinish: true, defersExpiry: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			reg := registration(t)
			if tc.reg != nil {
				reg = tc.reg(reg)
			}
			if tc.setup != nil {
				tc.setup(t, s, reg)
			}
			got, err := s.Outstanding(reg, tc.requested)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Outstanding = %+v, want %+v", got, tc.want)
			}
			if got.Pending() != tc.pending || got.Owed() != tc.owed || got.SyncCanFinish() != tc.syncCanFinish || got.DefersExpiry() != tc.defersExpiry {
				t.Fatalf("Pending %v Owed %v SyncCanFinish %v DefersExpiry %v, want %v %v %v %v", got.Pending(), got.Owed(), got.SyncCanFinish(), got.DefersExpiry(), tc.pending, tc.owed, tc.syncCanFinish, tc.defersExpiry)
			}
			if got.Rechecking() != tc.rechecking {
				t.Fatalf("Rechecking %v, want %v", got.Rechecking(), tc.rechecking)
			}
		})
	}
}

func pendingPublication(bundle archive.SourceBundle, readyAt time.Time) PendingPublication {
	return PendingPublication{Bundle: bundle, SourceKey: "k", MetadataKey: "m", SourceSHA256: durableRef([]byte{1}).SHA256, SourceBytes: []byte{1}, MetadataBytes: []byte(`{}`), ReadyAt: readyAt}
}

func savePublished(t *testing.T, s *Store, reg archive.SessionRegistration, bundle archive.SourceBundle, status CacheStatus) {
	t.Helper()
	p, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(bundle, bundle.Capture.CapturedAt, status); err != nil {
		t.Fatal(err)
	}
}

// TestOutstandingIsTheOnlyDefinition keeps "is this session owed an upload"
// in one place. Outside this package, no production code may read the
// files Outstanding is made of to decide it for itself, except where listed
// below with the reason. Before this existed five callers each had their
// own definition, and they disagreed.
func TestOutstandingIsTheOnlyDefinition(t *testing.T) {
	// The primitives Outstanding reads. A call to one of them, or a
	// comparison with the rate-limited status, is a definition of pending.
	forbidden := map[string]bool{"ScanPending": true, "HasPending": true, "CacheStatusRateLimited": true}
	// file -> names it may use, and why.
	allowed := map[string]map[string]string{
		// The collector's skip is the stat-only steady state: it asks only
		// whether a scan or upload is journaled, per session per pass,
		// where reading even the summary would cost a file read each.
		"internal/collector/skip.go": {"ScanPending": "stat-only skip", "HasPending": "stat-only skip"},
		// The collector writes the rate-limited state it compares with.
		"internal/collector/session.go": {"CacheStatusRateLimited": "writes the state"},
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (rel == "internal/state" || strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !forbidden[sel.Sel.Name] {
				return true
			}
			if _, ok := allowed[rel][sel.Sel.Name]; !ok {
				t.Errorf("%s: uses %s; ask state.Store.Outstanding instead", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
