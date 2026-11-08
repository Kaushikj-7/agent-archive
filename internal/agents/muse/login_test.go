package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func loginRecord() map[string]any {
	return map[string]any{"duration_ms": json.Number("1200"), "method": "device_code", "schema_version": json.Number("1"), "session_id": syntheticOwner, "subscribed": true, "trigger": "onboarding"}
}

func withLogin(t *testing.T, record map[string]any) ([]byte, int64) {
	t.Helper()
	return withRecordBeforeEnd(t, string(payloadSessionLoginCompleted), map[string]any{"kind": "login_completed", "record": record})
}

func TestLoginCompletionKeepsOnlyItsSession(t *testing.T) {
	t.Parallel()
	raw, _ := withLogin(t, loginRecord())
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	retained := append(bytes.Join(out.Records, []byte("\n")), '\n')
	for _, private := range []string{"device_code", "onboarding", "subscribed", "duration_ms"} {
		if bytes.Contains(retained, []byte(private)) {
			t.Fatalf("login retained %q", private)
		}
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-login-completed.golden.jsonl"), retained)
	b := bundleFor(t, out)
	if _, err := (Parser{}).Parse(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil || !slices.EqualFunc(again.Records, out.Records, bytes.Equal) {
		t.Fatalf("refilter changed the login: %v", err)
	}
}

func TestLoginCompletionThatDoesNotQualifyRefusesTheSession(t *testing.T) {
	t.Parallel()
	edit := func(change func(map[string]any)) map[string]any {
		r := loginRecord()
		change(r)
		return r
	}
	for name, record := range map[string]map[string]any{
		"another session":  edit(func(r map[string]any) { r["session_id"] = "00000000-0000-7000-8000-0000000000ff" }),
		"not a session ID": edit(func(r map[string]any) { r["session_id"] = "owner" }),
		"unknown member":   edit(func(r map[string]any) { r["account_email"] = "PRIVATE_LOGIN_SENTINEL" }),
		"missing member":   edit(func(r map[string]any) { delete(r, "trigger") }),
		"other schema":     edit(func(r map[string]any) { r["schema_version"] = json.Number("2") }),
		"negative time":    edit(func(r map[string]any) { r["duration_ms"] = json.Number("-1") }),
		"empty method":     edit(func(r map[string]any) { r["method"] = "" }),
		"subscribed text":  edit(func(r map[string]any) { r["subscribed"] = "yes" }),
	} {
		raw, line := withLogin(t, maps.Clone(record))
		out, err := filterBytes(t, raw, agentapi.ReadLimits{})
		if err == nil || len(out.Records) != 0 {
			t.Errorf("%s: unqualified login was not refused", name)
			continue
		}
		// A well-formed login naming another session fails root identity
		// validation, which refuses the whole file without a line.
		var refused *agentapi.NativeRefusalError
		if name != "another session" && (!errors.As(err, &refused) || refused.Line != line) {
			t.Errorf("%s: unqualified login was not refused at its line: %v", name, err)
		}
	}
}
