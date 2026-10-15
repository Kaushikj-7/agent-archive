package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// activitySessions publishes, by capture newest first: a session backfill
// imported a day ago that ended a week ago, and one a hook captured three
// days ago. By activity the hook capture is the newer. It returns their IDs.
func activitySessions(t *testing.T, mem *storagetest.MemoryStore) (imported, hooked string) {
	t.Helper()
	imported, hooked = "aaaaaaaa"+strings.Repeat("1", 24), "bbbbbbbb"+strings.Repeat("2", 24)
	ended := statsNow.Add(-7 * 24 * time.Hour)
	for _, m := range []archive.Metadata{
		syntheticSession{id: imported, harness: "codex", project: "p", captured: statsNow.Add(-24 * time.Hour), origin: archive.SessionOriginImport, turns: 1}.build(),
		syntheticSession{id: hooked, harness: "codex", project: "p", captured: statsNow.Add(-3 * 24 * time.Hour), turns: 1}.build(),
	} {
		if m.SessionID == imported {
			m.StartedAt, m.EndedAt = ended.Add(-time.Hour), &ended
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Put(context.Background(), key, data); err != nil {
			t.Fatal(err)
		}
	}
	return imported, hooked
}

// WHEN is when a session was last active, and the newest activity comes
// first, in list, the show and handoff browsers, and handoff's picker alike:
// an import is not dated to the day backfill ran.
func TestListingsDateAndSortSessionsByLastActivity(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	imported, hooked := activitySessions(t, mem)
	check := func(name, out string) {
		t.Helper()
		older, newer := strings.Index(out, imported[:minShortSessionID]), strings.Index(out, hooked[:minShortSessionID])
		if older < 0 || newer < 0 || newer > older {
			t.Fatalf("%s: not newest activity first:\n%s", name, out)
		}
		if line := pickerLine(t, out, imported); !strings.Contains(line, shortWhen(statsNow, statsNow.Add(-7*24*time.Hour))+"  ") {
			t.Errorf("%s: the import is not dated by its last activity: %q", name, line)
		}
		if line := pickerLine(t, out, hooked); !strings.Contains(line, shortWhen(statsNow, statsNow.Add(-3*24*time.Hour))+"  ") {
			t.Errorf("%s: the hook capture is not dated by its capture: %q", name, line)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--all-projects"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("list: code=%d stderr=%s", code, errOut.String())
	}
	check("list", out.String())
	picked, errText, code := runPicker(t, env, "q\n", "--source", "archive")
	if code != 0 {
		t.Fatalf("handoff: code=%d stderr=%s", code, errText)
	}
	check("handoff", picked)
	stdin := strings.NewReader("q\n")
	out.Reset()
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&out) }
	if code := Run([]string{"show"}, stdin, &out, &errOut, env); code != 0 {
		t.Fatalf("show: code=%d stderr=%s", code, errOut.String())
	}
	check("show", out.String())
	// --json keeps its order, newest capture first, and its fields' meaning.
	out.Reset()
	if code := Run([]string{"list", "--all-projects", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("list --json: code=%d stderr=%s", code, errOut.String())
	}
	if strings.Index(out.String(), imported) > strings.Index(out.String(), hooked) {
		t.Fatalf("list --json is not newest capture first:\n%s", out.String())
	}
}

func TestLastActivityFallsBack(t *testing.T) {
	t.Parallel()
	started, ended, captured := statsNow.Add(-9*time.Hour), statsNow.Add(-8*time.Hour), statsNow.Add(-time.Hour)
	for _, c := range []struct {
		name string
		m    archive.Metadata
		want time.Time
	}{
		{"ended", archive.Metadata{StartedAt: started, EndedAt: &ended, CapturedAt: captured, Origin: archive.SessionOriginImport}, ended},
		{"hook capture without an end", archive.Metadata{StartedAt: started, CapturedAt: captured}, captured},
		{"import without an end", archive.Metadata{StartedAt: started, CapturedAt: captured, Origin: archive.SessionOriginImport}, started},
		{"import without a start", archive.Metadata{CapturedAt: captured, Origin: archive.SessionOriginImport}, captured},
		{"no capture time", archive.Metadata{StartedAt: started}, started},
	} {
		if got := lastActivity(c.m); !got.Equal(c.want) {
			t.Errorf("%s: lastActivity = %v, want %v", c.name, got, c.want)
		}
	}
}
