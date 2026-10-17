package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type childRecoverySources struct {
	agentapi.SourcesLookup
	attempts map[string]int
}

type childRecoveryProvider struct {
	agentapi.SourceProvider
	attempts map[string]int
}

type childRecoveryFiles struct{ attempts map[string]int }

func (f childRecoveryFiles) Lstat(path string) (os.FileInfo, error) {
	f.attempts[path]++
	return nil, errors.New("native stat forbidden during frozen child recovery")
}

func (f childRecoveryFiles) EvalSymlinks(path string) (string, error) {
	f.attempts[path]++
	return "", errors.New("native resolution forbidden during frozen child recovery")
}

func (f childRecoveryFiles) OpenRegular(path string) (transcriptio.File, error) {
	f.attempts[path]++
	return nil, errors.New("native access forbidden during frozen child recovery")
}

func (s childRecoverySources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, filter, ok := s.SourcesLookup.LookupSources(name)
	if !ok {
		return nil, nil, false
	}
	return childRecoveryProvider{provider, s.attempts}, filter, true
}

func (p childRecoveryProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	env.Files = childRecoveryFiles{p.attempts}
	return p.SourceProvider.OpenPass(ctx, env)
}

func TestNativeChildLateParentPublicationRetryKeepsFrozenBytesAndNewRequest(t *testing.T) {
	local := newTestStore(t)
	nativeHome := t.TempDir()
	sessions := filepath.Join(nativeHome, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const parentID = "11111111-1111-4111-8111-111111111111"
	const childID = "22222222-2222-4222-8222-222222222222"
	// This is the supported Codex child format, including its independently
	// recorded first native task. No Muse proof or admission is fabricated.
	header, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{
		"id": childID, "timestamp": at.Format(time.RFC3339Nano), "cwd": nativeHome,
		"source": "cli", "originator": "codex_cli_rs", "cli_version": "dev",
		"parent_thread_id": parentID, "session_id": parentID, "subagent_history_start_ordinal": 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	task, err := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{
		"type": "task_started", "turn_id": childID, "started_at": at.Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, sessions, "rollout-"+childID+".jsonl", string(header)+"\n"+string(task)+"\n"+codexTranscript+"\n")
	child := nativeLinkRegistration("child", childID, parentID, nativeHome, at)
	child.ProjectRoot, child.TranscriptPath, child.NativeRootSessionID = nativeHome, path, parentID
	child.DestinationID = "" // Existing admitted legacy destination semantics, as registration(t).
	if err := local.SaveRegistration(child); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MachineID: "synthetic", Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: nativeHome, ProjectID: child.ProjectID, Included: true, ActivatedAt: at}}}}
	if err := config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	now := at.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1},
		ConfiguredCodexHomes: []string{nativeHome}, ResolveCodexReadHomes: func(config.Config) ([]string, error) { return []string{nativeHome}, nil }}
	initial, err := Run(t.Context(), local, remote, opts)
	if err != nil || initial.Errors[child.ArchiveSessionID] == nil || !strings.Contains(initial.Errors[child.ArchiveSessionID].Error(), "injected metadata failure") || len(initial.Published) != 0 {
		t.Fatal("metadata failure did not freeze the child's publication", initial, err)
	}
	pending, found, err := local.LoadPublicationPending(child.ArchiveSessionID)
	if err != nil || !found || !pending.Attempted || !pending.Bundle.NativeChild || pending.Bundle.ParentSessionID != "" {
		t.Fatal("unlinked child did not retain its own frozen publication", found, err, initial.Errors[child.ArchiveSessionID])
	}
	frozenMetadata := bytes.Clone(pending.MetadataBytes)
	frozenSource := pending.SourceReference()
	child, found, err = local.LoadRegistration(child.ArchiveSessionID)
	if err != nil || !found || child.CodexBinding == nil || !child.CodexBinding.Child || child.CodexBinding.FirstNativeTaskID != childID {
		t.Fatal("fixture never established supported own-task child binding", found, err)
	}
	immutable := child
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	parent := nativeLinkRegistration("parent", parentID, "", nativeHome, at)
	parent.ProjectRoot, parent.DestinationID = nativeHome, child.DestinationID
	parent.TranscriptPath = filepath.Join(sessions, "rollout-"+parentID+".jsonl")
	// No parent source is written: linking depends on independent persisted
	// ownership, not a new parent byte or a still-running parent.
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	attempts := map[string]int{}
	opts.Sources = childRecoverySources{testSources, attempts}
	for range 2 {
		local, err = openTestStore(local.Home())
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		result, err := Run(t.Context(), local, remote, opts)
		if err != nil || result.Errors[child.ArchiveSessionID] == nil || len(result.Published) != 0 {
			t.Fatal("failed frozen retry was acknowledged", result, err)
		}
		retained, found, err := local.LoadPublicationPending(child.ArchiveSessionID)
		if err != nil || !found || !bytes.Equal(retained.MetadataBytes, frozenMetadata) || retained.SourceReference() != frozenSource || retained.Bundle.ParentSessionID != "" {
			t.Fatal("late parent changed already-attempted publication bytes", found, err)
		}
		linked, found, err := local.LoadRegistration(child.ArchiveSessionID)
		if err != nil || !found || linked.ParentSessionID != parent.ArchiveSessionID || linked.NativeLinkVersion != 1 {
			t.Fatal("parent arrival failed to queue durable relationship", found, err)
		}
		linked.ParentSessionID, linked.NativeLinkVersion = immutable.ParentSessionID, immutable.NativeLinkVersion
		if !reflect.DeepEqual(linked, immutable) {
			t.Fatal("relationship reconciliation changed original child ownership")
		}
		if req, found, err := local.LoadRequest(child.ArchiveSessionID); err != nil || !found || req.Token == pending.RequestToken {
			t.Fatal("new link work was lost or reused frozen request token", found, err)
		}
	}
	remote.failMetadata = false
	now = now.Add(time.Minute)
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[child.ArchiveSessionID] != nil {
		t.Fatal("frozen child could not finish without its native source", result, err)
	}
	actual, err := remote.Get(t.Context(), pending.MetadataKey)
	if err != nil || !bytes.Equal(actual, frozenMetadata) {
		t.Fatal("first successful retry changed the frozen metadata target", err)
	}
	frozenPublished := fetchBundle(t, remote, fetchMetadata(t, remote, "codex", child.ArchiveSessionID))
	if _, found, err := local.LoadRequest(child.ArchiveSessionID); err != nil || !found {
		t.Fatal("old publication acknowledged the newer relationship request", found, err)
	}
	// Retained history deliberately prepares one input per pass. Drive its
	// real durable protocol after restarts; a typed pending result is valid
	// only while the matching successor and newer request remain present.
	linkedPublished := false
	for range 4 {
		local, err = openTestStore(local.Home())
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		result, err = Run(t.Context(), local, remote, opts)
		if err != nil {
			t.Fatal("retained relationship publication failed", result, err)
		}
		childErr := result.Errors[child.ArchiveSessionID]
		if childErr != nil {
			if !errors.Is(childErr, archive.ErrHistoryMutationPending) {
				t.Fatal("retained relationship publication failed", childErr)
			}
			staged, found, err := local.LoadPublicationPending(child.ArchiveSessionID)
			if err != nil || !found || staged.History == nil || staged.Bundle.ParentSessionID != parent.ArchiveSessionID {
				t.Fatal("pending result lacks its durable relationship successor", found, err)
			}
			if _, found, err := local.LoadRequest(child.ArchiveSessionID); err != nil || !found {
				t.Fatal("private preparation acknowledged the relationship request", found, err)
			}
			continue
		}
		metadata := fetchMetadata(t, remote, "codex", child.ArchiveSessionID)
		if metadata.ParentSessionID == parent.ArchiveSessionID && metadata.NativeChild {
			linkedPublished = true
			break
		}
	}
	if !linkedPublished {
		t.Fatal("bounded retained preparation did not publish the admitted parent")
	}
	metadata := fetchMetadata(t, remote, "codex", child.ArchiveSessionID)
	if _, found, err := local.LoadRequest(child.ArchiveSessionID); err != nil || found {
		t.Fatal("verified relationship publication was not acknowledged", found, err)
	}
	if attempts[path] != 0 {
		t.Fatalf("recovery attempted %d child native opens", attempts[path])
	}
	bundle := fetchBundle(t, remote, metadata)
	if !reflect.DeepEqual(bundle.NativeRecords, frozenPublished.NativeRecords) || !bundle.Capture.CapturedAt.Equal(pending.Bundle.Capture.CapturedAt) {
		t.Fatal("parent repair changed own records or original capture age")
	}
	// Parent scans may attempt its absent file; every such attempt is refused
	// and counted by the same opener, and no parent bytes can enter this test.
}
