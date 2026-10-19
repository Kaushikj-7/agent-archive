package retainedprivacy_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/retainedprivacy"
	"github.com/wangjohn/agent-archive/internal/archive"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

func retainedFixture(t *testing.T) (retainedprivacy.Expectation, []byte, *agentapi.NativeReadBudget) {
	t.Helper()
	at := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: "session", NativeSessionID: "native", ProjectID: "project", ProjectRoot: "/synthetic/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: "/synthetic/project/missing.jsonl", SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("confirmed"), DestinationID: "destination", StagedImportSHA256: strings.Repeat("a", 64)}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "claude", AdapterVersion: "obsolete", FilterVersion: "obsolete", CapturedAt: at, SourceFormat: "jsonl", Gaps: []archive.CaptureGap{{Code: "retained_omission", Detail: "already absent"}}}, NativeRecords: []map[string]any{{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "safe retained text"}}}, SupplementalEvidence: []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"snapshot": "synthetic retained body"}}}}
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Harness: reg.Harness, CapturedAt: at, FilterVersion: bundle.Capture.FilterVersion, SourceBundle: archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}}
	_, filter, _ := builtin.NewBuiltins().LookupSources("claude")
	expected := retainedprivacy.Expectation{Registration: reg, StageSHA256: reg.StagedImportSHA256, Metadata: metadata, OldPolicy: retainedprivacy.Policy{FilterVersion: "obsolete", AdapterVersion: "obsolete", SkillEvidence: "body"}, Target: retainedprivacy.Policy{FilterVersion: archive.FilterVersion, AdapterVersion: filter.Version(), SkillEvidence: "none"}, CoveredToken: "original-token", HadRequest: true}
	budget := agentapi.NewNativeReadBudget(32 << 20)
	t.Cleanup(budget.BeginScope(16 << 20))
	return expected, packed.Bytes, budget
}

