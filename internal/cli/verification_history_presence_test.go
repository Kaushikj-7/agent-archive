package cli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The retained local manifest/receipt cannot authorize a downgraded remote
// pointer, even when its machine, active source and publication age still match.
func TestHistoryPresenceDowngradeRefusesVerificationAndPrivacyReceipt(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	remote := storagetest.NewMemoryStore()
	cfg, store := publishSyntheticSessions(t, home, project, remote, at, 1)
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatal("registration fixture", err)
	}
	reg := regs[0]
	key, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	downgraded, err := reader.ReadMetadata(t.Context(), remote, key)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, publishedAt, found := published.LastPublished()
	if !found {
		t.Fatal("publication fixture")
	}
	expected := downgraded
	expected.SchemaVersion = archive.HistoryMetadataSchemaVersion
	expected.History = &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111"}
	raw, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.RestorePublication(bundle, expected.SourceBundle, raw, expected.MetadataDerivedAt); err != nil {
		t.Fatal(err)
	}
	set, err := expected.SourceSetDigest()
	if err != nil {
		t.Fatal(err)
	}
	receipt := verificationEvidence{ConfigurationID: sessionVerificationConfigurationID(cfg, reg), PublishedAt: publishedAt, SourceSHA256: expected.SourceBundle.SHA256, SourceSetDigest: set, MetadataDigest: storage.SHA256Hex(raw), Outcome: verificationOutcomeVerified, VerifiedAt: at.Add(time.Minute)}
	if err := local.Write(verificationPath(home, reg.ArchiveSessionID), receipt); err != nil {
		t.Fatal(err)
	}
	if !privacyPublicationVerified(home, cfg, store, reg, expected) {
		t.Fatal("unchanged history receipt refused")
	}
	if privacyPublicationVerified(home, cfg, store, reg, downgraded) {
		t.Fatal("remote history removal accepted stale receipt")
	}
	if _, err := verifyPublication(context.Background(), cfg, remote, reg, published); !errors.Is(err, errVerificationMismatch) {
		t.Fatalf("remote history removal must mismatch: %v", err)
	}
}
