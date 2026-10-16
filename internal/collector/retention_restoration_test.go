package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type restorationHookStore struct {
	*storagetest.MemoryStore
	hook         func()
	metadataFail bool
	puts         int
}

func (s *restorationHookStore) Delete(ctx context.Context, key string) error {
	err := s.MemoryStore.Delete(ctx, key)
	if strings.HasSuffix(key, "/metadata.json") && s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	if strings.HasSuffix(key, "/metadata.json") && s.metadataFail {
		s.metadataFail = false
		return errors.New("synthetic lost metadata delete acknowledgment")
	}
	return err
}
func (s *restorationHookStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts++
	return s.MemoryStore.Put(ctx, key, data)
}

func TestRetentionRestoresCompleteHistoryUnionAfterLostDeleteAcknowledgment(t *testing.T) {
	// Initial native admission retains its public fence. This fixture uses the
	// production private preparation and full source-set publication ports to
	// establish a closed, verified three-reference selecting baseline.
	scan, pending := privacyJournal(t)
	lookup := scan.opts.CodexRollouts.(*reconciliationLookup)
	remote := &restorationHookStore{MemoryStore: scan.remote.(*storagetest.MemoryStore)}
	scan.remote = remote
	if out, err := scan.sourceSetPublishPending(pending); err != nil || out != outcomePublished {
		t.Fatal("publish retained history fixture", out, err)
	}
	opts := scan.opts
	opts.Now = func() time.Time { return scan.now }
	key, _ := archive.MetadataObjectKey(scan.reg.Harness.Name, scan.id())
	raw, err := remote.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	metadata := fetchMetadata(t, remote, "codex", scan.id())
	refs, err := metadata.SourceReferences()
	if err != nil || len(refs) != 3 {
		t.Fatal("incomplete history fixture", len(refs), err)
	}
	originalSources := map[string][]byte{}
	for _, ref := range refs {
		originalSources[ref.Key], err = remote.Get(t.Context(), ref.Key)
		if err != nil {
			t.Fatal(err)
		}
	}
	beforeReads := lookup.reads
	expiry := scan.now.Add(91 * 24 * time.Hour)
	remote.metadataFail = true
	remote.hook = func() {
		if err := scan.local.SaveRequest(scan.id(), "stop", expiry); err != nil {
			t.Error(err)
		}
		for _, entries := range lookup.refs {
			for _, entry := range entries {
				if err := os.Remove(entry.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Error(err)
				}
			}
		}
	}
	if err := retention.DeleteOwnedSession(t.Context(), scan.local, remote, scan.reg, state.RemovalReasonRetention, expiry); err == nil {
		t.Fatal("lost delete acknowledgment not reported")
	}
	restarted, err := state.Open(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	opts.Now = func() time.Time { return expiry }
	for iteration := range 2 {
		result, err := Run(t.Context(), restarted, remote, opts)
		if err != nil || result.Errors[scan.id()] != nil {
			t.Fatal("restore history", iteration, result, err)
		}
	}
	after, err := remote.Get(t.Context(), key)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("exact history wire/age changed", err)
	}
	for _, ref := range refs {
		after, err := remote.Get(t.Context(), ref.Key)
		if err != nil || !bytes.Equal(originalSources[ref.Key], after) {
			t.Fatal("history union member changed", ref.Key, err)
		}
	}
	if lookup.reads != beforeReads {
		t.Fatal("retention restoration reread native history")
	}
	if _, found, err := restarted.LoadRequest(scan.id()); err != nil || !found {
		t.Fatal("restoration acknowledged uncovered hook", found, err)
	}
}

func TestRetentionRestorationRefusesMissingIntentChangedPolicyWinnerOrSource(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"intent", "policy", "winner", "source"} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			local := newTestStore(t)
			remote := &restorationHookStore{MemoryStore: storagetest.NewMemoryStore()}
			reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			at := reg.RegisteredAt.Add(time.Hour)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return at }}
			if result, err := Run(t.Context(), local, remote, opts); err != nil || result.Errors[reg.ArchiveSessionID] != nil {
				t.Fatal(result, err)
			}
			expiry := at.Add(91 * 24 * time.Hour)
			remote.hook = func() {
				if err := local.SaveRequest(reg.ArchiveSessionID, "stop", expiry); err != nil {
					t.Error(err)
				}
				if err := os.Remove(reg.TranscriptPath); err != nil {
					t.Error(err)
				}
			}
			if err := retention.DeleteOwnedSession(t.Context(), local, remote, reg, state.RemovalReasonRetention, expiry); !errors.Is(err, state.ErrDeletionWorkChanged) {
				t.Fatal("fixture did not preserve new work", err)
			}
			opts.Now = func() time.Time { return expiry }
			if result, err := Run(t.Context(), local, remote, opts); err != nil || result.Errors[reg.ArchiveSessionID] != nil {
				t.Fatal("prepare restoration", result, err)
			}
			p, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
			if err != nil || !found || p.Preparation == nil || p.Preparation.Retention == nil {
				t.Fatal("missing typed restoration", found, err)
			}
			key := p.MetadataKey
			var winner []byte
			switch fault {
			case "intent":
				if err := os.Remove(filepath.Join(local.Home(), "session-deletions", reg.ArchiveSessionID, "intent.json")); err != nil {
					t.Fatal(err)
				}
			case "policy":
				opts.SkillEvidence = config.SkillEvidenceNone
			case "winner":
				winner = append(bytes.Clone(p.MetadataBytes), ' ')
				if err := remote.MemoryStore.Put(t.Context(), key, winner); err != nil {
					t.Fatal(err)
				}
			case "source":
				path := filepath.Join(local.Home(), "session-deletions", reg.ArchiveSessionID, "sources", p.SourceSHA256+".gz")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw[0] ^= 1
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			puts := remote.puts
			if result, err := Run(t.Context(), local, remote, opts); (err == nil && result.Errors[reg.ArchiveSessionID] == nil) || len(result.Published) > 0 {
				t.Fatal("changed restoration authority accepted", result, err)
			}
			if remote.puts != puts {
				t.Fatal("refusal performed remote PUT", fault)
			}
			after, err := remote.Get(t.Context(), key)
			if fault == "winner" {
				if err != nil || !bytes.Equal(after, winner) {
					t.Fatal("winner changed", err)
				}
			} else if !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("refusal invented selecting metadata", err)
			}
			kept, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
			if err != nil || !found || kept.Commit == nil || *kept.Commit != *p.Commit || kept.Preparation.SHA256 != p.Preparation.SHA256 || !bytes.Equal(kept.MetadataBytes, p.MetadataBytes) {
				t.Fatal("refusal discarded exact baseline evidence", found, err)
			}
			if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("refusal acknowledged newer hook", found, err)
			}
		})
	}
}
