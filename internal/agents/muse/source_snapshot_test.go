package muse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func proofFixture(t *testing.T, name string) (*sourcePass, *sourceSnapshot, []byte, *agentapi.NativeReadBudget) {
	t.Helper()
	raw := readSynthetic(t, name)
	pass, e := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: agentapi.NewNativeReadBudget(8 << 20)})
	if e != nil {
		t.Fatal(e)
	}
	p := pass.(*sourcePass)
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
		if used, _ := p.env.ReadBudget.Charged(); used != 0 {
			t.Errorf("leaked %d", used)
		}
	})
	path := filepath.Join(t.TempDir(), "explicit.jsonl")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	snap, e := p.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	return p, snap.(*sourceSnapshot), raw, p.env.ReadBudget
}

func TestPreviewPassDoesNotSupplyContentAuthority(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "preview.jsonl")
	if err := os.WriteFile(path, readSynthetic(t, "synthetic-exec-conversation.jsonl"), 0600); err != nil {
		t.Fatal(err)
	}
	pass, err := (SourceProvider{}).OpenPreviewPass(t.Context(), agentapi.SourceEnvironment{Policy: transcriptio.OpenPolicy{Root: filepath.Dir(path), RejectSymlinks: true}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(errors.Join(err, pass.Close()))
	}
	if _, ok := snapshot.(agentapi.VerifiedContentSnapshot); ok {
		t.Error("preview snapshot supplied whole-source authority")
	}
	if err := errors.Join(snapshot.Close(), pass.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Input().File.ReadAt(make([]byte, 1), 0); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("closed preview input: %v", err)
	}
	for _, env := range []agentapi.SourceEnvironment{{RequireConfinedHistory: true}, {NativeRoot: &archive.NativeRoot{}}, {LegacyUnboundRegistration: true}} {
		if pass, err := (SourceProvider{}).OpenPreviewPass(t.Context(), env); err == nil || pass != nil {
			t.Fatal("preview pass bypassed capture refusal")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (SourceProvider{}).OpenPreviewPass(ctx, agentapi.SourceEnvironment{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLocalSnapshotProofOwnsExactInputAndLifetime(t *testing.T) {
	t.Parallel()
	p, s, raw, budget := proofFixture(t, "synthetic-exec-conversation.jsonl")
	proof, e := s.ContentProof(t.Context())
	if e != nil || proof.SHA256 != sha256.Sum256(raw) || proof.Boundary != int64(len(raw)) || proof.Provider != "muse-ordinary" {
		t.Fatal(proof, e)
	}
	in := s.Input()
	if !s.ContentProofFor(s, in) {
		t.Fatal("own input declined")
	}
	decorated := struct {
		agentapi.SourceSnapshot
		agentapi.VerifiedContentSnapshot
	}{s, s}
	if decorated.ContentProofFor(decorated, in) {
		t.Fatal("promoted snapshot attested")
	}
	for _, bad := range []agentapi.NativeInput{{File: in.File, SubagentMeta: []byte{}}, {File: in.File, SubagentMeta: []byte("x")}, {File: struct{ agentapi.FileInput }{in.File}}, {}} {
		if s.ContentProofFor(s, bad) {
			t.Fatal("foreign input attested")
		}
	}
	if _, e = p.Read(t.Context(), s.ref, agentapi.ReadLimits{}); e == nil {
		t.Fatal("live snapshot renewed")
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ContentProof(t.Context()); !errors.Is(e, agentapi.ErrClosed) {
		t.Fatal(e)
	}
	if _, e = in.File.ReadAt(make([]byte, 1), 0); !errors.Is(e, agentapi.ErrClosed) {
		t.Fatal(e)
	}
	if s.ContentProofFor(s, in) {
		t.Fatal("closed proof attested")
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal(used)
	}
}

func TestLocalRawStartupFactsUseStrictPhysicalPrefix(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"synthetic-exec-conversation.jsonl", "synthetic-msp-conversation.jsonl", "synthetic-model-resolution-omissions.jsonl"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, s, _, _ := proofFixture(t, name)
			facts, e := s.startupFacts(t.Context())
			if e != nil || facts.NativeID == "" || facts.StreamID != facts.NativeID || facts.Cwd != "/work/synthetic-muse" || !facts.NativeCreatedAt.Equal(time.UnixMicro(1792083855882704)) {
				t.Fatal(facts, e)
			}
			if facts.RootRevision != "" || facts.Checkpoint != nil {
				t.Fatal("local facts invented durable authority")
			}
			if _, e = s.AdmissionEvidence(t.Context(), agentapi.SourceAdmission{NativeID: facts.NativeID}); agentapi.Failure(e) != agentapi.Unavailable {
				t.Fatal("missing context acquired binding", e)
			}
			if _, e = (Filter{}).Filter(t.Context(), s.Input(), agentapi.FilterContext{}); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestLocalSnapshotDetectsSameStampRewriteAndNamedReplacement(t *testing.T) {
	t.Parallel()
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-stamp", true: "replace"}[replace], func(t *testing.T) {
			t.Parallel()
			_, s, raw, _ := proofFixture(t, "synthetic-exec-conversation.jsonl")
			before, e := os.Stat(s.ref.Path)
			if e != nil {
				t.Fatal(e)
			}
			changed := bytes.Replace(raw, []byte("synthetic"), []byte("rewritten"), 1)
			if len(changed) != len(raw) {
				t.Fatal("fixture mutation length")
			}
			if replace {
				temp := s.ref.Path + ".new"
				if e = os.WriteFile(temp, raw, 0600); e == nil {
					e = os.Rename(temp, s.ref.Path)
				}
			} else {
				e = os.WriteFile(s.ref.Path, changed, 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Chtimes(s.ref.Path, before.ModTime(), before.ModTime()); e != nil {
				t.Fatal(e)
			}
			if e = s.VerifyContent(t.Context()); e == nil {
				t.Fatal("rewrite accepted")
			}
		})
	}
}

func TestLocalProofBudgetCancelAndRawLimitRelease(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"memory", "raw", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			budget := agentapi.NewNativeReadBudget(32 << 10)
			if mode != "memory" {
				budget = agentapi.NewNativeReadBudget(128 << 10)
			}
			pass, e := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget})
			if e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(t.TempDir(), "raw")
			if e = os.WriteFile(path, []byte("synthetic\n"), 0600); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			limits := agentapi.ReadLimits{}
			if mode == "raw" {
				limits = agentapi.ReadLimits{RawBytes: 1}
			}
			if mode == "cancel" {
				cancel()
			}
			if _, e = pass.Read(ctx, agentapi.SourceRef{Path: path}, limits); e == nil {
				t.Fatal("expected refusal")
			}
			if e = pass.Close(); e != nil {
				t.Fatal(e)
			}
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal(used)
			}
		})
	}
}

