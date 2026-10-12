package state

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"maps"
	"os"
	"strings"
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
		maps.Copy(copyFields, fields)
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

func TestReadySelectionCaseAliasAuthorityRefusal(t *testing.T) {
	_, p := readySelectionFixture(t)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"JOURNAL_VERSION", `journal_versi\u006fn`} {
		duplicate := append([]byte(`{"`+key+`":2,`), raw[1:]...)
		if json.Unmarshal(duplicate, &PendingPublication{}) == nil {
			t.Fatal("case/escaped authority duplicate accepted")
		}
	}
	proof, err := json.Marshal(p.Selection)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append([]byte(`{"VERSION":2,`), proof[1:]...)
	if json.Unmarshal(duplicate, &ReadySelection{}) == nil {
		t.Fatal("inner authority duplicate accepted")
	}
}

func TestReadySelectionDigestChargesBothEncoderViews(t *testing.T) {
	s := newTestStore(t)
	value := strings.Repeat("x", 96<<10)
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []int64{-1, 0} {
		limit := int64(32<<10) + 2*int64(len(raw)) + extra
		budget := agentapi.NewNativeReadBudget(limit)
		read, closeRead := s.WithReadBudget(t.Context(), budget)
		_, err = read.readyDigest(t.Context(), value)
		closeRead()
		if (extra < 0) != (err != nil) {
			t.Fatalf("two-view allowance extra=%d err=%v", extra, err)
		}
		if budget.Available() != limit {
			t.Fatal("digest retained scratch or encoder reservation")
		}
	}
}

func TestReadyPublishedRefAndDetachedBodyCorruptionRefuse(t *testing.T) {
	s, p := readySelectionFixture(t)
	id := p.Bundle.ArchiveSessionID
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveReadyPublication(p, p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	if err = published.Save(p.Bundle, p.ReadyAt, CacheStatusRateLimited); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*publishedState){
		func(next *publishedState) {
			source := *next.LastPublished.Source
			source.SHA256 = strings.Repeat("0", 64)
			next.LastPublished.Source = &source
		},
		func(next *publishedState) { next.LastPublished.Bundle.NativeSessionID = "foreign" },
		func(next *publishedState) {
			next.LastPublished.Bundle.NativeRecords = []map[string]any{{"content": "foreign"}}
		},
	} {
		next := published.state
		snapshot := *next.LastPublished
		next.LastPublished = &snapshot
		change(&next)
		if err = published.write(next); err == nil {
			t.Fatal("changed acknowledged source/body accepted")
		}
		raw, e := os.ReadFile(s.publishedPath(id))
		if e != nil || !bytes.Equal(raw, original) {
			t.Fatal("failed write changed published evidence")
		}
		raw, e = json.Marshal(next)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(s.publishedPath(id), raw, 0600); e != nil {
			t.Fatal(e)
		}
		if _, e = s.LoadPublishedState(id); e == nil {
			t.Fatal("corrupt retained selecting snapshot read successfully")
		}
		got, e := os.ReadFile(s.publishedPath(id))
		if e != nil || !bytes.Equal(got, raw) {
			t.Fatal("corrupt evidence lost")
		}
		if e = os.WriteFile(s.publishedPath(id), original, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
