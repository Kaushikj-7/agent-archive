package archive

import "time"

// Analysis is one pure derivation of retained evidence for all shared builders.
// It is never a second persisted transcript and owns no source handles.
type Analysis struct {
	View              NormalizedView
	Facts             NativeFacts
	Observability     Observability
	TokenObservations *TokenObservationSummary
}

// AvailabilityState distinguishes a measured zero from unavailable evidence.
type AvailabilityState string

// Availability states distinguish measured evidence from unavailable or unknown evidence.
const (
	AvailabilityAvailable   AvailabilityState = "available"
	AvailabilityUnavailable AvailabilityState = "unavailable"
	AvailabilityUnknown     AvailabilityState = "unknown"
)

// AvailabilityReason is a fixed, content-free explanation of availability.
type AvailabilityReason string

// Availability reasons describe content-free limits of the retained evidence.
const (
	AvailabilityReasonText             AvailabilityReason = "text_structure_unproven"
	AvailabilityReasonNotRecorded      AvailabilityReason = "not_recorded"
	AvailabilityReasonHistoricalFilter AvailabilityReason = "historical_filter"
	AvailabilityReasonParseFailed      AvailabilityReason = "parse_failed"
	AvailabilityReasonObservationLimit AvailabilityReason = "observation_limit"
)

// Availability describes evidence in this bundle, not a current capability.
type Availability struct {
	State  AvailabilityState  `json:"state"`
	Reason AvailabilityReason `json:"reason,omitempty"`
}

// Available reports whether this evidence can support a measured value.
func (a Availability) Available() bool { return a.State == AvailabilityAvailable }

// Observability describes the actual retained source format and filter version.
type Observability struct {
	StructuredCounts Availability
	// ToolCalls, FilesTouched and GitActivity refine StructuredCounts. Their
	// zero value inherits StructuredCounts only for legacy producer compatibility;
	// explicit unknown or unavailable evidence never supports a measured value.
	ToolCalls    Availability
	FilesTouched Availability
	GitActivity  Availability
	Compactions  Availability
	ToolErrors   Availability
	ToolResults  Availability
}

// structuredEvidenceAvailable preserves legacy producers that predate a finer
// evidence category. Explicit unknown and unavailable values remain unmeasured.
func (o Observability) structuredEvidenceAvailable(evidence Availability) bool {
	if evidence == (Availability{}) {
		return o.StructuredCounts.Available()
	}
	return evidence.Available()
}

// NativeFacts are semantic facts read only from safely retained evidence.
type NativeFacts struct {
	Name          string
	TextTitle     string
	WorkspaceRoot string
	// FirstBranch is the initial retained branch, excluding inlined child evidence.
	FirstBranch string
	// NativeStartedAt is the original session start recorded by the integration.
	NativeStartedAt time.Time
	// EarliestRecordAt includes native bookkeeping timestamps.
	EarliestRecordAt time.Time
	Branch           string
	PullRequests     []PullRequestLink
	TurnEnd          NativeTurnEnd
	// Text preserves the historical text-only prompt eligibility policy.
	Text bool
	// TextOnly selects rendering when there is no retained structured evidence.
	TextOnly bool
	// IdentityConflict is interpreted by the native parser against the bundle ID.
	IdentityConflict bool
	// TokenScopeUnknown marks cumulative accounting that cannot establish own usage.
	TokenScopeUnknown bool
}

// NativeTurnEnd distinguishes an absent outcome from a recorded unknown outcome.
type NativeTurnEnd struct {
	Present bool
	State   MetadataState
	Outcome TurnOutcome
}

// ReconcileHookFinals relates common hook evidence to already parsed turns.
func ReconcileHookFinals(bundle SourceBundle, turns []NormalizedTurn) []HookFinalReconciliation {
	return reconcileHookFinals(bundle, turns)
}

// CollapseSessionTitle applies shared title presentation bounds.
func CollapseSessionTitle(text string) string { return collapseSessionTitle(text) }

// ValidBranch applies the common public branch shape.
func ValidBranch(branch string) string { return validBranch(branch) }

// SkillNameFromPath recognizes shared skill evidence paths.
func SkillNameFromPath(value string) string { return skillNameFromPath(value) }

// ValidateSourceBundle validates the common envelope before interpreting evidence.
func ValidateSourceBundle(bundle SourceBundle) error { return validateBundle(bundle) }

// ResolveSlashCommands resolves common classified turns without native traversal.
func ResolveSlashCommands(turns []NormalizedTurn) { resolveSlashCommands(turns) }

// ToolCallCandidate records the origin needed for common completion deduplication.
type ToolCallCandidate struct {
	Call           NormalizedToolCall
	CompletionEcho bool
}

// FinalizeToolCalls deduplicates candidates and attaches common tool results.
func FinalizeToolCalls(candidates []ToolCallCandidate, results []NormalizedToolResult) []NormalizedToolCall {
	calls := dedupeToolCalls(candidates)
	linkToolResults(calls, results)
	return calls
}

// TokenCount distinguishes missing accounting from a recorded zero without
// allocating a pointer for every streamed native accounting field.
type TokenCount struct {
	Value    int
	Recorded bool
}

// TokenObservation carries presence-aware accounting extracted from one native record.
type TokenObservation struct {
	Input      TokenCount
	Output     TokenCount
	CacheRead  TokenCount
	CacheWrite TokenCount
	Reasoning  TokenCount
}

// TokenAccumulator applies common message deduplication and model aggregation.
type TokenAccumulator struct {
	byMessage map[string]typedModelUsage
	anonymous []typedModelUsage
}

type typedModelUsage struct {
	usage TokenObservation
	model string
}

// Observe records accounting, replacing prior observations of the same message.
func (t *TokenAccumulator) Observe(usage TokenObservation, messageID, model string) {
	entry := typedModelUsage{usage: usage, model: model}
	if messageID == "" {
		t.anonymous = append(t.anonymous, entry)
		return
	}
	if t.byMessage == nil {
		t.byMessage = map[string]typedModelUsage{}
	}
	t.byMessage[messageID] = entry
}

// Usage returns common total and per-model accounting from the observed records.
func (t *TokenAccumulator) Usage() (TokenUsage, []ModelTokens) {
	var total TokenUsage
	byModel := map[string]*TokenUsage{}
	observe := func(source typedModelUsage) {
		if source.usage == (TokenObservation{}) {
			return
		}
		addTokenObservation(&total, source.usage)
		model := boundModelName(source.model)
		if model == "" {
			model = UnknownModel
		}
		if byModel[model] == nil {
			byModel[model] = &TokenUsage{}
		}
		addTokenObservation(byModel[model], source.usage)
	}
	for _, source := range t.anonymous {
		observe(source)
	}
	for _, source := range t.byMessage {
		observe(source)
	}
	foldExtraModels(byModel)
	return total, sortedModelTokens(byModel)
}

func addTokenObservation(out *TokenUsage, usage TokenObservation) {
	add := func(dst **int, count TokenCount) {
		if count.Recorded {
			addTokenCount(dst, count.Value)
		}
	}
	add(&out.Input, usage.Input)
	add(&out.Output, usage.Output)
	add(&out.CacheRead, usage.CacheRead)
	add(&out.CacheWrite, usage.CacheWrite)
	add(&out.Reasoning, usage.Reasoning)
}
