// Package nativecodec contains pure native format interpretation shared by built-in agents.
package nativecodec

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"maps"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type nativeProfile string

const (
	profileClaude nativeProfile = "claude"
	profileCodex  nativeProfile = "codex"
	profileCursor nativeProfile = "cursor"
)

func parse(ctx context.Context, bundle archive.SourceBundle, agent nativeProfile) (archive.Analysis, error) {
	if err := ctx.Err(); err != nil {
		return archive.Analysis{}, err
	}
	if err := archive.ValidateSourceBundle(bundle); err != nil {
		return archive.Analysis{}, &archive.ParseError{Reason: err.Error()}
	}
	view := archive.NormalizedView{}
	analysis := archive.Analysis{}
	setAvailability(&analysis, bundle, agent)
	isParentBundle := !bundle.IsChild()
	var codexModel, codexReasoning string
	var candidates []toolCallCandidate
	tokens := archive.TokenAccumulator{}
	names := archive.PreviewAccumulator{NativeID: bundle.NativeSessionID}
	for i, record := range bundle.NativeRecords {
		if err := ctx.Err(); err != nil {
			return archive.Analysis{}, err
		}
		if skipNativeRecord(bundle, i, record, agent, &names, &codexModel, &codexReasoning) {
			continue
		}
		collectFacts(&analysis.Facts, bundle, record, agent)
		collectExportFacts(&analysis.Facts, bundle, record, agent)
		if isParentBundle && isSidechainRecord(record) {
			// A subagent's records are archived as the child's own session.
			// Older Claude layouts inline them in the parent transcript; the
			// parent must not count the same messages, turns, and tool calls
			// a second time.
			continue
		}
		if at := latestRecordTime(bundle, record); at.After(view.LatestRecordAt) {
			view.LatestRecordAt = at
		}
		if agent == profileCodex && firstString(record, "type") == "turn_context" {
			codexModel, codexReasoning = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort")
			continue
		}
		if isCompactBoundary(record) {
			// A marker only: filter 5 keeps its ids and timestamp, no text.
			view.CompactBoundaries++
			continue
		}
		observeRecordTokens(bundle, record, agent, codexModel, &analysis, &tokens)
		calls, results, skillUses := toolActivity(record, i, codexModel, codexReasoning)
		for j := range calls {
			calls[j].Call.RecordedBranch = archive.ValidBranch(firstString(record, "gitBranch"))
			calls[j].Call.RecordedAt = parseNativeTimestamp(record)
		}
		for j := range results {
			results[j].RecordedAt = parseNativeTimestamp(record)
		}
		candidates = append(candidates, calls...)
		view.ToolResults = append(view.ToolResults, results...)
		view.NativeSkillUses = append(view.NativeSkillUses, skillUses...)
		role, text, kind, ok := visibleMessage(withoutCodexInjectedContext(agent, record))
		if !ok {
			continue
		}
		if isHiddenRole(role) {
			return archive.Analysis{}, &archive.ParseError{Reason: "hidden role present in filtered source"}
		}
		kind = refineUserKind(record, kind, text)
		if kind == archive.TurnKindCompactSummary {
			view.CompactSummaries++
		}
		var model, responseModel, reasoning string
		var modelSource archive.TurnModelSource
		switch agent {
		case profileCodex:
			model, reasoning, modelSource = codexModel, codexReasoning, archive.TurnModelSourceTurnContext
		case profileClaude:
			responseModel, modelSource = recordModel(record), archive.TurnModelSourceNativeResponse
		case profileCursor:
			model, reasoning, modelSource = recordModel(record), firstStringDeep(record, "reasoning_effort"), archive.TurnModelSourceNativeTranscript
		}
		view.Turns = append(view.Turns, archive.NormalizedTurn{
			RecordIndex:       i,
			Role:              role,
			Kind:              kind,
			MessageID:         nestedMessageID(record),
			Text:              text,
			PresentationText:  prepareTurnText(kind, text),
			PresentationKnown: true,
			Model:             model,
			ResponseModel:     responseModel,
			ModelSource:       modelSource,
			Provider:          firstStringDeep(record, "model_provider"),
			Reasoning:         reasoning,
			ID:                firstStringDeep(record, "id", "uuid"),
			ParentID:          firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"),
			TurnID:            firstStringDeep(record, "turn_id"),
			Timestamp:         firstStringDeep(record, "timestamp", "created_at"),
		})
	}
	if names.Labels.Name != "" {
		analysis.Facts.Name = names.Labels.Name
	}
	archive.ResolveSlashCommands(view.Turns)
	for _, turn := range view.Turns {
		if turn.Kind != archive.TurnKindHumanPrompt {
			continue
		}
		title := turn.Text
		if agent == profileCursor {
			title = stripCursorWrapper(title)
		}
		if title = archive.CollapseSessionTitle(title); title != "" {
			analysis.Facts.TextTitle = title
			break
		}
	}

	view.ToolCalls = archive.FinalizeToolCalls(candidates, view.ToolResults)
	view.Tokens, view.ModelTokens = tokens.Usage()
	view.HookFinals = archive.ReconcileHookFinals(bundle, view.Turns)
	analysis.View = view
	if len(bundle.NativeText) > 0 {
		parseText(&analysis, bundle)
	}
	if agent == profileCursor && len(view.ToolResults) == 0 && len(view.ToolCalls) > 0 {
		analysis.Observability.ToolResults = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	}
	if err := ctx.Err(); err != nil {
		return archive.Analysis{}, err
	}
	return analysis, nil
}

