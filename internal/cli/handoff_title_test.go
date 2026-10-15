package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// takeArchiveOffline makes every archive open fail, counting the attempts.
func takeArchiveOffline(env *Env) *int {
	opens := new(int)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		*opens++
		return nil, errors.New("offline")
	}
	return opens
}

// A title finds a session on this machine, which needs no network and no upload.
func TestHandoffTitleFindsALocalSessionWithoutTheArchive(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	opens := takeArchiveOffline(&f.env)
	for _, query := range []string{"not uploaded", "NOT UPLOADED YET", f.notUploaded[:minShortSessionID], f.notUploaded} {
		out, errOut, code := runHandoff(t, f.env, query)
		if code != 0 || !strings.Contains(out, "source: local") || !strings.Contains(out, "Not uploaded yet") {
			t.Fatalf("%q: code=%d stderr=%s\n%s", query, code, errOut, out)
		}
	}
	if *opens != 0 {
		t.Fatalf("opened the archive %d times for a session on this machine", *opens)
	}
}

// A title the local sessions lack is searched in the archive, which holds
// sessions from other machines.
func TestHandoffTitleFallsBackToTheArchive(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	out, errOut, code := runHandoff(t, f.env, "archived elsewhere")
	if code != 0 || !strings.Contains(out, "source: archive") || !strings.Contains(out, "Archived elsewhere") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	// The ID of that session, short or full, resolves the same way.
	for _, query := range []string{f.archiveOnly[:minShortSessionID], f.archiveOnly} {
		if out, errOut, code := runHandoff(t, f.env, query); code != 0 || !strings.Contains(out, "Archived elsewhere") {
			t.Fatalf("%q: code=%d stderr=%s", query, code, errOut)
		}
	}
}

// Local sessions are searched first: one match there is the answer, and the
// archive is never asked, even though it holds another match.
func TestHandoffTitleLocalMatchShadowsArchiveMatches(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	// "ed" is in the local "Not uploaded yet" and the archived "Archived
	// elsewhere"; "yet" only in the first.
	f.addSession(t, "codex", "native-both", "Uploaded and finished", f.env.now().Add(3*time.Hour))
	f.sync(t)
	opens := takeArchiveOffline(&f.env)
	if out, errOut, code := runHandoff(t, f.env, "yet"); code != 0 || !strings.Contains(out, "Not uploaded yet") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if *opens != 0 {
		t.Fatalf("opened the archive %d times", *opens)
	}
}

// An ID that is also part of another session's title names its own session.
func TestHandoffSessionIDBeatsATitleMatch(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addSession(t, "codex", "native-mention", "Compare with "+f.notUploaded+" please", f.env.now().Add(3*time.Hour))
	f.addSession(t, "codex", "native-mention-short", "Compare with "+f.notUploaded[:minShortSessionID]+" too", f.env.now().Add(4*time.Hour))
	for _, query := range []string{f.notUploaded, f.notUploaded[:minShortSessionID]} {
		out, errOut, code := runHandoff(t, f.env, query)
		if code != 0 || !strings.Contains(out, "Not uploaded yet") || strings.Contains(out, "Compare with") {
			t.Fatalf("%q: code=%d stderr=%s\n%s", query, code, errOut, out)
		}
	}
}

// A full archive ID that no session on this machine has, but that a local title
// mentions, still names the archived session.
func TestHandoffArchiveSessionIDBeatsALocalTitleMatch(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addSession(t, "codex", "native-mention", "Compare with "+f.archiveOnly+" please", f.env.now().Add(3*time.Hour))
	out, errOut, code := runHandoff(t, f.env, f.archiveOnly)
	if code != 0 || !strings.Contains(out, "source: archive") || !strings.Contains(out, "Archived elsewhere") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	// An unreachable exact identity cannot authorize a different local title match.
	takeArchiveOffline(&f.env)
	out, errOut, code = runHandoff(t, f.env, f.archiveOnly)
	if code != 1 || out != "" || !strings.Contains(errOut, "offline") {
		t.Fatalf("offline: code=%d stderr=%s", code, errOut)
	}
}

