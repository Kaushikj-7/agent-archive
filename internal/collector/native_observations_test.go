package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

func TestNativeCheckpointKeepsObservationsSeparateFromRawProofAndPricing(t *testing.T) {
	t.Parallel()
	scan, reader, _ := nativeCollectorFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-own-observations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("/work/synthetic-muse"), []byte(scan.reg.ProjectRoot))
	if err = os.WriteFile(reader.ref.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	filtered, observed, err := reader.Filter(t.Context(), muse.Filter{}, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if observed.prepared == nil || observed.prepared.closed {
		t.Fatal("missing live prepared proof")
	}
	if err = scan.persistNativeValidation(observed.prepared); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	checkpoint := scan.reg.NativeBinding.Checkpoint
	if !observed.prepared.closed || checkpoint.Boundary != int64(len(raw)) || checkpoint.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("checkpoint does not cover exact observation-bearing native bytes")
	}
	bundle, err := archive.NewSourceBundle(scan.reg, muse.Filter{}, filtered, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(reader.ref.Path); err != nil {
		t.Fatal(err)
	}
	analysis, err := (muse.Parser{}).Parse(t.Context(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	summary := analysis.TokenObservations
	if !summary.HasObservations() || *summary.Input != 202 || *summary.Output != 46 || summary.Coverage.Available() || summary.Composition.Available() || summary.Pricing.Available() {
		t.Fatal("post-loss observations changed accounting authority")
	}
	refiltered, err := (muse.Filter{}).Refilter(t.Context(), bundle, time.Time{})
	if err != nil || !reflect.DeepEqual(filtered.Records, refiltered.Records) || !(muse.Filter{}).EvidenceExtends(bundle, bundle) {
		t.Fatal("post-loss retained validation disagrees", err)
	}
	parser := archive.ParserInfo{Name: "muse", Version: (muse.Parser{}).Version()}
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, nil, "synthetic", scan.reg.SessionStartedAt, time.Now(), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, parser)
	if err != nil {
		t.Fatal(err)
	}
	export, err := archive.BuildEvalExportWithAnalysis(bundle, analysis, metadata, archive.EvalExportSourceArchive, archive.EvalExportDetailFull, parser)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := archive.BuildHandoffWithAnalysis(bundle, analysis, &metadata, archive.HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	computed := stats.Compute([]archive.Metadata{metadata}, stats.Options{Now: metadata.CapturedAt, Days: 1, Location: time.UTC})
	if !reflect.DeepEqual(summary, metadata.TokenObservations) || !reflect.DeepEqual(summary, export.TokenObservations) || !reflect.DeepEqual(summary, handoff.Session.TokenObservations) || !reflect.DeepEqual(summary, computed.TokenObservations) || computed.Overview.Cost.Value != nil || computed.Composition != nil || computed.Coverage.ObservationOnlySessions != 1 || metadata.Counts.InputTokens != nil || len(metadata.ModelTokens) != 0 {
		t.Fatal("combined consumers lost observations or enabled pricing")
	}
	for _, value := range []any{bundle, metadata, export, handoff, computed} {
		portable, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(portable, []byte(checkpoint.SHA256)) || bytes.Contains(portable, []byte("muse-ordinary")) {
			t.Fatal("raw checkpoint escaped local registration")
		}
	}
}
