package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

type syntheticMethod string

const (
	syntheticInitialize syntheticMethod = "initialize"
	syntheticRead       syntheticMethod = "thread/read"
)

type syntheticCase string

const (
	syntheticRequest   syntheticCase = "request"
	syntheticLine      syntheticCase = "line"
	syntheticAggregate syntheticCase = "aggregate"
	syntheticMalformed syntheticCase = "malformed"
)

type syntheticLabelHost struct {
	home          string
	name          string
	version       string
	threadID      string
	writes        [][]byte
	responses     [][]byte
	closed        int
	notify        bool
	serverRequest bool
	extra         string
}

func (h *syntheticLabelHost) WriteLine(_ context.Context, b []byte) error {
	h.writes = append(h.writes, append([]byte(nil), b...))
	var request struct {
		ID     int             `json:"id"`
		Method syntheticMethod `json:"method"`
		Params map[string]any  `json:"params"`
	}
	if json.Unmarshal(b, &request) != nil {
		return errors.New("invalid request")
	}
	switch request.Method {
	case syntheticInitialize:
		client, ok := request.Params["clientInfo"].(map[string]any)
		capabilities, capsOK := request.Params["capabilities"].(map[string]any)
		if !ok || !capsOK || client["name"] != "agent_archive_probe" || client["version"] != "0.0.0" || capabilities["experimentalApi"] != false {
			return errors.New("unexpected initialization contract")
		}
		h.responses = append(h.responses, []byte(fmt.Sprintf(`{"id":%d,"result":{"codexHome":%q,"userAgent":%q}}`, request.ID, h.home, "agent_archive_probe/"+h.version+" (synthetic)")))
	case syntheticRead:
		if request.Params["includeTurns"] != false {
			return errors.New("turn request forbidden")
		}
		if h.notify {
			h.responses = append(h.responses, []byte(`{"method":"progress","params":{"private":"discarded"}}`))
		}
		if h.serverRequest {
			h.responses = append(h.responses, []byte(`{"id":99,"method":"approval/request"}`))
		}
		name := ""
		if h.name != "missing" {
			name = `,"name":` + h.name
		}
		h.responses = append(h.responses, []byte(fmt.Sprintf(`{"id":%d,"result":{"thread":{"id":%q%s,"turns":[],"extra":"discarded"%s}}}`, request.ID, h.threadID, name, h.extra)))
	}
	return nil
}

func (h *syntheticLabelHost) ReadLine(ctx context.Context) ([]byte, error) {
	if len(h.responses) == 0 {
		return nil, ctx.Err()
	}
	b := h.responses[0]
	h.responses = h.responses[1:]
	return b, nil
}

func (h *syntheticLabelHost) Close() error { h.closed++; return nil }

func nativeRequest(home, id string) agentapi.LabelRequest {
	return agentapi.LabelRequest{Registration: archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: id, Harness: archive.Harness{Name: "codex"}, DiscoveryRoot: home, TranscriptPath: home + "/sessions/a.jsonl"}, Context: agentapi.LabelContext{NativeID: id, Ordinary: true, APICompatible: true, Producer: "0.159.2", Contract: "test"}}
}

func TestNativeLabelWireAndNullableName(t *testing.T) {
	home := t.TempDir()
	id := "01900000-0000-7000-8000-000000000001"
	for _, tc := range []struct {
		name    string
		home    string
		version string
		thread  string
		extra   string
		valid   bool
		state   archive.SessionLabelState
	}{
		{`"a native name"`, home, "0.159.2", id, "", true, archive.SessionLabelPresent},
		{"null", home, "0.159.2", id, "", true, archive.SessionLabelAbsent},
		{"null", home, "0.159.2", id, `,"turns":[ ]`, true, archive.SessionLabelAbsent},
		{"missing", home, "0.159.2", id, "", false, ""},
		{"123", home, "0.159.2", id, "", false, ""},
		{"null", home + "/other", "0.159.2", id, "", false, ""},
		{"null", home, "0.100.0", id, "", false, ""},
		{"null", home, "0.159.2", id + "x", "", false, ""},
		{"null", home, "0.159.2", id, `,"turns":[{"private":"not retained"}]`, false, ""},
	} {
		h := &syntheticLabelHost{home: tc.home, name: tc.name, version: tc.version, threadID: tc.thread, extra: tc.extra, notify: true}
		provider := NativeLabelProvider{Host: func(context.Context, string) (agentapi.LabelTransport, error) { return h, nil }}
		got := provider.LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{home}}, []agentapi.LabelRequest{nativeRequest(home, id)})
		label, ok := got[id]
		if tc.valid && (!ok || label.State != tc.state || label.Source != archive.SessionLabelAPI) {
			t.Errorf("%+v: got %+v", tc, label)
		}
		// A refused API answer falls back to the home's name index, which
		// holds no entry here.
		if !tc.valid && (!ok || label.State != archive.SessionLabelAbsent || label.Source != archive.SessionLabelIndex) {
			t.Errorf("%+v: fallback got %+v", tc, label)
		}
		if h.closed == 0 {
			t.Fatal("host not closed")
		}
		if tc.valid {
			if len(h.writes) != 3 || string(h.writes[1]) != `{"method":"initialized"}` {
				t.Fatalf("unexpected wire: %s", h.writes)
			}
			if !strings.Contains(string(h.writes[2]), `"includeTurns":false`) {
				t.Fatal("missing read-only projection")
			}
		}
	}
}

