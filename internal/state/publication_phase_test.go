package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestPublicationV2SinglePayloadAndSettledCharge(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	fixture.History = &PendingHistory{Version: 1}
	p, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.pendingPath(p.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		SourceBytes []byte              `json:"source_bytes"`
		Sources     []PublicationSource `json:"sources"`
	}
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.SourceBytes) != 0 || len(wire.Sources) != 1 || len(wire.Sources[0].Payload.Inline) == 0 {
		t.Fatal("duplicate or missing wire authority")
	}
	got, found, err := s.LoadPending(p.Bundle.ArchiveSessionID)
	if err != nil || !found || got.ValidatePublication() != nil {
		t.Fatal(found, err)
	}
	published, err := s.LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(got, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(s.publishedPath(p.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"publication_version":5,"summary":`) {
		t.Fatal("publication header is not before summary")
	}
	if err = s.RemovePending(p.Bundle.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	if owed, err := s.HasPending(p.Bundle.ArchiveSessionID); err != nil || owed {
		t.Fatal("settled state remains pending", owed, err)
	}
	if err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() {
			if err := q.Close(); err != nil {
				t.Error(err)
			}
		}()
		u, e := q.usageContext(t.Context())
		if e == nil && u.charged != 2*int64(len(raw)) {
			t.Fatalf("settled file not exactly charged: %d vs %d", u.charged, 2*len(raw))
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadPublishedState(p.Bundle.ArchiveSessionID); err != nil {
		t.Fatal("settled full codec", err)
	}
}

func TestPublicationV2UnknownEnvelopeRetainsEvidence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	fixture.History = &PendingHistory{Version: 1}
	p, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	path := s.pendingPath(p.Bundle.ArchiveSessionID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte(strings.Replace(string(raw), `"journal_version":5`, `"journal_version":99`, 1))
	if bytes.Equal(changed, raw) {
		t.Fatal("journal-version mutation did not change the fixture")
	}
	raw = changed
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadPending(p.Bundle.ArchiveSessionID); !found || !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal(found, err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("unknown journal discarded", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
		t.Fatal("journal quarantined", entries, err)
	}
}

func TestPublicationLegacyMigrationHasOwnedReplayAndOpaqueGenericRefusal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	fixture.History = &PendingHistory{Version: 1}
	if err := s.SavePending(fixture.Bundle.ArchiveSessionID, fixture); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(s.pendingPath(fixture.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	p, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(fixture.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadPending(fixture.Bundle.ArchiveSessionID); !found || !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("generic evidence bypass", found, err)
	}
	got, found, err := s.LoadPublicationPending(fixture.Bundle.ArchiveSessionID)
	if err != nil || !found || got.Commit == nil || got.Commit.MetadataSHA256 != p.Commit.MetadataSHA256 {
		t.Fatal("owned migration replay", found, err)
	}
	var ownedMetadata archive.Metadata
	if err = json.Unmarshal(got.MetadataBytes, &ownedMetadata); err != nil {
		t.Fatal(err)
	}
	owner := archive.SessionRegistration{ArchiveSessionID: ownedMetadata.SessionID, NativeSessionID: ownedMetadata.NativeSessionID, ProjectID: ownedMetadata.ProjectID, Harness: ownedMetadata.Harness, Origin: ownedMetadata.Origin, SessionStartedAt: ownedMetadata.StartedAt, PreviousGenerationID: ownedMetadata.PreviousGenerationID, DestinationID: "destination"}
	if err = s.CheckDurableSessionRead(owner.ArchiveSessionID); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("generic reader bypassed original evidence: %v", err)
	}
	if err = s.CheckPublicationSessionRead(owner, ownedMetadata.MachineID, "admission"); err != nil {
		t.Fatalf("owned replay reader refused validated original evidence: %v", err)
	}
	evidenceRaw, err := os.ReadFile(filepath.Join(s.home, evidencePath(fixture.Bundle.ArchiveSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	var e PublicationOriginalEvidence
	if err = closedPublicationDecode(evidenceRaw, &e); err != nil {
		t.Fatal(err)
	}
	if e.MigrationOrigin == nil || string(e.MigrationOrigin.Raw) != string(old) || e.Link.Target.Phase != PublicationReady {
		t.Fatal("original changed")
	}
	// The evidence-before-pending interruption admits only the exact old file.
	if err = os.WriteFile(s.pendingPath(fixture.Bundle.ArchiveSessionID), old, 0600); err != nil {
		t.Fatal(err)
	}
	if got, found, err = s.LoadPublicationPending(fixture.Bundle.ArchiveSessionID); err != nil || !found || got.JournalVersion != 0 {
		t.Fatal("immediate crash predecessor refused", found, err)
	}
	if err = s.SavePending(fixture.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal("matching target reinstall", err)
	}
	published, err := s.LoadPublishedState(fixture.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.SettlePublicationMigration(fixture.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	if owed, err := s.hasPublicationEvidence(fixture.Bundle.ArchiveSessionID); err != nil || owed {
		t.Fatal("committed migration remains owed", owed, err)
	}
}

func TestPublicationSelectingWriteBudgetRefusalKeepsPending(t *testing.T) {
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	fixture.History = &PendingHistory{Version: 1}
	p, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.pendingPath(p.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	for _, cancel := range []bool{false, true} {
		ctx, stop := context.WithCancel(t.Context())
		budget := agentapi.NewNativeReadBudget(32 << 10)
		scoped, closeScope := s.WithReadBudget(ctx, budget)
		published, loadErr := scoped.LoadPublishedState(p.Bundle.ArchiveSessionID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if cancel {
			stop()
		}
		err = published.SaveCommittedPublication(p, time.Now())
		if cancel && !errors.Is(err, context.Canceled) || !cancel && !errors.Is(err, agentapi.ErrReadBudget) {
			t.Fatal("wrong refusal", cancel, err)
		}
		if published.Found() {
			t.Fatal("partial published mutation")
		}
		if _, err = os.Stat(s.publishedPath(p.Bundle.ArchiveSessionID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("partial published file", err)
		}
		after, err := os.ReadFile(s.pendingPath(p.Bundle.ArchiveSessionID))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("pending changed on refusal", err)
		}
		closeScope()
		stop()
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("scope charge leaked", used)
		}
	}
}

func TestPublicationClosedVersionRejectsErasedAuthorityClaims(t *testing.T) {
	legacy := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var valid PendingPublication
	if err := json.Unmarshal(raw, &valid); err != nil {
		t.Fatal("known legacy producer rejected", err)
	}
	for _, claim := range []string{`"journal_version":0`, `"journal_version":null`, `"phase":null`, `"phase":""`, `"preparation":null`, `"progress":null`, `"cleanup":null`, `"PREPARATION":null`} {
		poisoned := append([]byte("{"+claim+","), raw[1:]...)
		var p PendingPublication
		if err := json.Unmarshal(poisoned, &p); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("erased pending authority accepted", claim, err)
		}
	}
	ready, err := PreparePublicationV2(legacy, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(publicationWire(ready))
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range []string{`"source_bytes":null`, `"source_bytes":""`, `"SOURCE_BYTES":null`} {
		var p PendingPublication
		if err := json.Unmarshal(append([]byte("{"+claim+","), raw[1:]...), &p); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("second wire byte authority accepted", claim, err)
		}
	}
	if err := json.Unmarshal(raw, &valid); err != nil {
		t.Fatal("current producer rejected", err)
	}
	published := publishedState{Bundle: legacy.Bundle, Status: CacheStatusPublished, PublishedAt: legacy.Bundle.Capture.CapturedAt}
	raw, err = json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	var loaded publishedState
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal("known ordinary legacy producer rejected", err)
	}
	for _, claim := range []string{`"publication_version":0`, `"publication_version":null`, `"settled_privacy":null`, `"privacy_receipts":null`, `"privacy_receipts":[]`, `"privacy_receipts":[{}]`, `"preparation":null`, `"payloads":[]`, `"cleanup":null`, `"PREPARATION":null`, `"status":"published","STATUS":"published"`} {
		if err := json.Unmarshal(append([]byte("{"+claim+","), raw[1:]...), &loaded); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("erased published authority accepted", claim, err)
		}
	}
	if err := json.Unmarshal(append(raw, []byte(` {}`)...), &loaded); err == nil {
		t.Fatal("trailing record accepted")
	}
}

func TestPublicationClosedNestedAuthorityRejectsCaseAlias(t *testing.T) {
	legacy := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	ready, err := PreparePublicationV2(legacy, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(publicationWire(ready))
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []publicationAliasVariant{publicationAliasVariantCommitEqual, publicationAliasVariantCommitConflicting, publicationAliasVariantPayload, publicationAliasVariantPreparation} {
		t.Run(string(variant), func(t *testing.T) {
			var shape map[string]any
			if err := json.Unmarshal(raw, &shape); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case publicationAliasVariantCommitEqual:
				shape["source_set_commit5"].(map[string]any)["VERSION"] = float64(2)
			case publicationAliasVariantCommitConflicting:
				shape["source_set_commit5"].(map[string]any)["VERSION"] = float64(9)
			case publicationAliasVariantPreparation:
				shape["preparation"].(map[string]any)["VERSION"] = shape["preparation"].(map[string]any)["version"]
			case publicationAliasVariantPayload:
				payload := shape["sources"].([]any)[0].(map[string]any)["payload"].(map[string]any)
				payload["KIND"] = payload["kind"]
			}
			poisoned, err := json.Marshal(shape)
			if err != nil {
				t.Fatal(err)
			}
			var loaded PendingPublication
			if err := json.Unmarshal(poisoned, &loaded); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatalf("nested authority aliases accepted: %v", err)
			}
		})
	}
	// Native payload maps are opaque case-sensitive data, not typed authority.
	var native struct {
		NativeRecords []map[string]any `json:"native_records"`
	}
	if err := closedPublicationDecode([]byte(`{"native_records":[{"Secret":"first","secret":"second"}]}`), &native); err != nil || native.NativeRecords[0]["Secret"] != "first" || native.NativeRecords[0]["secret"] != "second" {
		t.Fatal("case-distinct native payload changed", err)
	}
}

type publicationAliasVariant string

const (
	publicationAliasVariantCommitEqual       publicationAliasVariant = "commit-equal"
	publicationAliasVariantCommitConflicting publicationAliasVariant = "commit-conflicting"
	publicationAliasVariantPayload           publicationAliasVariant = "payload"
	publicationAliasVariantPreparation       publicationAliasVariant = "preparation"
)