func latestRecordTime(bundle archive.SourceBundle, record map[string]any) time.Time {
	at := parseNativeTimestamp(record)
	if bundle.Capture.SourceFormat == "cursor-composer" {
		if completed, ok := cursorTime(record["completed_at_ms"]); ok && completed.After(at) {
			at = completed
		}
	}
	return at
}

func tokenModel(agent nativeProfile, record map[string]any, codexModel string) string {
	if agent == profileCodex && codexModel != "" && !isPlaceholderModel(codexModel) {
		return codexModel
	}
	return recordModel(record)
}

func accumulateTokens(record map[string]any, model string, totals *archive.TokenAccumulator) {
	usage, owner := firstMapDeepOwner(record, "usage")
	if usage == nil {
		usage, owner = firstMapDeepOwner(record, "turn_token_usage")
	}
	if usage == nil {
		return
	}
	totals.Observe(tokenObservation(usage), firstString(owner, "id"), model)
}

func setAvailability(a *archive.Analysis, b archive.SourceBundle, agent nativeProfile) {
	a.Observability.StructuredCounts = archive.Availability{State: archive.AvailabilityAvailable}
	a.Observability.Compactions = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	a.Observability.ToolErrors = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	a.Observability.ToolResults = archive.Availability{State: archive.AvailabilityAvailable}
	if agent == profileClaude {
		version, err := strconv.Atoi(strings.TrimSpace(b.Capture.FilterVersion))
		if err == nil && version >= 5 {
			a.Observability.Compactions = archive.Availability{State: archive.AvailabilityAvailable}
		} else {
			a.Observability.Compactions.Reason = archive.AvailabilityReasonHistoricalFilter
		}
	}
	if agent == profileClaude || agent == profileCursor {
		a.Observability.ToolErrors = archive.Availability{State: archive.AvailabilityAvailable}
	}
	if len(b.NativeText) > 0 {
		a.Facts.Text = true
		a.Facts.TextOnly = len(b.NativeRecords) == 0
		a.Observability.StructuredCounts = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonText}
	}
}

func collectFacts(f *archive.NativeFacts, b archive.SourceBundle, r map[string]any, agent nativeProfile) {
	kind := firstString(r, "type")
	var name string
	switch {
	case kind == "subagent-meta":
		name = firstString(r, "description")
	case kind == "session" && b.Capture.SourceFormat == "cursor-composer":
		name = firstString(r, "name")
	case agent == profileCodex && b.IsChild():
		// The last own header wins: a forked child can carry its parent's
		// own NEW_TASK in an inherited prefix ahead of the one it received.
		name = codexSpawnedTaskName(r)
	}
	if name = archive.CollapseSessionTitle(name); name != "" {
		f.Name = name
	}
	if f.WorkspaceRoot == "" {
		if cwd := firstStringDeep(r, "cwd"); cwd != "" {
			f.WorkspaceRoot = path.Clean(cwd)
		}
	}
	if branch := firstStringDeep(r, "gitBranch"); branch != "" {
		f.Branch = branch
	}
	collectPullRequest(f, r)
	if agent == profileCursor && strings.ToLower(strings.TrimSpace(kind)) == "turn_ended" {
		f.TurnEnd = archive.NativeTurnEnd{Present: true, State: archive.MetadataStateIdle, Outcome: archive.TurnOutcomeUnknown}
		switch strings.ToLower(strings.TrimSpace(firstString(r, "status"))) {
		case "completed":
			f.TurnEnd.Outcome = archive.TurnOutcomeCompleted
		case "aborted", "cancelled", "canceled", "interrupted":
			f.TurnEnd.Outcome = archive.TurnOutcomeInterrupted
		case "error", "failed":
			f.TurnEnd.Outcome = archive.TurnOutcomeError
		}
	}
	if !isSidechainRecord(r) {
		for _, key := range []string{"sessionId", "session_id"} {
			if id := firstString(r, key); id != "" && id != b.NativeSessionID {
				f.IdentityConflict = true
			}
		}
		if agent == profileCodex && kind == "session_meta" {
			payload, _ := r["payload"].(map[string]any)
			// session_id identifies the root conversation, not this thread.
			if id := firstString(payload, "id"); id != b.NativeSessionID {
				f.IdentityConflict = true
			}
		}
	}
}

