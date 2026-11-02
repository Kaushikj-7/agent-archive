package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
	"github.com/wangjohn/agent-archive/internal/testutil/musefixture"
)

const syntheticOwner = "00000000-0000-7000-8000-000000000001"

// withFilesystem rewrites the synthetic exec profile's filesystem mode and minimal
// runtime set, then recomputes the permission frame checksum.
func withFilesystem(t *testing.T, mode, set string) []byte {
	t.Helper()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	old := []byte(`\"mode\":\"invented-mode\",\"minimal_runtime_set_version\":1,`)
	if bytes.Count(raw, old) != 1 {
		t.Fatal("synthetic filesystem snapshot changed")
	}
	raw = bytes.Replace(raw, old, []byte(`\"mode\":\"`+mode+`\",\"minimal_runtime_set_version\":`+set+`,`), 1)
	return musefixture.Rewrite(t, raw, syntheticOwner, "/work/synthetic-muse")
}

func TestUnrestrictedFilesystemProfileIsRetainedAsMode(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, withFilesystem(t, "unrestricted", "null"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Records[1], []byte(`"filesystem_mode":"unrestricted"`)) {
		t.Fatalf("profile row does not record the unrestricted mode: %s", out.Records[1])
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-exec-unrestricted.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	bundle := bundleFor(t, out)
	if _, err := (Parser{}).Parse(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	again, err := (Filter{}).Refilter(t.Context(), bundle, time.Time{})
	if err != nil || !bytes.Equal(bytes.Join(again.Records, nil), bytes.Join(out.Records, nil)) {
		t.Fatalf("refilter changed retained evidence: %v", err)
	}

	// Version 1 keeps the earlier retained row, without a mode.
	out, err = filterBytes(t, withFilesystem(t, "managed", "1"), agentapi.ReadLimits{})
	if err != nil || bytes.Contains(out.Records[1], []byte("filesystem_mode")) {
		t.Fatalf("restricted profile changed: %v", err)
	}
}

func TestMissingMinimalRuntimeSetRequiresUnrestrictedMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"managed", "invented-mode"} {
		if _, err := filterBytes(t, withFilesystem(t, mode, "null"), agentapi.ReadLimits{}); !errors.Is(err, errFormat) {
			t.Fatalf("%s without a minimal runtime set: %v", mode, err)
		}
	}
	if _, err := filterBytes(t, withFilesystem(t, "unrestricted", "2"), agentapi.ReadLimits{}); !errors.Is(err, errFormat) {
		t.Fatalf("unknown minimal runtime set version: %v", err)
	}
}

func TestRetainedFilesystemModeIsClosed(t *testing.T) {
	t.Parallel()
	r := retained{Type: payloadRuntimeSessionPermissionProfileCommitted, PayloadSchema: 1, Fields: map[string]any{"schema_version": json.Number("1"), "permission_epoch": json.Number("1"), "cause": "new_session_default", "filesystem_mode": "managed"}}
	if _, _, ok := fieldsRuntimeSessionPermissionProfileCommitted(r); ok {
		t.Fatal("retained reader accepted an unknown filesystem mode")
	}
	r.Fields["filesystem_mode"] = "unrestricted"
	if _, _, ok := fieldsRuntimeSessionPermissionProfileCommitted(r); !ok {
		t.Fatal("retained reader refused the unrestricted mode")
	}
}

func TestRouteFactsDropUnlistedLocalMembers(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-route-startup.jsonl")
	old := []byte(`"record":{"cwd":"/work/synthetic-muse"}`)
	if bytes.Count(raw, old) != 1 {
		t.Fatal("synthetic route facts changed")
	}
	raw = bytes.Replace(raw, old, []byte(`"record":{"cwd":"/work/synthetic-muse","pid":4242,"terminal_kind":"SyntheticTerminal","terminal_bundle_id":"invented.terminal.bundle","terminal_title_identity":"invented-title","local_runtime_command_socket_path":"/private/invented/runtime.sock","invented_future_member":{"nested":true}}`), 1)
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	retained := bytes.Join(out.Records, nil)
	for _, private := range []string{"4242", "SyntheticTerminal", "invented.terminal.bundle", "invented-title", "runtime.sock", "invented_future_member"} {
		if bytes.Contains(retained, []byte(private)) {
			t.Fatalf("retained unlisted route fact %q", private)
		}
	}
	if _, err := (Parser{}).Parse(t.Context(), bundleFor(t, out)); err != nil {
		t.Fatal(err)
	}
	if facts, err := routeStartupFacts(t, raw); err != nil || facts.Cwd != "/work/synthetic-muse" {
		t.Fatalf("startup cwd from route facts: %+v %v", facts, err)
	}
}

