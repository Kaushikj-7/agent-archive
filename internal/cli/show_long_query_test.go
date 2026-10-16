package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestShowLongTitlesWithHarnessReachJSONAndTranscript(t *testing.T) {
	f := newShowLimitFixture(t, 1)
	key, err := archive.MetadataObjectKey("codex", f.id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.mem.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Review the complete synthetic publication workflow", "syntheticpublicationworkflowtitlelongerthanthirtytwo"} {
		metadata.Title = title
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.mem.Put(t.Context(), key, data); err != nil {
			t.Fatal(err)
		}
		for _, transcript := range []bool{false, true} {
			args := []string{title, "--harness", "codex", "--json"}
			if transcript {
				args = append(args, "--transcript")
			}
			out, errOut, code := runShow(t, f.env, args...)
			if code != 0 || errOut != "" || !strings.Contains(out, f.id) || transcript && !strings.Contains(out, "reply-0") {
				t.Errorf("title=%q transcript=%v: code=%d stderr=%s output=%s", title, transcript, code, errOut, out)
			}
		}
	}
}

type showProbeFailureStore struct {
	storage.ObjectStore
	failure error
}

func (s showProbeFailureStore) Get(context.Context, string) ([]byte, error) {
	return nil, s.failure
}

func TestShowLongExactIDsAndProbeErrorsKeepPrecedence(t *testing.T) {
	id := "synthetic-long-session-id-that-exceeds-thirty-two"
	for _, harness := range []string{"", "codex"} {
		env, mem := statsEnv(t)
		(syntheticSession{id: id, harness: "codex", project: "p", captured: statsNow}).publish(t, mem)
		other := (syntheticSession{id: "other", harness: "codex", project: "p", captured: statsNow}).build()
		other.Title = id
		data, err := json.Marshal(other)
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Put(t.Context(), "sessions/codex/other/metadata.json", data); err != nil {
			t.Fatal(err)
		}
		args := []string{id, "--json"}
		if harness != "" {
			args = append(args, "--harness", harness)
		}
		out, errOut, code := runShow(t, env, args...)
		if code != 0 || errOut != "" || !strings.Contains(out, `"session_id": "`+id+`"`) {
			t.Fatalf("harness=%q exact ID: code=%d stderr=%s output=%s", harness, code, errOut, out)
		}
	}
	for _, failure := range []error{errors.New("synthetic provider outage"), errors.Join(reader.ErrInvalidMetadata, storage.ErrNotFound)} {
		env, mem := statsEnv(t)
		env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
			return showProbeFailureStore{ObjectStore: mem, failure: failure}, nil
		}
		_, errOut, code := runShow(t, env, id, "--harness", "codex", "--json")
		if code != 1 || !strings.Contains(errOut, failure.Error()) {
			t.Fatalf("probe failure: code=%d stderr=%s", code, errOut)
		}
	}
	env, mem := statsEnv(t)
	if err := mem.Put(t.Context(), "sessions/codex/"+id+"/metadata.json", []byte("invalid JSON")); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := runShow(t, env, id, "--harness", "codex", "--json")
	if code != 1 || !strings.Contains(errOut, "invalid metadata") {
		t.Fatalf("decode failure: code=%d stderr=%s", code, errOut)
	}
	(syntheticSession{id: id, harness: "codex", project: "p", captured: statsNow}).publish(t, mem)
	(syntheticSession{id: id, harness: "claude", project: "p", captured: statsNow}).publish(t, mem)
	_, errOut, code = runShow(t, env, id, "--json")
	if code != 1 || !strings.Contains(errOut, "more than one harness") {
		t.Fatalf("ambiguity: code=%d stderr=%s", code, errOut)
	}
}

func TestShowExact32ByteIDWithHarnessReadsOneBody(t *testing.T) {
	env, mem := statsEnv(t)
	id := strings.Repeat("a", 32)
	(syntheticSession{id: id, harness: "codex", project: "p", captured: statsNow}).publish(t, mem)
	store := storagetest.NewMeasuredStore(mem, 0)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	out, errOut, code := runShow(t, env, id, "--harness", "codex", "--json")
	metrics := store.Metrics()
	if code != 0 || errOut != "" || !strings.Contains(out, id) || metrics.Gets != 1 || metrics.Lists != 0 {
		t.Fatalf("exact ID: code=%d stderr=%s metrics=%+v output=%s", code, errOut, metrics, out)
	}
}
