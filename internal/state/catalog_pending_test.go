package state

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCatalogPendingCommitRoundTripAndMalformedRecovery(t *testing.T) {
	local := newTestStore(t)
	source := []byte("private source")
	pending := PendingPublication{Catalog: &CatalogPublication{Protocol: 10, ID: "frozen", ExpectedRevision: "previous"}, SourceKey: "source", MetadataKey: "metadata", SourceSHA256: durableRef(source).SHA256, SourceBytes: source, MetadataBytes: []byte(`{}`)}
	if err := local.SavePending("catalog", pending); err != nil {
		t.Fatal(err)
	}
	got, found, err := local.LoadPending("catalog")
	if err != nil || !found || got.Catalog == nil || *got.Catalog != *pending.Catalog {
		t.Fatal("commit roundtrip failed", err)
	}
	raw, err := os.ReadFile(local.pendingPath("catalog"))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	// The commit spelling is intentionally an old durable writer refusal field.
	if wire["commit"] == nil || wire["catalog"] != nil {
		t.Fatal("old writer compatibility fence missing")
	}
	for _, foreign := range []string{`{}`, `null`, `{"id":"frozen"}`, `{"id":"frozen","expected_revision":null}`, `{"id":"frozen","phase":"future"}`, `{"id":1}`} {
		t.Run(foreign, func(t *testing.T) {
			wire["commit"] = json.RawMessage(foreign)
			bad, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var decoded PendingPublication
			if err = json.Unmarshal(bad, &decoded); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatal("foreign commit accepted", foreign, err)
			}
		})
	}
	tooLong := pending
	tooLong.Catalog = &CatalogPublication{ID: "frozen", ExpectedRevision: strings.Repeat("x", 129)}
	if err = local.SavePending("catalog", tooLong); err == nil {
		t.Fatal("unreadable expected revision persisted")
	}
	var descriptor PendingPublication
	if err = json.Unmarshal([]byte(`{"commit":{"protocol":10,"id":"frozen","expected_revision":""}}`), &descriptor); err != nil || descriptor.Catalog == nil {
		t.Fatal("explicit creation revision refused", err)
	}
	for _, protocol := range []string{"", `"protocol":8,`, `"protocol":9,`} {
		if err = json.Unmarshal([]byte(`{"commit":{`+protocol+`"id":"frozen","expected_revision":""}}`), &descriptor); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("older publication admission accepted", err)
		}
	}
	if err = json.Unmarshal([]byte(`{}`), &descriptor); err != nil || descriptor.Catalog != nil {
		t.Fatal("legacy omitted commit retained catalog authority", err)
	}
}
