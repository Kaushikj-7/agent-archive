package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Extend known qualified synthetic assistant records before the actual source
// snapshot/proof/filter is acquired. No caller-made stage or receipt is used.
func largeNativeImportFixture(t *testing.T) nativeImportPrepareFixture {
	t.Helper()
	native := newNativeAdmissionFixture(t)
	raw, err := os.ReadFile(native.path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var output bytes.Buffer
	for _, line := range lines[:len(lines)-1] {
		output.Write(line)
		output.WriteByte('\n')
	}
	// Privacy retains at most 64KiB per text. Use many valid smaller records,
	// not oversized values whose retained output would silently shrink.
	const messages = 320
	for i := 0; i < messages; i++ {
		var row map[string]any
		if err := json.Unmarshal(lines[len(lines)-2], &row); err != nil {
			t.Fatal(err)
		}
		row["id"] = fmt.Sprintf("00000000-0000-0000-0000-%012x", 2000+i)
		row["sequence"] = 9 + i
		payload := row["payload"].(map[string]any)
		payload["source_run_record_id"] = fmt.Sprintf("00000000-0000-4000-8000-%012x", 3000+i)
		payload["source_run_record_sequence"] = 3 + i
		event := payload["event"].(map[string]any)
		event["message_id"] = fmt.Sprintf("synthetic-large-%d", i)
		event["text"] = strings.Repeat("x", 63<<10)
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(encoded)
		output.WriteByte('\n')
	}
	var end map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &end); err != nil {
		t.Fatal(err)
	}
	end["sequence"] = 9 + messages
	encoded, err := json.Marshal(end)
	if err != nil {
		t.Fatal(err)
	}
	output.Write(encoded)
	output.WriteByte('\n')
	if err := os.WriteFile(native.path, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	// The ordinary production filtered ceiling is MaxRecordBytes. The fixture
	// retains the same existing 256MiB native and 128MiB state ledgers.
	return nativeImportPrepareFixtureFromNative(t, native, "", archive.MaxRecordBytes)
}

func TestNativeImportLargeStagePublicRecoveryUsesBoundedTemporaryLoans(t *testing.T) {
	f := largeNativeImportFixture(t)
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, gap := range p.filtered.Gaps {
		if gap.Code == "content_truncated" {
			t.Fatal("large fixture depended on privacy truncation")
		}
	}
	interruption := errors.New("synthetic after stage")
	f.native.store.onIndexStep = func(step string) error {
		if step == "import-stage" {
			return interruption
		}
		return nil
	}
	if _, err := f.native.store.CommitNativeImport(t.Context(), p); !errors.Is(err, interruption) {
		t.Fatal("large valid stage not durably selected", err)
	}
	id := p.reg.ArchiveSessionID
	info, err := os.Stat(f.native.store.home + "/" + importStageRelative(id))
	if err != nil || info.Size() < 18<<20 || info.Size() > MaxImportStageBytes {
		t.Fatal("large stage fixture outside intended valid range", info, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	f.native.assertReleased(t)
	if err := f.confirmation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.candidate.Root.Canonical); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.native.store.home)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	read, closeRead := reopened.WithReadBudget(t.Context(), budget)
	before := budget.Available()
	if err := read.ResumeImportAdmissions(t.Context()); err != nil {
		closeRead()
		t.Fatal("large public recovery exhausted bounded ledger", err)
	}
	closeRead()
	if budget.Available() != before {
		t.Fatal("recovery retained expired temporary stage charge", budget.Available(), before)
	}
	if _, found, err := reopened.LoadRegistration(id); err != nil || !found {
		t.Fatal("large stage did not become one owner", found, err)
	}
}