func TestHandoffTitleAmbiguousWithoutATerminalListsCandidates(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	opens := takeArchiveOffline(&f.env)
	// "yet" is in the not-uploaded title; add a second local match.
	other := f.addSession(t, "claude", "claude-yet", "Not started yet", f.env.now().Add(3*time.Hour))
	out, errOut, code := runHandoff(t, f.env, "yet")
	if code != 1 || out != "" {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out, errOut)
	}
	for _, want := range []string{`"yet" matches 2 sessions`, f.notUploaded[:minShortSessionID], other[:minShortSessionID], "codex", "claude", "Not uploaded yet", "Not started yet", "just now"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, f.both[:minShortSessionID]) {
		t.Errorf("a non-matching session is listed:\n%s", errOut)
	}
	if *opens != 0 {
		t.Fatalf("opened the archive %d times", *opens)
	}
	// The listed ID then works.
	if out, _, code := runHandoff(t, f.env, other[:minShortSessionID]); code != 0 || !strings.Contains(out, "Not started yet") {
		t.Fatalf("code=%d\n%s", code, out)
	}
}

func TestHandoffTitleAmbiguousInTheArchiveWithoutATerminal(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	second := f.addSession(t, "codex", "native-archived-2", "Archived again", f.env.now().Add(-90*time.Minute))
	f.sync(t)
	f.unregister(t, second)
	out, errOut, code := runHandoff(t, f.env, "archived")
	if code != 1 || out != "" || !strings.Contains(errOut, `"archived" matches 2 sessions`) ||
		!strings.Contains(errOut, f.archiveOnly[:minShortSessionID]) || !strings.Contains(errOut, second[:minShortSessionID]) {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out, errOut)
	}
}

func TestHandoffTitleAmbiguousOnATerminalOpensThePickerOnTheMatches(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	other := f.addSession(t, "claude", "claude-yet", "Not started yet", f.env.now().Add(3*time.Hour))
	out, errOut, code := runPicker(t, f.env, "q\n", "yet")
	if code != 0 || strings.Contains(out, "source:") {
		t.Fatalf("quit: code=%d stderr=%s\n%s", code, errOut, out)
	}
	pickerLine(t, out, f.notUploaded)
	pickerLine(t, out, other)
	if strings.Contains(out, f.both[:minShortSessionID]) || strings.Contains(out, f.archiveOnly[:minShortSessionID]) {
		t.Fatalf("the picker lists sessions the title does not match:\n%s", out)
	}
	if !strings.Contains(out, `Hand off · "yet" matches 2`) {
		t.Fatalf("heading:\n%s", out)
	}
	// Row 1 is the newest match; choosing it hands that session off.
	out, errOut, code = runPicker(t, f.env, "1\n", "yet")
	if code != 0 || !strings.Contains(out, "Not started yet") {
		t.Fatalf("pick: code=%d stderr=%s\n%s", code, errOut, out)
	}
	out, errOut, code = runPicker(t, f.env, "2\n", "yet")
	if code != 0 || !strings.Contains(out, "Not uploaded yet") {
		t.Fatalf("pick: code=%d stderr=%s\n%s", code, errOut, out)
	}
}

// A single match on a terminal is taken without a picker; the one question
// left is where to continue, as for any handoff.
func TestHandoffTitleSingleMatchOnATerminalDoesNotPick(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	out, errOut, code := runPicker(t, f.env, "p\n", "not uploaded")
	if code != 0 || !strings.Contains(out, "? Continue in") || !strings.Contains(out, "Not uploaded yet") || strings.Contains(out, "Enter number") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
}

func TestHandoffTitleWithNoMatchNamesList(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	for _, args := range [][]string{{"no such title"}, {"no such title", "--source", "local"}, {"no such title", "--source", "archive"}, {"../registrations/x"}, {"a/b"}} {
		out, errOut, code := runHandoff(t, f.env, args...)
		if code != 1 || out != "" || !strings.Contains(errOut, "no session matches") || !strings.Contains(errOut, "agent-archive list") {
			t.Errorf("%v: code=%d stdout=%q stderr=%s", args, code, out, errOut)
		}
	}
	_, errOut, _ := runHandoff(t, f.env, "no such title", "--source", "local")
	if !strings.Contains(errOut, "on this machine (") {
		t.Errorf("--source local: %s", errOut)
	}
	_, errOut, _ = runHandoff(t, f.env, "no such title", "--source", "archive")
	if !strings.Contains(errOut, "in the archive (") {
		t.Errorf("--source archive: %s", errOut)
	}
}

// With the archive unreachable, a title only it could satisfy fails saying
// why, rather than looking like a plain miss.
func TestHandoffTitleNoLocalMatchReportsAnUnreachableArchive(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	takeArchiveOffline(&f.env)
	out, errOut, code := runHandoff(t, f.env, "archived elsewhere")
	if code != 1 || out != "" || !strings.Contains(errOut, "no session matches") || !strings.Contains(errOut, "offline") {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out, errOut)
	}
}

