package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Session issue codes: what kind of failure kept a session from being
// scanned, published, or cleaned up in the last pass. They are recorded in
// status.json (Status.SessionIssues per session, Status.IssueCounts per
// code) and are part of status --json, so a code, once written, keeps its
// spelling (capture_failed replaced capture_or_publication_failed, which
// status still reads; see issueGapDetail). They carry no error text: a raw
// error can hold transcript content or what a credential_process printed.
const (
	issueStorageAuth             = "storage_auth"
	issueStorageUnavailable      = "storage_unavailable"
	issueTranscriptDiscontinuity = "transcript_discontinuity"
	issueTranscriptSizeLimit     = "transcript_size_limit"
	issueLocalStateUnreadable    = "local_state_unreadable"
	issueRetentionFailed         = "retention_failed"
	issueSubagentNotCaptured     = "subagent_not_captured"
	issueCaptureFailed           = "capture_failed"
)

// issueCodes lists every session issue code in the order the last error
// names them: what needs the user first. issueLabels has a label for each.
var issueCodes = []string{
	issueStorageAuth,
	issueLocalStateUnreadable,
	issueStorageUnavailable,
	issueCaptureFailed,
	issueSubagentNotCaptured,
	issueRetentionFailed,
	issueTranscriptSizeLimit,
	issueTranscriptDiscontinuity,
}

// issueLabel words one kind of session issue: what happened, after a count
// ("2 sessions"), and the next step, or that there is nothing to do.
// headline is the problem status leads with when this kind is the most
// pressing one the last pass recorded; it is empty for a storage kind, which
// keeps status's storage headline, and for a kind with nothing to do, which
// needs no headline.
type issueLabel struct {
	what     string
	next     string
	headline string
}

var issueLabels = map[string]issueLabel{
	issueStorageAuth: {
		what: "failed to reach storage (credentials were refused or unavailable)",
		next: "check the credentials with agent-archive setup (choose storage), then run agent-archive sync",
	},
	issueStorageUnavailable: {
		what: "failed to reach storage (network or service unavailable)",
		next: "check the network and the storage service, then run agent-archive sync (the next pass also retries)",
	},
	// No error of this version carries it: the collector records a rewritten
	// transcript as a transcript_rewritten capture gap instead. It is kept so
	// a status.json from an older version still reads.
	issueTranscriptDiscontinuity: {
		what: "had a transcript that was truncated or rewritten",
		next: "nothing to do, the last snapshot is kept",
	},
	// No error of this version carries it either: the collector records a
	// transcript over the size limit as a capture gap, and its own notice.
	issueTranscriptSizeLimit: {
		what: "stopped being captured at the transcript size limit",
		next: "nothing to do, the last snapshot is kept",
	},
	issueLocalStateUnreadable: {
		what:     "had local state that could not be read and was moved aside",
		next:     "see quarantined_files in agent-archive status --json, then delete those files",
		headline: "Some local state could not be read",
	},
	issueRetentionFailed: {
		what:     "couldn't be removed after the retention period",
		next:     "the next pass retries, or run agent-archive sync for details",
		headline: "Some sessions couldn't be removed after the retention period",
	},
	// Only subagents carry it, so its count reads "1 subagent". Only the pass
	// that rejected the subagent reports it, so a later sync has no details
	// to show.
	issueSubagentNotCaptured: {
		what: "could not be captured (transcript unreadable, too large, or not matching its parent)",
		next: "nothing to do, its parent session records the link as unavailable",
	},
	issueCaptureFailed: {
		what:     "failed to capture or upload",
		next:     "run agent-archive sync for details",
		headline: "Some sessions could not be captured",
	},
}

// errRetentionFailed marks a retention sweep's per-session failure once it
// joins the pass's errors (see recordRetentionErrors).
var errRetentionFailed = errors.New("retention")

// storageIssues are the kinds about storage access, which keep status's
// storage headline (see issueHeadline).
var storageIssues = map[string]bool{
	issueStorageAuth:        true,
	issueStorageUnavailable: true,
}

// sessionIssueCode classifies one session's failure by what its error wraps,
// never by its text.
func sessionIssueCode(issue error) string {
	storageFailure := isStorageError(issue) || credentials.CredentialProcessFailed(issue)
	switch {
	case errors.Is(issue, state.ErrQuarantined):
		return issueLocalStateUnreadable
	case storageFailure && storageFailureState(issue) != "storage_unavailable":
		return issueStorageAuth
	case errors.Is(issue, collector.ErrSubagentNotCaptured):
		return issueSubagentNotCaptured
	case errors.Is(issue, errRetentionFailed):
		return issueRetentionFailed
	case storageFailure:
		return issueStorageUnavailable
	default:
		return issueCaptureFailed
	}
}

// issueGapDetail is the capture gap status shows for a session the last
// pass could not update, with the next step for its kind of failure. A code
// this version does not write, like capture_or_publication_failed from an
// older one, gets capture_failed's.
func issueGapDetail(code string) string {
	label, ok := issueLabels[code]
	if !ok {
		label = issueLabels[issueCaptureFailed]
	}
	return "Last scan could not update this session; retained evidence was kept. " + sentence(label.next)
}

// issueTally counts one kind of issue, top-level sessions and subagents
// apart, so the last error names each.
type issueTally struct {
	sessions  int
	subagents int
}

func (t issueTally) total() int { return t.sessions + t.subagents }

