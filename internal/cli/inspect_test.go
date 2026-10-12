package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// publishedFixture registers one Codex session through the hook path and
// publishes it with `sync` into a single in-memory store that the returned
// Env keeps handing back, so `list`/`show` read what `sync` wrote. It
// returns the archive session ID `list`/`show` address the session by.
func publishedFixture(t *testing.T) (Env, *storagetest.MemoryStore, string) {
	t.Helper()
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleTestHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	mem := storagetest.NewMemoryStore()
	env := testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("sync code=%d stderr=%s", code, stderr.String())
	}
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("regs=%#v err=%v", regs, err)
	}
	return env, mem, regs[0].ArchiveSessionID
}

// Like sync and pause, read-only commands report a missing setup on stderr
// with exit 1, so a script never takes the message for a result.
func TestListAndShowReportNotSetUp(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	for _, args := range [][]string{{"list"}, {"show", "session-1"}, {"backfill", "history"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 1 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut.String())
		}
		if out.Len() != 0 || !strings.Contains(errOut.String(), "Not set up") {
			t.Fatalf("%v: out=%q stderr=%q", args, out.String(), errOut.String())
		}
	}
}

func TestListShowsPublishedSessionMetadataOnly(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	for _, want := range []string{short, "codex", "just now", "1 session(s).", "WHEN", "PROJECT", "TITLE", "ID"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list output missing %q:\n%s", want, out.String())
		}
	}
	// "visible" is the assistant message text in writeCodexTranscript; a
	// listing must never surface transcript content.
	if strings.Contains(out.String(), "visible") {
		t.Fatalf("list printed transcript content:\n%s", out.String())
	}
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	cases := []struct {
		args  []string
		match bool
	}{
		{[]string{"list", "--harness", "codex"}, true},
		{[]string{"list", "--harness", "cursor"}, false},
		{[]string{"list", "--model", "gpt-test"}, true},
		{[]string{"list", "--model", "other-model"}, false},
		{[]string{"list", "--skill", "review"}, false},
		{[]string{"list", "--since", "2026-01-02"}, true},
		{[]string{"list", "--since", "2026-01-03"}, false},
		{[]string{"list", "--since", "1d"}, true},
		{[]string{"list", "--since", "2026-01-02T00:00:01Z"}, false},
		// The fixture's parser status is "partial" (BuildMetadata's default),
		// so requiring complete coverage excludes it.
		{[]string{"list", "--complete"}, false},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if code := Run(c.args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", c.args, code, errOut.String())
		}
		if got := strings.Contains(out.String(), id[:min(8, len(id))]); got != c.match {
			t.Fatalf("%v: matched=%v want=%v\n%s", c.args, got, c.match, out.String())
		}
		if !c.match && !strings.Contains(out.String(), "No archived sessions match.") {
			t.Fatalf("%v: expected the no-match line:\n%s", c.args, out.String())
		}
	}
}

