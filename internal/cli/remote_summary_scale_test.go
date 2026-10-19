package cli

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// This parent owns the process-wide trace recorder and therefore runs serially.
// Bulk fixture construction is shared with the independently checked picker
// qualification fixture; N individual catalog commits are never setup work.
func TestRemoteSummaryDeltaScale(t *testing.T) {
	sizes := []int{1800}
	if os.Getenv("AGENT_ARCHIVE_PERF") == "1" {
		sizes = append(sizes, 10000, 25000, 50000)
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("sessions=%d", size), func(t *testing.T) {
			fixture := newQualificationFixture(t, qualificationMetadata(size, 0))
			meter := &qualificationMeter{MeasuredStore: storagetest.NewMeasuredStore(fixture.store, 0)}
			cache, err := reader.OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var cold qualificationReads
			for _, phase := range []string{"cold", "unchanged", "small-delta"} {
				if phase == "small-delta" {
					key, err := archive.MetadataObjectKey(fixture.metadata[1].Harness.Name, fixture.metadata[1].SessionID)
					if err != nil {
						t.Fatal(err)
					}
					if err = fixture.store.DeleteSession(t.Context(), key); err != nil {
						t.Fatal(err)
					}
				}
				meter.Reset()
				meter.mu.Lock()
				meter.reads = qualificationReads{}
				meter.mu.Unlock()
				disable := trace.Enable()
				started := time.Now()
				got, err := reader.CatalogSummariesWithOptions(t.Context(), meter, reader.Filter{Replays: reader.ReplaysIncluded}, reader.ListOptions{Cache: cache})
				elapsed := time.Since(started)
				var accounting bytes.Buffer
				trace.Write(&accounting)
				disable()
				if err != nil {
					t.Fatal(phase, err)
				}
				reads := meter.snapshot()
				want := size
				if phase == "small-delta" {
					want--
				}
				if len(got) != want || reads.Lists != 0 || reads.Metadata != 0 {
					t.Fatalf("%s lost complete summaries or fetched full bodies/LIST: rows=%d want=%d reads=%+v", phase, len(got), want, reads)
				}
				if phase == "cold" {
					cold = reads
				} else if reads.Nodes >= cold.Nodes {
					t.Fatalf("%s did not reduce remote tree reads: cold=%+v current=%+v", phase, cold, reads)
				}
				t.Logf("%s size=%d elapsed=%s reads=%+v; complete local validation remains O(N)\n%s", phase, size, elapsed, reads, accounting.String())
				// Compare both warm states against the complete remote oracle,
				// outside measured reads and trace accounting.
				wantRows, err := reader.CatalogSummaries(t.Context(), fixture.store, reader.Filter{Replays: reader.ReplaysIncluded})
				if err != nil || !reflect.DeepEqual(got, wantRows) {
					t.Fatal(phase, "complete summary oracle differs", err)
				}
			}
			// Prefix and title candidates share the same delta-backed reader.
			for _, word := range []string{"00000000", "Q000002"} {
				c, err := reader.OpenRemoteSessionCatalog(t.Context(), meter, reader.ListOptions{Cache: cache})
				if err != nil || c == nil {
					t.Fatal("open warm candidate catalog", err)
				}
				page, err := c.Query(t.Context(), reader.CatalogQuery{Words: []string{word}, Metadata: reader.MetadataQuery{Filter: reader.Filter{Replays: reader.ReplaysIncluded}}})
				_ = c.Close()
				if err != nil || !page.Complete || page.Next != "" {
					t.Fatal("complete warm candidate query", err)
				}
				for _, metadata := range fixture.metadata {
					if metadata.SessionID == fixture.metadata[1].SessionID || !strings.HasPrefix(strings.ToLower(metadata.SessionID), strings.ToLower(word)) && !strings.Contains(strings.ToLower(metadata.Title), strings.ToLower(word)) {
						continue
					}
					found := false
					for _, row := range page.Rows {
						found = found || row.Summary.SessionID == metadata.SessionID
					}
					if !found {
						t.Fatal("warm candidate query excluded an oracle match")
					}
				}
			}
			if meter.snapshot().Lists != 0 {
				t.Fatal("warm prefix/text candidates issued canonical LIST")
			}
		})
	}
}
