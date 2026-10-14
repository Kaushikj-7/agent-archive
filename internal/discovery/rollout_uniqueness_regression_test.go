package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// Only real parsed native header facts enter the actual lookup.
func uniquenessObserve(t *testing.T, lookup *CodexRolloutLookup, root, path string) {
	t.Helper()
	header := sourcefacts.ReadHeader(t.Context(), root, path)
	if header.Identity == nil {
		t.Fatal("fixture header unavailable", header.Outcome)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	lookup.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: header.Identity.RolloutID}, Fingerprint{Size: info.Size(), Mtime: info.ModTime().UnixNano()}, header.Identity)
}

func uniquenessClose(t *testing.T, pass agentapi.SourcePass, lookup *CodexRolloutLookup) {
	t.Helper()
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
		t.Fatal("shared source/lookup charge leaked", used)
	}
}

// Fresh actual passes delimit each phase; this does not assert fresh directory
// membership after a listing was already cached in a pass.
func TestRolloutUniquenessRealProviderCausal(t *testing.T) {
	for _, scenario := range []string{"unobserved-copy", "both-observed", "archive-not-directory", "other-home-not-directory", "other-home-copy", "hardlink-copy"} {
		t.Run(scenario, func(t *testing.T) {
			root, other := chainRoot(t), chainRoot(t)
			thread, basePath, _, tipPath := twoSegmentThread(t, root, 730, "")
			db := hintDatabase(t, root, false)
			addHint(t, db, thread, tipPath, chainEpoch)
			baseRaw, err := os.ReadFile(basePath)
			if err != nil {
				t.Fatal(err)
			}
			tipRaw, err := os.ReadFile(tipPath)
			if err != nil {
				t.Fatal(err)
			}
			store, _, _, _ := fixture(t)
			newLookup := func() *CodexRolloutLookup {
				t.Helper()
				homes := []string{root}
				if scenario == "other-home-not-directory" || scenario == "other-home-copy" {
					homes = append(homes, other)
				}
				lookup, err := NewCodexRolloutLookup(t.Context(), store, homes)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lookup.CloseReadOnly() })
				uniquenessObserve(t, lookup, root, basePath)
				return lookup
			}
			ref := agentapi.SourceRef{Kind: archive.SourceKindFile, Path: tipPath, Key: thread}
			positive := func() {
				t.Helper()
				lookup := newLookup()
				refs, err := lookup.Rollout(t.Context(), thread)
				if err != nil || len(refs) != 1 || refs[0].Path != basePath || refs[0].Key != thread {
					t.Fatal("positive actual lookup", refs, err)
				}
				pass := chainPass(t, root, lookup)
				observed, err := pass.Signature(t.Context(), ref)
				if err != nil {
					t.Fatal("positive actual signature", err)
				}
				snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
				if err != nil {
					t.Fatal("positive actual graph read", err)
				}
				if snapshot.Observation().Signature != observed.Signature {
					t.Fatal("signature/read disagreement")
				}
				if got := revisionPaths(t, snapshot); !slices.Equal(got, []string{basePath, tipPath}) {
					t.Fatal("incomplete graph", got)
				}
				input := snapshot.Input().Records
				if input == nil {
					t.Fatal("history record input missing")
				}
				var raw bytes.Buffer
				records, spans := 0, 0
				for {
					record, more, err := input.Next(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if !more {
						break
					}
					if record.History != nil {
						spans = len(record.History.Spans)
					} else {
						records++
						raw.Write(record.Raw)
					}
				}
				want := bytes.ReplaceAll(append(slices.Clone(baseRaw), tipRaw...), []byte("\n"), nil)
				if spans != 2 || records != 4 || !bytes.Equal(bytes.ReplaceAll(raw.Bytes(), []byte("\n"), nil), want) {
					t.Fatal("complete graph records differ", spans, records)
				}
				if err := snapshot.Close(); err != nil {
					t.Fatal(err)
				}
				uniquenessClose(t, pass, lookup)
			}
			positive()
			t.Log("UNIQUENESS_POSITIVE", scenario)
			badRoot := root
			if scenario == "other-home-not-directory" || scenario == "other-home-copy" {
				badRoot = other
			}
			archivePath := filepath.Join(badRoot, "archived_sessions")
			duplicate := filepath.Join(archivePath, filepath.Base(basePath))
			uncertain := scenario == "archive-not-directory" || scenario == "other-home-not-directory"
			if uncertain {
				if err := os.WriteFile(archivePath, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(archivePath, 0700); err != nil {
					t.Fatal(err)
				}
				if scenario == "hardlink-copy" {
					if err := os.Link(basePath, duplicate); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(duplicate, baseRaw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			lookup := newLookup()
			if scenario == "both-observed" {
				uniquenessObserve(t, lookup, badRoot, duplicate)
			}
			refs, lookupErr := lookup.Rollout(t.Context(), thread)
			t.Logf("UNIQUENESS_LOOKUP_RESULT case=%s err_nil=%t err_type=%T unavailable=%t refs=%d", scenario, lookupErr == nil, lookupErr, agentapi.HasFailure(lookupErr, agentapi.Unavailable), len(refs))
			if uncertain {
				if lookupErr == nil || !agentapi.HasFailure(lookupErr, agentapi.Unavailable) || agentapi.Failure(lookupErr) != agentapi.Unavailable || len(refs) != 0 {
					t.Error("accepted relevant-store uncertainty", refs, lookupErr)
				}
			} else if lookupErr != nil || len(refs) != 2 {
				t.Error("ignored physical candidate", refs, lookupErr)
			}
			pass := chainPass(t, root, lookup)
			_, signatureErr := pass.Signature(t.Context(), ref)
			t.Logf("UNIQUENESS_SIGNATURE_RESULT case=%s err_nil=%t err_type=%T unavailable=%t", scenario, signatureErr == nil, signatureErr, agentapi.HasFailure(signatureErr, agentapi.Unavailable))
			if signatureErr == nil || !agentapi.HasFailure(signatureErr, agentapi.Unavailable) || agentapi.Failure(signatureErr) != agentapi.Unavailable {
				t.Error("signature accepted unresolved base", signatureErr)
			}
			snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			t.Logf("UNIQUENESS_READ_RESULT case=%s err_nil=%t err_type=%T unavailable=%t snapshot_nil=%t snapshot_type=%T", scenario, err == nil, err, agentapi.HasFailure(err, agentapi.Unavailable), snapshot == nil, snapshot)
			if snapshot != nil {
				_ = snapshot.Close()
				t.Error("returned snapshot with unresolved base")
			}
			if err == nil || !agentapi.HasFailure(err, agentapi.Unavailable) || agentapi.Failure(err) != agentapi.Unavailable {
				t.Error("graph accepted unresolved base", err)
			}
			uniquenessClose(t, pass, lookup)
			t.Log("UNIQUENESS_REFUSAL_CHECKED", scenario)
			remove := duplicate
			if uncertain {
				remove = archivePath
			}
			if err := os.Remove(remove); err != nil {
				t.Fatal(err)
			}
			positive()
			t.Log("UNIQUENESS_RESTORED", scenario)
		})
	}
}

func TestRolloutUniquenessCancellationAndClose(t *testing.T) {
	// Failure is a classification convenience, not an error-existence test.
	if agentapi.Failure(nil) != agentapi.Unavailable || agentapi.HasFailure(nil, agentapi.Unavailable) {
		t.Fatal("actual failure-classification nil contract changed")
	}
	root := chainRoot(t)
	lookup := chainLookup(t, root)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := lookup.Rollout(ctx, v7ID(chainEpoch, 731)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup.Rollout(t.Context(), v7ID(chainEpoch, 731)); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal(err)
	}
	if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
		t.Fatal("lookup charge leaked", used)
	}
}

func TestRolloutUniquenessDeduplicatesAtPhysicalLimit(t *testing.T) {
	root := chainRoot(t)
	id := v7ID(chainEpoch, 732)
	path, raw := chainRollout(t, root, "archived_sessions", id, id, 0, nil, "base")
	lookup := chainLookup(t, root)
	uniquenessObserve(t, lookup, root, path)
	for n := 1; n < archive.MaxHistorySpans; n++ {
		// Different permitted time spellings name the same physical ID.
		name := filepath.Join(filepath.Dir(path), strings.Replace(filepath.Base(path), "12-00-00", fmt.Sprintf("12-00-%02d", n), 1))
		if err := os.WriteFile(name, raw, 0600); err != nil {
			t.Fatal(err)
		}
		uniquenessObserve(t, lookup, root, name)
	}
	refs, err := lookup.Rollout(t.Context(), id)
	if err != nil || len(refs) != archive.MaxHistorySpans {
		t.Fatal("counted observed/located overlap twice", len(refs), err)
	}
	for _, ref := range refs {
		if ref.Key != id {
			t.Fatal("lost observed thread hint", ref)
		}
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
		t.Fatal("charge leaked", used)
	}
}
