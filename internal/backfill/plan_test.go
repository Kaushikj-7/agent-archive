package backfill

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
)

// Regression: backfill review, 2026-09 (cbce179).
func TestDuplicateSessions(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	// Two files named s1: the one whose records carry s1 is kept.
	tr.write(filepath.Join("home", claudeFile("a", "s1")), claudeTranscript("s1", repo, start)+claudeTranscript("s1", repo, start))
	tr.write(filepath.Join("home", claudeFile("b", "s1")), claudeTranscript("other", repo, start))
	// Two good files named s2: the larger is kept.
	tr.write(filepath.Join("home", claudeFile("a", "s2")), claudeTranscript("s2", repo, start))
	big := tr.write(filepath.Join("home", claudeFile("b", "s2")), claudeTranscript("s2", repo, start)+claudeTranscript("s2", repo, start))
	// A Codex session in sessions/ and, under another name, archived_sessions/.
	const id = "0a9b3c4d-0000-4000-8000-0000000000dd"
	active := tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, repo, start))
	tr.write(filepath.Join("home", ".codex", "archived_sessions", "rollout-2026-09-19T10-00-00-"+id+".jsonl"), codexTranscript(id, id, repo, start)+codexTranscript(id, id, repo, start))

	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	kept := map[string]string{}
	for _, c := range p.Candidates {
		if c.Skip == "" {
			if kept[c.NativeSessionID] != "" {
				t.Fatalf("%s imported twice", c.NativeSessionID)
			}
			kept[c.NativeSessionID] = c.TranscriptPath
		} else if c.Skip != SkipDuplicateSession {
			t.Errorf("%s: unexpected skip %q", c.TranscriptPath, c.Skip)
		}
	}
	if want := tr.path(filepath.Join("home", claudeFile("a", "s1"))); kept["s1"] != want {
		t.Errorf("s1 kept %s", kept["s1"])
	}
	if kept["s2"] != big {
		t.Errorf("s2 kept %s", kept["s2"])
	}
	if kept[id] != active {
		t.Errorf("codex kept %s", kept[id])
	}
	if got := p.Skipped()[SkipDuplicateSession]; got != 3 {
		t.Errorf("duplicates: %d", got)
	}
}

// A file that disappears during planning is not counted at all; one that
// grows past the limit after discovery is too_large.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestFilesChangingMidScan(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	before := tr.write(filepath.Join("home", claudeFile("s", "gone-before")), claudeTranscript("gone-before", repo, start))
	during := tr.write(filepath.Join("home", claudeFile("s", "gone-during")), claudeTranscript("gone-during", repo, start))
	grows := tr.write(filepath.Join("home", claudeFile("s", "grows")), claudeTranscript("grows", repo, start))
	tr.write(filepath.Join("home", claudeFile("s", "stays")), claudeTranscript("stays", repo, start))
	env := tr.env()
	env.Open = func(path string) (io.ReadCloser, error) {
		switch path {
		case before:
			return nil, os.ErrNotExist
		case during:
			f, err := os.Open(path)
			if err == nil {
				err = os.Remove(path)
			}
			return f, err
		case grows:
			if err := os.Truncate(path, collector.DefaultMaxRawTranscriptBytes+1); err != nil {
				return nil, err
			}
		}
		return os.Open(path)
	}
	p := plan(t, env, nil, config.Config{}, Filters{})
	if len(p.Candidates) != 2 {
		t.Fatalf("got %d candidates, want grows and stays", len(p.Candidates))
	}
	if c := candidate(t, p, "grows"); c.Skip != SkipTooLarge {
		t.Fatalf("grows: %q", c.Skip)
	}
	if c := candidate(t, p, "stays"); c.Skip != "" {
		t.Fatalf("stays: %q", c.Skip)
	}
}

// A Codex rollout is too large only past the Codex raw limit capture uses,
// not the general one: planning refuses no rollout capture would read.
func TestCodexTooLargeFollowsTheCodexRawLimit(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	const within, over = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	withinPath := tr.write(filepath.Join("home", codexFile(within)), codexTranscript(within, within, repo, start))
	overPath := tr.write(filepath.Join("home", codexFile(over)), codexTranscript(over, over, repo, start))
	env := tr.env()
	env.Open = func(path string) (io.ReadCloser, error) {
		size := int64(0)
		switch path {
		case withinPath:
			size = collector.DefaultMaxRawTranscriptBytes + 1
		case overPath:
			size = archive.MaxCodexRawTranscriptBytes + 1
		}
		if size > 0 {
			if err := os.Truncate(path, size); err != nil {
				return nil, err
			}
		}
		return os.Open(path)
	}
	p := plan(t, env, nil, config.Config{}, Filters{})
	// It grew while being planned, as the general limit would have made it
	// too large: it is read again, never refused for its size.
	if c := candidate(t, p, within); c.Skip != SkipSourceChanged {
		t.Fatalf("a Codex rollout under the Codex raw limit: %q", c.Skip)
	}
	if c := candidate(t, p, over); c.Skip != SkipTooLarge {
		t.Fatalf("over the Codex raw limit: %q", c.Skip)
	}
}

