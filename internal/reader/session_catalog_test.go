package reader

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionCatalogRefreshPagesAndWarmBodies(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 31 {
		putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i)*time.Minute))
	}
	reads := 0
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{BodyRead: func(string, bool) { reads++ }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	if reads != 31 {
		t.Fatalf("cold bodies=%d", reads)
	}
	reads = 0
	store.reset()
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("warm decoded %d bodies", reads)
	}
	q := CatalogQuery{Metadata: MetadataQuery{Limit: 7, Order: ActivityOrder}}
	first, err := c.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 31 || len(first.Rows) != 7 || !first.Complete {
		t.Fatalf("page=%+v", first)
	}
	all := append([]CatalogRow(nil), first.Rows...)
	q.Cursor = first.Next
	for q.Cursor != "" {
		p, e := c.Query(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		all = append(all, p.Rows...)
		q.Cursor = p.Next
	}
	oracle, err := ListMetadataWithOptions(ctx, store, "sessions/", Filter{}, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(oracle) {
		t.Fatal("lost rows")
	}
	for i, row := range all {
		if row.Summary.SessionID != oracle[i].SessionID {
			t.Fatalf("order[%d]=%s != %s", i, row.Summary.SessionID, oracle[i].SessionID)
		}
		if len(row.Hash) != 64 {
			t.Fatal("missing body hash")
		}
	}
	q.Cursor = first.Next
	q.Words = []string{"changed project label binding"}
	if _, err = c.Query(ctx, q); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("query cursor=%v", err)
	}
	if err = store.Delete(ctx, all[0].Key); err != nil {
		t.Fatal(err)
	}
	h, err = DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	q.Words = nil
	if _, err = c.Query(ctx, q); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("generation cursor=%v", err)
	}
	q.Cursor = ""
	p, err := c.Query(ctx, q)
	if err != nil || p.Total != 30 {
		t.Fatalf("deleted total=%d err=%v", p.Total, err)
	}
	q.Cursor = p.Next
	putSession(t, store, "codex", all[1].Summary.SessionID, baseTime.Add(2*time.Hour))
	h, err = DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Query(ctx, q); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("changed cursor=%v", err)
	}
	q.Cursor = ""
	p, err = c.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 30 || p.Rows[0].Summary.SessionID != all[1].Summary.SessionID || p.Rows[0].ETag == all[1].ETag || p.Rows[0].Hash == all[1].Hash {
		t.Fatal("changed revision did not reconcile")
	}
}

