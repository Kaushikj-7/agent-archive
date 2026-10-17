package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestOrdinaryPrivacyMaintenanceCannotCertifyHistoryAuthority(t *testing.T) {
	t.Parallel()
	local, cloud := newTestStore(t), storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishCodexSession(t, local, cloud, at)
	simulateFilterUpgrade(t, local)
	reg, _, err := local.LoadRegistration("session-1")
	if err != nil {
		t.Fatal(err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := newSessionScan(t.Context(), local, cloud, reg, state.Request{}, published, at.Add(time.Hour), Options{MachineID: "m"})
	if needsRecovery, err := scan.requiresReferenceRecovery(); err != nil || needsRecovery {
		t.Fatalf("ordinary privacy maintenance lost its retained input: %v %v", needsRecovery, err)
	}
	if _, _, err := published.LastPublishedMetadata(); err == nil {
		t.Fatal("ordinary maintenance certified mismatched history authority")
	}
	var original archive.Metadata
	if err := json.Unmarshal(published.Metadata(), &original); err != nil {
		t.Fatal(err)
	}
	type damageVariant0 string
	const (
		damageSchema0    damageVariant0 = "schema"
		damageHistory0   damageVariant0 = "history"
		damageNative0    damageVariant0 = "native"
		damageProject0   damageVariant0 = "project"
		damageHarness0   damageVariant0 = "harness"
		damageParent0    damageVariant0 = "parent"
		damageCapture0   damageVariant0 = "capture"
		damageReference0 damageVariant0 = "reference"
		damageOwner0     damageVariant0 = "owner"
	)
	for _, damage := range []damageVariant0{damageSchema0, damageHistory0, damageNative0, damageProject0, damageHarness0, damageParent0, damageCapture0, damageReference0, damageOwner0} {
		t.Run(string(damage), func(t *testing.T) {
			t.Parallel()
			isolated := newTestStore(t)
			isolatedPublished, err := isolated.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			bundle, when, _ := published.LastPublished()
			if err := isolatedPublished.SavePublication(bundle, when, original.SourceBundle, published.Metadata()); err != nil {
				t.Fatal(err)
			}
			scan := newSessionScan(t.Context(), isolated, cloud, reg, state.Request{}, isolatedPublished, at.Add(time.Hour), Options{MachineID: "m"})
			metadata := original
			switch damage {
			case damageSchema0:
				metadata.SchemaVersion = 99
			case damageHistory0:
				metadata.History = &archive.RevisionHistory{}
			case damageNative0:
				metadata.NativeSessionID = "another-thread"
			case damageProject0:
				metadata.ProjectID = "another-project"
			case damageHarness0:
				metadata.Harness.Name = "claude-code"
			case damageParent0:
				metadata.ParentSessionID = "another-parent"
			case damageCapture0:
				metadata.CapturedAt = at.Add(time.Minute)
			case damageReference0:
				metadata.SourceBundle.SHA256 = "another-checksum"
			case damageOwner0:
				metadata.MachineID = "another-machine"
			}
			raw, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := isolatedPublished.CacheMetadata(raw); err != nil {
				t.Fatal(err)
			}
			if scan.ordinaryPrivacyMaintenance() {
				t.Fatal("uncertain metadata entered ordinary maintenance")
			}
		})
	}
}

func TestRunRecoversWholeHistoryAuthorityAfterStateLoss(t *testing.T) {
	t.Parallel()
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "corrupt"}[corrupt], func(t *testing.T) {
			t.Parallel()
			producer, pending := privacyJournal(t)
			if _, err := producer.publishPending(producer.ctx, pending); err != nil {
				t.Fatal("real complete history publication", err)
			}
			local := newTestStore(t)
			cfg, _, err := config.Load(producer.local.Home())
			if err != nil {
				t.Fatal(err)
			}
			if err := config.Save(local.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := local.SaveRegistration(producer.reg); err != nil {
				t.Fatal(err)
			}
			remote := &privacyPutStore{ObjectStore: producer.remote}
			before, err := remote.Get(t.Context(), pending.MetadataKey)
			if err != nil {
				t.Fatal(err)
			}
			opts := producer.opts
			if corrupt {
				path := publishedPath(local, producer.id())
				bad := []byte("{")
				if err := os.WriteFile(path, bad, 0600); err != nil {
					t.Fatal(err)
				}
				if err := local.SaveRequest(producer.id(), "stop", producer.now); err != nil {
					t.Fatal(err)
				}
				request, err := os.ReadFile(requestPath(local, producer.id()))
				if err != nil {
					t.Fatal(err)
				}
				first, err := Run(t.Context(), local, remote, opts)
				if err != nil || !errors.Is(first.Errors[producer.id()], state.ErrQuarantined) || len(first.Published) != 0 {
					t.Fatalf("corrupt authority not quarantined: %+v %v", first, err)
				}
				files := local.QuarantinedFiles()
				if len(files) != 1 {
					t.Fatal("corrupt authority lost", files)
				}
				raw, err := os.ReadFile(filepath.Join(local.Home(), files[0]))
				if err != nil || !bytes.Equal(raw, bad) {
					t.Fatal("quarantined bytes changed", err)
				}
				retry, err := Run(t.Context(), local, remote, opts)
				if !errors.Is(err, state.ErrDurableStorageRecovery) || len(retry.Published) != 0 {
					t.Fatalf("anonymous corruption not owed: %+v %v", retry, err)
				}
				raw, err = os.ReadFile(requestPath(local, producer.id()))
				if err != nil || !bytes.Equal(raw, request) {
					t.Fatal("owed request changed", err)
				}
			} else {
				result, err := Run(t.Context(), local, remote, opts)
				if err != nil {
					t.Fatalf("missing authority recovery: %+v %v", result, err)
				}
				published, err := local.LoadPublishedState(producer.id())
				if err != nil {
					t.Fatal(err)
				}
				metadata, found, err := published.LastPublishedMetadata()
				if err != nil || !found || metadata.History == nil || len(metadata.History.Preserved) != 2 || !bytes.Equal(published.Metadata(), before) {
					t.Fatalf("incomplete restored baseline: %#v %v %v result=%+v", metadata, found, err, result)
				}
				if len(result.Errors) != 0 {
					t.Fatalf("restored complete baseline followup: %+v", result)
				}
			}
			after, err := remote.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(after, before) || len(remote.puts) != 0 {
				t.Fatal("state loss guessed or replaced remote authority", err, remote.puts)
			}
		})
	}
}

