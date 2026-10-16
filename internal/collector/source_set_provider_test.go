package collector

import (
	"bytes"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestOrdinarySourceSetAttemptKeepsCapturedProvider(t *testing.T) {
	t.Parallel()
	for _, nestedPublication := range []bool{false, true} {
		t.Run(map[bool]string{false: "source-verifier", true: "nested-publication"}[nestedPublication], func(t *testing.T) {
			t.Parallel()
			local, actual, reg, opts, pending, _ := ordinaryPrivacyFixture(t)
			if err := actual.MemoryStore.Delete(t.Context(), pending.SourceKey); err != nil {
				t.Fatal(err)
			}
			if err := actual.MemoryStore.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
				t.Fatal(err)
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			scan := newSessionScan(t.Context(), local, actual, reg, state.Request{}, published, opts.Now(), opts)
			defer scan.releaseRetained()
			end, err := scan.beginPublicationAttempt()
			if err != nil {
				t.Fatal(err)
			}
			defer end()
			ambient := storagetest.NewMemoryStore()
			if err := ambient.Put(t.Context(), pending.SourceKey, pending.SourceBytes); err != nil {
				t.Fatal(err)
			}
			winner := []byte(`{"synthetic":"unrelated ambient metadata"}`)
			if err := ambient.Put(t.Context(), pending.MetadataKey, winner); err != nil {
				t.Fatal(err)
			}
			scan.remote = ambient
			if nestedPublication {
				if outcome, err := scan.sourceSetPublishPending(pending); err != nil || outcome != outcomePublished {
					t.Fatal("nested publication retargeted captured provider", outcome, err)
				}
			} else {
				receipt, err := scan.sourceSetUpload(pending)
				if err != nil || receipt == nil {
					t.Fatal("captured publication did not complete", err)
				}
				if _, _, err := receipt.Consume(pending.MetadataKey, pending.MetadataBytes); err != nil {
					t.Fatal(err)
				}
			}
			source, err := actual.MemoryStore.Get(t.Context(), pending.SourceKey)
			if err != nil || !bytes.Equal(source, pending.SourceBytes) {
				t.Fatal("ambient checksum hid missing source in captured provider", err)
			}
			selected, err := actual.MemoryStore.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(selected, pending.MetadataBytes) {
				t.Fatal("captured metadata changed", err)
			}
			unrelated, err := ambient.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(unrelated, winner) {
				t.Fatal("ambient winner was touched", err)
			}
		})
	}
}