func TestListFiltersExactSkillHashFromMetadataOnly(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var first archive.Metadata
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	hashA, hashB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	first.SkillsUsed = []archive.SkillUse{{Name: "review", SHA256: hashA, Evidence: archive.SkillUseEvidenceNativeInvocation}}
	first.SkillDetection = archive.SkillDetectionObserved
	encoded, _ := json.Marshal(first)
	if err := mem.Put(context.Background(), key, encoded); err != nil {
		t.Fatal(err)
	}
	second := first
	second.SessionID = id + "-v2"
	second.SkillsUsed = []archive.SkillUse{{Name: "review", SHA256: hashB, Evidence: archive.SkillUseEvidenceNativeInvocation}}
	secondKey, err := archive.MetadataObjectKey("codex", second.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(second)
	if err := mem.Put(context.Background(), secondKey, encoded); err != nil {
		t.Fatal(err)
	}
	// Catalog filtering must not load the source object.
	if err := mem.Delete(context.Background(), first.SourceBundle.Key); err != nil {
		t.Fatal(err)
	}

	for hash, ids := range map[string][2]string{
		hashA: {id, second.SessionID},
		hashB: {second.SessionID, id},
	} {
		wantID, rejectID := ids[0], ids[1]
		var out, errOut bytes.Buffer
		if code := Run([]string{"list", "--skill", "review", "--skill-sha256", hash, "--limit", "0", "--verbose"}, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("hash=%s code=%d stderr=%s", hash, code, errOut.String())
		}
		// One ID is a prefix of the other, and tabwriter pads columns with
		// spaces rather than tabs, so a substring search for either ID is
		// satisfied by the other session's row. Compare the listed rows.
		listed := listedSessionIDs(out.String())
		if len(listed) != 1 || listed[0] != wantID {
			t.Fatalf("hash=%s listed=%v want=[%s] (reject %s)\n%s", hash, listed, wantID, rejectID, out.String())
		}
	}
}

// listedSessionIDs returns each row's session identity from `list` text:
// the SESSION column when --verbose, otherwise the trailing ID column.
func listedSessionIDs(out string) []string {
	var ids []string
	verbose := false
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "Showing" || strings.Contains(line, "session(s).") {
			continue
		}
		if fields[0] == "TITLE" || (fields[0] == "#" && len(fields) > 1 && fields[1] == "TITLE") {
			for _, f := range fields {
				if f == "SESSION" {
					verbose = true
				}
			}
			continue
		}
		if fields[0] == "#" {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		if verbose {
			// TITLE SESSION HARNESS ...
			if len(fields) < 2 {
				continue
			}
			ids = append(ids, fields[1])
			continue
		}
		ids = append(ids, fields[len(fields)-1])
	}
	return ids
}

func TestListRejectsBadArguments(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	for _, args := range [][]string{
		{"list", "--since", "yesterday"},
		{"list", "--skill-usage", "sometimes"},
		{"list", "--skill-sha256", "abc"},
		{"list", "--skill-sha256", strings.Repeat("A", 64)},
		{"list", "--limit", "-1"},
		{"list", "extra", "more"},
		{"list", "--bogus"},
		{"show"},
		{"show", "id", "extra"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 2 {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, out.String(), errOut.String())
		}
	}
}

// An invalid --skill-usage value must be reported as the invalid value it is,
// even though it also fails the "requires --skill or --skill-sha256" rule.
func TestListReportsInvalidSkillUsageValueBeforeMissingSkillFlag(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--skill-usage", "bogus"}, nil, &out, &errOut, env); code != 2 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), `must be used or available, not "bogus"`) {
		t.Fatalf("wrong error for an invalid value: %s", errOut.String())
	}
	if strings.Contains(errOut.String(), "requires --skill") {
		t.Fatalf("reported the missing companion flag instead: %s", errOut.String())
	}
	// A valid value still requires --skill or --skill-sha256.
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"list", "--skill-usage", "available"}, nil, &out, &errOut, env); code != 2 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "requires --skill or --skill-sha256") {
		t.Fatalf("missing companion-flag error: %s", errOut.String())
	}
}

// No parser version proves non-use. Reject the value even before setup or
// checking the companion skill flag, so callers cannot mistake it for an
// actual zero-match query.
func TestEligibleNoUseReturnsUsageError(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	for _, args := range [][]string{
		{"list", "--skill-usage", "eligible_no_use"},
		{"list", "--skill", "review", "--skill-usage", "eligible_no_use"},
		{"list", "--json", "--skill", "review", "--skill-usage", "eligible_no_use"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 2 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut.String())
		}
		if out.Len() != 0 || !strings.Contains(errOut.String(), "eligible_no_use is unsupported") || !strings.Contains(errOut.String(), "cannot prove non-use") {
			t.Fatalf("%v: stdout=%q stderr=%q", args, out.String(), errOut.String())
		}
	}
	var errOut bytes.Buffer
	var help bytes.Buffer
	if code := Run([]string{"list", "--help"}, nil, &help, &errOut, env); code != 0 {
		t.Fatalf("help code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(help.String(), "Non-use queries are unsupported") || !strings.Contains(help.String(), "used|available") {
		t.Fatalf("help does not describe supported values:\n%s", help.String())
	}
}

func TestShowPrintsMetadataAndOnlyPrintsContentWithNormalizedFlag(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id, "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(out.Bytes(), &metadata); err != nil {
		t.Fatalf("show output is not one metadata document: %v\n%s", err, out.String())
	}
	if metadata.SessionID != id || metadata.SourceBundle.Key == "" {
		t.Fatalf("metadata=%#v", metadata)
	}
	if strings.Contains(out.String(), "visible") {
		t.Fatalf("show without --normalized printed transcript content:\n%s", out.String())
	}

	for _, args := range [][]string{{"show", id, "--normalized"}, {"show", "--normalized", "--harness", "codex", id}} {
		out.Reset()
		errOut.Reset()
		if code := Run(args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut.String())
		}
		decoder := json.NewDecoder(bytes.NewReader(out.Bytes()))
		var first archive.Metadata
		var second normalizedOutput
		if err := decoder.Decode(&first); err != nil {
			t.Fatalf("%v: first document: %v\n%s", args, err, out.String())
		}
		if err := decoder.Decode(&second); err != nil {
			t.Fatalf("%v: second document: %v\n%s", args, err, out.String())
		}
		if first.SessionID != id {
			t.Fatalf("%v: metadata=%#v", args, first)
		}
		if len(second.Turns) != 1 || second.Turns[0].Role != "assistant" || second.Turns[0].Text != "visible" {
			t.Fatalf("%v: normalized view=%#v", args, second)
		}
	}
}

