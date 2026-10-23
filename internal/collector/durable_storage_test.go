package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type refuseNativeSources struct{ t *testing.T }

type durableConfigMode string

const (
	durableConfigModeProtected durableConfigMode = "protected"
	durableConfigModeFuture    durableConfigMode = "future"
	durableConfigModeMissing   durableConfigMode = "missing"
	durableConfigModeCorrupt   durableConfigMode = "corrupt"
	durableConfigModeLegacy    durableConfigMode = "legacy"
)

type durableNamingMode string

const (
	durableNamingModeMissing       durableNamingMode = "missing"
	durableNamingModeCorrupt       durableNamingMode = "corrupt"
	durableNamingModeLeadingHeader durableNamingMode = "leading-header"
	durableNamingModeChangedWarm   durableNamingMode = "changed-warm"
)

type durableSessionMode string

const (
	durableSessionModeSourceOnly        durableSessionMode = "source-only"
	durableSessionModeOpaque            durableSessionMode = "opaque"
	durableSessionModeMissingPending    durableSessionMode = "missing-pending"
	durableSessionModeMissingSource     durableSessionMode = "missing-source"
	durableSessionModeCorruptPending    durableSessionMode = "corrupt-pending"
	durableSessionModeCanceled          durableSessionMode = "canceled"
	durableSessionModeEmptyPending      durableSessionMode = "empty-pending"
	durableSessionModeNullPending       durableSessionMode = "null-pending"
	durableSessionModeIncompletePending durableSessionMode = "incomplete-pending"
	durableSessionModeChecksumPending   durableSessionMode = "checksum-pending"
	durableSessionModeMetadataPending   durableSessionMode = "metadata-pending"
)

func (s refuseNativeSources) LookupSources(string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	s.t.Fatal("refused durable work opened a native provider/filter")
	return nil, nil, false
}

