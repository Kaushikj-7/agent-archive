package reader

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRemoteSelectedReadsUseOrderedEightWorkersAndWarmCache(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 50)
	measured := storagetest.NewMeasuredStore(remote.ObjectStore, 10*time.Millisecond)
	wrapped, err := catalog.Wrap(measured)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.OpenSnapshot(t.Context(), wrapped, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := snapshot.Query(t.Context(), catalog.Query{Index: catalog.IdentityIndex}, "", 50)
	if err != nil || len(page.Rows) != 50 {
		t.Fatal("selected fixture", err)
	}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, row := range page.Rows {
		want = append(want, row.Key)
	}
	for _, warm := range []bool{false, true} {
		measured.Reset()
		var seen []string
		progress := 0
		started := time.Now()
		metadata, err := hydrateCatalogRows(t.Context(), snapshot, page.Rows, ListOptions{Cache: cache,
			Progress: func(done, total int) {
				progress++
				if done != progress || total != 50 {
					t.Error("progress was not serial")
				}
			},
			BodyRead: func(key string, cached bool) {
				if progress != 50 || cached != warm {
					t.Error("body observer ran before join or reported wrong cache authority")
				}
				seen = append(seen, key)
			}})
		if err != nil || len(metadata) != 50 || !reflect.DeepEqual(seen, want) {
			t.Fatal("selected output/observer order changed", err)
		}
		metrics := measured.Metrics()
		if metrics.Lists != 0 || (!warm && (metrics.Gets != 50 || metrics.PeakReads != 8)) || (warm && (metrics.Gets != 0 || metrics.PeakReads != 0)) {
			t.Fatalf("warm=%v metrics=%+v", warm, metrics)
		}
		t.Logf("private delayed hydration warm=%v elapsed=%s GET=%d LIST=%d peak=%d", warm, time.Since(started), metrics.Gets, metrics.Lists, metrics.PeakReads)
	}
	measured.Reset()
	ctx, cancel := context.WithCancel(t.Context())
	_, err = hydrateCatalogRows(ctx, snapshot, page.Rows, ListOptions{Cache: cache, Progress: func(done, total int) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("warm joined selection ignored cancellation", err)
	}
	// Reset panics if any worker is still reading: cancellation must join.
	if measured.Metrics().Gets != 0 {
		t.Fatal("canceled warm selection read remote bodies")
	}
	measured.Reset()
}