func TestRouteFactsStillRequireCwd(t *testing.T) {
	t.Parallel()
	raw := bytes.Replace(readSynthetic(t, "synthetic-route-startup.jsonl"), []byte(`"record":{"cwd":"/work/synthetic-muse"}`), []byte(`"record":{"terminal_kind":"SyntheticTerminal"}`), 1)
	if _, err := filterBytes(t, raw, agentapi.ReadLimits{}); !errors.Is(err, errFormat) {
		t.Fatalf("route facts without cwd: %v", err)
	}
}

func refusalOf(t *testing.T, err error) *agentapi.NativeRefusalError {
	t.Helper()
	var refused *agentapi.NativeRefusalError
	if !errors.As(err, &refused) || !errors.Is(err, errFormat) {
		t.Fatalf("not a native refusal: %v", err)
	}
	return refused
}

func TestSideLogWithoutPermissionFrameIsNotASession(t *testing.T) {
	t.Parallel()
	lines := bytes.Split(readSynthetic(t, "synthetic-exec-conversation.jsonl"), []byte("\n"))
	// A side log starts directly with ordinary records in the session layout.
	side := append(bytes.Join(lines[1:3], []byte("\n")), '\n')
	_, err := filterBytes(t, side, agentapi.ReadLimits{})
	refused := refusalOf(t, err)
	if refused.Reason != agentapi.RefusalNoSessionStart || refused.Family == "" || refused.Line != 1 {
		t.Fatalf("side log refusal: %+v", refused)
	}
	if _, err := inspectSynthetic(t, side); refusalOf(t, err).Reason != agentapi.RefusalNoSessionStart {
		t.Fatalf("header refusal: %v", err)
	}
	if _, err := routeStartupFacts(t, side); refusalOf(t, err).Reason != agentapi.RefusalNoSessionStart {
		t.Fatalf("startup refusal: %v", err)
	}
}

func TestUnsupportedRecordNamesItsFamilyAndLine(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	future := bytes.Replace(lines[2], []byte(`"payload_type":"`), []byte(`"payload_type":"session.invented_future.observed","ignored_type":"`), 1)
	raw = append(bytes.Join(append(lines[:2:2], future), []byte("\n")), '\n')
	_, err := filterBytes(t, raw, agentapi.ReadLimits{})
	refused := refusalOf(t, err)
	if refused.Reason != agentapi.RefusalUnsupportedRecord || refused.Line != 3 || refused.Family == "" || !bytes.HasPrefix([]byte(refused.Family), []byte("session.invented_future.observed")) {
		t.Fatalf("unsupported record refusal: %+v", refused)
	}
}

func TestPermissionFrameAloneIsAnIncompleteStart(t *testing.T) {
	t.Parallel()
	first, _, _ := bytes.Cut(readSynthetic(t, "synthetic-exec-conversation.jsonl"), []byte("\n"))
	_, err := filterBytes(t, append(first, '\n'), agentapi.ReadLimits{})
	if refused := refusalOf(t, err); refused.Reason != agentapi.RefusalIncompleteStart || refused.Line != 1 {
		t.Fatalf("frame-only refusal: %+v", refused)
	}
	// Startup facts keep the end-of-file identity callers already handle.
	_, err = routeStartupFacts(t, append(first, '\n'))
	var refused *agentapi.NativeRefusalError
	if !errors.As(err, &refused) || refused.Reason != agentapi.RefusalIncompleteStart || !errors.Is(err, io.EOF) {
		t.Fatalf("frame-only startup refusal: %v", err)
	}
}