// Regression: backfill review, 2026-09 (cbce179).
func TestSubagentChecks(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	tr.write(filepath.Join("home", claudeFile("s", "parent")), claudeTranscript("parent", repo, start))
	dir := filepath.Join("home", ".claude", "projects", "s", "parent", "subagents")
	tr.write(filepath.Join(dir, "agent-ok1.jsonl"), subagentTranscript("parent", "ok1", start.Add(time.Minute)))
	tr.write(filepath.Join(dir, "agent-bad.jsonl"), `{"type":"file-history-snapshot","snapshot":{}}`+"\n")
	big := tr.write(filepath.Join(dir, "agent-big.jsonl"), subagentTranscript("parent", "big", start))
	if err := os.Truncate(big, collector.DefaultMaxRawTranscriptBytes+1); err != nil {
		t.Fatal(err)
	}
	// Each of these reads cleanly, but the collector would refuse to
	// register it, so it is not counted as imported either.
	refused := map[string]string{
		"empty":        "",
		"no-agent-id":  claudeTranscript("parent", repo, start),
		"wrong-agent":  subagentTranscript("parent", "someone-else", start),
		"wrong-parent": subagentTranscript("another-session", "wrong-parent", start),
		"no-time":      `{"type":"assistant","sessionId":"parent","agentId":"no-time","message":{"role":"assistant","content":"looked"}}` + "\n",
		"part-time": subagentTranscript("parent", "part-time", start) +
			`{"type":"assistant","sessionId":"parent","agentId":"part-time","message":{"role":"assistant","content":"more"}}` + "\n",
		"before-parent": subagentTranscript("parent", "before-parent", start.Add(-time.Minute)),
		"ends-later":    subagentTranscript("parent", "ends-later", fixedNow.Add(time.Minute)),
	}
	for id, body := range refused {
		tr.write(filepath.Join(dir, "agent-"+id+".jsonl"), body)
	}
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	c := candidate(t, p, "parent")
	if want := 2 + len(refused); len(c.Subagents) != 1 || c.Subagents[0].AgentID != "ok1" || c.SubagentsSkipped != want || p.SubagentsSkipped() != want {
		t.Fatalf("subagents %+v, skipped %d, want %d", c.Subagents, c.SubagentsSkipped, want)
	}
}

// The plan is the same whatever the worker count.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestPlanDeterministic(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	for i, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		tr.write(filepath.Join("home", claudeFile("s", id)), claudeTranscript(id, repo, fixedNow.Add(-time.Duration(i+1)*time.Hour)))
		tr.write(filepath.Join("home", claudeFile("t", id)), claudeTranscript(id, filepath.Join(repo, "x"), fixedNow.Add(-time.Duration(i+1)*time.Hour)))
	}
	one, eight := tr.env(), tr.env()
	one.Workers, eight.Workers = 1, 8
	a, b := plan(t, one, nil, config.Config{}, Filters{}), plan(t, eight, nil, config.Config{}, Filters{})
	if !reflect.DeepEqual(a.Candidates, b.Candidates) {
		t.Fatal("plans differ between 1 and 8 workers")
	}
}