func TestRunRefusesUncertainRemoteAuthorityWithoutCachingOrMutation(t *testing.T) {
	t.Parallel()
	type damageVariant1 string
	const (
		damageMissing1          damageVariant1 = "missing"
		damageChecksum1         damageVariant1 = "checksum"
		damageWrongThread1      damageVariant1 = "wrong_thread"
		damageFutureSchema1     damageVariant1 = "future_schema"
		damageForeignOwner1     damageVariant1 = "foreign_owner"
		damageFilterProvenance1 damageVariant1 = "filter_provenance"
		damageFormatProvenance1 damageVariant1 = "format_provenance"
	)
	for _, damage := range []damageVariant1{damageMissing1, damageChecksum1, damageWrongThread1, damageFutureSchema1, damageForeignOwner1, damageFilterProvenance1, damageFormatProvenance1} {
		t.Run(string(damage), func(t *testing.T) {
			t.Parallel()
			scan, pending, cloud, previous := frozenHistoryFixture(t)
			if err := scan.local.SaveRegistration(scan.reg); err != nil {
				t.Fatal(err)
			}
			if err := scan.local.RemovePending(scan.id()); err != nil {
				t.Fatal(err)
			}
			var metadata archive.Metadata
			if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case damageMissing1:
				if err := cloud.Delete(t.Context(), previous.Key); err != nil {
					t.Fatal(err)
				}
			case damageChecksum1:
				if err := cloud.Put(t.Context(), previous.Key, []byte("corrupt")); err != nil {
					t.Fatal(err)
				}
			case damageWrongThread1:
				foreign := pending.Bundle
				foreign.NativeSessionID = "33333333-3333-4333-8333-333333333333"
				foreign.Capture.CapturedAt = metadata.History.Preserved[0].CapturedAt
				packed, err := archive.BuildCompressedSource(foreign)
				if err != nil {
					t.Fatal(err)
				}
				key, err := archive.SourceObjectKey(foreign, packed.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				if err := cloud.Put(t.Context(), key, packed.Bytes); err != nil {
					t.Fatal(err)
				}
				metadata.History.Preserved[0].Source = archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
			case damageFutureSchema1:
				metadata.SchemaVersion = 99
				metadata.History = nil
			case damageForeignOwner1:
				metadata.MachineID = "another"
			case damageFilterProvenance1:
				metadata.History.Preserved[0].FilterVersion = "different"
			case damageFormatProvenance1:
				metadata.History.Preserved[0].SourceSchemaVersion = 3
			}
			raw, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := cloud.Put(t.Context(), pending.MetadataKey, raw); err != nil {
				t.Fatal(err)
			}
			result, err := Run(t.Context(), scan.local, cloud, Options{MachineID: "m", Sources: testSources, Parsers: testParsers, Now: func() time.Time { return scan.now }})
			if err != nil || result.Errors[scan.id()] == nil {
				t.Fatalf("uncertain authority accepted: %#v %v", result, err)
			}
			published, err := scan.local.LoadPublishedState(scan.id())
			if err != nil || published.Found() {
				t.Fatal("cached partial authority", err)
			}
			unchanged, err := cloud.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(raw, unchanged) {
				t.Fatal("uncertain remote mutated", err)
			}
		})
	}
}

func TestAuthorityRecoveryPreservesNewerLocalCandidate(t *testing.T) {
	t.Parallel()
	scan, pending, cloud, _ := frozenHistoryFixture(t)
	if err := cloud.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	published, err := scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	newer := pending.Bundle
	newer.Capture.CapturedAt = scan.now.Add(time.Hour)
	newer.NativeRecords = append(newer.NativeRecords, map[string]any{"type": "synthetic_new"})
	if err := published.Save(newer, scan.now, state.CacheStatusRateLimited); err != nil {
		t.Fatal(err)
	}
	scan.published = published
	if err := scan.recoverReferenceAuthority(); err != nil {
		t.Fatal(err)
	}
	cached, _, status, _ := published.Cached()
	if status != state.CacheStatusRateLimited || len(cached.NativeRecords) != 2 || !cached.Capture.CapturedAt.Equal(newer.Capture.CapturedAt) {
		t.Fatal("newer evidence lost")
	}
	if metadata, found, err := published.LastPublishedMetadata(); err != nil || !found || len(metadata.History.Preserved) != 1 {
		t.Fatalf("acknowledged set missing: %v %v", found, err)
	}
}
