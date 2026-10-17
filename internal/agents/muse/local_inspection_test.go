package muse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

type localInspectionFiles struct{}

func (localInspectionFiles) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }
func (localInspectionFiles) Lstat(path string) (fs.FileInfo, error)     { return os.Lstat(path) }
func (localInspectionFiles) Open(path string) (io.ReadCloser, error)    { return os.Open(path) }

func TestLocalInspectionKeepsPurposeBounds(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	p := LocalInspection{}
	for _, purpose := range []agentapi.DiscoveryPurpose{agentapi.DiscoveryImport, agentapi.DiscoveryProjects, agentapi.DiscoveryHandoff} {
		h, err := p.InspectHeader(agentapi.NativeHeaderRequest{Purpose: purpose, Path: "/store/00000000-0000-7000-8000-000000000001/session.jsonl", Scan: func(visit func([]byte) bool) error {
			for line := range bytes.SplitSeq(raw, []byte("\n")) {
				if len(line) > 0 && !visit(line) {
					break
				}
			}
			return nil
		}})
		if err != nil || h.Directory != "/work/synthetic-muse" || h.NativeID == "" {
			t.Fatalf("purpose %v: %+v %v", purpose, h, err)
		}
	}
	if _, err := p.InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryBoundedProjects, Scan: func(func([]byte) bool) error {
		t.Fatal("first-record project matching read beyond its capability")
		return nil
	}}); err == nil {
		t.Fatal("accepted unsupported first-record project matching")
	}
	if roots := p.Roots(agentapi.NativeLocations{DataHome: "/synthetic/data"}, agentapi.DiscoveryBoundedProjects); len(roots) != 0 {
		t.Fatal("advertised unavailable project matching")
	}
}

func TestLocalInspectionRecognizesOnlyQualifiedOrdinaryPaths(t *testing.T) {
	t.Parallel()
	p := LocalInspection{}
	e := agentapi.NativePathEnvironment{Locations: agentapi.NativeLocations{DataHome: "/synthetic/data"}}
	path := filepath.Join(e.Locations.DataHome, "muse/sessions/2026/10/15/00000000-0000-7000-8000-000000000001/session.jsonl")
	if !p.RecognizesTranscriptPath(e, path) {
		t.Fatal("ordinary locator unrecognized")
	}
	for _, bad := range []string{"relative/session.jsonl", path + "/extra", "/synthetic/data/muse/sessions/2026/99/15/00000000-0000-7000-8000-000000000001/session.jsonl", "/synthetic/data/muse/sessions/2026/10/15/not-an-id/session.jsonl"} {
		if p.RecognizesTranscriptPath(e, bad) {
			t.Fatalf("recognized %q", bad)
		}
	}
	if p.RecognizesTranscriptPath(agentapi.NativePathEnvironment{}, path) || len(p.ProjectPaths(e).Worktrees) != 0 {
		t.Fatal("inferred unavailable native locations")
	}
}

func TestImportInspectionAgreesWithRetainedAnalysis(t *testing.T) {
	t.Parallel()
	f, err := filterBytes(t, readSynthetic(t, "synthetic-exec-conversation.jsonl"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := (Parser{}).Parse(t.Context(), bundleFor(t, f))
	if err != nil {
		t.Fatal(err)
	}
	r := agentapi.ImportInspectionRequest{Session: agentapi.NativeSession{Agent: agentmeta.Muse, NativeID: f.SessionIDs[0]}, Filtered: f}
	got, err := Imports().InspectImport(t.Context(), r)
	if err != nil || got.IdentityMismatch || got.Conversation != (len(a.View.Turns) > 0) || !got.StartedAt.Equal(a.Facts.NativeStartedAt) {
		t.Fatalf("inspection %+v: %v", got, err)
	}
	r.Session.NativeID = "00000000-0000-7000-8000-000000000099"
	got, err = Imports().InspectImport(t.Context(), r)
	if err != nil || !got.IdentityMismatch {
		t.Fatalf("copied identity %+v: %v", got, err)
	}
	r.Filtered.NativeStartAt = r.Filtered.NativeStartAt.AddDate(0, 0, 1)
	if _, err := Imports().InspectImport(t.Context(), r); err == nil {
		t.Fatal("accepted conflicting creation date")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Imports().InspectImport(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestImportInspectionRefusesPartialAndForeignEvidence(t *testing.T) {
	t.Parallel()
	for _, f := range []archive.FilteredTranscript{{}, {Format: "muse-jsonl", Text: []string{"unqualified conversation"}}, {Format: "other", NativeStartComplete: true}} {
		if _, err := Imports().InspectImport(t.Context(), agentapi.ImportInspectionRequest{Session: agentapi.NativeSession{Agent: agentmeta.Muse}, Filtered: f}); err == nil {
			t.Fatal("accepted unqualified evidence")
		}
	}
}

func TestImportInspectionRejectsHeaderSourceConflicts(t *testing.T) {
	t.Parallel()
	f, err := filterBytes(t, readSynthetic(t, "synthetic-exec-conversation.jsonl"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []agentapi.NativeHeader{{Directory: "/different/checkout"}, {StartedAt: f.NativeStartAt.AddDate(0, 0, 1)}, {SubagentOnly: true}, {CapturePending: "unsupported native history"}} {
		r := agentapi.ImportInspectionRequest{Session: agentapi.NativeSession{Agent: agentmeta.Muse, NativeID: f.SessionIDs[0]}, Filtered: f, Header: header}
		if _, err := Imports().InspectImport(t.Context(), r); err == nil {
			t.Fatalf("accepted incompatible header %+v", header)
		}
	}
}

func TestImportDiscoveryRetainsRefusalsAndHonorsBudgets(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	root := filepath.Join(data, "muse", "sessions")
	path := filepath.Join(root, "2026", "10", "15", "00000000-0000-7000-8000-000000000001", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	req := agentapi.DiscoveryRequest{Purpose: agentapi.DiscoveryImport, Stage: agentapi.DiscoveryIdentities, Locations: agentapi.NativeLocations{DataHome: data}, Files: localInspectionFiles{}, MaxFiles: 5, HeaderBytes: 64 * 1024, RecordBytes: 64 * 1024}
	var candidates []agentapi.DiscoveryCandidate
	report, err := (LocalInspection{}).Discover(t.Context(), req, func(c agentapi.DiscoveryCandidate) error { candidates = append(candidates, c); return nil })
	if err != nil || report.Incomplete || len(candidates) != 1 || candidates[0].IdentityError != nil || candidates[0].Header.Directory != "/work/synthetic-muse" {
		t.Fatalf("discovery %+v %+v: %v", report, candidates, err)
	}
	// An identity refusal remains a preview row instead of disappearing as an
	// apparently empty store. A truncated scan cannot establish a start date.
	req.HeaderBytes = 1
	candidates = nil
	_, err = (LocalInspection{}).Discover(t.Context(), req, func(c agentapi.DiscoveryCandidate) error { candidates = append(candidates, c); return nil })
	if err != nil || len(candidates) != 1 || candidates[0].IdentityError == nil || !candidates[0].Header.StartedAt.IsZero() {
		t.Fatalf("budget refusal %+v: %v", candidates, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (LocalInspection{}).Discover(ctx, req, func(agentapi.DiscoveryCandidate) error { t.Fatal("emitted after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("read-only inspection changed native source")
	}
}
