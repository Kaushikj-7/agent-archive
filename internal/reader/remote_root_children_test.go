package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The recent root's descendants predate the window. The root also needs an
// overflow envelope, proving date selection does not need its body to seed.
func rootRemoteFixture(t *testing.T, count int) (*catalog.Store, *storagetest.MemoryStore, time.Time) {
	t.Helper()
	remote, legacy := remoteReaderFixture(t, count)
	writer, err := catalog.New(remote)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := baseTime.Add(time.Duration(count+10) * time.Minute)
	for i := range 3 {
		id := fmt.Sprintf("session-%04d", i)
		key, err := archive.MetadataObjectKey("claude", id)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := legacy.Get(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		var metadata archive.Metadata
		if err = json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		metadata.ParentSessionID = ""
		if i == 0 {
			metadata.CapturedAt = cutoff.Add(time.Hour)
			for n := range 2000 {
				metadata.LinkedSessions = append(metadata.LinkedSessions, archive.LinkedSessionReference{SessionID: fmt.Sprintf("linked-%04d", n), Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: metadata.CapturedAt})
			}
		} else {
			metadata.ParentSessionID = fmt.Sprintf("session-%04d", i-1)
			metadata.CapturedAt = baseTime.Add(-time.Duration(i) * time.Hour)
		}
		raw, err = json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err = legacy.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
		ref, err := writer.PutImmutable(t.Context(), catalog.KindMetadata, raw)
		if err != nil {
			t.Fatal(err)
		}
		_, revision, err := writer.Find(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		_, err = writer.Commit(t.Context(), catalog.CatalogMutation{ID: "root-window/" + id, SessionKey: key, ExpectedRevision: revision, Next: &catalog.CatalogEntry{Metadata: ref, Summary: metadata}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return remote, legacy, cutoff
}

func TestRemoteRootChildrenUsesFullAuthorityAndLazyEmptyWindow(t *testing.T) {
	remote, legacy, cutoff := rootRemoteFixture(t, 16)
	measured := storagetest.NewMeasuredStore(remote, 0)
	query := MetadataQuery{Filter: Filter{From: cutoff}, IncludeRootChildren: true}
	var downloads int
	opts := ListOptions{BodyRead: func(_ string, cached bool) {
		if !cached {
			downloads++
		}
	}}
	got, err := SelectMetadata(t.Context(), measured, "sessions", query, opts)
	if err != nil {
		t.Fatal(err)
	}
	want, err := SelectMetadata(t.Context(), legacy, "sessions", query, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Sessions, want.Sessions) || len(got.Sessions) < 3 || len(got.Sessions[0].LinkedSessions) != 2000 || downloads != len(got.Sessions) {
		t.Fatalf("root accounting/body authority differs: selected=%d downloaded=%d", len(got.Sessions), downloads)
	}
	if measured.Metrics().Lists != 0 {
		t.Fatal("catalog used canonical LIST")
	}
	// No-cache future windows must not fetch even the root's overflow body.
	query.Filter.From = cutoff.Add(24 * time.Hour)
	downloads = 0
	measured.Reset()
	got, err = SelectMetadata(t.Context(), measured, "sessions", query, opts)
	if err != nil || len(got.Sessions) != 0 || downloads != 0 || measured.Metrics().Lists != 0 {
		t.Fatalf("future selection read bodies: %+v downloads=%d err=%v", measured.Metrics(), downloads, err)
	}
}

func TestRemoteRootChildrenReusesOnlyCompleteSameRootSQL(t *testing.T) {
	for _, count := range []int{128, 512} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			remote, legacy, cutoff := rootRemoteFixture(t, count)
			measured := storagetest.NewMeasuredStore(remote, 0)
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			query := MetadataQuery{Filter: Filter{From: cutoff}, IncludeRootChildren: true}
			var downloaded int
			opts := ListOptions{Cache: cache, BodyRead: func(_ string, cached bool) {
				if !cached {
					downloaded++
				}
			}}
			if _, err = SelectMetadata(t.Context(), measured, "sessions", query, opts); err != nil {
				t.Fatal(err)
			}
			local, err := OpenSessionCatalog(t.Context(), cache, measured, opts)
			if err != nil {
				t.Fatal(err)
			}
			if err = local.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = local.Close(); err != nil {
				t.Fatal(err)
			}
			measured.Reset()
			downloaded = 0
			got, err := SelectMetadata(t.Context(), measured, "sessions", query, opts)
			if err != nil {
				t.Fatal(err)
			}
			want, err := SelectMetadata(t.Context(), legacy, "sessions", query, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			metrics := measured.Metrics()
			if !reflect.DeepEqual(got.Sessions, want.Sessions) || downloaded != 0 || metrics.Lists != 0 || metrics.Gets > 16 {
				t.Fatalf("warm same-root oracle/bounds: %+v downloaded=%d", metrics, downloaded)
			}
			t.Logf("private warm stats universe N=%d GET=%d LIST=%d bodyDownloads=%d bytes=%d", count, metrics.Gets, metrics.Lists, downloaded, metrics.Bytes)
		})
	}
}

func TestRemoteRootChildrenDoesNotRepairCorruptOrBootstrapMissingSQL(t *testing.T) {
	remote, _, cutoff := rootRemoteFixture(t, 16)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(cache.dir), "catalog", "sessions.sqlite")
	query := MetadataQuery{Filter: Filter{From: cutoff.Add(24 * time.Hour)}, IncludeRootChildren: true}
	var bodies int
	opts := ListOptions{Cache: cache, BodyRead: func(string, bool) { bodies++ }}
	got, err := SelectMetadata(t.Context(), remote, "sessions", query, opts)
	if err != nil || len(got.Sessions) != 0 || bodies != 0 {
		t.Fatal("cold empty window", err, bodies)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cold stats bootstrapped SQL", err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	damaged := []byte("private damaged disposable SQLite bytes")
	if err = os.WriteFile(path, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	got, err = SelectMetadata(t.Context(), remote, "sessions", query, opts)
	if err != nil || len(got.Sessions) != 0 || bodies != 0 {
		t.Fatal("corrupt cache fallback", err, bodies)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, damaged) {
		t.Fatal("reuse probe retired or repaired corrupt database", err)
	}
}

func TestRemoteRootChildrenCanceledRequestHasNoReads(t *testing.T) {
	remote, _, cutoff := rootRemoteFixture(t, 3)
	measured := storagetest.NewMeasuredStore(remote, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := SelectMetadata(ctx, measured, "sessions", MetadataQuery{Filter: Filter{From: cutoff}, IncludeRootChildren: true}, ListOptions{})
	if !errors.Is(err, context.Canceled) || measured.Metrics().Gets != 0 || measured.Metrics().Lists != 0 {
		t.Fatal("canceled root query attempted discovery", err, measured.Metrics())
	}
}

func TestRemoteRootChildrenChangedRootFallsBackWithoutSQLRebuild(t *testing.T) {
	original, _, _ := rootRemoteFixture(t, 16)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := ListOptions{Cache: cache}
	local, err := OpenSessionCatalog(t.Context(), cache, original, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err = local.db.QueryRowContext(t.Context(), "SELECT root FROM remote_root WHERE id=1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = local.Close(); err != nil {
		t.Fatal(err)
	}
	changed, legacy, cutoff := rootRemoteFixture(t, 17)
	measured := storagetest.NewMeasuredStore(changed, 0)
	query := MetadataQuery{Filter: Filter{From: cutoff}, IncludeRootChildren: true}
	got, err := SelectMetadata(t.Context(), measured, "sessions", query, opts)
	if err != nil {
		t.Fatal(err)
	}
	want, err := SelectMetadata(t.Context(), legacy, "sessions", query, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Sessions, want.Sessions) || measured.Metrics().Lists != 0 {
		t.Fatal("changed root fallback authority differs", measured.Metrics())
	}
	local, err = openExistingRootCatalog(t.Context(), changed, opts)
	if err != nil || local == nil {
		t.Fatal(err)
	}
	defer func() {
		if err := local.Close(); err != nil {
			t.Error(err)
		}
	}()
	var after []byte
	if err = local.db.QueryRowContext(t.Context(), "SELECT root FROM remote_root WHERE id=1").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("stats rebuilt an unrelated prior-root SQL universe")
	}
}