// codexNewTaskHeader opens the plain header of the message a Codex
// multi-agent parent sends a thread it spawns. The task itself follows as
// encrypted content, which the filter never keeps, so a spawned thread has no
// prompt of its own: the task name in the header is all that names it.
const codexNewTaskHeader = "Message Type: NEW_TASK"

// codexSpawnedTaskName is the readable task name in a Codex agent_message
// record's NEW_TASK header: the last segment of its "Task name: /root/…"
// agent path, with underscores, hyphens and runs of space as single spaces.
// "" for any other record.
func codexSpawnedTaskName(r map[string]any) string {
	payload, _ := r["payload"].(map[string]any)
	if firstString(r, "type") != "response_item" || firstString(payload, "type") != "agent_message" {
		return ""
	}
	content, _ := payload["content"].([]any)
	for _, item := range content {
		header, ok := strings.CutPrefix(contentText(item), codexNewTaskHeader+"\n")
		if !ok {
			continue
		}
		for line := range strings.SplitSeq(header, "\n") {
			if strings.HasPrefix(line, "Payload:") {
				// The header ends here; what follows is the task, never a name.
				break
			}
			agentPath, found := strings.CutPrefix(line, "Task name: ")
			if !found {
				continue
			}
			agentPath = strings.TrimSpace(agentPath)
			task := agentPath[strings.LastIndex(agentPath, "/")+1:]
			return strings.Join(strings.FieldsFunc(task, func(c rune) bool { return c == '_' || c == '-' || unicode.IsSpace(c) }), " ")
		}
	}
	return ""
}

// ParseClaude interprets retained safe Claude evidence without source I/O.
func ParseClaude(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	return parse(ctx, b, profileClaude)
}

// ParseCodex interprets retained safe Codex evidence without source I/O.
func ParseCodex(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	return parse(ctx, b, profileCodex)
}

// ParseCursor interprets retained safe Cursor evidence without source I/O.
func ParseCursor(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	return parse(ctx, b, profileCursor)
}

func collectPullRequest(f *archive.NativeFacts, r map[string]any) {
	if firstString(r, "type") == "pr-link" && len(f.PullRequests) < archive.MaxPullRequests {
		repository := firstString(r, "prRepository")
		owner, name, ok := splitRepository(repository)
		number, numberOK := claudePRNumber(r["prNumber"])
		if ok && numberOK {
			var url string
			if firstString(r, "prUrl") == claudePRURL(owner, name, number) {
				url = claudePRURL(owner, name, number)
			}
			link := archive.PullRequestLink{Repository: repository, Number: number, URL: url}
			seen := false
			for _, prior := range f.PullRequests {
				if prior.Repository == link.Repository && prior.Number == link.Number {
					seen = true
					break
				}
			}
			if !seen {
				f.PullRequests = append(f.PullRequests, link)
			}
		}
	}
}

// collectExportFacts retains the initial branch and native start semantics used by export.
func collectExportFacts(f *archive.NativeFacts, b archive.SourceBundle, r map[string]any, agent nativeProfile) {
	if b.ParentSessionID != "" || !isSidechainRecord(r) {
		if f.FirstBranch == "" {
			f.FirstBranch = firstStringDeep(r, "gitBranch")
		}
	}
	observe := func(at time.Time) {
		if !at.IsZero() && (f.EarliestRecordAt.IsZero() || at.Before(f.EarliestRecordAt)) {
			f.EarliestRecordAt = at
		}
	}
	observe(parseNativeTimestamp(r))
	if firstString(r, "type") == "session_meta" {
		payload, _ := r["payload"].(map[string]any)
		at := parseNativeTimestamp(payload)
		observe(at)
		if agent == profileCodex && f.NativeStartedAt.IsZero() {
			if at.IsZero() {
				at = parseNativeTimestamp(r)
			}
			f.NativeStartedAt = at
		}
	}
}