func TestNativeLabelsAreLazyAndIsolateHomes(t *testing.T) {
	homes := []string{t.TempDir(), t.TempDir()}
	ids := []string{"01900000-0000-7000-8000-000000000001", "01900000-0000-7000-8000-000000000002"}
	starts := map[string]int{}
	hosts := map[string]*syntheticLabelHost{}
	provider := NativeLabelProvider{Host: func(_ context.Context, home string) (agentapi.LabelTransport, error) {
		starts[home]++
		id := ids[0]
		if home == homes[1] {
			id = ids[1]
		}
		h := &syntheticLabelHost{home: home, name: `"name"`, version: "0.159.2", threadID: id}
		hosts[home] = h
		return h, nil
	}}
	requests := []agentapi.LabelRequest{nativeRequest(homes[0], ids[0]), nativeRequest(homes[0], ids[0]), nativeRequest(homes[1], ids[1])}
	got := provider.LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: homes}, requests)
	if len(got) != 2 || starts[homes[0]] != 1 || starts[homes[1]] != 1 {
		t.Fatalf("got %+v starts %+v", got, starts)
	}
	for _, h := range hosts {
		if h.closed != 1 {
			t.Fatal("close must be once after successful pass")
		}
	}
	starts = map[string]int{}
	requests[0].Context.Producer = "unknown"
	requests[1].Context.Ordinary = false
	requests[2].Registration.CaptureFrozen = true
	_ = provider.LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: homes}, requests)
	if len(starts) != 0 {
		t.Fatal("ineligible sessions started hosts")
	}
}

func TestNativeLabelRefusesRequestsAndResponseBudgets(t *testing.T) {
	for _, kind := range []syntheticCase{syntheticRequest, syntheticLine, syntheticAggregate, syntheticMalformed} {
		h := &syntheticLabelHost{home: "/synthetic", version: "0.159.2", threadID: "id", name: "null"}
		switch kind {
		case syntheticRequest:
			h.serverRequest = true
		case syntheticLine:
			h.responses = [][]byte{[]byte(strings.Repeat("x", (256<<10)+1))}
		case syntheticAggregate:
			for range 5 {
				h.responses = append(h.responses, []byte(`{"method":"progress","params":"`+strings.Repeat("x", 240<<10)+`"}`))
			}
		case syntheticMalformed:
			h.responses = [][]byte{[]byte("{")}
		}
		budget := 1 << 20
		r := labelRPC{host: h, remaining: &budget}
		if _, ok := r.read(context.Background(), "id"); ok {
			t.Fatal(string(kind) + " accepted")
		}
	}
}

type deadlineLabelHost struct {
	syntheticLabelHost
	t *testing.T
}

func (h *deadlineLabelHost) ReadLine(ctx context.Context) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		h.t.Fatal("unbounded native operation")
	}
	limit := 2 * time.Second
	if len(h.writes) > 2 {
		limit = 250 * time.Millisecond
	}
	if time.Until(deadline) > limit {
		h.t.Fatal("native deadline exceeds budget")
	}
	return h.syntheticLabelHost.ReadLine(ctx)
}

func TestNativeLabelPassAndReadHaveDeadlines(t *testing.T) {
	home := t.TempDir()
	id := "01900000-0000-7000-8000-000000000001"
	h := &deadlineLabelHost{syntheticLabelHost: syntheticLabelHost{home: home, version: "0.159.2", threadID: id, name: "null"}, t: t}
	starts := 0
	p := NativeLabelProvider{Host: func(ctx context.Context, _ string) (agentapi.LabelTransport, error) {
		starts++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Fatal("unbounded startup")
		}
		return h, nil
	}}
	request := nativeRequest(home, id)
	if got := p.LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{home}}, []agentapi.LabelRequest{request}); len(got) != 1 {
		t.Fatal(got)
	}
	starts = 0
	requests := make([]agentapi.LabelRequest, 65)
	for i := range requests {
		requests[i] = request
	}
	_ = p.LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{home}}, requests)
	if starts != 0 {
		t.Fatal("over-budget pass started a host")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = p.LookupLabels(ctx, agentapi.LabelEnvironment{Homes: []string{home}}, []agentapi.LabelRequest{request})
	if starts != 0 {
		t.Fatal("cancelled pass started a host")
	}
}