func TestShowReportsUnknownAndUnavailableSessions(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"show", "no-such-session"}, nil, &out, &errOut, env); code != 1 {
		t.Fatalf("code=%d stdout=%s", code, out.String())
	}
	if !strings.Contains(errOut.String(), `no archived session "no-such-session"`) {
		t.Fatalf("stderr=%s", errOut.String())
	}

	// A wrong --harness derives a key that does not exist.
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", "--harness", "cursor", id}, nil, &out, &errOut, env); code != 1 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}

	// Metadata stays readable after its source is gone, but --normalized
	// must fail closed with an actionable message and print no content.
	metadata, err := readSingleMetadata(t, mem)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Delete(context.Background(), metadata.SourceBundle.Key); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", id}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("metadata-only show should still work: code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", id, "--normalized"}, nil, &out, &errOut, env); code != 1 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "source bundle is not available") {
		t.Fatalf("stderr=%s", errOut.String())
	}
}

func TestShowRefusesAmbiguousSessionWithoutHarness(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	metadata, err := readSingleMetadata(t, mem)
	if err != nil {
		t.Fatal(err)
	}
	// Publish a copy of the same session ID under a second harness.
	metadata.Harness.Name = "cursor"
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey("cursor", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id}, nil, &out, &errOut, env); code != 1 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "codex, cursor") || !strings.Contains(errOut.String(), "--harness") {
		t.Fatalf("stderr=%s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", "--harness", "cursor", id, "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"name": "cursor"`) {
		t.Fatalf("expected the cursor copy: %s", out.String())
	}
}

func TestParseSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"2026-03-01":           time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		"2026-03-01T06:30:00Z": time.Date(2026, 3, 1, 6, 30, 0, 0, time.UTC),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"12h":                  now.Add(-12 * time.Hour),
	}
	for input, want := range cases {
		got, err := parseSince(input, now)
		if err != nil || !got.Equal(want) {
			t.Fatalf("%q: got %v err=%v want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "yesterday", "-7d", "-1h", "3 days"} {
		if _, err := parseSince(input, now); err == nil {
			t.Fatalf("%q: expected an error", input)
		}
	}
}

// readSingleMetadata returns the one metadata sidecar publishedFixture wrote.
func readSingleMetadata(t *testing.T, mem *storagetest.MemoryStore) (archive.Metadata, error) {
	t.Helper()
	objects, err := mem.List(context.Background(), "sessions")
	if err != nil {
		return archive.Metadata{}, err
	}
	for _, object := range objects {
		if strings.HasSuffix(object.Key, "/metadata.json") {
			data, err := mem.Get(context.Background(), object.Key)
			if err != nil {
				return archive.Metadata{}, err
			}
			var metadata archive.Metadata
			err = json.Unmarshal(data, &metadata)
			return metadata, err
		}
	}
	t.Fatal("no metadata sidecar in store")
	return archive.Metadata{}, nil
}

// Metadata comes from the bucket, so a model or skill name can hold anything:
// `list` prints it without escape sequences, controls, or a tab or newline
// that would break the table.
func TestListPrintsBucketNamesWithoutControls(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Models) == 0 {
		t.Fatal("fixture has no model")
	}
	m.Title = "fix\x1b[2J this\tx\ny"
	m.Models[0].Attributes = map[string]string{"gen_ai.request.model": "gpt\x1b]52;c;aGk=\x07\tx\ny"}
	m.SkillsUsed = []archive.SkillUse{{Name: "rev\x1b[2Jiew\u009b31m\r", SHA256: strings.Repeat("a", 64), Evidence: archive.SkillUseEvidenceNativeInvocation}}
	m.SkillDetection = archive.SkillDetectionObserved
	encoded, _ := json.Marshal(m)
	if err := mem.Put(context.Background(), key, encoded); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--limit", "0"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	for _, r := range out.String() {
		if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("control %U in list output:\n%q", r, out.String())
		}
	}
	if got := len(strings.Split(strings.TrimSpace(out.String()), "\n")); got != 3 {
		t.Fatalf("list printed %d lines, want header, one row, count:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "fix[2J this x y") {
		t.Fatalf("title not shown as text:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"list", "--limit", "0", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("verbose code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "gpt]52;c;aGk= x y") || !strings.Contains(out.String(), "rev[2Jiew31m") {
		t.Fatalf("verbose names not shown as text:\n%s", out.String())
	}
}

// `show` and `list --json` print bucket metadata as JSON. encoding/json
// escapes C0 controls but writes DEL, the C1 controls (U+009B is a
// one-byte CSI to many terminals), and bidi overrides as they are; the
// printed JSON escapes them too, and still decodes to the same names.
func TestJSONOutputEscapesEveryControl(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	name := "rev\u009b2Jiew\x7f\u202eevil\u0085"
	m.SkillsUsed = []archive.SkillUse{{Name: name, SHA256: strings.Repeat("a", 64), Evidence: archive.SkillUseEvidenceNativeInvocation}}
	m.SkillDetection = archive.SkillDetectionObserved
	encoded, _ := json.Marshal(m)
	if err := mem.Put(context.Background(), key, encoded); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"show", id, "--json"}, {"list", "--json"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut.String())
		}
		for _, r := range out.String() {
			if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) {
				t.Fatalf("%v: control %U in output:\n%q", args, r, out.String())
			}
		}
		if !strings.Contains(out.String(), `rev\u009b2Jiew\u007f\u202eevil\u0085`) {
			t.Fatalf("%v: name not escaped:\n%q", args, out.String())
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("%v: output is not JSON:\n%s", args, out.String())
		}
	}
}