// A cumulative counter is not proof that a child reset it or carried its parent.
// Independently recorded per-call usage remains useful even beside such counters.
func historyTokenScopeUnknown(bundle archive.SourceBundle, record map[string]any) bool {
	if usage, _ := firstMapDeepOwner(record, "usage"); usage != nil {
		return false
	}
	if turn, _ := firstMapDeepOwner(record, "turn_token_usage"); turn != nil {
		if bundle.History.OwnStart != nil && *bundle.History.OwnStart > 0 {
			return true
		}
		for _, span := range bundle.History.Spans {
			if span.ThreadID != bundle.NativeSessionID {
				return true
			}
		}
		return false
	}
	for _, key := range []string{"total_token_usage", "thread_token_usage", "last_token_usage"} {
		if value, _ := firstMapDeepOwner(record, key); value != nil {
			return true
		}
	}
	return false
}

func ownCodexRecord(bundle archive.SourceBundle, index int, record map[string]any, model, reasoning *string) bool {
	if bundle.History == nil {
		return true
	}
	if firstString(record, "type") == "turn_context" {
		*model, *reasoning = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort")
	}
	if firstString(record, "type") == "session_meta" {
		span, _ := bundle.History.SpanAt(index)
		return span.RolloutID == bundle.History.ActiveRolloutID
	}
	return bundle.OwnRecord(index)
}

func observeRecordTokens(bundle archive.SourceBundle, record map[string]any, agent nativeProfile, codexModel string, analysis *archive.Analysis, tokens *archive.TokenAccumulator) {
	if agent == profileCodex && bundle.History != nil && historyTokenScopeUnknown(bundle, record) {
		analysis.Facts.TokenScopeUnknown = true
	} else if !isPlaceholderModel(firstStringDeep(record, "model", "model_id")) {
		accumulateTokens(record, tokenModel(agent, record, codexModel), tokens)
	}
}

func isClaudeTitleRecord(r map[string]any) bool {
	kind := claudeLabelKind(firstString(r, "type"))
	return kind == claudeCustomTitleType || kind == claudeAITitleType
}

// skipNativeRecord applies agent ownership and handles naming-only evidence
// before conversation facts can affect activity or counts.
func skipNativeRecord(bundle archive.SourceBundle, index int, record map[string]any, agent nativeProfile, names *archive.PreviewAccumulator, model, reasoning *string) bool {
	if agent == profileCodex {
		return !ownCodexRecord(bundle, index, record, model, reasoning)
	}
	if agent == profileClaude && isClaudeTitleRecord(record) {
		if !isSidechainRecord(record) {
			names.AddFacts(claudeTitlePreview(record), true)
		}
		return true
	}
	return false
}

// withoutCodexInjectedContext applies the current filter's injected-context
// rule to a Codex user message's text. Filter 16 drops a leading
// <external_codex_apps_open_page> wrapper, but sources an earlier filter
// retained still hold it, and a metadata refresh re-derives from those
// retained bytes, not the live transcript. Without this, a desktop turn
// whose first user record is only that wrapper becomes the session title.
// Text the current filter kept is a fixed point, so new captures derive
// what they did before. The record is copied only when text changes, and a
// block left empty loses its text the way the filter drops it.
func withoutCodexInjectedContext(agent nativeProfile, record map[string]any) map[string]any {
	if agent != profileCodex {
		return record
	}
	payload, ok := record["payload"].(map[string]any)
	if !ok || firstString(payload, "role") != "user" {
		return record
	}
	content, changed := stripInjectedContent(payload["content"])
	if !changed {
		return record
	}
	nextPayload := maps.Clone(payload)
	nextPayload["content"] = content
	next := maps.Clone(record)
	next["payload"] = nextPayload
	return next
}

// stripInjectedContent strips injected context from a string content or
// from each text block of a block list, reporting whether any text changed.
func stripInjectedContent(content any) (any, bool) {
	switch value := content.(type) {
	case string:
		if injected, stripped := archive.StripInjectedContext(value); injected {
			return stripped, true
		}
	case []any:
		out := make([]any, len(value))
		changed := false
		for i, raw := range value {
			out[i] = raw
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text, ok := block["text"].(string)
			if !ok {
				continue
			}
			injected, stripped := archive.StripInjectedContext(text)
			if !injected {
				continue
			}
			kept := maps.Clone(block)
			delete(kept, "text")
			if stripped != "" {
				kept["text"] = stripped
			}
			out[i], changed = kept, true
		}
		if changed {
			return out, true
		}
	}
	return content, false
}