// Regression: backfill review, 2026-09 (cbce179).
func TestPlanCancelled(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write(filepath.Join("home", claudeFile("s", "a")), claudeTranscript("a", tr.home, fixedNow))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildPlan(ctx, tr.env(), states{}, config.Config{}, Filters{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// A larger copy of a session that cannot be imported never displaces a copy
// that can: the good copy is imported and the bad one is the duplicate.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestDuplicateKeepsTheImportableCopy(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	good := map[string]string{}
	for _, id := range []string{"too-large", "empty", "refused"} {
		good[id] = tr.write(filepath.Join("home", claudeFile("a", id)), claudeTranscript(id, repo, start))
	}
	big := tr.write(filepath.Join("home", claudeFile("b", "too-large")), claudeTranscript("too-large", repo, start))
	if err := os.Truncate(big, collector.DefaultMaxRawTranscriptBytes+1); err != nil {
		t.Fatal(err)
	}
	summary := `{"type":"summary","summary":"recap","leafUuid":"x","cwd":"` + repo + `"}` + "\n"
	tr.write(filepath.Join("home", claudeFile("b", "empty")), strings.Repeat(summary, 20))
	// No record the adapter recognizes: it refuses the file.
	tr.write(filepath.Join("home", claudeFile("b", "refused")), strings.Repeat(`{"type":"mystery","sessionId":"refused","cwd":"`+repo+`"}`+"\n", 20))

	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	for _, c := range p.Candidates {
		want := SkipDuplicateSession
		if c.TranscriptPath == good[c.NativeSessionID] {
			want = ""
		}
		if c.Skip != want {
			t.Errorf("%s (%s): skip %q, want %q", c.NativeSessionID, c.TranscriptPath, c.Skip, want)
		}
	}
	if got := len(p.Imported()); got != 3 {
		t.Fatalf("imported %d sessions, want 3", got)
	}
}

// A blank native ID is an identity problem for that session, not a reason
// to abandon the whole plan.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestBlankNativeID(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "0a9b3c4d-0000-4000-8000-0000000000ab"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(" ", " ", repo, fixedNow.Add(-time.Hour)))
	p, err := BuildPlan(t.Context(), tr.env(), refusingBlank{}, config.Config{}, Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 1 || p.Candidates[0].Skip != SkipIdentityMismatch {
		t.Fatalf("candidates %+v", p.Candidates)
	}
}

// refusingBlank fails as the local store does for a blank native ID.
type refusingBlank struct{}

func (refusingBlank) Classify(_, nativeSessionID string) (SkipReason, error) {
	if strings.TrimSpace(nativeSessionID) == "" {
		return "", os.ErrInvalid
	}
	return "", nil
}

// A session retention removed is reported as removed_by_retention, ahead of
// the reasons after it, and --include-removed imports it.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestRemovedByRetentionInPlan(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	tr.write(filepath.Join("home", claudeFile("s", "removed")), claudeTranscript("removed", repo, start))
	tr.write(filepath.Join("home", claudeFile("s", "removed-home")), claudeTranscript("removed-home", tr.home, start))
	st := states{"removed": SkipRemovedByRetention, "removed-home": SkipRemovedByRetention}
	p := plan(t, tr.env(), st, config.Config{}, Filters{})
	for _, id := range []string{"removed", "removed-home"} {
		if c := candidate(t, p, id); c.Skip != SkipRemovedByRetention {
			t.Errorf("%s: %q", id, c.Skip)
		}
	}
	p = plan(t, tr.env(), st, config.Config{}, Filters{IncludeRemoved: true})
	if c := candidate(t, p, "removed"); c.Skip != "" || c.ProjectRoot != repo {
		t.Errorf("with --include-removed: %+v", c)
	}
	if c := candidate(t, p, "removed-home"); c.Skip != SkipHomeDirectory {
		t.Errorf("with --include-removed, the next reason applies: %q", c.Skip)
	}
}

// A Claude Code or Codex session whose records carry no timestamp is
// start_unknown, even though the file's creation time is available: only
// Cursor's start comes from the file. A date filter does not claim it.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestNoRecordTimeIsStartUnknown(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	tr.write(filepath.Join("home", claudeFile("s", "no-time")), `{"type":"user","uuid":"a","sessionId":"no-time","cwd":"`+repo+`","message":{"role":"user","content":"please check it"}}`+"\n")
	const codexID = "0a9b3c4d-0000-4000-8000-0000000000c1"
	tr.write(filepath.Join("home", codexFile(codexID)), `{"type":"session_meta","payload":{"id":"`+codexID+`","cwd":"`+repo+`","source":"cli"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the file"}]}}
`)
	env := tr.env()
	env.FileCreated = func(string) (time.Time, error) { return fixedNow.Add(-time.Hour), nil }
	for _, filters := range []Filters{{}, {Since: "2026-09-01"}} {
		p := plan(t, env, nil, config.Config{}, filters)
		for _, id := range []string{"no-time", codexID} {
			if c := candidate(t, p, id); c.Skip != SkipStartUnknown || !c.StartedAt.IsZero() || c.StartedAtSource != "" {
				t.Fatalf("%+v: %+v", filters, c)
			}
		}
	}
}

// A Claude Code conversation whose records carry no sessionId at all cannot
// be matched with a hook's registration: identity_mismatch. A file with no
// conversation and no ID is still reported as empty.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestClaudeWithoutSessionID(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	tr.write(filepath.Join("home", claudeFile("s", "no-id")), `{"type":"user","uuid":"a","cwd":"`+repo+`","timestamp":"2026-09-22T10:00:00Z","message":{"role":"user","content":"please check it"}}`+"\n")
	tr.write(filepath.Join("home", claudeFile("s", "no-id-summary")), `{"type":"summary","summary":"recap","cwd":"`+repo+`"}`+"\n")
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if c := candidate(t, p, "no-id"); c.Skip != SkipIdentityMismatch {
		t.Errorf("conversation without an ID: %q", c.Skip)
	}
	if c := candidate(t, p, "no-id-summary"); c.Skip != SkipEmpty {
		t.Errorf("no conversation and no ID: %q", c.Skip)
	}
}
