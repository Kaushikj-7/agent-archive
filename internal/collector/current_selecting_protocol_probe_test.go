package collector

// This probe uses only APIs present on exact pre-feature main+460. ROOT can
// copy the same bytes into isolated baseline/fixed collector packages. It must
// not be mistaken for a native/provider/CAS qualification.
import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestCurrentOrdinarySelectingProtocolProbe(t *testing.T) {
	local := newTestStore(t)
	cfg, _, err := config.Load(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["schema_version"] = json.RawMessage(`{"version":12,"writer":"publication-composition-v12"}`)
	fields["durable_storage_protection"] = json.RawMessage(`true`)
	fields["publication_composition_protection"] = json.RawMessage(`true`)
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("exact selecting writer is unsupported: %v", err)
	}
	if err = config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript)
	if err = local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) == 0 || len(result.Published) != 0 {
		t.Fatalf("failed metadata acknowledged: %+v %v", result, err)
	}
	pending, found, err := local.LoadPending("session-1")
	if err != nil || !found {
		t.Fatalf("missing exact pending: %v", err)
	}
	exact := bytes.Clone(pending.MetadataBytes)
	raw, err = json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	var journal struct {
		Version   int             `json:"journal_version"`
		Selection json.RawMessage `json:"selection_commit"`
	}
	if err = json.Unmarshal(raw, &journal); err != nil || journal.Version != 2 || len(journal.Selection) == 0 {
		t.Fatalf("actual producer lacks mandatory selecting authority: %v", err)
	}
	retained := bytes.Clone(journal.Selection)
	local, err = openTestStore(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err = local.LoadPending("session-1")
	if err != nil || !found {
		t.Fatal("restart lost journal", err)
	}
	raw, err = json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &journal); err != nil || !bytes.Equal(journal.Selection, retained) || !bytes.Equal(pending.MetadataBytes, exact) {
		t.Fatal("restart changed authority", err)
	}
	remote.failMetadata = false
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	raw, err = remote.Get(t.Context(), pending.MetadataKey)
	if err != nil || !bytes.Equal(raw, exact) {
		t.Fatal("selected remote bytes changed", err)
	}
}