func TestSessionCatalogCancellationConcurrentAndPrivacy(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// Reopening an adopted index also repairs permissive database/WAL modes.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := filepath.Join(home, "cache", "catalog", "sessions.sqlite"+suffix)
		if _, e := os.Stat(path); e == nil {
			if e = os.Chmod(path, 0644); e != nil {
				t.Fatal(e)
			}
		}
	}
	other, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = c.Refresh(canceled, h); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	page, err := c.Query(ctx, CatalogQuery{})
	if err != nil || page.Total != 0 {
		t.Fatal("canceled rebuild committed")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, db := range []*SQLiteSessionCatalog{c, other} {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- db.Refresh(ctx, h) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if err = filepath.WalkDir(filepath.Join(home, "cache", "catalog"), func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		want := os.FileMode(0600)
		if d.IsDir() {
			want = 0700
		}
		if i.Mode().Perm() != want {
			return fmt.Errorf("%s mode=%o", path, i.Mode().Perm())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCatalogCorruptionRebuild(t *testing.T) {
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "cache", "catalog")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "sessions.sqlite"), []byte("damaged index"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(context.Background(), cache, newCountingStore(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	p, err := c.Query(context.Background(), CatalogQuery{})
	if err != nil || p.Total != 0 {
		t.Fatalf("rebuilt=%+v err=%v", p, err)
	}
}

func TestSessionCatalogRefusesDanglingLockSymlinkBeforeCreatingTarget(t *testing.T) {
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "cache", "catalog")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside-catalog")
	if err = os.Symlink(target, filepath.Join(dir, "open.lock")); err != nil {
		t.Fatal(err)
	}
	if catalog, e := OpenSessionCatalog(t.Context(), cache, newCountingStore(), ListOptions{}); e == nil {
		_ = catalog.Close()
		t.Fatal("accepted catalog lock symlink")
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("lock symlink target was created: %v", err)
	}
}

func TestSessionCatalogTypedFiltersEqualExhaustiveOracle(t *testing.T) {
	ctx := context.Background()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, newCountingStore(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err = c.Refresh(ctx, HeaderSnapshot{CanonicalComplete: true}); err != nil {
		t.Fatal(err)
	}
	summaries := []SearchSummary{
		{SessionID: "one", Harness: archive.Harness{Name: "codex"}, CapturedAt: baseTime, ParentSessionID: "parent", Replay: &archive.Replay{}, Models: []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "gpt-test"}}}, SkillsUsed: []archive.SkillUse{{Name: "used", SHA256: "abc"}}},
		{SessionID: "two", Harness: archive.Harness{Name: "claude"}, CapturedAt: baseTime.Add(time.Nanosecond), Parser: archive.ParserInfo{Status: archive.ParserStatusComplete}, SkillsAvailable: []archive.SkillSnapshot{{Name: "available", SHA256: "def"}}},
		{SessionID: "three", Harness: archive.Harness{Name: "codex"}, CapturedAt: baseTime.Add(time.Hour), Parser: archive.ParserInfo{Status: archive.ParserStatusComplete}, CaptureGaps: []archive.CaptureGap{{Code: "gap"}}},
	}
	for _, s := range summaries {
		data, e := json.Marshal(s)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.db.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?,?,?,?,?,?,?,?,?)", s.SessionID, "etag", "hash", catalogTime(s.CapturedAt), catalogTime(s.CapturedAt), data, catalogSearch(s), strings.ToLower(s.SessionID), s.ProjectName == "", catalogRecordChecksum(catalogRecord{key: s.SessionID, etag: "etag", hash: "hash", capture: catalogTime(s.CapturedAt), activity: catalogTime(s.CapturedAt), summary: data, search: catalogSearch(s), lowerID: strings.ToLower(s.SessionID), unlabeled: s.ProjectName == ""})); e != nil {
			t.Fatal(e)
		}
	}
	filters := []Filter{{}, {Harness: "codex"}, {Harness: "missing"}, {From: baseTime.Add(time.Nanosecond), To: baseTime.Add(time.Nanosecond)}, {From: baseTime.Add(24 * time.Hour)}, {Replays: ReplaysOnly}, {Replays: ReplaysHidden}, {Model: "gpt-test"}, {Model: "unknown"}, {Skill: "used"}, {Skill: "available", SkillUsage: SkillUsageAvailable}, {SkillSHA256: "missing"}, {RequireCompleteCoverage: true}}
	for _, f := range filters {
		for _, top := range []bool{false, true} {
			var want []string
			for i := len(summaries) - 1; i >= 0; i-- {
				m := summaries[i].Metadata()
				if matches(m, f) && (!top || m.ParentSessionID == "") {
					want = append(want, m.SessionID)
				}
			}
			q := CatalogQuery{Metadata: MetadataQuery{Filter: f, TopLevelOnly: top, Limit: 1}}
			var got []string
			for {
				page, e := c.Query(ctx, q)
				if e != nil {
					t.Fatal(e)
				}
				if page.Total != len(want) || !page.Complete {
					t.Fatalf("filter=%+v total=%d want=%d", f, page.Total, len(want))
				}
				for _, r := range page.Rows {
					got = append(got, r.Summary.SessionID)
				}
				if page.Next == "" {
					break
				}
				q.Cursor = page.Next
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("filter=%+v top=%t got=%v want=%v", f, top, got, want)
			}
		}
	}
}

type interruptedCatalogStore struct {
	*countingStore
	started chan struct{}
}

func (s *interruptedCatalogStore) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, "", ctx.Err()
}

func TestSessionCatalogCancelColdReadsJoinsWorkers(t *testing.T) {
	store := &interruptedCatalogStore{countingStore: newCountingStore(), started: make(chan struct{}, 1)}
	for i := range 20 {
		putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime)
	}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(context.Background(), cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h, err := DiscoverCatalogHeaders(context.Background(), store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Refresh(ctx, h) }()
	select {
	case <-store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("cold reads did not start")
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("cancel=%v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not join")
	}
	p, err := c.Query(context.Background(), CatalogQuery{})
	if err != nil || p.Total != 0 {
		t.Fatalf("partial rebuild visible=%+v err=%v", p, err)
	}
}

