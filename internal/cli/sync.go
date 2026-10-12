package cli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// waitingSummary is sync's count of publications held for the upload
// interval, with the local time the earliest is due, or "" when none wait.
func waitingSummary(waiting []string, next time.Time) string {
	if len(waiting) == 0 {
		return ""
	}
	if next.IsZero() {
		return fmt.Sprintf("%d waiting for the upload interval, ", len(waiting))
	}
	return fmt.Sprintf("%d waiting for the upload interval (next at %s), ", len(waiting), next.Local().Format("15:04"))
}

// subagentsSummary is sync's count of subagents whose transcripts are not
// written yet, of those resumed and still running, and of those the
// collector decided not to capture, or "" when there are none. Neither is a failure: a waiting subagent is
// registered once its transcript appears, and a rejection that lost
// something is also among the failed sessions, so it is not counted again.
func subagentsSummary(result collector.Result) string {
	summary := ""
	if n := len(result.WaitingSubagents); n > 0 {
		summary += fmt.Sprintf("; %d subagent(s) waiting for transcripts", n)
	}
	if n := len(result.RunningSubagents); n > 0 {
		summary += fmt.Sprintf("; %d subagent(s) still running", n)
	}
	notCaptured := 0
	for id := range result.RejectedSubagents {
		if _, failed := result.Errors[id]; !failed {
			notCaptured++
		}
	}
	if notCaptured > 0 {
		summary += fmt.Sprintf("; %d subagent(s) not captured", notCaptured)
	}
	return summary
}

// deferredWhat words each reason a pass deferred a session's scan, after a
// count, for sync and status.
var deferredWhat = map[string]string{
	collector.DeferredHistoryCoverage: "waiting for Codex history coverage",
	collector.DeferredSourceChanged:   "rechecking after the source changed while being read",
	collector.DeferredReadBudget:      "waiting for read memory other sessions held",
}

// deferredReasonWhat words one deferral reason, with a general wording for a
// reason this version does not write.
func deferredReasonWhat(reason string) string {
	if what, ok := deferredWhat[reason]; ok {
		return what
	}
	return "rechecking on the next pass"
}

// deferredSummary is sync's count of the sessions a pass deferred, by
// reason, or "" when it deferred none. A deferral is not a failure: the next
// pass retries the session, so sync lists no error for it. A session in
// failed (a retention failure recorded after collection, say) is counted
// there only.
func deferredSummary(deferred map[string]string, failed map[string]error) string {
	counts := map[string]int{}
	for id, reason := range deferred {
		if failed[id] == nil {
			counts[deferredReasonWhat(reason)]++
		}
	}
	var summary strings.Builder
	for _, what := range slices.Sorted(maps.Keys(counts)) {
		fmt.Fprintf(&summary, "; %d %s", counts[what], what)
	}
	return summary.String()
}

// syncSummary is the line sync reports a pass on: what it published, held,
// left unchanged, failed and deferred. A deferred session is not counted
// as failed, and has no error line of its own.
func syncSummary(result collector.Result) string {
	return fmt.Sprintf("Scanned %d session(s): %d published, %s%d unchanged, %d failed%s%s.",
		result.Scanned, len(result.Published), waitingSummary(result.Waiting, result.NextReadyAt), len(result.Skipped), len(result.Errors), deferredSummary(result.Deferred, result.Errors), subagentsSummary(result))
}

// runSyncCommand implements `agent-archive sync`: one explicit collection
// pass, preserving queued work on failure. It respects paused state and
// does not implicitly resume, per the spec.
func runSyncCommand(args []string, stdout, stderr io.Writer, env Env) int {
	if !env.newCommandFlags("sync", stderr).parseFlagsOnly(args) {
		return 2
	}
	// Like every command: what was asked for goes to stdout; why it was not
	// done (or not all of it) goes to stderr, with exit 1.
	stopScan := startActivity(stdout, "Scanning sessions…")
	result, err := runOnePass(env, false)
	stopScan()
	if err != nil {
		switch {
		case errors.Is(err, errPaused), errors.Is(err, errNotSetUp):
			terminal.Println(stderr, "agent-archive: sync: "+err.Error())
		case errors.Is(err, local.ErrBusy):
			holder := "another agent-archive command"
			if home, homeErr := env.readHome(); homeErr == nil {
				holder = lockHolder(home)
			}
			terminal.Printf(stderr, "agent-archive: sync: %s holds the collector lock; retry when it finishes\n", holder)
		default:
			terminal.Printf(stderr, "agent-archive: sync: %v\n", err)
			// A Keychain failure has one specific fix; say which.
			if action := credentials.RecoveryAction(err); action != "" {
				terminal.Println(stderr, "agent-archive: sync: "+action)
			}
		}
		return 1
	}
	terminal.Println(stdout, syncSummary(result))
	for id, sessionErr := range result.Errors {
		for _, line := range sessionErrorLines(id, sessionErr) {
			terminal.Println(stderr, line)
		}
	}
	if len(result.Errors) > 0 {
		return 1
	}
	return 0
}

// sessionErrorLines are the lines sync reports a session's error on: one
// per error it joins, and one per line of an error that still spans several
// (a join wrapped by fmt.Errorf), each prefixed so none reads as another
// session's.
func sessionErrorLines(id string, err error) []string {
	var lines []string
	for _, part := range joinedErrors(err) {
		for line := range strings.SplitSeq(part.Error(), "\n") {
			lines = append(lines, fmt.Sprintf("agent-archive: sync: %s: %s", id, line))
		}
	}
	return lines
}

// joinedErrors lists the errors err joins (errors.Join), each join inside it
// flattened too, so each is reported on a line of its own; any other error,
// including one fmt.Errorf wrapped around several, is the one entry. A join
// is told by its text, its parts' on lines of their own: errors.Join's type
// is not exported.
func joinedErrors(err error) []error {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var parts, flat []error
	var texts []string
	for _, part := range joined.Unwrap() {
		if part != nil {
			parts = append(parts, part)
			texts = append(texts, part.Error())
		}
	}
	if len(parts) == 0 || err.Error() != strings.Join(texts, "\n") {
		return []error{err}
	}
	for _, part := range parts {
		flat = append(flat, joinedErrors(part)...)
	}
	return flat
}