// list and backfill read --since the same way; backfill then selects whole
// local days.
func TestSinceFormsAreShared(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("PDT", -7*3600)
	now := time.Date(2026, 9, 24, 1, 30, 0, 0, loc) // 08:30 UTC
	for value, want := range map[string]string{
		"":                     "",
		"2026-09-01":           "2026-09-01",
		"7d":                   "2026-09-17",
		"2h":                   "2026-09-23", // 23:30 the previous local day
		"2026-09-20T03:00:00Z": "2026-09-19", // 20:00 on the 19th, local
	} {
		got, err := backfillDay(value, now)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"yesterday", "-7d", "100001d", "9999999999999d"} {
		if _, err := backfillDay(value, now); err == nil {
			t.Errorf("%q accepted", value)
		}
		if _, err := parseSince(value, now); err == nil {
			t.Errorf("list accepted %q", value)
		}
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestShowKeepsParentReadableWhenChildIsGone(t *testing.T) {
	t.Parallel()
	env, remote, id := publishedFixture(t)
	parent, err := readSingleMetadata(t, remote)
	if err != nil {
		t.Fatal(err)
	}
	parent.LinkedSessions = []archive.LinkedSessionReference{{SessionID: "missing-child", Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: time.Now().UTC()}}
	key, _ := archive.MetadataObjectKey(parent.Harness.Name, id)
	data, _ := json.Marshal(parent)
	if err := remote.Put(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
	if err := remote.Delete(context.Background(), parent.SourceBundle.Key); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id, "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut.String())
	}
	// `"hook_finals"` appears only in the normalized view, which this
	// metadata-only invocation must not print. (It replaces a `"turns":`
	// check, which parser 0.6 also emits inside `counts`.)
	if !strings.Contains(out.String(), `"unavailable_or_expired"`) || strings.Contains(out.String(), `"hook_finals"`) {
		t.Fatalf("bad metadata-only output: %s", out.String())
	}
	var decoded archive.Metadata
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded.SessionID != id {
		t.Fatalf("metadata compatibility lost: %v", err)
	}
}

// A sidecar that does not validate (written by a newer version, or damaged)
// is left out of `list` with a warning on stderr; the rest is listed and
// stdout keeps its format.
func TestListWarnsAboutInvalidSidecarAndListsTheRest(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	bad := "sessions/codex/newer-session/metadata.json"
	if err := mem.Put(context.Background(), bad, []byte(`{"schema_version":99}`)); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--no-cache", "--limit", "0", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), id) || !strings.Contains(out.String(), "1 session(s).") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "warning: skipped a session whose metadata could not be read") || !strings.Contains(errOut.String(), bad) {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}
