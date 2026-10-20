package archive

import (
	"slices"
	"time"
)

// RecordPreview holds only filtered display facts from one complete record.
type RecordPreview struct {
	Name         string
	NameSource   SessionNameSource
	NameNativeID string
	Title        string
	// Branch is the record's git branch as recorded, unvalidated: as for a
	// full parse, only the last one recorded is validated, by
	// PreviewAccumulator.ValidBranch.
	Branch   string
	Activity time.Time
	Gaps     []CaptureGap
	Kind     TurnKind
	Command  string
	// PullRequest is the pull request a pr-link record names; its Number is
	// 0 for any other record.
	PullRequest PullRequestLink
}

// PreviewAccumulator keeps only filtered display facts and one pending slash
// command, so an assistant reply can establish that it was a human prompt.
type PreviewAccumulator struct {
	Labels     Labels
	NativeID   string
	NameSource SessionNameSource
	Activity   time.Time
	Gaps       []CaptureGap
	pending    string
}

// AddFacts combines bounded safe display facts without native interpretation.
func (a *PreviewAccumulator) AddFacts(p RecordPreview, head bool) {
	if p.Name != "" && (p.NameNativeID == "" || p.NameNativeID == a.NativeID) && (a.NameSource != SessionNameCustom || p.NameSource == SessionNameCustom) {
		a.Labels.Name = p.Name
		a.NameSource = p.NameSource
	}
	if p.Branch != "" {
		a.Labels.Branch = p.Branch
	}
	if p.PullRequest.Number > 0 && len(a.Labels.PullRequests) < MaxPullRequests && !slices.ContainsFunc(a.Labels.PullRequests, func(prior PullRequestLink) bool {
		return prior.Repository == p.PullRequest.Repository && prior.Number == p.PullRequest.Number
	}) {
		a.Labels.PullRequests = append(a.Labels.PullRequests, p.PullRequest)
	}
	if p.Activity.After(a.Activity) {
		a.Activity = p.Activity
	}
	if head && a.Labels.Title == "" {
		switch p.Kind {
		case TurnKindAssistant:
			if a.pending != "" {
				a.Labels.Title = a.pending
				a.pending = ""
			}
		case TurnKindHumanPrompt:
			a.pending = ""
			a.Labels.Title = p.Title
		case TurnKindLocalCommand:
			a.pending = p.Command
		case TurnKindToolResult, TurnKindHarnessMeta, TurnKindCommandOutput, TurnKindHarnessNotification:
		// Harness records do not answer or interrupt a pending slash command.
		case TurnKindShellCommand, TurnKindCompactSummary:
			a.pending = ""
		}
	}
	for _, g := range p.Gaps {
		found := false
		for _, old := range a.Gaps {
			if old.Code == g.Code {
				found = true
				break
			}
		}
		if !found {
			a.Gaps = append(a.Gaps, CaptureGap{Code: g.Code})
		}
	}
}

// ValidBranch is the last branch the records recorded, validated as
// LabelsFromAnalysis validates a full parse's: "" when it is not a valid
// branch name or is HEAD.
func (a *PreviewAccumulator) ValidBranch() string {
	if branch := validBranch(a.Labels.Branch); branch != "HEAD" {
		return branch
	}
	return ""
}

// SessionNameSource identifies native title precedence without interpreting raw records.
type SessionNameSource string

const (
	// SessionNameCustom is an explicit native rename.
	SessionNameCustom SessionNameSource = "custom"
	// SessionNameGenerated is a native generated title.
	SessionNameGenerated SessionNameSource = "generated"
)