// startupRefusals reads raw through the strict filter, the local startup facts
// and the handoff header, which must all name the same refusal.
func startupRefusals(t *testing.T, raw []byte) map[string]*agentapi.NativeRefusalError {
	t.Helper()
	_, filterErr := filterBytes(t, raw, agentapi.ReadLimits{})
	_, startupErr := routeStartupFacts(t, raw)
	_, headerErr := inspectSynthetic(t, raw)
	return map[string]*agentapi.NativeRefusalError{"filter": refusalOf(t, filterErr), "startup": refusalOf(t, startupErr), "header": refusalOf(t, headerErr)}
}

func TestUnqualifiedStartupOrderIsNamed(t *testing.T) {
	t.Parallel()
	// Each record qualifies, but a resumed opening cannot complete the
	// route-facts startup lane.
	raw := bytes.Replace(readSynthetic(t, "synthetic-route-startup.jsonl"), []byte(`"resume":false`), []byte(`"resume":true`), 1)
	for name, refused := range startupRefusals(t, raw) {
		if refused.Reason != agentapi.RefusalUnsupportedStartup || refused.Family != string(payloadSessionOpenedObserved) || refused.Line != 4 {
			t.Fatalf("%s refusal: %+v", name, refused)
		}
	}
}

func TestContradictingStartupRecordIsNamed(t *testing.T) {
	t.Parallel()
	// Route facts naming another directory contradict the session's workspace;
	// the refusal names that record, not the startup order.
	raw := bytes.Replace(readSynthetic(t, "synthetic-route-startup.jsonl"), []byte(`"record":{"cwd":"/work/synthetic-muse"}`), []byte(`"record":{"cwd":"/work/elsewhere"}`), 1)
	for name, refused := range startupRefusals(t, raw) {
		if refused.Reason != agentapi.RefusalInconsistentRecord || refused.Family != string(payloadRuntimeSessionRouteFacts) || refused.Line != 3 {
			t.Fatalf("%s refusal: %+v", name, refused)
		}
	}
}

func TestStartupCutShortOfItsLaneIsIncomplete(t *testing.T) {
	t.Parallel()
	for name, cut := range map[string]struct {
		fixture string
		lines   int
	}{
		// The route-facts lane needs its opening on line 4.
		"route facts": {"synthetic-route-startup.jsonl", 3},
		// The capture-selection lane needs seven lines.
		"capture selection": {"synthetic-msp-conversation.jsonl", 5},
	} {
		lines := bytes.SplitAfter(readSynthetic(t, cut.fixture), []byte("\n"))
		raw := bytes.Join(lines[:cut.lines], nil)
		for path, refused := range startupRefusals(t, raw) {
			if refused.Reason != agentapi.RefusalIncompleteStart || refused.Line != int64(cut.lines) {
				t.Fatalf("%s %s refusal: %+v", name, path, refused)
			}
		}
	}
}

func TestShortStartupWithAContradictingRecordIsNotIncomplete(t *testing.T) {
	t.Parallel()
	lines := bytes.SplitAfter(readSynthetic(t, "synthetic-route-startup.jsonl"), []byte("\n"))
	raw := bytes.Replace(bytes.Join(lines[:2], nil), []byte(`"workspace_root":"/work/synthetic-muse"`), []byte(`"workspace_root":"work/relative"`), 1)
	for path, refused := range startupRefusals(t, raw) {
		if refused.Reason != agentapi.RefusalInconsistentRecord || refused.Family != string(payloadRuntimeSessionMetadata) || refused.Line != 2 {
			t.Fatalf("%s refusal: %+v", path, refused)
		}
	}
}

func TestRefusedRecordFamilyKeepsEveryDistinctKind(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		// A kind that is a substring of the payload type is still distinct.
		`{"payload_type":"runtime.session.route_facts","payload":{"kind":"route"}}`:          "runtime.session.route_facts/route",
		`{"payload_type":"runtime.session","payload":{"kind":"run","event":{"kind":"run"}}}`: "runtime.session/run",
		// A kind that is not a reportable name keeps the payload type.
		`{"payload_type":"runtime.session","payload":{"kind":"Run-2","record":{"kind":"task"}}}`: "runtime.session/task",
	} {
		if refused := refusalOf(t, refusal([]byte(raw), 3, errFormat)); refused.Family != want {
			t.Fatalf("%s family %q, want %q", raw, refused.Family, want)
		}
	}
}
