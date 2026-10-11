package archive

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Shapes the eval export schema requires of fields copied from a sidecar.
var (
	evalCodeShape       = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	evalShortSHAShape   = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	evalBranchShape     = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
	evalRepositoryShape = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// ValidateEvalExport reports whether a session record keeps the invariants
// schemas/eval-export.schema.json states beyond what its Go types hold. A
// sidecar that decodes is not necessarily one the collector wrote: a
// corrupted or hand-edited one can carry a negative count or an unknown
// enum, and a record built from it would break the published schema. The
// caller turns such a session into an error record instead.
func ValidateEvalExport(e EvalExport) error {
	var problems []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Errorf(format, args...))
		}
	}
	check(e.SessionID != "" && e.Harness.Name != "", "missing session ID or harness")
	check(validEvalRevisionSelection(e), "invalid revision selection")
	check(e.Project.RepoKey == "" || IsRepoKey(e.Project.RepoKey), "invalid repo_key")
	if h := e.GitHead; h != nil {
		check(h.Start != nil || h.Last != nil, "empty git_head")
		check(h.Start == nil || IsGitObjectName(h.Start.SHA), "invalid git_head.start")
		check(h.Last == nil || (IsGitObjectName(h.Last.SHA) && h.Last.Dirty == nil), "invalid git_head.last")
	}
	check(e.Replay == nil || e.Replay.RunID == "" || IsReplayRunID(e.Replay.RunID), "invalid replay run_id")
	check(e.State == "" || slices.Contains([]MetadataState{MetadataStateActive, MetadataStateIdle, MetadataStateClosed, MetadataStateUnknown}, e.State), "invalid state")
	check(e.TurnOutcome == "" || slices.Contains([]TurnOutcome{TurnOutcomeCompleted, TurnOutcomeInterrupted, TurnOutcomeError, TurnOutcomeUnknown}, e.TurnOutcome), "invalid turn_outcome")
	check(slices.Contains([]ParserStatus{ParserStatusPartial, ParserStatusFailed, ParserStatusComplete}, e.Parser.Status), "invalid parser status")
	check(nonNegativeCounts(e.Counts), "negative count")
	for _, m := range e.Models {
		check(m.Attributes != nil, "missing model attributes")
		check(slices.Contains([]ModelSummarySource{ModelSummarySourceNativeTranscript, ModelSummarySourceHook}, m.Source), "invalid model source")
		check(slices.Contains([]ResponseModelStatus{ResponseModelStatusNotExposed, ResponseModelStatusObserved}, m.ResponseModelStatus), "invalid model response status")
		check(m.TurnCount == nil || *m.TurnCount >= 0, "negative model turn count")
	}
	check(len(e.ModelTokens) <= 32, "too many model_tokens")
	for _, m := range e.ModelTokens {
		check(m.Model != "" && utf8.RuneCountInString(m.Model) <= maxModelNameRunes && nonNegativeCounts(m), "invalid model_tokens entry")
	}
	check(len(e.ToolsUsed) <= 10 && len(e.MCPCalls) <= 50, "too many tools")
	for _, tool := range slices.Concat(e.ToolsUsed, e.MCPCalls) {
		check(tool.Name != "" && utf8.RuneCountInString(tool.Name) <= toolNameLimit && tool.Count >= 1, "invalid tool usage")
	}
	for _, s := range e.SkillsUsed {
		check(s.Name != "" && (s.TurnCount == nil || *s.TurnCount >= 0) &&
			slices.Contains([]SkillUseEvidence{SkillUseEvidenceNativeInvocation, SkillUseEvidenceReadInference}, s.Evidence), "invalid skill use")
	}
	check(len(e.GitActivity) <= MaxGitActivity, "too much git_activity")
	for _, g := range e.GitActivity {
		check(validEvalGitEvent(g), "invalid git_activity entry")
	}
	for _, gap := range e.CaptureGaps {
		check(evalCodeShape.MatchString(gap.Code) && gap.Record >= 0, "invalid capture gap")
	}
	return errors.Join(problems...)
}

func validEvalGitEvent(g GitEvent) bool {
	return evalCodeShape.MatchString(string(g.Kind)) &&
		slices.Contains([]GitEventSource{GitEventSourceShell, GitEventSourceMCP}, g.Source) &&
		(g.SHA == "" || evalShortSHAShape.MatchString(g.SHA)) &&
		(g.Branch == "" || (len(g.Branch) <= 255 && evalBranchShape.MatchString(g.Branch))) &&
		(g.Repository == "" || evalRepositoryShape.MatchString(g.Repository)) &&
		g.PRNumber >= 0 && g.PRNumber <= 1<<30 &&
		(g.URL == "" || validEvalGitURL(g.URL))
}

// nonNegativeCounts reports whether every *int field of a counts struct is
// absent or at least zero, so a count added later is covered too.
func nonNegativeCounts(counts any) bool {
	v := reflect.ValueOf(counts)
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() == reflect.Pointer && !f.IsNil() && f.Elem().Kind() == reflect.Int && f.Elem().Int() < 0 {
			return false
		}
	}
	return true
}

// validEvalGitURL enforces the schema's HTTPS pattern and URI format. Do not
// normalize a corrupt sidecar URL or return parser errors containing its text:
// the caller emits the established safe read_failed record instead.
func validEvalGitURL(raw string) bool {
	if !strings.HasPrefix(raw, "https://") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() {
		return false
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		if !strings.Contains(parsed.Host, "[") || !strings.Contains(parsed.Host, "]") {
			return false
		}
		address, err := netip.ParseAddr(host)
		return err == nil && address.Zone() == ""
	}
	return true
}

func validEvalRevisionSelection(e EvalExport) bool {
	return e.RevisionID == "" || (historyID.MatchString(e.RevisionID) && e.Source == EvalExportSourceArchive && e.Detail == EvalExportDetailFull)
}
