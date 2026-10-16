package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func accountingFixture(t *testing.T, id string, large bool, ordinary bool) PendingPublication {
	t.Helper()
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	p := publicationFixture(t, publicationThread, at)
	b := p.Bundle
	b.ArchiveSessionID = id
	if large {
		b.NativeRecords[0]["accounting_fixture_text"] = strings.Repeat("x", 24<<20)
	}
	if ordinary {
		b.SchemaVersion = archive.SourceSchemaVersion
		b.History = nil
		b.Ordinals = nil
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
	m, err := archive.BuildMetadataWithAnalysis(b, archive.Analysis{}, nil, "synthetic-machine", at, at, ref, archive.ParserInfo{Name: "codex", Version: "synthetic-v1"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	return PendingPublication{Bundle: b, SourceKey: key, SourceSHA256: packed.SHA256, SourceBytes: packed.Bytes, MetadataKey: metadataKey, MetadataBytes: raw}
}

func saveAccountingFixture(t *testing.T, s *Store, p PendingPublication, ordinary bool) PendingPublication {
	t.Helper()
	published, err := s.LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary {
		if err = published.SavePublication(p.Bundle, p.Bundle.Capture.CapturedAt, p.SourceReference(), p.MetadataBytes); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p.History = &PendingHistory{Version: 1}
	p, err = PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(p, p.Bundle.Capture.CapturedAt); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublicationAccountingLargePeersSurvivePressureAndSavedRefresh(t *testing.T) {
	s := newTestStore(t)
	first := saveAccountingFixture(t, s, accountingFixture(t, "peer-one", true, false), false)
	saveAccountingFixture(t, s, accountingFixture(t, "peer-two", true, false), false)
	legacy := saveAccountingFixture(t, s, accountingFixture(t, "ordinary-peer", true, true), true)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	accounting, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
	defer closeAccounting()
	if accounting.publicationAccounting.seeded {
		t.Fatal("eager accounting read")
	}
	scoped, end := accounting.WithReadBudget(t.Context(), budget)
	published, err := scoped.LoadPublishedState(first.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounting.publicationAccounting.facts) != 3 {
		t.Fatal("missing large peer classification", len(accounting.publicationAccounting.facts))
	}
	pressure := budget.Available() - (4 << 20)
	pressureOwner := budget.Reservation()
	if !pressureOwner.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if err = scoped.SavePending(first.Bundle.ArchiveSessionID, first); err != nil {
		t.Fatal("pending beside large peers", err)
	}
	if err = published.SaveCommittedPublication(first, first.Bundle.Capture.CapturedAt); err != nil {
		t.Fatal("typed saved refresh", err)
	}
	if err = scoped.RemoveExactSourceSetPending(first.Bundle.ArchiveSessionID, first); err != nil {
		t.Fatal("cleanup after published rewrite", err)
	}
	err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
		q, e := scoped.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, q.Close()) }()
		path := filepath.Join("published", legacy.Bundle.ArchiveSessionID+".json")
		info, e := q.home.Root.Lstat(path)
		if e != nil {
			return e
		}
		charge, e := q.publishedAdditional(t.Context(), path, info, 64)
		if e != nil {
			return e
		}
		if charge != info.Size()+64 {
			t.Fatalf("legacy granted replacement credit: %d", charge)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pressureOwner.Release(pressure)
	end()
	closeAccounting()
	closeAccounting()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("accounting ownership leaked", used)
	}
}

func TestPublicationAccountingDetectsSameStampMutation(t *testing.T) {
	s := newTestStore(t)
	p := saveAccountingFixture(t, s, accountingFixture(t, "peer-one", false, false), false)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	accounting, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
	defer closeAccounting()
	scoped, end := accounting.WithReadBudget(t.Context(), budget)
	defer end()
	if _, err := scoped.LoadPublishedState(p.Bundle.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	path := s.publishedPath(p.Bundle.ArchiveSessionID)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte(strings.Replace(string(raw), `"publication_version":5`, `"publication_version":9`, 1))
	if len(changed) != len(raw) || bytes.Equal(changed, raw) {
		t.Fatal("invalid fault size")
	}
	if err = os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
		q, e := scoped.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, q.Close()) }()
		info, e := q.home.Root.Lstat(filepath.Join("published", p.Bundle.ArchiveSessionID+".json"))
		if e != nil {
			return e
		}
		mode, e := q.publishedProtocol(t.Context(), filepath.Join("published", p.Bundle.ArchiveSessionID+".json"), info)
		if mode != 0 || !errors.Is(e, ErrDurableStorageRecovery) {
			t.Fatalf("changed bytes reused: %d %v", mode, e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(changed) {
		t.Fatal("fault evidence changed", err)
	}
}

func TestPublicationAccountingCurrentReadIsSingleAndCloseFailureOwnsNoFact(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			s := newTestStore(t)
			p := saveAccountingFixture(t, s, accountingFixture(t, "current-read", false, false), false)
			budget := agentapi.NewNativeReadBudget(128 << 20)
			accounting, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
			scoped, end := accounting.WithReadBudget(t.Context(), budget)
			closes := 0
			closeFailure := errors.New("actual current descriptor close failed")
			scoped.publicationFileClose = func(f *os.File) error {
				err := f.Close()
				if filepath.Base(f.Name()) == p.Bundle.ArchiveSessionID+".json" {
					closes++
					if fail {
						return errors.Join(err, closeFailure)
					}
				}
				return err
			}
			published, err := scoped.LoadPublishedState(p.Bundle.ArchiveSessionID)
			if fail {
				if !errors.Is(err, closeFailure) || published != nil {
					t.Fatal(published, err)
				}
				for _, f := range accounting.publicationAccounting.facts {
					if f.mode != 0 {
						t.Fatal("failed current close minted fact")
					}
				}
			} else if err != nil || published == nil || !published.found {
				t.Fatal(published, err)
			}
			if closes != 1 {
				t.Fatal("current peer decoded more than once", closes)
			}
			end()
			closeAccounting()
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("current ownership leak", used)
			}
		})
	}
}