func TestNativeLabelCompatibilityComesFromRetainedSource(t *testing.T) {
	_, request := labelFixture(t)
	for _, tc := range []struct {
		mode       any
		compatible bool
	}{{"legacy", true}, {"paginated", true}, {"unknown", false}, {3, false}} {
		b := request.Bundle
		b.NativeRecords = append([]map[string]any(nil), b.NativeRecords...)
		b.NativeRecords[0] = map[string]any{"type": "session_meta", "payload": map[string]any{"id": b.NativeSessionID, "history_mode": tc.mode}}
		proof := (NativeLabelProvider{}).LabelContext(b)
		if proof.APICompatible != tc.compatible {
			t.Fatalf("%v: %+v", tc.mode, proof)
		}
		b.Capture.Harness.Version = "unknown"
		if (NativeLabelProvider{}).LabelContext(b).APICompatible {
			t.Fatal("installed version substituted for retained producer")
		}
	}
}

func FuzzNativeLabelResponse(f *testing.F) {
	id := "01900000-0000-7000-8000-000000000001"
	for _, seed := range []string{
		`{"id":1,"result":{"thread":{"id":"` + id + `","name":"Synthetic name","turns":[]}}}`,
		`{"id":1,"result":{"thread":{"id":"` + id + `","name":null,"turns":[]}}}`,
		`{"id":1,"error":{"message":"Synthetic private diagnostic"}}`,
		`{"id":1,"method":"approval/request"}`,
		`{"method":"progress"}`, `{`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		h := &syntheticLabelHost{threadID: id, name: "null", responses: [][]byte{line}}
		budget := 1 << 20
		r := labelRPC{host: h, remaining: &budget}
		label, ok := r.read(context.Background(), id)
		if !ok {
			return
		}
		if label.NativeID != id || label.Source != archive.SessionLabelAPI || label.Contract != LabelAPIContract || label.Fingerprint() == "" || len(label.Name) > 512 {
			t.Fatalf("response escaped bounded owning projection: %+v", label)
		}
	})
}

func TestNativeLegacyCompatibilityRefusesOmittedShapeWithCoverageGaps(t *testing.T) {
	_, request := labelFixture(t)
	b := request.Bundle
	b.Capture.Gaps = []archive.CaptureGap{{Code: "unknown_field_omitted"}}
	if (NativeLabelProvider{}).LabelContext(b).APICompatible {
		t.Fatal("omitted unsupported native shape became implicit legacy authority")
	}
}

func TestNativeLabelContractSourcePairs(t *testing.T) {
	label := archive.SessionLabel{NativeID: "01900000-0000-7000-8000-000000000001", Source: archive.SessionLabelAPI, Contract: LabelAPIContract}
	if !supportedSessionLabel(label) {
		t.Fatal("pinned API rejected")
	}
	label.Contract = LabelContract
	if supportedSessionLabel(label) {
		t.Fatal("API accepted file contract")
	}
	label.Source = archive.SessionLabelDatabase
	label.Contract = LabelAPIContract
	if supportedSessionLabel(label) {
		t.Fatal("file accepted API contract")
	}
}

func TestNativeCompatibilityRejectsAdditionalMetadataWithoutStartingHost(t *testing.T) {
	home, request := labelFixture(t)
	for _, id := range []string{request.Bundle.NativeSessionID, "01900000-0000-7000-8000-000000000002"} {
		b := request.Bundle
		b.NativeRecords = append(append([]map[string]any(nil), b.NativeRecords...), map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "history_mode": "paginated"}})
		request.Context = (NativeLabelProvider{}).LabelContext(b)
		if request.Context.APICompatible {
			t.Fatal("additional metadata authorized native lookup")
		}
		provider := NativeLabelProvider{Host: func(context.Context, string) (agentapi.LabelTransport, error) {
			t.Fatal("ambiguous metadata started a host")
			return nil, agentapi.ErrLabelHostUnavailable
		}}
		_ = provider.LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{home}}, []agentapi.LabelRequest{request})
	}
}
