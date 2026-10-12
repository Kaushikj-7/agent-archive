package state

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// APIs/fixture and proof layout are present on frozen ea974. Run identical
// assertions there and on the correction: this is a first-save owner bypass,
// rather than replacement of an existing immutable journal.
func TestReadySelectionFirstSaveRejectsRecomputedForeignOwner(t *testing.T) {
	for name, change := range map[string]func(*archive.SourceBundle){
		"parent":       func(b *archive.SourceBundle) { b.ParentSessionID = "foreign-parent" },
		"native-child": func(b *archive.SourceBundle) { b.NativeChild = true; b.ParentSessionID = "foreign-parent" },
		"native-id":    func(b *archive.SourceBundle) { b.NativeSessionID = "foreign-native" },
		"project":      func(b *archive.SourceBundle) { b.ProjectID = "foreign-project" },
		"harness":      func(b *archive.SourceBundle) { b.Capture.Harness.Name = "claude" },
	} {
		t.Run(name, func(t *testing.T) {
			s, p := readySelectionFixture(t)
			id := p.Bundle.ArchiveSessionID
			if _, found, err := s.LoadPending(id); err != nil || found {
				t.Fatalf("control slot is not empty: %v %v", found, err)
			}
			q := p
			proof := *p.Selection
			q.Selection = &proof
			change(&q.Bundle)
			var metadata archive.Metadata
			if err := json.Unmarshal(q.MetadataBytes, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata.NativeSessionID = q.Bundle.NativeSessionID
			metadata.ProjectID = q.Bundle.ProjectID
			metadata.ParentSessionID = q.Bundle.ParentSessionID
			metadata.NativeChild = q.Bundle.NativeChild
			metadata.Harness = q.Bundle.Capture.Harness
			var err error
			q.SourceKey, err = archive.SourceObjectKey(q.Bundle, q.SourceSHA256)
			if err != nil {
				t.Fatal(err)
			}
			q.MetadataKey, err = archive.MetadataObjectKey(q.Bundle.Capture.Harness.Name, id)
			if err != nil {
				t.Fatal(err)
			}
			metadata.SourceBundle = q.SourceReference()
			q.MetadataBytes, err = json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			q.Selection.MetadataSHA256 = readySHA(q.MetadataBytes)
			refs, err := metadata.SourceReferences()
			if err != nil {
				t.Fatal(err)
			}
			q.Selection.SourceSetSHA256, err = s.readyDigest(t.Context(), refs)
			if err != nil {
				t.Fatal(err)
			}
			q.Selection.BundleSHA256, err = s.readyDigest(t.Context(), q.Bundle)
			if err != nil {
				t.Fatal(err)
			}
			q.Selection.BindingSHA256, err = s.readyBinding(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			recomputed := *q.Selection
			recomputed.SHA256 = ""
			q.Selection.SHA256, err = s.readyDigest(t.Context(), recomputed)
			if err != nil {
				t.Fatal(err)
			}
			if q.Selection.AdmissionContext != p.Selection.AdmissionContext {
				t.Fatal("control changed real admission hash")
			}
			if err = q.Selection.validateProofDigest(); err != nil {
				t.Fatalf("control proof was not internally recomputed: %v", err)
			}
			if err = s.SavePending(id, q); err == nil {
				t.Fatal("empty-slot save accepted recomputed foreign owner")
			}
			if _, err = os.Lstat(s.pendingPath(id)); !os.IsNotExist(err) {
				t.Fatalf("rejected foreign save left a pending file: %v", err)
			}
			if err = s.SavePending(id, p); err != nil {
				t.Fatalf("unchanged owned first save refused: %v", err)
			}
			loaded, found, err := s.LoadPending(id)
			if err != nil || !found || loaded.Bundle.ParentSessionID != p.Bundle.ParentSessionID || loaded.Bundle.NativeChild != p.Bundle.NativeChild || *loaded.Selection != *p.Selection {
				t.Fatalf("owned positive control failed: %v", err)
			}
		})
	}
}
