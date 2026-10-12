package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Public admission resumes only the parent page. The unadmitted child's full
// stage is outside that lexical page and must borrow the same tight owner.
func TestStagedAdmissionChildCannotEscapeCapturedPageBudget(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := s.LoadRegistration(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Use a separate fresh store: the template owner is never reinterpreted.
	s = newTestStore(t)
	if err = config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	nativeParent, nativeChild := "native-one", "native-two"
	parentID, _, err := s.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: nativeParent})
	if err != nil {
		t.Fatal(err)
	}
	childID, _, err := s.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: nativeChild})
	if err != nil {
		t.Fatal(err)
	}
	// These state-only synthetic owners may exchange roles, preserving each
	// actual reservation's native key while guaranteeing the lexical boundary.
	if parentID < childID {
		parentID, childID = childID, parentID
		nativeParent, nativeChild = nativeChild, nativeParent
	}
	parent.ArchiveSessionID = parentID
	parent.NativeSessionID = nativeParent
	parent.StagedImportSHA256 = ""
	child := parent
	child.ArchiveSessionID = childID
	child.NativeSessionID = nativeChild
	child.ParentSessionID = parent.ArchiveSessionID
	child.ParentNativeSessionID = parent.NativeSessionID
	child.SubagentID = "agent-one"
	childBundle := p.Bundle
	childBundle.ArchiveSessionID = child.ArchiveSessionID
	childBundle.NativeSessionID = child.NativeSessionID
	childBundle.ParentSessionID = parent.ArchiveSessionID
	childBundle.NativeRecords = []map[string]any{{"content": strings.Repeat("child", 64<<10)}}
	childSHA, err := s.SaveImportStage(t.Context(), child, childBundle, cfg.EffectiveSkillEvidence())
	if err != nil {
		t.Fatal(err)
	}
	candidate := SubagentCandidate{ArchiveSessionID: child.ArchiveSessionID, NativeSessionID: child.NativeSessionID, ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID, ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: child.SubagentID, TranscriptPath: child.TranscriptPath, ObservedAt: p.ReadyAt, Origin: archive.SessionOriginImport, StagedImportSHA256: childSHA}
	bundle := p.Bundle
	bundle.ArchiveSessionID = parent.ArchiveSessionID
	bundle.NativeSessionID = parent.NativeSessionID
	if _, err = s.SaveImportStage(t.Context(), parent, bundle, cfg.EffectiveSkillEvidence(), candidate); err != nil {
		t.Fatal(err)
	}
	confirmation := struct {
		ID            string `json:"id"`
		DestinationID string `json:"destination_id"`
	}{parent.ImportBatch.Recorded(), cfg.DestinationID()}
	if err = local.Write(filepath.Join(s.Home(), "imports", parent.ImportBatch.Recorded()+".json"), confirmation); err != nil {
		t.Fatal(err)
	}
	cursor := struct {
		After string `json:"after"`
	}{childID + "z"}
	if err = local.Write(filepath.Join(s.Home(), "import-stage-cursor.json"), cursor); err != nil {
		t.Fatal(err)
	}
	if !(childID < cursor.After && cursor.After < parentID) {
		t.Fatal("actual child/cursor/parent lexical boundary")
	}
	page, _, closePage, pageErr := s.ImportStagesOwned(t.Context(), cursor.After, 50)
	if pageErr != nil {
		t.Fatal(pageErr)
	}
	if len(page) != 1 || page[0].Registration.ArchiveSessionID != parentID {
		closePage()
		t.Fatal("actual page must contain only parent, child outside")
	}
	closePage()
	budget := agentapi.NewNativeReadBudget(512 << 10)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	err = read.ResumeImportAdmissions(t.Context())
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("child admission escaped captured budget: %v", err)
	}
	if _, found, err := s.LoadRegistration(parent.ArchiveSessionID); err != nil || found {
		t.Fatalf("parent admitted despite refused child: %v %v", found, err)
	}
	if _, found, err := s.LoadRegistration(child.ArchiveSessionID); err != nil || found {
		t.Fatalf("child admitted despite refusal: %v %v", found, err)
	}
	if _, found, err := s.LoadImportStage(t.Context(), child); err != nil || !found {
		t.Fatalf("child source lost: %v %v", found, err)
	}
	loose, closeLoose := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(2<<20))
	defer closeLoose()
	if err := loose.ResumeImportAdmissions(t.Context()); err != nil {
		t.Fatalf("adequate shared owner control: %v", err)
	}
	if _, found, err := s.LoadRegistration(parent.ArchiveSessionID); err != nil || !found {
		t.Fatalf("adequate owner did not admit parent: %v %v", found, err)
	}
}