func TestSessionCatalogRejectsRevisionRaceAndBadHash(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	key := putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	body, etag, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := listingindex.NewRevision(key, body, etag)
	if err != nil {
		t.Fatal(err)
	}
	revision.Hash = strings.Repeat("0", 64)
	h.Revisions = map[RevisionID]listingindex.Revision{{Key: key, ETag: etag}: revision}
	if err = c.Refresh(ctx, h); !errors.Is(err, ErrRefreshRequired) {
		t.Fatalf("bad hash=%v", err)
	}
	h.Revisions = nil
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime.Add(time.Hour))
	if err = c.Refresh(ctx, h); !errors.Is(err, ErrRefreshRequired) {
		t.Fatalf("race=%v", err)
	}
	p, err := c.Query(ctx, CatalogQuery{})
	if err != nil || p.Total != 0 {
		t.Fatalf("race committed=%+v err=%v", p, err)
	}
}

func TestSessionCatalogWordsAreUnicodeAndLabelSafeCandidates(t *testing.T) {
	ctx := context.Background()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, newCountingStore(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err = c.Refresh(ctx, HeaderSnapshot{CanonicalComplete: true}); err != nil {
		t.Fatal(err)
	}
	summaries := []SearchSummary{{SessionID: "abcd123456", Name: "ÉCOLE", ProjectName: "published"}, {SessionID: "212abcdef", Title: "different", ProjectName: "published", PullRequests: []archive.PullRequestLink{{Number: 21}}}, {SessionID: "unlabeled", Name: "different"}}
	for _, s := range summaries {
		data, e := json.Marshal(s)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.db.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?,?,?,?,?,?,?,?,?)", s.SessionID, "etag", "hash", catalogTime(baseTime), catalogTime(baseTime), data, catalogSearch(s), strings.ToLower(s.SessionID), s.ProjectName == "", catalogRecordChecksum(catalogRecord{key: s.SessionID, etag: "etag", hash: "hash", capture: catalogTime(baseTime), activity: catalogTime(baseTime), summary: data, search: catalogSearch(s), lowerID: strings.ToLower(s.SessionID), unlabeled: s.ProjectName == ""})); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct {
		word string
		want []string
	}{{"école", []string{"abcd123456", "unlabeled"}}, {"abcd1234", []string{"abcd123456", "unlabeled"}}, {"#21", []string{"212abcdef", "unlabeled"}}, {"#0021", []string{"212abcdef", "unlabeled"}}, {"000021", []string{"212abcdef", "unlabeled"}}, {"212", []string{"212abcdef", "unlabeled"}}, {"configured label", []string{"unlabeled"}}} {
		p, e := c.Query(ctx, CatalogQuery{Words: []string{tc.word}})
		if e != nil {
			t.Fatal(e)
		}
		var ids []string
		for _, r := range p.Rows {
			ids = append(ids, r.Summary.SessionID)
		}
		if !reflect.DeepEqual(ids, tc.want) {
			t.Fatalf("word=%q got=%v want=%v", tc.word, ids, tc.want)
		}
	}
}

func TestSessionCatalogRejectsPartialCanonicalSnapshots(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if !h.CanonicalComplete {
		t.Fatal("discovery omitted proof")
	}
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	for _, partial := range []HeaderSnapshot{{}, {Canonical: h.Canonical}} {
		if err = c.Refresh(ctx, partial); err == nil {
			t.Fatal("accepted unproven canonical snapshot")
		}
	}
	p, err := c.Query(ctx, CatalogQuery{})
	if err != nil || p.Total != 1 || !p.Complete {
		t.Fatalf("partial snapshot altered catalog=%+v err=%v", p, err)
	}
}

