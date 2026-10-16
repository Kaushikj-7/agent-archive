package archive

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestObservationBoundsCloneAndWireSizing(t *testing.T) {
	t.Parallel()
	var forward, reverse ObservationAccumulator
	for i := range 50 {
		if forward.Add(fmt.Sprintf("model-%02d", i), 1, 2) != nil || reverse.Add(fmt.Sprintf("model-%02d", 49-i), 1, 2) != nil {
			t.Fatal("aggregate")
		}
	}
	s := forward.Summary()
	if !reflect.DeepEqual(s, reverse.Summary()) || len(s.Models) != MaxModelTokens || *s.Input != 50 || *s.Output != 100 {
		t.Fatal("bounded deterministic models")
	}
	c := CloneTokenObservations(s)
	*c.Input = 999
	c.Models[0].Model = "changed"
	if *s.Input != 50 || s.Models[0].Model == "changed" {
		t.Fatal("shared summary ownership")
	}
	m := Metadata{TokenObservations: s}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	n, err := agentmeta.JSONWireBound(t.Context(), m, 1<<20)
	if err != nil || n < int64(len(raw)) {
		t.Fatal("wire bound", n, err)
	}
	if _, err := agentmeta.JSONWireBound(t.Context(), m, n-1); err == nil {
		t.Fatal("wire limit ignored")
	}
	derived := assembleAnalyzedMetadata(SourceBundle{}, Analysis{TokenObservations: s}, Metadata{})
	*derived.TokenObservations.Input = 1000
	if *s.Input != 50 {
		t.Fatal("metadata aliases Analysis")
	}
	e := EvalExportFromMetadata(m, EvalExportSourceArchive)
	*e.TokenObservations.Input = 2000
	if *m.TokenObservations.Input != 50 {
		t.Fatal("eval aliases metadata")
	}
}

func TestFittingKeepsObservationAvailabilityAndOwnsSummary(t *testing.T) {
	t.Parallel()
	var a ObservationAccumulator
	if a.Add("known", 0, 0) != nil {
		t.Fatal("synthetic")
	}
	h := Handoff{Version: HandoffVersion, Session: HandoffSession{Harness: "muse", TokenObservations: a.Summary()}, Exchanges: []HandoffExchange{}}
	fitted, _ := FitHandoff(h, 1, func(h Handoff) int { return len(RenderHandoffMarkdown(h, HandoffRenderOptions{})) })
	if !reflect.DeepEqual(h.Session.TokenObservations, fitted.Session.TokenObservations) {
		t.Fatal("fit stripped availability")
	}
	*fitted.Session.TokenObservations.Input = 1
	if *h.Session.TokenObservations.Input != 0 {
		t.Fatal("fit aliased summary")
	}
	e := EvalExport{TokenObservations: a.Summary()}
	f := FitEvalExport(e, 1)
	if !reflect.DeepEqual(e.TokenObservations, f.TokenObservations) {
		t.Fatal("eval fit stripped availability")
	}
	*f.TokenObservations.Input = 1
	if *e.TokenObservations.Input != 0 {
		t.Fatal("eval fit aliased summary")
	}
}