func TestLocalBundleFuturePublishedRefusesBeforeNativeFilter(t *testing.T) {
	s := newTestStore(t)
	if err := os.Remove(filepath.Join(s.Home(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Home(), "published", "foreign.json"), []byte(`{"commit":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "foreign", Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/never-open"}
	bundle, err := ReadLocalBundle(t.Context(), s.Home(), reg, time.Now(), "", refuseNativeSources{t})
	if !errors.Is(err, state.ErrDurableStorageRecovery) || bundle.ArchiveSessionID != "" {
		t.Fatalf("foreign preview emitted: %+v %v", bundle, err)
	}
}

func TestCollectorSourceOnlyRecoveryWithoutRegistration(t *testing.T) {
	s := newTestStore(t)
	data := []byte("frozen-original")
	sum := sha256.Sum256(data)
	ref := archive.SourceReference{SHA256: hex.EncodeToString(sum[:]), Key: "synthetic", CompressedBytes: len(data)}
	stage, err := s.StagePendingSource("orphan", ref, data)
	if err != nil {
		t.Fatal(err)
	}
	loads := state.PublishedStateLoads()
	budget := agentapi.NewNativeReadBudget(128 << 20)
	result, err := Run(t.Context(), s, storagetest.NewMemoryStore(), Options{MachineID: "synthetic", Sources: refuseNativeSources{t}, readBudgetOwner: budget})
	defer result.CloseStagedResults()
	if err != nil || !errors.Is(result.Errors["orphan"], state.ErrDurableStorageRecovery) || result.Scanned != 0 || len(result.Published) != 0 {
		t.Fatalf("orphan collection: %+v %v", result, err)
	}
	status, err := s.LoadStatus()
	if err != nil || status.PendingCount != 1 {
		t.Fatalf("orphan status: %+v %v", status, err)
	}
	raw, err := s.ReadPendingSource("orphan", stage)
	if err != nil || string(raw) != string(data) {
		t.Fatal("original lost", err)
	}
	if state.PublishedStateLoads() != loads {
		t.Fatal("source-only pass decoded published bodies")
	}
	if used, _ := budget.Charged(); used == 0 {
		t.Fatal("durable diagnostic ID escaped its result owner")
	}
	alias := result
	if !errors.Is(alias.Errors["orphan"], state.ErrDurableStorageRecovery) {
		t.Fatal("result alias lost orphan")
	}
	alias.CloseStagedResults()
	result.CloseStagedResults()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("durable result close leaked charge", used)
	}
	t.Log("source-only cost: native opens=0, filters=0, published body decodes=0, registration fabricated=0")
}

func TestCollectorAnonymousTempsRefuseBeforeNativeAndRemainCharged(t *testing.T) {
	for _, mode := range []durableConfigMode{durableConfigModeProtected, durableConfigModeFuture, durableConfigModeMissing, durableConfigModeCorrupt, durableConfigModeLegacy} {
		t.Run(string(mode), func(t *testing.T) {
			for _, name := range []string{".pending-123", ".pending-abcdef0123456789abcdef0123456789"} {
				t.Run(name, func(t *testing.T) {
					s := newTestStore(t)
					if mode == durableConfigModeProtected {
						if err := config.WithDurableStorage(s.Home(), func(g config.DurableStorageGuard) error { return g.CheckHome(s.Home()) }); err != nil {
							t.Fatal(err)
						}
					}
					switch mode {
					case durableConfigModeProtected, durableConfigModeLegacy: // This mode needs no additional fixture mutation.
					case durableConfigModeFuture:
						if err := os.WriteFile(filepath.Join(s.Home(), "config.json"), []byte(`{"schema_version":{"version":8,"writer":"publication-composition-v8"},"publication_composition_protection":true}`), 0600); err != nil {
							t.Fatal(err)
						}
					case durableConfigModeMissing:
						if err := os.Remove(filepath.Join(s.Home(), "config.json")); err != nil {
							t.Fatal(err)
						}
					case durableConfigModeCorrupt:
						if err := os.WriteFile(filepath.Join(s.Home(), "config.json"), []byte("{"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					path := filepath.Join(s.Home(), "pending", name)
					if err := os.WriteFile(path, []byte("sole-original"), 0600); err != nil {
						t.Fatal(err)
					}
					past := time.Now().Add(-2 * time.Hour)
					if err := os.Chtimes(path, past, past); err != nil {
						t.Fatal(err)
					}
					result, err := Run(t.Context(), s, storagetest.NewMemoryStore(), Options{MachineID: "synthetic", Sources: refuseNativeSources{t}})
					if mode == durableConfigModeLegacy {
						if err != nil {
							t.Fatal(err)
						}
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatalf("legacy temp not cleaned: %v", err)
						}
						return
					}
					if !errors.Is(err, state.ErrDurableStorageRecovery) || result.Scanned != 0 || len(result.Published) != 0 {
						t.Fatalf("anonymous collection: %+v %v", result, err)
					}
					s.RemoveStaleTemps()
					raw, err := os.ReadFile(path)
					if err != nil || string(raw) != "sole-original" {
						t.Fatal("original discarded", err)
					}
					if mode == durableConfigModeProtected {
						data := []byte("new")
						sum := sha256.Sum256(data)
						_, err = s.StagePendingSource("new", archive.SourceReference{Key: "synthetic", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(data)}, data)
						if !errors.Is(err, state.ErrDurableStorageRecovery) {
							t.Fatalf("unknown temp not charged/refused: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestLocalBundleAnonymousTempRefusesBeforeNativeFilter(t *testing.T) {
	s := newTestStore(t)
	if err := os.WriteFile(filepath.Join(s.Home(), "pending", ".pending-123"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "absent", Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/never-open"}
	bundle, err := ReadLocalBundle(t.Context(), s.Home(), reg, time.Now(), "", refuseNativeSources{t})
	if !errors.Is(err, state.ErrDurableStorageRecovery) || bundle.ArchiveSessionID != "" {
		t.Fatalf("anonymous preview emitted: %+v %v", bundle, err)
	}
}

func TestFuturePublishedNamingRefusesBeforeLookupAndNativeFilter(t *testing.T) {
	for _, mode := range []durableNamingMode{durableNamingModeMissing, durableNamingModeCorrupt, durableNamingModeLeadingHeader, durableNamingModeChangedWarm} {
		t.Run(string(mode), func(t *testing.T) {
			s := newTestStore(t)
			path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
			reg := registration(t, path)
			reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := storagetest.NewMemoryStore()
			now := reg.RegisteredAt.Add(time.Hour)
			provider := &mutableLabels{}
			opts := Options{MachineID: "machine", Sources: testSources, Parsers: testParsers, Now: func() time.Time { return now }}
			if result, err := Run(t.Context(), s, remote, opts); err != nil || len(result.Errors) != 0 {
				t.Fatal(result, err)
			}
			opts.Labels = mutableLabelLookup{provider}
			now = now.Add(time.Hour)
			if result, err := Run(t.Context(), s, remote, opts); err != nil || len(result.Errors) != 0 {
				t.Fatal(result, err)
			}
			provider.calls = 0
			p := filepath.Join(s.Home(), "published", reg.ArchiveSessionID+".json")
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			raw = bytes.TrimSpace(raw)
			if mode == durableNamingModeLeadingHeader {
				raw = append([]byte(`{"publication_version":2,`), raw[1:]...)
			} else {
				raw = append(raw[:len(raw)-1], []byte(`,"commit":null}`)...)
			}
			if err := os.WriteFile(p, raw, 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case durableNamingModeLeadingHeader, durableNamingModeChangedWarm: // This mode needs no additional fixture mutation.
			case durableNamingModeMissing:
				if err := os.Remove(filepath.Join(s.Home(), "config.json")); err != nil {
					t.Fatal(err)
				}
			case durableNamingModeCorrupt:
				if err := os.WriteFile(filepath.Join(s.Home(), "config.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			opts.Sources = refuseNativeSources{t}
			now = now.Add(time.Hour)
			result, err := Run(t.Context(), s, remote, opts)
			if !errors.Is(err, state.ErrDurableStorageRecovery) && !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrDurableStorageRecovery) {
				t.Fatalf("recovery quietly skipped: %+v %v", result, err)
			}
			if provider.calls != 0 || len(result.Published) != 0 {
				t.Fatal("foreign publication launched label lookup or publication")
			}
			after, err := os.ReadFile(p)
			if err != nil || string(after) != string(raw) {
				t.Fatal("foreign naming state changed", err)
			}
		})
	}
}

func TestLocalBundleSessionObligationsRefuseBeforeNative(t *testing.T) {
	for _, mode := range []durableSessionMode{durableSessionModeSourceOnly, durableSessionModeOpaque, durableSessionModeMissingPending, durableSessionModeMissingSource, durableSessionModeCorruptPending, durableSessionModeCanceled} {
		t.Run(string(mode), func(t *testing.T) {
			s := newTestStore(t)
			switch mode {
			case durableSessionModeEmptyPending, durableSessionModeNullPending, durableSessionModeIncompletePending, durableSessionModeChecksumPending, durableSessionModeMetadataPending:
				t.Fatalf("incomplete fixture belongs to the protected preview regression: %s", mode)
			case durableSessionModeCanceled: // This mode needs no additional fixture mutation.
			case durableSessionModeSourceOnly, durableSessionModeMissingSource:
				data := []byte("original")
				sum := sha256.Sum256(data)
				if _, err := s.StagePendingSource("session", archive.SourceReference{SHA256: hex.EncodeToString(sum[:]), Key: "synthetic", CompressedBytes: len(data)}, data); err != nil {
					t.Fatal(err)
				}
			case durableSessionModeOpaque:
				path := filepath.Join(s.Home(), "publication-evidence", "session", "opaque")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
			case durableSessionModeMissingPending, durableSessionModeCorruptPending:
				if err := os.WriteFile(filepath.Join(s.Home(), "pending", "session.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == durableSessionModeMissingPending || mode == durableSessionModeMissingSource {
				if err := os.Remove(filepath.Join(s.Home(), "config.json")); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			if mode == durableSessionModeCanceled {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			reg := archive.SessionRegistration{ArchiveSessionID: "session", Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/never-open"}
			bundle, err := ReadLocalBundle(ctx, s.Home(), reg, time.Now(), "", refuseNativeSources{t})
			if !errors.Is(err, state.ErrDurableStorageRecovery) || bundle.ArchiveSessionID != "" {
				t.Fatalf("unknown preview emitted: %+v %v", bundle, err)
			}
			if mode == durableSessionModeCanceled && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
		})
	}
}

func TestLocalBundleMalformedLocatorRefusesBeforeNative(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"abc\n\x1b[2Jrun-this" + strings.Repeat("y", 10_000), strings.Repeat("λ", 126)} {
		reg := archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/never-open"}
		bundle, err := ReadLocalBundle(context.Background(), t.TempDir(), reg, time.Now(), "", refuseNativeSources{t})
		if !errors.Is(err, state.ErrDurableStorageRecovery) || err.Error() != state.ErrDurableStorageRecovery.Error() || bundle.ArchiveSessionID != "" {
			t.Fatalf("malformed locator read authority: %v %+v", err, bundle)
		}
	}
}
