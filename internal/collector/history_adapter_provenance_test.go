package collector

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"testing"
)

func TestPreservedPrivacyUsesOwnAdapterVersion(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		older, forged, adapterOnly bool
	}{
		{name: "same-adapter-control"},
		{name: "older-preserved-adapter", older: true},
		{name: "older-adapter-unchanged-filter", older: true, adapterOnly: true},
		{name: "forged-active-adapter", older: true, forged: true},
	} {
		t.Run(tc.name, func(t *testing.T) { preservedAdapterPrivacy(t, tc.older, tc.forged, tc.adapterOnly) })
	}
}
func preservedAdapterPrivacy(t *testing.T, older, forged, adapterOnly bool) {
	scan, p := privacyJournal(t)
	defer scan.releaseRetained()
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.History.Preserved) == 0 {
		t.Fatal("no preserved fixture")
	}
	old := m.History.Preserved[0]
	raw, err := scan.readRetainedInputBytes(old.Source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := scan.decodeRevision(m, old.RevisionID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if older {
		b.Capture.AdapterVersion = "synthetic-prior-adapter"
	}
	if !adapterOnly {
		b.Capture.FilterVersion = "14"
	}
	packed, err := archive.BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(b, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	if _, err = scan.local.StagePublicationSource(scan.id(), ref, packed.Bytes); err != nil {
		t.Fatal(err)
	}
	m.History.Preserved[0].Source = ref
	m.History.Preserved[0].FilterVersion = b.Capture.FilterVersion
	m.History.Preserved[0].SourceSchemaVersion = b.SchemaVersion
	for i := range p.History.Inputs {
		// This imported producer predates per-input adapter provenance.
		p.History.Inputs[i].AdapterVersion = ""
		if p.History.Inputs[i].Reference == old.Source {
			p.History.Inputs[i].Reference = ref
			p.History.Inputs[i].FilterVersion = b.Capture.FilterVersion
			p.History.Inputs[i].SourceSchemaVersion = b.SchemaVersion
			if forged {
				p.History.Inputs[i].AdapterVersion = m.Adapter.Version
			}
		}
	}
	p.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p = freshLegacyHistoryOriginal(t, scan, p)
	for p.History.Preparing {
		if err := scan.advanceHistoryPreparation(&p); err != nil {
			if forged {
				return
			}
			t.Fatal("valid mixed-adapter original failed retained privacy", err)
		}
	}
	if forged {
		t.Fatal("forged active adapter accepted for older preserved bytes")
	}
	for i, input := range p.Preparation.Inputs {
		if input.Selection.RevisionID == old.RevisionID {
			if input.AdapterVersion != b.Capture.AdapterVersion {
				t.Fatal("preserved producer overwritten by active adapter", input.AdapterVersion)
			}
			if proof := p.Progress.Outputs[i].Privacy; proof == nil || proof.PreviousPolicy.AdapterVersion != b.Capture.AdapterVersion {
				t.Fatal("receipt lost original producer", proof)
			}
		}
	}
	if p.Commit == nil || p.Commit.Purpose != state.PublicationPrivacyRewrite {
		t.Fatal("no complete privacy successor")
	}
}

func TestFrozenInputAdapterProvenanceKeepsItsOwnedCharge(t *testing.T) {
	scan, p := privacyJournal(t)
	defer scan.releaseRetained()
	for i := range p.History.Inputs {
		p.History.Inputs[i].AdapterVersion = ""
		parent, child := "", false
		p.History.Inputs[i].ParentSessionID = &parent
		p.History.Inputs[i].NativeChild = &child
	}
	mark := len(scan.retainedReleases)
	if _, err := scan.freezeNativeHeaderInputs(&p); err != nil {
		t.Fatal(err)
	}
	// The independent metadata view also retains its encoded-equivalent loan.
	retained := int64(len(p.MetadataBytes))
	for _, input := range p.History.Inputs {
		if input.AdapterVersion == "" {
			t.Fatal("producer provenance missing")
		}
		retained += int64(len(input.AdapterVersion))
	}
	after, _ := scan.readBudget().Charged()
	scan.releaseRetainedAfter(mark)
	released, _ := scan.readBudget().Charged()
	if after-released != retained {
		t.Fatalf("metadata and producer ownership release=%d, want %d", after-released, retained)
	}
}