// String is the count a label follows: "2 sessions", "1 subagent", or
// "2 sessions and 1 subagent".
func (t issueTally) String() string {
	switch {
	case t.subagents == 0:
		return plural(t.sessions, "session")
	case t.sessions == 0:
		return plural(t.subagents, "subagent")
	default:
		return plural(t.sessions, "session") + " and " + plural(t.subagents, "subagent")
	}
}

// issueSummary is the one problem the last error records for a pass's
// failed sessions: each kind, in issueCodes order, with its count and next
// step. It is "" when nothing failed.
func issueSummary(tallies map[string]issueTally) string {
	var parts []string
	for _, code := range issueCodes {
		tally := tallies[code]
		if tally.total() == 0 {
			continue
		}
		label := issueLabels[code]
		parts = append(parts, fmt.Sprintf("%s %s — %s", tally, label.what, label.next))
	}
	return strings.Join(parts, " · ")
}

// sessionIssues is what a pass's failed sessions come to: each one's issue
// code, and the count of each code, sessions and subagents apart.
type sessionIssues struct {
	codes   map[string]string
	tallies map[string]issueTally
}

// classifySessions classifies each failed session and counts the codes.
// isSubagent reports whether a session is a subagent; a subagent
// candidate's failure says so itself, since a candidate never became a
// registration.
func classifySessions(sessionErrs map[string]error, isSubagent func(string) bool) sessionIssues {
	issues := sessionIssues{codes: make(map[string]string, len(sessionErrs)), tallies: map[string]issueTally{}}
	for id, sessionErr := range sessionErrs {
		code := sessionIssueCode(sessionErr)
		issues.codes[id] = code
		tally := issues.tallies[code]
		if errors.Is(sessionErr, collector.ErrSubagentCandidate) || isSubagent(id) {
			tally.subagents++
		} else {
			tally.sessions++
		}
		issues.tallies[code] = tally
	}
	return issues
}

// carryRetrying adds the sessions on the Codex lookup allowance's backoff
// schedule (state.Status.NextRetryAt) that the pass recorded no failure
// for, because it held them until their next attempt: each keeps the issue
// its last attempt recorded, which the collector carried into
// current.SessionIssues (capture_failed, which every such failure is, when
// it carried none), and counts in the summary as it did then, so status
// still shows it failing.
func (i sessionIssues) carryRetrying(current state.Status, isSubagent func(string) bool) {
	for id := range current.NextRetryAt {
		if _, failed := i.codes[id]; failed {
			continue
		}
		code := current.SessionIssues[id]
		if code == "" {
			code = issueCaptureFailed
		}
		i.codes[id] = code
		tally := i.tallies[code]
		if isSubagent(id) {
			tally.subagents++
		} else {
			tally.sessions++
		}
		i.tallies[code] = tally
	}
}

// summary is the problem the last error records for these sessions.
func (i sessionIssues) summary() string { return issueSummary(i.tallies) }

// recordSessionIssues records a pass's per-session failures in status.json:
// each session's issue code, replacing the previous pass's, the count of
// each code, and one problem summarizing them, in place of old, the problem
// the same sessions were recorded as before (collector.Run's own count, or
// an earlier summary), or after the pass's other problems when old is not
// recorded. Both collection and retention record through here, so a pass
// with both kinds of failure reports one message covering all of them. Best
// effort, like recordPreflightError: the pass's own error is what the
// caller reports. It returns the summary it recorded. A Codex session the
// pass held for a later retry keeps its issue among them (carryRetrying).
func recordSessionIssues(localStore *state.Store, sessionErrs map[string]error, isSubagent func(string) bool, old string) (string, error) {
	current, err := localStore.LoadStatus()
	if err != nil {
		return "", err
	}
	issues := classifySessions(sessionErrs, isSubagent)
	issues.carryRetrying(current, isSubagent)
	current.SessionIssues = issues.codes
	current.IssueCounts = make(map[string]int, len(issues.tallies))
	for code, tally := range issues.tallies {
		current.IssueCounts[code] = tally.total()
	}
	summary := issues.summary()
	current.ReplaceLastError(old, summary)
	return summary, localStore.SaveStatus(current)
}

// subagentLookup reports whether a session is a subagent, from its
// registration. A session whose registration is gone or unreadable is
// counted as a session.
func subagentLookup(localStore *state.Store) func(string) bool {
	return func(id string) bool {
		reg, found, err := localStore.LoadRegistration(id)
		return err == nil && found && reg.ParentSessionID != ""
	}
}

// issueHeadline is the problem and next step status leads with for the last
// pass's problems when their kinds decide it: the recorded problems are the
// summary of failed sessions, whose counts hold no storage kind and only
// codes this version knows, and the collector's size-limit notice, either of
// them alone or both. ok is false otherwise, and status keeps its general
// headline for a failed sync. A problem of "" with ok means every problem
// has nothing to do, so status needs no failure headline for them.
func issueHeadline(status state.Status) (problem, next string, ok bool) {
	if len(status.LastErrors) == 0 {
		// Nothing recorded, or a status file from before LastErrors.
		return "", "", false
	}
	others := 0
	for _, recorded := range status.LastErrors {
		if !collector.IsSizeLimitProblem(recorded) {
			others++
		}
	}
	if len(status.IssueCounts) == 0 {
		// Only the size-limit notice: nothing to do.
		return "", "", others == 0
	}
	if others != 1 {
		return "", "", false
	}
	for code := range status.IssueCounts {
		if _, known := issueLabels[code]; !known || storageIssues[code] {
			return "", "", false
		}
	}
	for _, code := range issueCodes {
		label := issueLabels[code]
		if status.IssueCounts[code] > 0 && label.headline != "" {
			return label.headline, sentence(label.next), true
		}
	}
	return "", "", true
}