func TestSessionCatalogProjectionStoresOnlySearchFields(t *testing.T) {
	m := archive.Metadata{Name: "published native name", Title: "filtered prompt preview", SourceBundle: archive.SourceReference{Key: "private-source-pointer"}, Replay: &archive.Replay{RunID: "private-replay-run"}, CaptureGaps: []archive.CaptureGap{{Code: "gap", Detail: "private-gap-detail"}}, PullRequests: []archive.PullRequestLink{{Number: 21, URL: "private-pr-url", Repository: "private-repository"}}, GitActivity: []archive.GitEvent{{Kind: archive.GitEventCommit, SHA: "private-commit"}, {Kind: archive.GitEventPRCreated, PRNumber: 22, URL: "private-git-url"}}}
	summary := summarize(m)
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-source-pointer", "private-replay-run", "private-gap-detail", "private-pr-url", "private-repository", "private-commit", "private-git-url", "source_bundle", "counts", "history"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("projection retained %q", secret)
		}
	}
	if !strings.Contains(string(data), m.Name) || !strings.Contains(string(data), m.Title) || summary.PullRequests[0].Number != 21 || summary.GitActivity[0].PRNumber != 22 {
		t.Fatal("lost published search fields")
	}
}

// A command must not borrow another handle's authority after its own discovery.
func TestSessionCatalogFirstQueryBindsRefreshedGeneration(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	first, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	other, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	unready, err := other.Query(ctx, CatalogQuery{})
	if err != nil || unready.Complete || unready.Total != 0 {
		t.Fatalf("unrefreshed view=%+v err=%v", unready, err)
	}
	if err = other.Refresh(ctx, HeaderSnapshot{CanonicalComplete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Query(ctx, CatalogQuery{}); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("first query accepted replaced generation: %v", err)
	}
	if err = first.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	page, err := first.Query(ctx, CatalogQuery{})
	if err != nil || !page.Complete || page.Total != 1 {
		t.Fatalf("refreshed view=%+v err=%v", page, err)
	}
}

func TestSessionCatalogLegacyRowsReloadBeforeCompleteness(t *testing.T) {
	ctx := t.Context()
	store := newCountingStore()
	key := putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "cache", "catalog")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "sessions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE sessions (key TEXT PRIMARY KEY, etag TEXT NOT NULL, hash TEXT NOT NULL, capture TEXT NOT NULL, activity TEXT NOT NULL, summary BLOB NOT NULL, search TEXT NOT NULL, lowerid TEXT NOT NULL, unlabeled INTEGER NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	var etag string
	for _, obj := range headers.Canonical {
		if obj.Key == key {
			etag = obj.ETag
		}
	}
	_, err = db.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?,?,?,?,?,?,?,?)", key, etag, "bodyhash", catalogTime(baseTime), catalogTime(baseTime), []byte(`{"SessionID":"untrusted legacy identity"}`), "wrong", "wrong", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reads := 0
	catalog, err := OpenSessionCatalog(ctx, cache, store, ListOptions{BodyRead: func(string, bool) { reads++ }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	page, err := catalog.Query(ctx, CatalogQuery{})
	if err != nil || page.Complete || page.Total != 0 {
		t.Fatalf("legacy authority leaked: %+v %v", page, err)
	}
	if err = catalog.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	page, err = catalog.Query(ctx, CatalogQuery{})
	if err != nil || !page.Complete || reads != 1 || len(page.Rows) != 1 || page.Rows[0].Summary.SessionID != fmt.Sprintf("%032x", 1) {
		t.Fatalf("legacy reload: %+v reads=%d err=%v", page, reads, err)
	}
}

func TestSessionCatalogRejectsSummaryDamageAfterRefresh(t *testing.T) {
	ctx := t.Context()
	store := newCountingStore()
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = catalog.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.db.ExecContext(ctx, "UPDATE sessions SET summary=?", []byte(`{"SessionID":"altered"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Query(ctx, CatalogQuery{}); err == nil {
		t.Fatal("accepted damaged projection")
	}
	if err = catalog.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	page, err := catalog.Query(ctx, CatalogQuery{})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Summary.SessionID != fmt.Sprintf("%032x", 1) {
		t.Fatalf("repair=%+v err=%v", page, err)
	}
}

func TestSessionCatalogRefreshRepairsDerivedSelectionColumns(t *testing.T) {
	ctx := t.Context()
	store := newCountingStore()
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = catalog.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	query := CatalogQuery{Metadata: MetadataQuery{Filter: Filter{From: baseTime}}, Words: []string{"configured label"}}
	before, err := catalog.Query(ctx, query)
	if err != nil || len(before.Rows) != 1 {
		t.Fatalf("fixture %+v err=%v", before, err)
	}
	if _, err = catalog.db.ExecContext(ctx, "UPDATE sessions SET capture='1900',activity='1900',search='missing',lowerid='wrong',unlabeled=0"); err != nil {
		t.Fatal(err)
	}
	if err = catalog.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	after, err := catalog.Query(ctx, query)
	if err != nil || !reflect.DeepEqual(before.Rows, after.Rows) {
		t.Fatalf("derived damage hid canonical row: %+v err=%v", after, err)
	}
}

// Date seeds retain transitive old descendants through the shared reader policy.
func TestSessionCatalogRootChildrenDatesPagingAndFilters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newCountingStore()
	rootID, childID, grandID := fmt.Sprintf("%032x", 1), fmt.Sprintf("%032x", 2), fmt.Sprintf("%032x", 3)
	for i, age := range []time.Duration{0, -24 * time.Hour, -48 * time.Hour, -72 * time.Hour} {
		id := fmt.Sprintf("%032x", i+1)
		key := putSession(t, store, "codex", id, baseTime.Add(age))
		data, err := store.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		var m archive.Metadata
		if err = json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 1:
			m.ParentSessionID = rootID
		case 2:
			m.ParentSessionID = childID
		}
		m.Models = []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "kept"}}}
		if i == 2 {
			m.Models[0].Attributes["gen_ai.request.model"] = "other"
		}
		data, err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		query MetadataQuery
		want  []string
	}{
		{"ordinary dates", MetadataQuery{Filter: Filter{From: baseTime, To: baseTime}}, []string{rootID}},
		{"descendants", MetadataQuery{Filter: Filter{From: baseTime, To: baseTime}, IncludeRootChildren: true}, []string{rootID, childID, grandID}},
		{"model", MetadataQuery{Filter: Filter{From: baseTime, To: baseTime, Model: "kept"}, IncludeRootChildren: true}, []string{rootID, childID}},
		{"harness", MetadataQuery{Filter: Filter{From: baseTime, Harness: "missing"}, IncludeRootChildren: true}, nil},
		{"future", MetadataQuery{Filter: Filter{From: baseTime.Add(time.Hour)}, IncludeRootChildren: true}, nil},
		{"top level", MetadataQuery{Filter: Filter{From: baseTime}, TopLevelOnly: true, IncludeRootChildren: true}, []string{rootID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := CatalogQuery{Metadata: MetadataQuery{Filter: tc.query.Filter, Limit: 1, Order: tc.query.Order, TopLevelOnly: tc.query.TopLevelOnly, IncludeRootChildren: tc.query.IncludeRootChildren}}
			var got []string
			for {
				page, e := c.Query(ctx, q)
				if e != nil {
					t.Fatal(e)
				}
				if !page.Complete || page.Total != len(tc.want) {
					t.Fatalf("page=%+v want=%v", page, tc.want)
				}
				for _, row := range page.Rows {
					got = append(got, row.Summary.SessionID)
				}
				if page.Next == "" {
					break
				}
				changed := q
				changed.Cursor = page.Next
				changed.Metadata.IncludeRootChildren = !changed.Metadata.IncludeRootChildren
				if _, e = c.Query(ctx, changed); !errors.Is(e, ErrStaleCatalogCursor) {
					t.Fatalf("flag cursor=%v", e)
				}
				q.Cursor = page.Next
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestSessionCatalogKeepsUnresolvedNativeChildOwnership(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newCountingStore()
	childID := fmt.Sprintf("%032x", 1)
	key := putSession(t, store, "codex", childID, baseTime.Add(time.Hour))
	body, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err = json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	m.NativeChild = true
	body, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(ctx, key, body); err != nil {
		t.Fatal(err)
	}
	rootID := fmt.Sprintf("%032x", 2)
	putSession(t, store, "codex", rootID, baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for range 2 {
		headers, e := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
		if e != nil {
			t.Fatal(e)
		}
		if e = c.Refresh(ctx, headers); e != nil {
			t.Fatal(e)
		}
		all, e := c.Query(ctx, CatalogQuery{})
		if e != nil || len(all.Rows) != 2 {
			t.Fatalf("all=%+v err=%v", all, e)
		}
		projected := all.Rows[0].Summary.Metadata()
		if !projected.IsChild() || projected.ParentSessionID != "" {
			t.Errorf("unresolved child ownership lost: %+v", projected)
		}
		for _, query := range []MetadataQuery{{TopLevelOnly: true, Limit: 1}, {TopLevelOnly: true, Limit: 1, IncludeRootChildren: true, Filter: Filter{From: baseTime}}} {
			page, e := c.Query(ctx, CatalogQuery{Metadata: query})
			if e != nil || page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].Summary.SessionID != rootID || page.Next != "" {
				t.Errorf("top-level=%+v err=%v", page, e)
			}
		}
	}
	m.ParentSessionID = rootID
	body, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(ctx, key, body); err != nil {
		t.Fatal(err)
	}
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	page, err := c.Query(ctx, CatalogQuery{})
	if err != nil || len(page.Rows) != 2 {
		t.Fatalf("linked page=%+v err=%v", page, err)
	}
	linked := page.Rows[0].Summary.Metadata()
	if !linked.NativeChild || linked.ParentSessionID != rootID {
		t.Errorf("linked ownership=%+v", linked)
	}
	top, err := c.Query(ctx, CatalogQuery{Metadata: MetadataQuery{TopLevelOnly: true, Limit: 1}})
	if err != nil || top.Total != 1 || top.Rows[0].Summary.SessionID != rootID {
		t.Errorf("linked top=%+v err=%v", top, err)
	}
}

// Persist the prior checksum format literally; calling the production helper
// here would silently upgrade the fixture along with the implementation.
func priorCatalogChecksum(r catalogRecord) string {
	var data []byte
	for _, field := range []string{r.key, r.etag, r.hash, r.capture, r.activity, r.search, r.lowerID} {
		data = binary.AppendUvarint(data, uint64(len(field)))
		data = append(data, field...)
	}
	data = binary.AppendUvarint(data, uint64(len(r.summary)))
	data = append(data, r.summary...)
	if r.unlabeled {
		data = append(data, 1)
	} else {
		data = append(data, 0)
	}
	return storage.SHA256Hex(data)
}

func TestSessionCatalogReloadsPriorOwnershipProjection(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newCountingStore()
	key := putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	body, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err = json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	m.NativeChild = true
	body, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(ctx, key, body); err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", fmt.Sprintf("%032x", 2), baseTime.Add(-time.Hour))
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{BodyRead: func(string, bool) { reads++ }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	first, err := c.Query(ctx, CatalogQuery{Metadata: MetadataQuery{Limit: 1}})
	if err != nil || first.Next == "" {
		t.Fatalf("page=%+v err=%v", first, err)
	}
	var record catalogRecord
	if err = record.scan(c.db.QueryRowContext(ctx, "SELECT key,etag,hash,capture,activity,summary,search,lowerid,unlabeled,summary_hash FROM sessions WHERE key=?", key)); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(record.summary, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "NativeChild")
	record.summary, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.db.ExecContext(ctx, "UPDATE sessions SET summary=?,summary_hash=? WHERE key=?", record.summary, priorCatalogChecksum(record), key); err != nil {
		t.Fatal(err)
	}
	reads = 0
	if err = c.Refresh(ctx, HeaderSnapshot{}); err == nil {
		t.Fatal("upgraded without complete canonical authority")
	}
	if err = c.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	page, err := c.Query(ctx, CatalogQuery{})
	if err != nil || len(page.Rows) != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	projected := page.Rows[0].Summary.Metadata()
	if !projected.IsChild() || reads != 1 {
		t.Errorf("prior tuple was not reloaded: child=%t bodies=%d", projected.IsChild(), reads)
	}
	if _, err = c.Query(ctx, CatalogQuery{Metadata: MetadataQuery{Limit: 1}, Cursor: first.Next}); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Errorf("upgrade cursor=%v", err)
	}
	reads = 0
	if err = c.Refresh(ctx, headers); err != nil || reads != 0 {
		t.Fatalf("warm refresh err=%v bodies=%d", err, reads)
	}
}