func TestPublicationAccountingSeedFinalCloseInvalidatesPeers(t *testing.T) {
	s := newTestStore(t)
	p := saveAccountingFixture(t, s, accountingFixture(t, "current", false, false), false)
	saveAccountingFixture(t, s, accountingFixture(t, "other", false, false), false)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	accounting, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
	scoped, end := accounting.WithReadBudget(t.Context(), budget)
	closeFailure := errors.New("seed root close failed")
	scoped.publicationRootClose = func(root *os.Root) error { return errors.Join(root.Close(), closeFailure) }
	// Seed failure grants no accounting fact; the original supported read remains.
	if _, err := scoped.LoadPublishedState(p.Bundle.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	if accounting.publicationAccounting.seedValid {
		t.Fatal("failed seed marked valid")
	}
	for _, f := range accounting.publicationAccounting.facts {
		if f.mode != 0 {
			t.Fatal("seed close retained peer fact")
		}
	}
	end()
	closeAccounting()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("seed ownership leak", used)
	}
}

func TestPublicationAccountingConfigBytesAndReplacementInvalidateReuse(t *testing.T) {
	for _, configMutation := range []bool{false, true} {
		t.Run(strconv.FormatBool(configMutation), func(t *testing.T) {
			s := newTestStore(t)
			p := saveAccountingFixture(t, s, accountingFixture(t, "peer", false, false), false)
			budget := agentapi.NewNativeReadBudget(128 << 20)
			accounting, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
			defer closeAccounting()
			scoped, end := accounting.WithReadBudget(t.Context(), budget)
			defer end()
			if _, err := scoped.LoadPublishedState(p.Bundle.ArchiveSessionID); err != nil {
				t.Fatal(err)
			}
			err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
				q, err := scoped.openDurableQuota(g)
				if err != nil {
					return err
				}
				defer func() { err = errors.Join(err, q.Close()) }()
				path := filepath.Join("published", p.Bundle.ArchiveSessionID+".json")
				faultPath := s.publishedPath(p.Bundle.ArchiveSessionID)
				if configMutation {
					faultPath = filepath.Join(s.home, "config.json")
				}
				before, err := os.Stat(faultPath)
				if err != nil {
					return err
				}
				raw, err := os.ReadFile(faultPath)
				if err != nil {
					return err
				}
				if configMutation {
					var decoded map[string]json.RawMessage
					if err = json.Unmarshal(raw, &decoded); err != nil {
						return err
					}
					original := decoded["schema_version"]
					if !bytes.Contains(original, []byte(`"version":17`)) {
						t.Fatal("wrong seed schema", string(original))
					}
					needle := regexp.MustCompile(`("version"\s*:\s*)17`)
					changed := needle.ReplaceAll(raw, []byte(`${1}19`))
					if len(changed) != len(raw) || bytes.Equal(changed, raw) {
						t.Fatal("bad same-stamp config fault")
					}
					if err = os.WriteFile(faultPath, changed, 0600); err != nil {
						return err
					}
				} else {
					tmp := faultPath + ".disposable-replacement"
					if err = os.WriteFile(tmp, raw, 0600); err != nil {
						return err
					}
					if err = os.Rename(tmp, faultPath); err != nil {
						return err
					}
				}
				if err = os.Chtimes(faultPath, before.ModTime(), before.ModTime()); err != nil {
					return err
				}
				info, err := q.home.Root.Lstat(path)
				if err != nil {
					return err
				}
				mode, reused, err := q.accountingClassification(path, info)
				if err != nil || reused || mode != 0 {
					t.Fatal("changed observation reused accounting", mode, reused, err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublicationAccountingPostSaveCallbackErrorCannotInstallSavedFact(t *testing.T) {
	s := newTestStore(t)
	pending := saveAccountingFixture(t, s, accountingFixture(t, "outer-close", false, false), false)
	id := pending.Bundle.ArchiveSessionID
	reg := registration(t)
	reg.ArchiveSessionID = id
	reg.NativeSessionID = pending.Bundle.NativeSessionID
	reg.ProjectID = pending.Bundle.ProjectID
	reg.Harness = pending.Bundle.Capture.Harness
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest(id, "owed synthetic request", pending.Bundle.Capture.CapturedAt); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	accounting, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
	scoped, end := accounting.WithReadBudget(t.Context(), budget)
	published, err := scoped.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = scoped.SavePending(id, pending); err != nil {
		t.Fatal(err)
	}
	oldPublished, err := os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{s.pendingPath(id), s.requestPath(id)}
	if _, err = os.Stat(filepath.Join(s.home, evidencePath(id))); err == nil {
		paths = append(paths, filepath.Join(s.home, evidencePath(id)))
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	originals := make([][]byte, len(paths))
	for i, path := range paths {
		originals[i], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	oldAt := published.state.PublishedAt
	closes := 0
	callbackFailure := errors.New("synthetic post-save callback failure")
	scoped.publicationAfterSavedRoot = func(root *os.Root) error { closes++; return errors.Join(root.Close(), callbackFailure) }
	err = published.SaveCommittedPublication(pending, oldAt.Add(time.Hour))
	if !errors.Is(err, callbackFailure) {
		t.Fatal("actual post-save callback did not fail", err)
	}
	if closes != 1 || !published.state.PublishedAt.Equal(oldAt) {
		t.Fatal("failed outer save mutated memory", closes, published.state.PublishedAt)
	}
	newPublished, err := os.ReadFile(s.publishedPath(id))
	if err != nil || bytes.Equal(newPublished, oldPublished) {
		t.Fatal("renamed durable file did not exist", err)
	}
	for _, f := range accounting.publicationAccounting.facts {
		if f.path == filepath.Join("published", id+".json") && f.mode != 0 {
			t.Fatal("outer failure installed saved fact")
		}
	}
	for i, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, originals[i]) {
			t.Fatal("owed original changed", path, err)
		}
	}
	scoped.publicationAfterSavedRoot = nil
	if err = published.SaveCommittedPublication(pending, oldAt.Add(time.Hour)); err != nil {
		t.Fatal("real retry", err)
	}
	if err = scoped.RemovePending(id); err != nil {
		t.Fatal(err)
	}
	end()
	closeAccounting()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("outer Close leaked ownership", used)
	}
}

func TestPublicationAccountingFactGrowthAndCancellationHaveNoEffects(t *testing.T) {
	s := newTestStore(t)
	var first PendingPublication
	for i := range 20 {
		p := saveAccountingFixture(t, s, accountingFixture(t, fmt.Sprintf("growth-%02d", i), false, false), false)
		if i == 0 {
			first = p
		}
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	ctx, cancel := context.WithCancel(t.Context())
	accounting, closeAccounting := s.WithPublicationAccounting(ctx, budget)
	scoped, end := accounting.WithReadBudget(ctx, budget)
	published, err := scoped.LoadPublishedState(first.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounting.publicationAccounting.facts) != 20 || cap(accounting.publicationAccounting.facts) < 20 {
		t.Fatal("fact growth lost peers", len(accounting.publicationAccounting.facts))
	}
	if err = scoped.SavePending(first.Bundle.ArchiveSessionID, first); err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := os.ReadFile(s.pendingPath(first.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	publishedBefore, err := os.ReadFile(s.publishedPath(first.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = published.SaveCommittedPublication(first, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled save", err)
	}
	pendingAfter, err := os.ReadFile(s.pendingPath(first.Bundle.ArchiveSessionID))
	if err != nil || !bytes.Equal(pendingBefore, pendingAfter) {
		t.Fatal("canceled pending changed", err)
	}
	publishedAfter, err := os.ReadFile(s.publishedPath(first.Bundle.ArchiveSessionID))
	if err != nil || !bytes.Equal(publishedBefore, publishedAfter) {
		t.Fatal("canceled published changed", err)
	}
	end()
	closeAccounting()
	closeAccounting()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("grown/canceled accounting ownership leak", used)
	}
}

func TestPublicationAccountingRootReplacementInvalidatesReuse(t *testing.T) {
	s := newTestStore(t)
	p := saveAccountingFixture(t, s, accountingFixture(t, "root-peer", false, false), false)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	accounting, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
	defer closeAccounting()
	scoped, end := accounting.WithReadBudget(t.Context(), budget)
	defer end()
	if _, err := scoped.LoadPublishedState(p.Bundle.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(s.home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.publishedPath(p.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	original := s.home + ".disposable-original"
	if err = os.Rename(s.home, original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(original); err != nil {
			t.Error(err)
		}
	})
	if err = os.Mkdir(s.home, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(s.home, "published"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.home, "config.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.publishedPath(p.Bundle.ArchiveSessionID), raw, 0600); err != nil {
		t.Fatal(err)
	}
	err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
		q, err := scoped.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, q.Close()) }()
		path := filepath.Join("published", p.Bundle.ArchiveSessionID+".json")
		info, err := q.home.Root.Lstat(path)
		if err != nil {
			return err
		}
		mode, reused, err := q.accountingClassification(path, info)
		if err != nil || mode != 0 || reused {
			t.Fatal("replaced root reused fact", mode, reused, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPublicationAccountingReleasePreservesLaterScopeCharges(t *testing.T) {
	store := newTestStore(t)
	budget := agentapi.NewNativeReadBudget(8192)
	endFirst := budget.BeginScope(4096)
	_, closeAccounting := store.WithPublicationAccounting(t.Context(), budget)
	endFirst()
	endSecond := budget.BeginScope(2048)
	defer endSecond()
	later := budget.Reservation()
	if !later.Reserve(1536) {
		t.Fatal("later scope setup refused")
	}
	closeAccounting()
	closeAccounting()
	if later.Reserve(1024) {
		t.Fatal("accounting cleanup released another scope's charges")
	}
	later.Release(1536)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("accounting reservation leaked after scope transition", used)
	}
}
