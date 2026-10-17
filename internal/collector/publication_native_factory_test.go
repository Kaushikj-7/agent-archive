package collector

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestNativeHeaderFactoryChecksClaimsAndFrozenOwner(t *testing.T) {
	scan, p := privacyJournal(t)
	defer scan.releaseRetained()
	var origin archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &origin); err != nil {
		t.Fatal(err)
	}
	adapter, err := sourceAdapter(scan.opts.Sources, scan.reg.Harness.Name)
	if err != nil {
		t.Fatal(err)
	}
	reg := scan.reg
	binding := *reg.CodexBinding
	binding.Child = true
	binding.RootID = ""
	binding.ParentID = ""
	reg.CodexBinding = &binding
	reg.NativeChild = true
	reg.ParentSessionID = "receipt-parent"
	reg.NativeRootSessionID = ""
	reg.ParentNativeSessionID = ""
	reg.NativeSourceHome = binding.Home
	cases := []struct {
		name          string
		positive      bool
		badParent     bool
		badMarker     bool
		unbound       bool
		later         bool
		pressure      bool
		cancel        bool
		invalidUnused bool
		knownConflict bool
		unadmitted    bool
	}{
		{name: "licensed_legacy"}, {name: "parent_claim", badParent: true}, {name: "marker_claim", badMarker: true},
		{name: "forged_target_binding", unbound: true}, {name: "positive_header", positive: true},
		{name: "frozen_empty_target", later: true}, {name: "pressure", pressure: true}, {name: "cancel", cancel: true},
		{name: "positive_header_unused_binding", positive: true, invalidUnused: true},
		{name: "known_target_conflict", knownConflict: true},
		{name: "positive_header_forged_target_owner", positive: true, unadmitted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := p.Bundle
			original.ParentSessionID = ""
			original.NativeChild = tc.positive
			packed, e := archive.BuildCompressedSource(original)
			if e != nil {
				t.Fatal(e)
			}
			key, e := archive.SourceObjectKey(original, packed.SHA256)
			if e != nil {
				t.Fatal(e)
			}
			ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
			metadata := origin
			metadata.SourceBundle = ref
			metadata.ParentSessionID = ""
			metadata.NativeChild = tc.positive
			body, e := json.Marshal(metadata)
			if e != nil {
				t.Fatal(e)
			}
			parent := ""
			marker := tc.positive
			input := state.PreparationInput{ParentSessionID: &parent, NativeChild: &marker, Reference: ref, Selection: state.PublicationSelection{Role: state.PublicationCurrent, RevisionID: metadata.History.CurrentRevision, CapturedAt: original.Capture.CapturedAt, SourceSchemaVersion: original.SchemaVersion}, FilterVersion: original.Capture.FilterVersion, AdapterVersion: original.Capture.AdapterVersion, SkillPolicy: "body"}
			budget := agentapi.NewNativeReadBudget(128 << 20)
			reservation := budget.Reservation()
			if !reservation.Reserve(int64(len(packed.Bytes))) {
				t.Fatal("original byte loan")
			}
			targetBundle := original
			targetBundle.NativeChild = true
			targetBundle.ParentSessionID = reg.ParentSessionID
			if tc.later {
				targetBundle.ParentSessionID = ""
			}
			candidate := state.PendingPublication{Bundle: targetBundle, History: &state.PendingHistory{Version: 1, Preparing: true}, MetadataKey: p.MetadataKey, MetadataBytes: body, SourceKey: key, SourceSHA256: ref.SHA256, SourceBytes: packed.Bytes, SkillEvidence: "body"}
			producer := reg
			if tc.invalidUnused {
				invalid := binding
				invalid.Version = 2
				producer.CodexBinding = &invalid
			}
			targetRelease, e := state.FreezePublicationNativeTarget(t.Context(), &candidate, producer, budget)
			if e != nil {
				t.Fatal(e)
			}
			policy := state.PublicationPolicy{FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(), SkillEvidence: config.SkillEvidenceBody}
			candidate, e = state.PreparePublicationV2(candidate, state.PublicationPredecessor{State: state.PredecessorAbsent}, reg.DestinationID, scan.publicationAdmission(), policy.Context(), state.PublicationPrivacyRewrite)
			if e != nil {
				t.Fatal(e)
			}
			if tc.badParent {
				parent = "forged-parent"
			}
			if tc.badMarker {
				marker = !marker
			}
			admitted := producer
			if tc.unadmitted {
				admitted.NativeChild = false
			}
			if tc.knownConflict {
				admitted.ParentSessionID = "different-known-parent"
			}
			if tc.unbound {
				admitted.CodexBinding = nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			if tc.cancel {
				cancel()
			}
			held := int64(0)
			if tc.pressure {
				held = budget.Available() - 1024
				if !reservation.Reserve(held) {
					t.Fatal("pressure")
				}
			}
			before := budget.Available()
			out, _, _, _, release, e := state.RefilterPublicationInput(ctx, admitted, adapter, metadata, body, input, 0, packed.Bytes, state.PublicationContext{NativeTarget: candidate.Preparation.NativeTarget, DestinationID: reg.DestinationID, AdmissionContext: scan.publicationAdmission()}, policy, policy, config.SkillEvidenceBody, budget)
			cancel()
			refused := tc.badParent || tc.badMarker || tc.unbound || tc.pressure || tc.cancel || tc.knownConflict || tc.unadmitted
			if refused {
				if e == nil || release != nil {
					t.Fatal("invalid claim minted transform", e)
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				if !out.NativeChild || out.ParentSessionID != targetBundle.ParentSessionID || !out.Capture.CapturedAt.Equal(original.Capture.CapturedAt) {
					t.Fatal("frozen output relationship/age changed")
				}
				release()
				release()
			}
			if budget.Available() != before {
				t.Fatal("transform loan leaked", before, budget.Available())
			}
			if held != 0 {
				reservation.Release(held)
			}
			targetRelease()
			targetRelease()
			reservation.Release(int64(len(packed.Bytes)))
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("original/target loans leaked", used)
			}
		})
	}
}
