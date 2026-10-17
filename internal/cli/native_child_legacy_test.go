package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This shape was verified against the main writer before native_child existed:
// positive private Child binding, absent public markers and Codex parser0.22.
// One preserved ordinary source also pins supported mixed source2/source3 sets.
func legacyNativeChildEnvelope(t *testing.T, local *state.Store, cloud *storagetest.MemoryStore, reg archive.SessionRegistration, metadata archive.Metadata, sources map[string]archive.SourceBundle, parser string) archive.Metadata {
	t.Helper()
	// The caller's first registration predates the provider's binding persistence.
	admitted, found, err := local.LoadRegistration(reg.ArchiveSessionID)
	must(t, err)
	if !found {
		t.Fatal("legacy admitted owner missing")
	}
	binding := admitted.CodexBinding
	if binding == nil || !binding.Child || binding.Validate() != nil {
		t.Fatal("legacy child must have a real persisted native binding")
	}
	withoutPending := func(gaps []archive.CaptureGap) []archive.CaptureGap {
		var out []archive.CaptureGap
		for _, gap := range gaps {
			if gap.Code != "native_parent_link_pending" {
				out = append(out, gap)
			}
		}
		return out
	}
	for id, bundle := range sources {
		bundle.NativeChild = false
		bundle.Capture.Gaps = withoutPending(bundle.Capture.Gaps)
		if id != metadata.History.CurrentRevision {
			bundle.SchemaVersion, bundle.History, bundle.Ordinals = archive.SourceSchemaVersion, nil, nil
		}
		packed, err := archive.BuildCompressedSource(bundle)
		must(t, err)
		key, err := archive.SourceObjectKey(bundle, packed.SHA256)
		must(t, err)
		ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
		must(t, cloud.Put(t.Context(), key, packed.Bytes))
		sources[id] = bundle
		if id == metadata.History.CurrentRevision {
			metadata.SourceBundle = ref
		} else {
			for i := range metadata.History.Preserved {
				if metadata.History.Preserved[i].RevisionID == id {
					metadata.History.Preserved[i].Source = ref
					metadata.History.Preserved[i].SourceSchemaVersion = bundle.SchemaVersion
				}
			}
		}
	}
	metadata.NativeChild, metadata.Parser.Version = false, parser
	metadata.CaptureGaps = withoutPending(metadata.CaptureGaps)
	raw, err := json.Marshal(metadata)
	must(t, err)
	key, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	must(t, err)
	must(t, cloud.Put(t.Context(), key, raw))
	// Create the complete older producer shape through its validated persistence
	// API in a fresh private scope. Never prune a current selecting Commit into legacy.
	cfg, _, err := config.Load(local.Home())
	must(t, err)
	legacyHome := t.TempDir()
	must(t, os.Chmod(legacyHome, 0700))
	must(t, config.Save(legacyHome, cfg))
	legacyStore, err := state.Open(legacyHome)
	must(t, err)
	must(t, legacyStore.SaveRegistration(admitted))
	published, err := legacyStore.LoadPublishedState(reg.ArchiveSessionID)
	must(t, err)
	must(t, published.SavePublication(sources[metadata.History.CurrentRevision], metadata.CapturedAt, metadata.SourceBundle, raw))
	// Ownership interpretation can upgrade before retained source headers do.
	// Both the public reader and acknowledged-state port retain this supported
	// legacy shape; exact identity, reference and parent checks still apply.
	upgraded := metadata
	upgraded.NativeChild = true
	_, err = reader.LoadSource(t.Context(), cloud, upgraded, reader.Limits{})
	must(t, err)
	for _, revision := range upgraded.History.Preserved {
		_, err = reader.LoadRevision(t.Context(), cloud, upgraded, revision.RevisionID, reader.Limits{})
		must(t, err)
	}
	upgradedRaw, err := json.Marshal(upgraded)
	must(t, err)
	must(t, published.SavePublication(sources[metadata.History.CurrentRevision], metadata.CapturedAt, metadata.SourceBundle, upgradedRaw))
	ack, found, err := published.LastPublishedMetadata()
	must(t, err)
	if !found || !ack.NativeChild {
		t.Fatal("optional legacy source marker invalidated acknowledged metadata")
	}
	must(t, published.SavePublication(sources[metadata.History.CurrentRevision], metadata.CapturedAt, metadata.SourceBundle, raw))
	legacyRaw, err := os.ReadFile(filepath.Join(legacyHome, "published", reg.ArchiveSessionID+".json"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(local.Home(), "published", reg.ArchiveSessionID+".json"), legacyRaw, 0600))
	_, err = local.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
		current.NativeChild, current.NativeRootSessionID, current.ParentNativeSessionID, current.NativeSourceHome, current.NativeLinkVersion = false, "", "", "", 0
		return nil
	})
	must(t, err)
	signature, found, err := local.LoadScanSignature(reg.ArchiveSessionID)
	must(t, err)
	if !found {
		t.Fatal("legacy settled signature missing")
	}
	signature.SourceSetVersion, signature.ParserVersion = 2, parser
	must(t, local.SaveScanSignature(reg.ArchiveSessionID, signature))
	return metadata
}