func TestStagedTypedDigestPreservesCanonicalWire(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	reg, _, err := s.LoadRegistration(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	stage, found, err := s.LoadImportStage(t.Context(), reg)
	if err != nil || !found {
		t.Fatalf("typed stage: %v", err)
	}
	want := stage.SHA256
	stage.SHA256 = ""
	stage.Registration.StagedImportSHA256 = ""
	raw, err := json.Marshal(stage)
	if err != nil {
		t.Fatal(err)
	}
	if readySHA(raw) != want {
		t.Fatal("typed digest changed canonical legacy-compatible bytes")
	}
	digest, err := stage.digest(t.Context())
	if err != nil || digest != want {
		t.Fatalf("bounded digest: %v", err)
	}
}

func TestStagedReceiptOnlyPageBindsExactImportOwner(t *testing.T) {
	for _, mode := range []string{"matching", "foreign", "changed", "missing"} {
		t.Run(mode, func(t *testing.T) {
			s, p, guard := stagedSelectionFixture(t, true)
			id := p.Bundle.ArchiveSessionID
			reg, _, err := s.LoadRegistration(id)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.VerifyAndBeginStagedRelease(t.Context(), id, stagedRemote(p), guard); err != nil {
				t.Fatal(err)
			}
			if mode == "changed" {
				altered := reg
				altered.ImportBatch = archive.NewImportBatch("changed-import")
				if err = local.Write(s.registrationPath(id), altered); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing" {
				if err = os.Remove(s.registrationPath(id)); err != nil {
					t.Fatal(err)
				}
			}
			stages, _, closePage, err := s.ImportStagesOwned(t.Context(), "", 50)
			defer closePage()
			if mode == "changed" || mode == "missing" {
				if !errors.Is(err, ErrReadySelectionFrozen) {
					t.Fatalf("unproven owner classified: %v", err)
				}
				return
			}
			if err != nil || len(stages) != 1 || !stages[0].ReleaseOnly {
				t.Fatalf("receipt-only page: %v %v", stages, err)
			}
			target := reg.ImportBatch.Recorded()
			if mode == "foreign" {
				target = "unrelated-import"
			}
			if stages[0].Registration.InBatch(target) != (mode == "matching") {
				t.Fatal("receipt-only import attribution mismatch")
			}
			if stages[0].Registration.ArchiveSessionID != id || stages[0].Registration.StagedImportSHA256 != reg.StagedImportSHA256 {
				t.Fatal("receipt lost pinned registration facts")
			}
		})
	}
}

func TestStagedReadablePendingFitsOneVerificationOwner(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	stageInfo, err := os.Stat(filepath.Join(s.Home(), importStageRelative(p.Bundle.ArchiveSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	pendingInfo, err := os.Stat(s.pendingPath(p.Bundle.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	largest, err := archive.SourceEncodingLineBound(t.Context(), p.Bundle, 128<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Enough for one selecting-stage retained view and actual deterministic
	// compression scratch. A duplicated validation retains a second full view
	// and exceeds the same captured owner; the wire and assertions are identical.
	limit := int64(1<<20) + 2*largest + 2*stageInfo.Size() + pendingInfo.Size() - 1
	budget := agentapi.NewNativeReadBudget(limit)
	reader, closeReader := s.WithReadBudget(t.Context(), budget)
	defer closeReader()
	loaded, found, err := reader.LoadPendingContext(t.Context(), p.Bundle.ArchiveSessionID)
	if err != nil || !found || loaded.StagedSelection == nil || *loaded.StagedSelection != *p.StagedSelection {
		t.Fatalf("single actual readable validation: %v %v", found, err)
	}
	if loaded.SourceReference() != p.SourceReference() || loaded.RequestToken != p.RequestToken {
		t.Fatal("verification changed selected authority")
	}
}
