package collector

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This witness exercises real admission, native lookup, source admission,
// filtering and revision planning. It does not claim lifecycle publication.
func TestWork72RealRegistrationUnobservedBaseRestoration(t *testing.T) {
	const aID = "00000000-0000-0000-0000-000000000071"
	const cID = "00000000-0000-0000-0000-000000000072"
	const dID = "00000000-0000-0000-0000-000000000073"
	const bID = "00000000-0000-0000-0000-000000000074"
	canonical := func(p string) string {
		t.Helper()
		p, e := filepath.EvalSymlinks(p)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	fixtureHome, project, native := canonical(t.TempDir()), canonical(t.TempDir()), canonical(t.TempDir())
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store, e := state.Open(fixtureHome)
	if e != nil {
		t.Fatal(e)
	}
	unlock, e := local.Lock(fixtureHome)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	cfg := config.Config{MachineID: "work72", Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(-time.Minute)}}}, Discovery: &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{native}}}
	if e = config.ReconcileDiscovery(&cfg, config.Config{}, at.Add(-time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e = config.Save(fixtureHome, cfg); e != nil {
		t.Fatal(e)
	}
	write := func(id string, start uint64, base string, end uint64, offset int, text string) (string, []byte) {
		t.Helper()
		meta := map[string]any{"id": aID, "cwd": project, "timestamp": at.Format(time.RFC3339Nano), "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli", "history_mode": "paginated"}
		if base != "" {
			meta["history_base"] = map[string]any{"thread_id": base, "end_ordinal_exclusive": end, "end_byte_offset": offset}
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		emit := func(v any) {
			t.Helper()
			if e := enc.Encode(v); e != nil {
				t.Fatal(e)
			}
		}
		emit(map[string]any{"type": "session_meta", "ordinal": start, "payload": meta})
		next := start + 1
		if base == "" {
			emit(map[string]any{"type": "event_msg", "ordinal": next, "timestamp": at.Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": aID, "root_turn_id": aID, "started_at": at.Format(time.RFC3339Nano)}})
			next++
		}
		emit(map[string]any{"type": "response_item", "ordinal": next, "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}})
		path := filepath.Join(native, "archived_sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
		if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, buf.Bytes(), 0600); e != nil {
			t.Fatal(e)
		}
		return path, bytes.Clone(buf.Bytes())
	}
	a, aBytes := write(aID, 0, "", 0, 0, "A unique")
	// Establish absence using the real bounded recovery API before admission.
	complete := false
	for n := 0; n < 8; n++ {
		var err error
		complete, err = store.RecoverSessionIndexScheduled(t.Context(), state.SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
	}
	if !complete {
		t.Fatal("real session-index recovery did not finish in the existing slice bound")
	}
	var reg archive.SessionRegistration
	for n := 0; n < 8; n++ {
		h, err := discovery.Run(t.Context(), store, cfg, discovery.Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(time.Minute) }})
		if err != nil {
			t.Fatal("real discovery admission", h, err)
		}
		regs, err := store.LoadRegistrations()
		if err != nil {
			t.Fatal(err)
		}
		if len(regs) == 1 {
			reg = regs[0]
			break
		}
		done, err := store.RecoverSessionIndexScheduled(t.Context(), state.SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		_ = done
	}
	if reg.CodexBinding == nil || reg.Origin != archive.SessionOriginDiscovery || reg.CodexBinding.NativeThreadID != aID || reg.CodexBinding.Home != native || reg.CodexBinding.FirstNativeTaskAt.IsZero() {
		t.Fatalf("no genuine native candidate.Binding admission: %#v", reg)
	}
	regBytes, e := json.Marshal(reg)
	if e != nil {
		t.Fatal(e)
	}
	c, _ := write(cID, 3, aID, 3, len(aBytes), "C unique")
	d, dBytes := write(dID, 3, aID, 3, len(aBytes), "D unique")
	b, _ := write(bID, 5, dID, 5, len(dBytes), "B unique")
	// Real SQLite current-locator authority, fixed for all three observations.
	db, e := sql.Open("sqlite", filepath.Join(native, "state_5.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT,created_at_ms INTEGER,updated_at_ms INTEGER)"); e != nil {
		t.Fatal(e)
	}
	if _, e = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?,?,?)", aID, c, at.UnixMilli(), at.UnixMilli()); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	type held struct {
		Hash  [32]byte
		Size  int64
		Mtime int64
		Mode  os.FileMode
	}
	facts := func() map[string]held {
		t.Helper()
		out := map[string]held{}
		for _, p := range []string{a, c, b, filepath.Join(native, "state_5.sqlite")} {
			raw, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			s, e := os.Stat(p)
			if e != nil {
				t.Fatal(e)
			}
			out[p] = held{sha256.Sum256(raw), s.Size(), s.ModTime().UnixNano(), s.Mode()}
		}
		return out
	}
	fixed := facts()
	request, found, e := store.LoadRequest(reg.ArchiveSessionID)
	if e != nil || !found || request.Token == "" {
		t.Fatal("admission did not owe request", e)
	}
	var initialSet agentapi.CodexRolloutSet
	var initialSignature agentapi.SourceSignature
	checkPlan := func(plan *revisionPlan) {
		t.Helper()
		if plan == nil || plan.Current != cID {
			t.Fatalf("wrong selected plan: %#v", plan)
		}
		found := false
		for _, s := range plan.Sources {
			if revisionID(s.Bundle) != bID {
				continue
			}
			found = true
			decoded, e := archive.ReadSourceBundle(bytes.NewReader(s.Bytes), archive.DecodeOptions{MaxUncompressedBytes: 128 << 20})
			if e != nil {
				t.Fatal("decode planned native source", e)
			}
			messages := 0
			for _, row := range decoded.NativeRecords {
				raw, e := json.Marshal(row)
				if e != nil {
					t.Fatal(e)
				}
				if bytes.Contains(raw, []byte(`"role":"user"`)) {
					messages++
				}
			}
			if messages != 3 {
				t.Fatalf("incomplete B message set: %d", messages)
			}
			if !work72OwnedRecord(t, decoded, "A unique", 2) || work72OwnedRecord(t, decoded, "C unique", 4) {
				t.Fatal("B graph is incomplete or mixed with C")
			}
			if !work72OwnedRecord(t, decoded, "B unique", 6) {
				t.Fatal("B suffix not retained at owned ordinal")
			}
			if !work72OwnedRecord(t, decoded, "D unique", 4) {
				t.Fatal("B dependency suffix not retained at owned ordinal")
			}
			if sha256.Sum256(s.Bytes) == ([32]byte{}) || !s.Bundle.Capture.CapturedAt.Equal(at.Add(2*time.Minute)) {
				t.Fatal("invalid stage/time")
			}
			if fmt.Sprintf("%x", sha256.Sum256(s.Bytes)) != s.Reference.SHA256 {
				t.Fatal("B compressed checksum disagrees")
			}
		}
		if !found {
			t.Fatal("B complete native source absent")
		}
	}
	// Each phase opens fresh real provider/lookup to avoid cached directory entries.
	phase := func(label string, missing bool) {
		t.Helper()
		lookup, e := discovery.NewCodexRolloutLookup(t.Context(), store, []string{native})
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			if e := lookup.CloseReadOnly(); e != nil {
				t.Fatal(e)
			}
		}()
		// Observe only actual validated header facts, never synthesized identities/D.
		for _, p := range []string{a, c, b} {
			h := sourcefacts.ReadHeader(t.Context(), native, p)
			if h.Identity == nil {
				t.Fatal("invalid observed native header", p, h.Outcome)
			}
			s, e := os.Stat(p)
			if e != nil {
				t.Fatal(e)
			}
			lookup.Observe(discovery.SourceDescriptor{Kind: archive.SourceKindFile, Root: native, Locator: p, StableKey: h.Identity.RolloutID}, discovery.Fingerprint{Size: s.Size(), Mtime: s.ModTime().UnixNano()}, h.Identity)
		}
		set, e := lookup.Thread(t.Context(), aID)
		if e != nil {
			t.Fatal(e)
		}
		if set.Current == nil || set.Current.Path != c || len(set.Candidates) != 3 {
			t.Fatalf("wrong native candidates: %#v", set)
		}
		for _, r := range set.Candidates {
			if r.Path == d {
				t.Fatal("D became observed")
			}
		}
		published, e := store.LoadPublishedState(reg.ArchiveSessionID)
		if e != nil {
			t.Fatal(e)
		}
		opts := Options{Sources: builtin.NewBuiltins(), Parsers: builtin.NewBuiltins(), CodexRollouts: lookup, MachineID: "work72", Now: func() time.Time { return at.Add(2 * time.Minute) }}
		scan := newSessionScan(t.Context(), store, storagetest.NewMemoryStore(), reg, request, published, at.Add(2*time.Minute), opts)
		defer scan.releaseRetained()
		read, ok, e := scan.read()
		if e != nil || !ok {
			t.Fatal(label, "selected native read", e)
		}
		if label == "positive" {
			reg = scan.reg
			regBytes, e = json.Marshal(reg)
			if e != nil {
				t.Fatal(e)
			}
		}
		reader, available := newSourceReader(scan.reg, scan.opts)
		if !available {
			t.Fatal("registered reader unavailable")
		}
		signed, e := reader.Signature(t.Context())
		if e != nil || signed.observation.Signature != read.observed.observation.Signature {
			t.Fatal("real registered Signature disagrees with read", e)
		}
		active, _, e := scan.build(read)
		if e != nil {
			t.Fatal(e)
		}
		if active.History == nil || active.History.ActiveRolloutID != cID || len(active.History.Spans) != 2 || active.History.Spans[0].RolloutID != aID || active.History.Spans[1].RolloutID != cID {
			t.Fatalf("wrong selected A/C graph: %#v", active.History)
		}
		if label == "positive" {
			initialSet = set
			initialSignature = read.observed.observation.Signature
		} else if !reflect.DeepEqual(set, initialSet) || read.observed.observation.Signature != initialSignature {
			t.Fatal("candidate facts/signature changed with only unobserved D")
		}
		plan, e := scan.reconcileRevisions(read, active)
		if missing {
			if !agentapi.HasFailure(e, agentapi.Unavailable) || !work72BaseUnavailable(e) || plan != nil {
				t.Errorf("missing D must leave classified retry owed, plan=%#v err=%v", plan, e)
			}
		} else {
			if e != nil {
				t.Fatal(label, e)
			}
			checkPlan(plan)
		}
		current, found, e := store.LoadRequest(reg.ArchiveSessionID)
		if e != nil || !found || current.Token != request.Token {
			t.Fatal("planning acknowledged request", e)
		}
		if _, found, e := store.LoadScanSignature(reg.ArchiveSessionID); e != nil || found {
			t.Fatal("planning settled signature", e)
		}
		if pending, e := store.HasPending(reg.ArchiveSessionID); e != nil || pending {
			t.Fatal("planner manufactured lifecycle journal", e)
		}
		if !reflect.DeepEqual(facts(), fixed) {
			t.Fatal("A/C/B facts mutated")
		}
		currentReg, found, e := store.LoadRegistration(reg.ArchiveSessionID)
		if e != nil || !found {
			t.Fatal(e)
		}
		raw, e := json.Marshal(currentReg)
		if e != nil || !bytes.Equal(raw, regBytes) {
			t.Fatal("registration mutated", e)
		}
	}
	phase("positive", false)
	t.Log("WORK72 positive phase completed")
	hidden := filepath.Join(t.TempDir(), "D-hidden.jsonl")
	if e = os.Rename(d, hidden); e != nil {
		t.Fatal(e)
	}
	phase("hidden", true)
	t.Log("WORK72 hidden phase completed")
	if e = os.Rename(hidden, d); e != nil {
		t.Fatal(e)
	}
	restored, e := os.ReadFile(d)
	if e != nil || !bytes.Equal(restored, dBytes) {
		t.Fatal("D restoration changed bytes", e)
	}
	phase("restored", false)
	t.Log("WORK72 restored phase completed")
}

// Local assertion helper; does not alter archive.SourceBundle or native providers.
func work72OwnedRecord(t *testing.T, b archive.SourceBundle, text string, ordinal uint64) bool {
	t.Helper()
	for i, row := range b.NativeRecords {
		raw, e := json.Marshal(row)
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(raw, []byte(text)) && b.OwnRecord(i) && i < len(b.Ordinals) && b.Ordinals[i] == ordinal {
			return true
		}
	}
	return false
}

func work72BaseUnavailable(err error) bool {
	var source *agentapi.SourceError
	return errors.As(err, &source) && source.Kind == agentapi.Unavailable && source.Cause == agentapi.CauseHistoryBaseUnavailable
}