type sourceFactMutation string

const (
	mutateFactWorkspace sourceFactMutation = "workspace"
	mutateFactStream    sourceFactMutation = "stream"
	mutateFactDomain    sourceFactMutation = "domain"
	mutateFactVersion   sourceFactMutation = "version"
	mutateFactDigest    sourceFactMutation = "digest"
)

func TestSuppliedBindingFactsAndSignatureStayOnOneDescriptor(t *testing.T) {
	t.Parallel()
	p, s, raw, _ := proofFixture(t, "synthetic-exec-conversation.jsonl")
	facts, e := s.startupFacts(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	path := filepath.Join(root, "2026", "10", "15", facts.NativeID, "session.jsonl")
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	facts.Path = path
	facts.RootRevision = "independent-reviewed-root"
	digest := sha256.Sum256(raw)
	facts.Checkpoint = &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: int64(len(raw)), SHA256: hex.EncodeToString(digest[:])}
	admission := agentapi.SourceAdmission{NativeID: facts.NativeID, Binding: &agentapi.SourceBinding{Native: &facts}}
	ref := agentapi.SourceRef{Path: path}
	if e = p.ValidateSourceAdmission(t.Context(), ref, admission); e != nil {
		t.Fatal(e)
	}
	held := p.prepared
	if held == nil {
		t.Fatal("no prepared descriptor")
	}
	temp := path + ".new"
	if e = os.WriteFile(temp, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(temp, path); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Signature(t.Context(), ref); e == nil {
		t.Fatal("path replacement accepted")
	}
	if !held.closed || len(p.live) != 0 {
		t.Fatal("prepared descriptor leaked")
	}
	if e = p.ValidateSourceAdmission(t.Context(), ref, admission); e != nil {
		t.Fatal(e)
	}
	observed, e := p.Signature(t.Context(), ref)
	if e != nil || len(observed.Signature.Token) != 55 || observed.Signature.Provider != "file" {
		t.Fatal(observed, e)
	}
	// An unchanged baseline shortcuts only the prior-prefix pass; a subsequent
	// same-stamp body rewrite must fail the mandatory Signature verification.
	if e = p.ValidateSourceAdmission(t.Context(), ref, admission); e != nil {
		t.Fatal(e)
	}
	stamp, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	changed := bytes.Replace(raw, []byte("metadata"), []byte("metadatz"), 1)
	if e = os.WriteFile(path, changed, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Chtimes(path, stamp.ModTime(), stamp.ModTime()); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Signature(t.Context(), ref); e == nil {
		t.Fatal("warm same-stamp rewrite accepted")
	}
	if _, e = p.Signature(t.Context(), ref); e == nil {
		t.Fatal("bound Signature reused admission after release")
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []sourceFactMutation{mutateFactWorkspace, mutateFactStream, mutateFactDomain, mutateFactVersion, mutateFactDigest} {
		bad := facts
		cp := *facts.Checkpoint
		bad.Checkpoint = &cp
		switch mode {
		case mutateFactWorkspace:
			bad.Cwd = "/foreign"
		case mutateFactStream:
			bad.StreamID = "foreign"
		case mutateFactDomain:
			cp.Domain = "foreign"
		case mutateFactVersion:
			cp.Version = 2
		case mutateFactDigest:
			cp.SHA256 = hex.EncodeToString(make([]byte, 32))
		}
		if e = p.ValidateSourceAdmission(t.Context(), ref, agentapi.SourceAdmission{NativeID: facts.NativeID, Binding: &agentapi.SourceBinding{Native: &bad}}); e == nil {
			t.Fatalf("warm %s authority accepted", mode)
		}
	}
	// A private injected context exercises the future known-root predicate while
	// the public OpenPass native-root fence remains unchanged.
	knownRoot := archive.NativeRoot{Agent: "muse", Configured: root, Canonical: root, Revision: facts.RootRevision}
	p.env.NativeRoot = &knownRoot
	if e = p.ValidateSourceAdmission(t.Context(), ref, admission); e != nil {
		t.Fatal("valid independently supplied root refused", e)
	}
	if _, e = p.Signature(t.Context(), ref); e != nil {
		t.Fatal(e)
	}
	wrongPath, wrongAgent, wrongRevision, invalidRoot := knownRoot, knownRoot, knownRoot, knownRoot
	wrongPath.Canonical = filepath.Join(root, "foreign")
	wrongAgent.Agent = "codex"
	wrongRevision.Revision = "foreign"
	invalidRoot.Canonical = ""
	for _, candidate := range []archive.NativeRoot{wrongPath, wrongAgent, wrongRevision, invalidRoot} {
		p.env.NativeRoot = &candidate
		if e = p.ValidateSourceAdmission(t.Context(), ref, admission); e == nil {
			t.Fatal("invalid or inconsistent known root admitted")
		}
		if len(p.live) != 0 {
			t.Fatal("known-root refusal leaked snapshot")
		}
	}
	p.env.NativeRoot = nil
	// A valid body under another owner directory cannot become locator authority.
	other := filepath.Join(root, "2026", "10", "15", "00000000-0000-4000-8000-000000000000", "session.jsonl")
	if e = os.MkdirAll(filepath.Dir(other), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(other, raw, 0600); e != nil {
		t.Fatal(e)
	}
	facts.Path = other
	if e = p.ValidateSourceAdmission(t.Context(), agentapi.SourceRef{Path: other}, admission); e == nil {
		t.Fatal("foreign owner directory admitted")
	}
}

// Keep the concrete filesystem proof dependency visible in this package's tests.
var _ agentapi.FileInput = snapshotInput{}

var _ transcriptio.Opener = transcriptio.OS{}