func TestFixedRetainedTransformPreservesOriginalFactsAndOwnsDetachedOutput(t *testing.T) {
	expected, source, budget := retainedFixture(t)
	request, err := freezeRetainedRequest(t.Context(), expected, source, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, request)
	source[0] ^= 1
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, proof)
	called := false
	err = proof.WithVerifiedTransform(t.Context(), request, budget, func(_ context.Context, bundle archive.SourceBundle, packed archive.CompressedSource, facts retainedprivacy.Facts) error {
		called = true
		if len(bundle.SupplementalEvidence) != 0 || !bundle.Capture.CapturedAt.Equal(expected.Metadata.CapturedAt) || bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.SourceFormat != "jsonl" {
			t.Fatal("retained projection changed capture facts", bundle.Capture)
		}
		if facts.Input != expected.Metadata.SourceBundle || facts.OriginalPolicy != expected.OldPolicy || facts.TargetPolicy != expected.Target || facts.CoveredToken != expected.CoveredToken || !facts.HadRequest || facts.StageSHA256 != expected.StageSHA256 {
			t.Fatal("original authority changed", facts)
		}
		packed.Bytes[0] ^= 1
		bundle.NativeRecords[0]["type"] = "changed detached loan"
		return nil
	})
	if err != nil || !called {
		t.Fatal(called, err)
	}
	if err := proof.WithVerifiedTransform(t.Context(), request, budget, func(_ context.Context, bundle archive.SourceBundle, _ archive.CompressedSource, _ retainedprivacy.Facts) error {
		if bundle.NativeRecords[0]["type"] == "changed detached loan" {
			t.Fatal("loan mutated retained proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	copied := *proof
	if err := proof.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copied.WithVerifiedTransform(t.Context(), request, budget, func(context.Context, archive.SourceBundle, archive.CompressedSource, retainedprivacy.Facts) error {
		t.Fatal("closed proof used")
		return nil
	}); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("terminal close retained charges", used)
	}
}

func TestFixedRetainedTransformRejectsCustomRegistryAndForeignRequest(t *testing.T) {
	expected, source, budget := retainedFixture(t)
	request, err := freezeRetainedRequest(t.Context(), expected, source, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, request)
	var bindings []builtin.Integration
	for _, descriptor := range agentmeta.Builtins().All() {
		integration, ok := builtin.NewBuiltins().Lookup(string(descriptor.ID))
		if !ok {
			t.Fatal("missing builtin fixture", descriptor.ID)
		}
		integration.Descriptor = agentmeta.Descriptor{ID: descriptor.ID}
		bindings = append(bindings, integration)
	}
	custom, err := builtin.New(agentmeta.Builtins(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	if proof, err := custom.TransformRetainedPrivacy(request); err == nil || proof != nil {
		t.Fatal("custom registry minted fixed evidence")
	}
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, proof)
	other, err := freezeRetainedRequest(t.Context(), expected, source, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, other)
	if err := proof.WithVerifiedTransform(t.Context(), other, budget, func(context.Context, archive.SourceBundle, archive.CompressedSource, retainedprivacy.Facts) error {
		t.Fatal("foreign request used")
		return nil
	}); err == nil {
		t.Fatal("foreign request accepted")
	}
	if err := proof.WithVerifiedTransform(t.Context(), request, agentapi.NewNativeReadBudget(32<<20), func(context.Context, archive.SourceBundle, archive.CompressedSource, retainedprivacy.Facts) error {
		t.Fatal("foreign budget used")
		return nil
	}); err == nil {
		t.Fatal("foreign budget accepted")
	}
}

func TestFixedRetainedTransformOriginalCloseReachesActualLoanContext(t *testing.T) {
	expected, source, budget := retainedFixture(t)
	request, err := freezeRetainedRequest(t.Context(), expected, source, budget)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, proof)
	err = proof.WithVerifiedTransform(context.Background(), request, budget, func(ctx context.Context, _ archive.SourceBundle, _ archive.CompressedSource, _ retainedprivacy.Facts) error {
		if err := request.Close(); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() == nil {
			t.Fatal("host close did not reach actual consumer context")
		}
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("closed original request accepted loan completion")
	}
}

func TestFixedRetainedTransformKeepsCapturedScopeAcrossPortDispatch(t *testing.T) {
	expected, source, budget := retainedFixture(t)
	request, err := freezeRetainedRequest(t.Context(), expected, source, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, request)
	endOther := budget.BeginScope(1)
	defer endOther()
	other := budget.Reservation()
	if !other.Reserve(1) {
		t.Fatal("later scope fixture")
	}
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err != nil {
		t.Fatal("producer escaped original scope", err)
	}
	if err := proof.WithVerifiedTransform(t.Context(), request, budget, func(context.Context, archive.SourceBundle, archive.CompressedSource, retainedprivacy.Facts) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := proof.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 1 {
		t.Fatal("proof consumed later owner's charge", used)
	}
	other.Release(1)
}

func TestFixedRetainedTransformCannotBorrowLaterScopeCapacity(t *testing.T) {
	expected, source, _ := retainedFixture(t)
	budget := agentapi.NewNativeReadBudget(32 << 20)
	endOriginal := budget.BeginScope(96 << 10)
	defer endOriginal()
	captured, err := budget.Reservation().CapturedBudget()
	if err != nil {
		t.Fatal(err)
	}
	request, err := freezeRetainedRequest(t.Context(), expected, source, budget)
	if err != nil {
		t.Fatal(err)
	}
	endOther := budget.BeginScope(32 << 20)
	defer endOther()
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err == nil || proof != nil || !captured.ScopeExceeded() || budget.ScopeExceeded() {
		t.Fatal("producer borrowed later scope's capacity", err)
	}
	if err := request.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("refusal leaked charges", used)
	}
}

func TestFixedRetainedTransformRejectsChangedPolicyAndCorruptInput(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		expected, source, budget := retainedFixture(t)
		if corrupt {
			source[len(source)/2] ^= 1
		} else {
			expected.Target.AdapterVersion = "forged-production-version"
		}
		request, err := freezeRetainedRequest(t.Context(), expected, source, budget)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
		if err == nil || proof != nil {
			t.Fatal("changed input or policy minted evidence")
		}
		if err := request.Close(); err != nil {
			t.Fatal(err)
		}
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("refusal retained resources", used)
		}
	}
}

func TestRetainedRequestLeaseCopiesShareTerminalClose(t *testing.T) {
	expected, source, budget := retainedFixture(t)
	request, err := freezeRetainedRequest(t.Context(), expected, source, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, request)
	lease, err := request.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	alias := *lease
	lease.Close()
	if err := alias.Check(); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("closed lease alias remained usable: %v", err)
	}
	defer func() {
		if value := recover(); value != nil {
			t.Errorf("copied lease released another owner's charges: %v", value)
		}
	}()
	alias.Close()
	if err := request.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("copied close retained charges", used)
	}
}

func closeRetained(t *testing.T, resource interface{ Close() error }) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Error(err)
	}
}

func TestRetainedRequestCapturedOwnerKeepsRawLedgerIdentityAndExistingCharges(t *testing.T) {
	expected, source, budget := retainedFixture(t)
	original := budget.Reservation()
	if !original.Reserve(123) {
		t.Fatal("original other reservation")
	}
	defer original.Release(123)
	endOther := budget.BeginScope(1)
	defer endOther()
	other := budget.Reservation()
	if !other.Reserve(1) {
		t.Fatal("later scope fixture")
	}
	defer other.Release(1)
	request, err := retainedprivacy.FreezeRequestCaptured(t.Context(), expected, source, budget, original)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRetained(t, request)
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err != nil {
		t.Fatal(err)
	}
	err = proof.WithVerifiedTransform(t.Context(), request, budget, func(context.Context, archive.SourceBundle, archive.CompressedSource, retainedprivacy.Facts) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := proof.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 124 {
		t.Fatal("request released original or later owner's charge", used)
	}
}

// Test convenience only: production callers must supply their actual captured
// owner explicitly, as the state stage factory does.
func freezeRetainedRequest(ctx context.Context, expected retainedprivacy.Expectation, source []byte, budget *agentapi.NativeReadBudget) (*retainedprivacy.Request, error) {
	return retainedprivacy.FreezeRequestCaptured(ctx, expected, source, budget, budget.Reservation())
}
