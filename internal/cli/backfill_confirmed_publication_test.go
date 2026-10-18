package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type confirmedPublicationFault struct {
	*storagetest.MemoryStore
	lostAck bool
}

func (s *confirmedPublicationFault) Put(ctx context.Context, key string, body []byte) error {
	if err := s.MemoryStore.Put(ctx, key, body); err != nil {
		return err
	}
	if s.lostAck && strings.HasSuffix(key, "/metadata.json") {
		return context.Canceled
	}
	return nil
}

func TestBackfillConfirmedRetriesProviderFailureWithFrozenSelection(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider unavailable", true: "lost metadata acknowledgement"}[lostAck], func(t *testing.T) {
			f, bucket, mapping := newHistoricalOnlyFixture(t)
			remote := &confirmedPublicationFault{MemoryStore: bucket}
			unavailable := false
			f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				if unavailable {
					return nil, errors.New("synthetic provider unavailable")
				}
				return remote, nil
			}
			f.env.backfillCheckpoint = func(step string) error {
				if step == "uploading" {
					unavailable, remote.lostAck = !lostAck, lostAck
				}
				return nil
			}
			args := []string{"--yes", "--mapping-file", mapping}
			out, stderr, code := f.importRun(t, nil, false, args...)
			if code != 1 {
				t.Fatal("provider fault returned success", code, stderr, out)
			}
			local := state.OpenReadOnly(f.data)
			regs, err := local.LoadRegistrations()
			must(t, err)
			if len(regs) != 1 || regs[0].StagedImportSHA256 == "" {
				t.Fatal("provider failure lost exact admitted owner", regs)
			}
			reg := regs[0]
			stage, found, err := local.LoadImportStage(t.Context(), reg)
			must(t, err)
			if !found {
				t.Fatal("provider failure released staged evidence")
			}
			pending, hadPending, err := local.LoadPending(reg.ArchiveSessionID)
			must(t, err)
			if lostAck && !hadPending {
				t.Fatal("lost acknowledgement lost frozen pending publication")
			}
			must(t, os.RemoveAll(filepath.Join(f.userHome, ".claude")))
			unavailable, remote.lostAck = false, false
			f.env.backfillCheckpoint = nil
			out, stderr, code = f.importRun(t, nil, false, args...)
			if code != 0 {
				t.Fatal("retained provider retry", code, stderr, out)
			}
			current, found, err := local.LoadRegistration(reg.ArchiveSessionID)
			must(t, err)
			if !found || current.StagedImportSHA256 != stage.SHA256 || current.AdmittedAt != reg.AdmittedAt || current.HistoricalProject == nil || current.HistoricalProject.ProjectID != reg.HistoricalProject.ProjectID {
				t.Fatal("provider retry changed frozen provenance")
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			must(t, err)
			metadata, found, err := published.LastPublishedMetadata()
			must(t, err)
			if !found {
				t.Fatal("provider retry returned without verified publication")
			}
			_, err = reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
			must(t, err)
			if hadPending && !bytes.Equal(pending.MetadataBytes, published.Metadata()) {
				t.Fatal("lost acknowledgement changed exact pending metadata")
			}
			if _, found, err := local.LoadImportStage(t.Context(), current); err != nil || found {
				t.Fatal("verified retry did not release its stage", found, err)
			}
		})
	}
}

func TestBackfillQuotaPressurePreservesUnrelatedStaging(t *testing.T) {
	f, _ := foregroundChildren(t, 2)
	_, stderr, code := f.importRun(t, nil, false, "--yes", "--background", "--harness", "claude")
	if code != 0 {
		t.Fatal("unrelated durable cohort", code, stderr)
	}
	local := state.OpenReadOnly(f.data)
	stages, _, err := local.ImportStages(t.Context(), "", 50)
	must(t, err)
	if len(stages) == 0 {
		t.Fatal("missing unrelated stage")
	}
	owner := stages[0].Registration
	directory := filepath.Join(f.data, "sessions", owner.ArchiveSessionID, "pending-sources")
	must(t, os.MkdirAll(directory, 0700))
	pressure := filepath.Join(directory, strings.Repeat("a", 64)+".gz")
	file, err := os.OpenFile(pressure, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	must(t, err)
	// This sparse retained payload exercises real quota accounting, not a
	// physical provider/performance claim or permission to evict unknown bytes.
	must(t, errors.Join(file.Truncate(1<<29), file.Close()))
	before, err := os.Stat(pressure)
	must(t, err)
	bucket, err := f.env.OpenStore(mustLoadConfig(t, f.data))
	must(t, err)
	out, stderr, code := f.importRun(t, nil, false, "--yes", "--harness", "codex")
	if code != 1 || !strings.Contains(stderr, "run agent-archive backfill again with the same options") {
		t.Fatal("pressure lacks precise continuation", code, stderr, out)
	}
	after, err := os.Stat(pressure)
	must(t, err)
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		t.Fatal("unrelated retained quota payload changed")
	}
	stage, found, err := local.LoadImportStage(t.Context(), owner)
	must(t, err)
	if !found || stage.SHA256 != stages[0].SHA256 {
		t.Fatal("quota pressure discarded unrelated staged evidence")
	}
	objects, err := bucket.List(t.Context(), "")
	must(t, err)
	if len(objects) != 0 {
		t.Fatal("pressure uploaded unrelated archive content")
	}
	// Release only this test-created pressure fixture, then retry the same
	// exact import. The unrelated real staged selection remains untouched.
	must(t, os.Remove(pressure))
	out, stderr, code = f.importRun(t, nil, false, "--yes", "--harness", "codex")
	if code != 0 {
		t.Fatal("quota retry failed", code, stderr, out)
	}
	if stage, found, err := local.LoadImportStage(t.Context(), owner); err != nil || !found || stage.SHA256 != stages[0].SHA256 {
		t.Fatal("retry changed unrelated stage", found, err)
	}
}