// Titles are the first prompt; a word deeper in the conversation is not one.
func TestHandoffTitleDoesNotSearchTranscriptContent(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	// The fixture session's reply mentions "fake clock"; its prompt does not.
	_, errOut, code := runHandoff(t, f.env, "fake clock")
	if code != 1 || !strings.Contains(errOut, "no session matches") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if out, _, code := runHandoff(t, f.env, "flaky widget"); code != 0 || !strings.Contains(out, "Fix the flaky widget test") {
		t.Fatalf("code=%d", code)
	}
}

func TestHandoffTitleHarnessNarrowsTheMatches(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	claude := f.addSession(t, "claude", "claude-widget", "Widget work in Claude", f.env.now().Add(3*time.Hour))
	if _, errOut, code := runHandoff(t, f.env, "widget"); code != 1 || !strings.Contains(errOut, "matches 2 sessions") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	out, errOut, code := runHandoff(t, f.env, "widget", "--harness", "claude")
	if code != 0 || !strings.Contains(out, "Widget work in Claude") {
		t.Fatalf("claude: code=%d stderr=%s\n%s", code, errOut, out)
	}
	out, errOut, code = runHandoff(t, f.env, "widget", "--harness", "codex")
	if code != 0 || !strings.Contains(out, "Fix the flaky widget test") || strings.Contains(out, claude) {
		t.Fatalf("codex: code=%d stderr=%s\n%s", code, errOut, out)
	}
	if _, errOut, code := runHandoff(t, f.env, "Widget work", "--harness", "codex"); code != 1 || !strings.Contains(errOut, "no session matches") || !strings.Contains(errOut, "for codex") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// A subagent is not offered on its own by a title.
func TestHandoffTitleSkipsSubagents(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addSubagent(t, f.notUploaded, "native-sub")
	if out, errOut, code := runHandoff(t, f.env, "not uploaded"); code != 0 || !strings.Contains(out, "Not uploaded yet") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

func TestHandoffTitleWithToLaunchesTheMatch(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	document := recordLaunch(t, &f.env)
	if _, errOut, code := runHandoff(t, f.env, "archived elsewhere", "--to", "claude"); code != 0 || !strings.Contains(*document, "Archived elsewhere") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

func TestHandoffTitleBeforeSetup(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	_, errOut, code := runHandoff(t, f.env, "some title")
	if code != 1 || !strings.Contains(errOut, "no verified local sessions") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

// --source keeps a title search to one side, and --harness to one app, for a
// full ID as for a title.
func TestHandoffTitleSourceAndHarnessLimitTheSearch(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	for _, args := range [][]string{
		{"archived elsewhere", "--source", "local"},
		{"not uploaded", "--source", "archive"},
		{"archived elsewhere", "--harness", "claude"},
		{f.archiveOnly, "--harness", "claude"},
	} {
		out, errOut, code := runHandoff(t, f.env, args...)
		if code != 1 || out != "" || !strings.Contains(errOut, "no session matches") {
			t.Errorf("%v: code=%d stdout=%q stderr=%s", args, code, out, errOut)
		}
	}
}

// --source and --harness bound an ID as they bound a title: a registered ID
// is not taken from the archive side, nor by the wrong app, and a full ID
// is not read from the archive when only this machine is wanted.
func TestHandoffFullIDRespectsSourceAndHarness(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	for _, args := range [][]string{
		{f.notUploaded, "--source", "archive"},
		{f.notUploaded, "--harness", "claude"},
	} {
		out, errOut, code := runHandoff(t, f.env, args...)
		if code != 1 || out != "" || !strings.Contains(errOut, "no session matches") {
			t.Errorf("%v: code=%d stdout=%q stderr=%s", args, code, out, errOut)
		}
	}
	opens := takeArchiveOffline(&f.env)
	_, errOut, code := runHandoff(t, f.env, f.archiveOnly, "--source", "local")
	if code != 1 || !strings.Contains(errOut, "no session matches") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if *opens != 0 {
		t.Fatalf("--source local opened the archive %d times", *opens)
	}
}

// The chosen session's harness travels with its ID, so one ID published under
// two harnesses is not a second question.
func TestHandoffTitleKeepsTheMatchesHarness(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	data, err := f.mem.Get(context.Background(), "sessions/codex/"+f.archiveOnly+"/metadata.json")
	must(t, err)
	var sidecar map[string]any
	must(t, json.Unmarshal(data, &sidecar))
	sidecar["title"] = "A different title"
	data, err = json.Marshal(sidecar)
	must(t, err)
	must(t, f.mem.Put(context.Background(), "sessions/claude/"+f.archiveOnly+"/metadata.json", data))
	out, errOut, code := runHandoff(t, f.env, "archived elsewhere")
	if code != 0 || !strings.Contains(out, "Archived elsewhere") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
}

// An ID read from stored metadata is checked before it names a file or key.
func TestHandoffTitleRefusesAnUnsafeStoredID(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	data, err := f.mem.Get(context.Background(), "sessions/codex/"+f.archiveOnly+"/metadata.json")
	must(t, err)
	var sidecar map[string]any
	must(t, json.Unmarshal(data, &sidecar))
	sidecar["session_id"], sidecar["title"] = "../../escape", "Hostile title"
	data, err = json.Marshal(sidecar)
	must(t, err)
	must(t, f.mem.Put(context.Background(), "sessions/codex/hostile/metadata.json", data))
	out, errOut, code := runHandoff(t, f.env, "hostile title")
	if code != 1 || out != "" || !strings.Contains(errOut, "unusable ID") {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out, errOut)
	}
}

// A stored session ID and title are printed to a terminal or an agent, so
// neither may carry a terminal escape into the candidate list.
func TestHandoffTitleCandidatesCarryNoTerminalEscapes(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	data, err := f.mem.Get(context.Background(), "sessions/codex/"+f.archiveOnly+"/metadata.json")
	must(t, err)
	var sidecar map[string]any
	must(t, json.Unmarshal(data, &sidecar))
	hostile := "\x1b]0;pwned\x07beef-\x1b[2Jid"
	sidecar["session_id"], sidecar["title"] = hostile, "Archived \x1b[31mred\x1b[0m elsewhere\u202e"
	data, err = json.Marshal(sidecar)
	must(t, err)
	must(t, f.mem.Put(context.Background(), "sessions/codex/hostile-id/metadata.json", data))
	_, errOut, code := runHandoff(t, f.env, "elsewhere")
	if code != 1 || !strings.Contains(errOut, "matches 2 sessions") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	for _, r := range errOut {
		if r == '\x1b' || r == '\x07' || r == '\u202e' {
			t.Fatalf("stderr carries a control character %q:\n%q", r, errOut)
		}
	}
}

// The same ID under two harnesses is two candidates, not the last one twice.
func TestHandoffTitleListsAnIDPublishedUnderTwoHarnesses(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	data, err := f.mem.Get(context.Background(), "sessions/codex/"+f.archiveOnly+"/metadata.json")
	must(t, err)
	var sidecar map[string]any
	must(t, json.Unmarshal(data, &sidecar))
	sidecar["harness"] = map[string]any{"name": "claude"}
	data, err = json.Marshal(sidecar)
	must(t, err)
	must(t, f.mem.Put(context.Background(), "sessions/claude/"+f.archiveOnly+"/metadata.json", data))
	_, errOut, code := runHandoff(t, f.env, "archived elsewhere", "--source", "archive")
	if code != 1 || !strings.Contains(errOut, "matches 2 sessions") || !strings.Contains(errOut, "codex") || !strings.Contains(errOut, "claude") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// A title as common as a word in every prompt lists a few candidates and
// counts the rest on stderr, rather than flooding the caller. The picker is
// for a person, who scrolls it and narrows it with the filter, so it lists
// every match.
func TestHandoffTitleLimitsTheCandidatesListed(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	const extra = handoffCandidateLimit + 5
	for i := range extra {
		f.addSession(t, "codex", fmt.Sprintf("native-bulk-%d", i), fmt.Sprintf("Bulk job %d", i), f.env.now().Add(time.Duration(10+i)*time.Hour))
	}
	_, errOut, code := runHandoff(t, f.env, "bulk job")
	lines := strings.Split(strings.TrimSpace(errOut), "\n")
	if code != 1 || len(lines) != 1+handoffCandidateLimit+1+2 || !strings.Contains(errOut, fmt.Sprintf("matches %d sessions", extra)) ||
		!strings.Contains(errOut, "and 5 more") {
		t.Fatalf("code=%d, %d lines:\n%s", code, len(lines), errOut)
	}
	// The newest are the ones listed.
	if !strings.Contains(errOut, fmt.Sprintf("Bulk job %d", extra-1)) || strings.Contains(errOut, "Bulk job 0 ") {
		t.Fatalf("not the newest first:\n%s", errOut)
	}
	out, errOut, code := runPicker(t, f.env, "q\n", "bulk job")
	if code != 0 || !strings.Contains(out, fmt.Sprintf(`"bulk job" matches %d`, extra)) || !strings.Contains(out, "Bulk job 0\n") || strings.Contains(out, "Showing") {
		t.Fatalf("picker: code=%d stderr=%s\n%s", code, errOut, out)
	}
}

// The ID of a session this machine has registered names it even before it has a
// prompt to title it by, as handoff always allowed.
func TestHandoffExactIDNeedsNoTitle(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	opens := takeArchiveOffline(&f.env)
	_, errOut, code := runHandoff(t, f.env, f.noPrompt)
	if strings.Contains(errOut, "no session matches") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if *opens != 0 {
		t.Fatalf("opened the archive %d times", *opens)
	}
}

// A subagent is not offered by a title, but its own ID still names it, as it
// did before titles.
func TestHandoffSubagentIDStillNamesIt(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	sub := f.addSubagent(t, f.notUploaded, "native-sub")
	opens := takeArchiveOffline(&f.env)
	out, errOut, code := runHandoff(t, f.env, sub)
	if code != 0 || out == "" || strings.Contains(errOut, "no session matches") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if *opens != 0 {
		t.Fatalf("opened the archive %d times", *opens)
	}
}

// A subagent's archived record is not offered by a title either.
func TestHandoffTitleSkipsArchivedSubagents(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	key := "sessions/codex/" + f.archiveOnly + "/metadata.json"
	data, err := f.mem.Get(context.Background(), key)
	must(t, err)
	var sidecar map[string]any
	must(t, json.Unmarshal(data, &sidecar))
	sub := "eeeeeeee" + f.archiveOnly[8:]
	sidecar["session_id"], sidecar["parent_session_id"] = sub, f.archiveOnly
	data, err = json.Marshal(sidecar)
	must(t, err)
	must(t, f.mem.Put(context.Background(), "sessions/codex/"+sub+"/metadata.json", data))
	out, errOut, code := runHandoff(t, f.env, "archived elsewhere")
	if code != 0 || !strings.Contains(out, "Archived elsewhere") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// --latest takes no query: a title next to it is a usage error, and it never
// searches titles or asks the archive when told to stay local.
func TestHandoffLatestTakesNoTitle(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	if _, errOut, code := runHandoff(t, f.env, "title", "--latest"); code != 2 || errOut == "" {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	opens := takeArchiveOffline(&f.env)
	out, errOut, code := runHandoff(t, f.env, "--latest", "--source", "local")
	if code != 0 || !strings.Contains(out, "Not uploaded yet") {
		t.Fatalf("--latest: code=%d stderr=%s", code, errOut)
	}
	if *opens != 0 {
		t.Fatalf("--latest --source local opened the archive %d times", *opens)
	}
}

// Words never offer an archived session with no prompt, which has nothing to
// hand off, without a terminal or on one.
func TestHandoffTitleSkipsArchivedSessionsWithNoPrompt(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.archiveNoPrompt(t)
	f.unregister(t, f.noPrompt)
	// "codex" is in every session's harness; --source archive leaves the archive's.
	out, errOut, code := runHandoff(t, f.env, "codex", "--source", "archive")
	if code != 1 || out != "" || !strings.Contains(errOut, `"codex" matches 3 sessions`) || strings.Contains(errOut, f.noPrompt[:minShortSessionID]) {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out, errOut)
	}
	out, errOut, code = runPicker(t, f.env, "q\n", "codex", "--source", "archive")
	if code != 0 || strings.Contains(out, f.noPrompt[:minShortSessionID]) || !strings.Contains(out, `"codex" matches 3`) {
		t.Fatalf("picker: code=%d stderr=%s\n%s", code, errOut, out)
	}
}

// An archived session's ID, whole or short, names it even with no prompt, as
// a registered one's does (TestHandoffExactIDNeedsNoTitle): only the picker
// and words pass it over.
func TestHandoffArchivedIDNeedsNoPrompt(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.archiveNoPrompt(t)
	f.unregister(t, f.noPrompt)
	for _, query := range []string{f.noPrompt, f.noPrompt[:minShortSessionID]} {
		out, errOut, code := runHandoff(t, f.env, query)
		if code != 0 || !strings.Contains(out, "session "+f.noPrompt+" · source: archive") {
			t.Fatalf("%q: code=%d stderr=%s\n%s", query, code, errOut, out)
		}
	}
}
