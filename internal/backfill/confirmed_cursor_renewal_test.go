package backfill

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type confirmedCursorMutation struct {
	agentapi.SourcesLookup
	settledCapturePort
	liveCapturePort
	after func()
}

func (s confirmedCursorMutation) CaptureSettledCursor(request *nativefacts.CaptureRequest) (*nativecapture.Proof, error) {
	proof, err := s.settledCapturePort.CaptureSettledCursor(request)
	if err == nil {
		s.after()
	}
	return proof, err
}

func (s confirmedCursorMutation) CaptureLiveCursor(request *nativefacts.LiveCaptureRequest) (*nativecapture.LiveProof, error) {
	proof, err := s.liveCapturePort.CaptureLiveCursor(request)
	if err == nil {
		s.after()
	}
	return proof, err
}

func TestConfirmedCursorRenewsAuthorityAfterCapture(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := map[bool]string{false: "settled", true: "live"}[live]
		t.Run(name, func(t *testing.T) {
			for _, mutation := range []string{"proof", "destination", "privacy", "paused"} {
				t.Run(mutation, func(t *testing.T) {
					r, c, cfg, database := settledCursorFixture(t)
					if live {
						writer := startCursorWriter(t, database, true)
						writer.do("unselected")
					}
					current, captured := true, false
					c.projectResolutionCurrent = func() bool { return current }
					r.Sources = confirmedCursorMutation{SourcesLookup: testSources, settledCapturePort: testSources, liveCapturePort: testSources, after: func() {
						captured = true
						switch mutation {
						case "proof":
							current = false
						case "destination":
							cfg.Storage.Bucket = "changed-synthetic-destination"
						case "privacy":
							cfg.SkillEvidence = config.SkillEvidenceNone
						case "paused":
							cfg.Paused = true
						}
						if mutation != "proof" {
							if err := config.Save(r.Home, cfg); err != nil {
								t.Fatal(err)
							}
						}
					}}
					result, err := r.Run([]Candidate{c})
					if !captured {
						t.Fatal("actual native capture boundary not reached", result, err)
					}
					stages, _, loadErr := r.Store.ImportStages(t.Context(), "", 50)
					if loadErr != nil || len(stages) != 0 || len(result.Sessions) != 0 {
						t.Fatal("changed capture authority became durable", len(stages), result, err, loadErr)
					}
				})
			}
		})
	}
}

func TestSettledCursorPreparedStageDoesNotRenewNativeProof(t *testing.T) {
	r, c, cfg, database := settledCursorFixture(t)
	id := ""
	interrupted := errors.New("synthetic prepared Cursor interruption")
	r.AfterStage = func(stagedID string) error {
		id = stagedID
		return interrupted
	}
	if _, err := r.Run([]Candidate{c}); !errors.Is(err, interrupted) || id == "" {
		t.Fatal("prepared boundary not reached", id, err)
	}
	if err := os.Remove(database); err != nil {
		t.Fatal(err)
	}
	c.projectResolutionCurrent = func() bool { t.Fatal("frozen Cursor rechecked native authority"); return false }
	r.AfterStage = nil
	work := parentWork{c: c, id: id}
	if err := r.stage(t.Context(), &work); err != nil || work.stagedSHA256 == "" {
		t.Fatal("loaded Cursor stage renewed native proof", err)
	}
	// Production CLI resumes the existing durable admission owner before
	// invoking Run on fresh candidates; direct Run is not a recovery owner.
	if err := r.Store.ResumeAllImportAdmissions(t.Context()); err != nil {
		t.Fatal(err)
	}
	fresh := state.OpenReadOnly(r.Home)
	reg, found, err := fresh.LoadRegistration(id)
	if err != nil || !found || reg.StagedImportSHA256 != work.stagedSHA256 {
		t.Fatal("prepared Cursor recovery changed ownership", found, err)
	}
	guard, err := local.LockCollectorGuard(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	remote := storagetest.NewMemoryStore()
	result, err := collector.Run(t.Context(), fresh, remote, collector.Options{CollectorGuard: guard, Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return r.AdmittedAt.Add(time.Minute) }, AcceptSession: cfg.AcceptSession})
	defer result.CloseStagedResults()
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal("frozen Cursor publication after native deletion", result, err)
	}
	published, err := fresh.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	metadata, found, err := published.LastPublishedMetadata()
	if err != nil || !found {
		t.Fatal("retained Cursor publication missing", found, err)
	}
	if _, err := reader.LoadSource(t.Context(), remote, metadata, reader.Limits{}); err != nil {
		t.Fatal(err)
	}
}
