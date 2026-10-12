package state

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func readySelectionFixture(t *testing.T) (*Store, PendingPublication) {
	t.Helper()
	s := newTestStore(t)
	p, _ := exactLegacyPendingFixture(t, s)
	if err := s.RemovePending(p.Bundle.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	if err = config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	reg := registration(t)
	reg.ArchiveSessionID = p.Bundle.ArchiveSessionID
	reg.NativeSessionID = p.Bundle.NativeSessionID
	reg.ProjectID = p.Bundle.ProjectID
	reg.Harness = p.Bundle.Capture.Harness
	reg.DestinationID = cfg.DestinationID()
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Adapter = archive.AdapterInfo{Name: p.Bundle.Capture.AdapterName, Version: p.Bundle.Capture.AdapterVersion}
	p.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	p.SkillEvidence = string(cfg.EffectiveSkillEvidence())
	p, err = s.FreezeReadySelection(context.Background(), p.Bundle.ArchiveSessionID, p, "absent", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestReadySelectionActualPendingAndPublishedRoundTrip(t *testing.T) {
	s, p := readySelectionFixture(t)
	id := p.Bundle.ArchiveSessionID
	p.Catalog = &CatalogPublication{Protocol: 10, ID: "synthetic-mutation", ExpectedRevision: ""}
	if err := s.SavePending(id, p); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := s.LoadPending(id)
	if err != nil || !found {
		t.Fatalf("load: %v %v", found, err)
	}
	if loaded.Selection == nil || *loaded.Selection != *p.Selection || loaded.Catalog == nil || loaded.Catalog.ID != p.Catalog.ID {
		t.Fatal("distinct authorities lost")
	}
	before, err := s.CatalogJournalDigest(id, loaded)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Attempted = !loaded.Attempted
	after, err := s.CatalogJournalDigest(id, loaded)
	if err != nil || before != after {
		t.Fatalf("attempt changed frozen digest: %v", err)
	}
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveReadyPublication(p, p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	published, err = s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if published.state.PublicationVersion != 2 || published.state.LastPublished.Selection == nil || *published.state.LastPublished.Selection != *p.Selection {
		t.Fatal("published selecting proof lost")
	}
	if err = published.Save(p.Bundle, p.ReadyAt, CacheStatusRateLimited); err != nil {
		t.Fatal(err)
	}
	if published.state.LastPublished.Selection == nil {
		t.Fatal("deferred candidate dropped selecting proof")
	}
}

func TestReadySelectionMutationsRetainExactEvidence(t *testing.T) {
	s, p := readySelectionFixture(t)
	id := p.Bundle.ArchiveSessionID
	if err := s.SavePending(id, p); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.pendingPath(id))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PendingPublication){
		"metadata":     func(q *PendingPublication) { q.MetadataBytes = append(bytes.Clone(q.MetadataBytes), ' ') },
		"source":       func(q *PendingPublication) { q.SourceBytes = []byte("different source") },
		"registration": func(q *PendingPublication) { q.Bundle.ParentSessionID = "foreign" },
		"policy":       func(q *PendingPublication) { q.SkillEvidence = "none" },
		"predecessor":  func(q *PendingPublication) { q.Selection.Predecessor = "unknown" },
		"destination":  func(q *PendingPublication) { q.Selection.DestinationID = "foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			q := p
			proof := *p.Selection
			q.Selection = &proof
			mutate(&q)
			if s.SavePending(id, q) == nil {
				t.Fatal("mutated authority accepted")
			}
			got, e := os.ReadFile(s.pendingPath(id))
			if e != nil || !bytes.Equal(got, raw) {
				t.Fatal("failed save changed evidence")
			}
		})
	}
}

func TestReadySelectionWireRefusesAmbiguousAndUnversionedAuthority(t *testing.T) {
	_, p := readySelectionFixture(t)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, version := range []json.RawMessage{nil, []byte(`null`), []byte(`1`)} {
		copyFields := map[string]json.RawMessage{}
		for k, v := range fields {
			copyFields[k] = v
		}
		if version == nil {
			delete(copyFields, "journal_version")
		} else {
			copyFields["journal_version"] = version
		}
		b, e := json.Marshal(copyFields)
		if e != nil {
			t.Fatal(e)
		}
		if json.Unmarshal(b, &PendingPublication{}) == nil {
			t.Fatal("selection without exact version accepted")
		}
	}
	duplicate := append([]byte(`{"journal_version":2,`), raw[1:]...)
	if json.Unmarshal(duplicate, &PendingPublication{}) == nil {
		t.Fatal("duplicate authority accepted")
	}
	if json.Unmarshal([]byte(`{"commit":{"version":2,"purpose":"capture"}}`), &PendingPublication{}) == nil {
		t.Fatal("old selecting commit became catalog ownership")
	}
}

func TestReadySelectionRecomputedProofCannotReplacePendingOwner(t *testing.T) {
	s, p := readySelectionFixture(t)
	id := p.Bundle.ArchiveSessionID
	if err := s.SavePending(id, p); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.pendingPath(id))
	if err != nil {
		t.Fatal(err)
	}
	replacement := *p.Selection
	replacement.Predecessor = "unknown"
	replacement.SHA256 = ""
	replacement.SHA256, err = s.readyDigest(t.Context(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	p.Selection = &replacement
	if err = s.ValidateReadySelection(t.Context(), p); err != nil {
		t.Fatalf("control replacement is not internally valid: %v", err)
	}
	if err = s.SavePending(id, p); err == nil {
		t.Fatal("internally valid replacement changed durable owner")
	}
	got, err := os.ReadFile(s.pendingPath(id))
	if err != nil || !bytes.Equal(raw, got) {
		t.Fatal("replacement changed pending evidence")
	}
}
