package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestRealCLICatalogNativeChildMatchesLegacyDiscoveryAndSource(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		for _, linked := range []bool{false, true} {
			t.Run(fmt.Sprintf("overflow=%v/linked=%v", overflow, linked), func(t *testing.T) {
				env, legacy, id := publishedFixture(t)
				key, err := archive.MetadataObjectKey("codex", id)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := legacy.Get(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				var child archive.Metadata
				if err = json.Unmarshal(raw, &child); err != nil {
					t.Fatal(err)
				}
				child.NativeChild = true
				child.Title = "Independent native child"
				// The supported legacy source marker is absent even after metadata learns
				// native ownership. Readers must not add an equality requirement here.
				bundle, err := reader.LoadSource(t.Context(), legacy, child, reader.Limits{})
				if err != nil || bundle.NativeChild {
					t.Fatal("legacy optional source marker", err)
				}
				if linked {
					child.ParentSessionID = "known-parent"
					bundle.ParentSessionID = child.ParentSessionID
					packed, err := archive.BuildCompressedSource(bundle)
					if err != nil {
						t.Fatal(err)
					}
					sourceKey, err := archive.SourceObjectKey(bundle, packed.SHA256)
					if err != nil {
						t.Fatal(err)
					}
					if err = legacy.Put(t.Context(), sourceKey, packed.Bytes); err != nil {
						t.Fatal(err)
					}
					child.SourceBundle = archive.SourceReference{Key: sourceKey, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
				}
				if overflow {
					for n := range 2000 {
						child.LinkedSessions = append(child.LinkedSessions, archive.LinkedSessionReference{SessionID: fmt.Sprintf("linked-%04d", n), Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: child.CapturedAt})
					}
				}
				raw, err = json.Marshal(child)
				if err != nil {
					t.Fatal(err)
				}
				if err = legacy.Put(t.Context(), key, raw); err != nil {
					t.Fatal(err)
				}
				parent := archive.Metadata{SchemaVersion: 1, SessionID: "known-parent", Harness: archive.Harness{Name: "codex"}, ProjectID: child.ProjectID, CapturedAt: child.CapturedAt, Title: "Known parent"}
				parentBundle := bundle
				parentBundle.ArchiveSessionID = parent.SessionID
				parentBundle.NativeSessionID = "native-known-parent"
				parentBundle.NativeChild = false
				parentBundle.ParentSessionID = ""
				packed, err := archive.BuildCompressedSource(parentBundle)
				if err != nil {
					t.Fatal(err)
				}
				sourceKey, err := archive.SourceObjectKey(parentBundle, packed.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				if err = legacy.Put(t.Context(), sourceKey, packed.Bytes); err != nil {
					t.Fatal(err)
				}
				parent.NativeSessionID = parentBundle.NativeSessionID
				parent.Harness = parentBundle.Capture.Harness
				parent.SourceBundle = archive.SourceReference{Key: sourceKey, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
				raw, err = json.Marshal(parent)
				if err != nil {
					t.Fatal(err)
				}
				if err = legacy.Put(t.Context(), "sessions/codex/known-parent/metadata.json", raw); err != nil {
					t.Fatal(err)
				}
				remote := privateCatalogFromLegacy(t, legacy)
				oracle := env
				env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
				for _, warm := range []bool{false, true} {
					for _, args := range [][]string{
						{"list", "--all-projects", "--json"},
						{"show", id, "--json"},
						{"show", id, "--transcript", "--no-pager"},
						{"show", "Independent", "--json"},
						{"stats", "--days", "1", "--all", "--json"},
					} {
						var got, errs, want, wantErr bytes.Buffer
						code := Run(args, nil, &got, &errs, env)
						wantCode := Run(args, nil, &want, &wantErr, oracle)
						if code != 0 || wantCode != 0 || got.String() != want.String() || errs.String() != wantErr.String() {
							t.Fatalf("warm=%v %v native oracle differs: code=%d/%d err=%s/%s", warm, args, code, wantCode, errs.String(), wantErr.String())
						}
					}
				}
			})
		}
	}
}
