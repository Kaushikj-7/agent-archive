package agentapi

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"time"
)

// CodexRolloutLookup supplies bounded existing catalog evidence without enumeration.
// Every locator is an untrusted hint, revalidated by the source provider.
type CodexRolloutLookup interface {
	Rollout(context.Context, string) ([]SourceRef, error)
	Thread(context.Context, string) (CodexRolloutSet, error)
	Check(context.Context, string, string) error
}

// CodexRolloutSet separates a native current locator from complete lineage evidence.
// Complete must never be inferred from a prunable observation cache alone.
// Revision changes whenever the current locator, candidate membership, or candidate
// identity evidence changes. Check must revalidate this token within the same budget.
type CodexRolloutSet struct {
	Current    *SourceRef
	Candidates []SourceRef
	Revision   string
	Complete   bool
	// Lineage identifies the same current locator and candidate evidence as
	// Revision, leaving out whether coverage was complete. Persisted source
	// signatures use it (see LineageToken), so coverage changing between
	// passes does not by itself make an unchanged source look changed.
	Lineage string
	// Retained reports that Complete rests on an earlier coverage epoch's
	// proof rather than this pass's. That proof stays sound for every
	// candidate it names (each is reopened and rechecked), but a segment
	// created since it, in a directory not listed again yet, can be missing.
	// A reader may still use it to recognize an unchanged source, but not to
	// choose a lineage tip from the candidates alone.
	Retained bool
}

// LineageToken is Lineage, or Revision for a lookup whose Revision never
// depends on coverage completeness.
func (s CodexRolloutSet) LineageToken() string {
	if s.Lineage != "" {
		return s.Lineage
	}
	return s.Revision
}

// CodexHistoryHeader and CodexHistoryRecord frame selected identity and physical records.
const (
	CodexHistoryHeader NativeRecordKind = "codex_history_header"
	CodexHistoryRecord NativeRecordKind = "codex_history_record"
)

// SourceAdmission contains immutable native observations, never capture permission.
type SourceAdmission struct {
	NativeID                  string
	Cwd                       string
	Binding                   *archive.CodexSourceBinding
	NativeCreatedAt           time.Time
	InitialProducerVersion    string
	InitialProducerOriginator string
	InitialProducerSource     string
}

// SourceAdmissionValidator checks opened source facts without reopening a locator.
type SourceAdmissionValidator interface {
	ValidateAdmission(context.Context, SourceAdmission) error
}

// CodexValidationLimits bounds use after one completed validation sweep. Zero
// values select 512 calls and 30 seconds; maxima are 512 calls and 60 seconds.
type CodexValidationLimits struct {
	Steps    int
	Duration time.Duration
}

// CodexRolloutSliceProvider optionally shares validation within a caller-owned
// operation slice. The caller closes all snapshots before renewing a slice.
type CodexRolloutSliceProvider interface {
	BeginValidationSlice(context.Context, CodexValidationLimits) (CodexRolloutSlice, error)
}

// CodexRolloutSlice expires without automatic renewal. It never supplies capture
// permission; providers still validate selected files on the opened handle.
type CodexRolloutSlice interface {
	CodexRolloutLookup
	Valid(context.Context) error
	Close() error
}

// SourceAdmissionFacts exposes bounded facts from the actual opened snapshot.
type SourceAdmissionFacts interface {
	AdmissionFacts(context.Context) (archive.CodexSourceBinding, error)
}

// SourceAdmissionSignature validates opened native facts without decoding a
// complete transcript merely to decide whether its signature is unchanged.
type SourceAdmissionSignature interface {
	ValidateSourceAdmission(context.Context, SourceRef, SourceAdmission) error
}

// SourceRevisions reads validated historical physical segments under existing
// thread admission, independently of which segment the native row selects now.
type SourceRevisions interface {
	ReadRevision(context.Context, SourceRef, SourceAdmission, ReadLimits) (SourceSnapshot, error)
}

// SourceRevisionCandidates exposes bounded same-thread candidates from validated
// graph and lookup evidence; these observations never grant capture admission.
type SourceRevisionCandidates interface {
	RevisionCandidates(context.Context) ([]SourceRef, error)
}

// OwnTaskFacts contains the first owned native task observation, never permission.
// Seen with Native false refuses a later task as replacement evidence.
type OwnTaskFacts struct {
	// Refusal is a fixed metadata validation code, never native content.
	Refusal        SourceCause
	Seen           bool
	Native         bool
	LocalExecution bool
	StartedAt      time.Time
	TurnID         string
}

// ValidNativeCreation reports whether the first observed task can originate in
// this native thread. The one-second tolerance matches bounded header admission;
// a rejected first event remains decisive even if a later event looks native.
func (f OwnTaskFacts) ValidNativeCreation(created time.Time) bool {
	return f.Seen && f.Native && f.LocalExecution && !created.IsZero() && !f.StartedAt.IsZero() && !f.StartedAt.Before(created.Add(-time.Second))
}

// SourceOwnTaskFacts observes the first task within the snapshot's own logical
// boundary, even when a copied model-context prefix exceeds a header window.
type SourceOwnTaskFacts interface {
	FirstOwnTask(context.Context) (OwnTaskFacts, error)
}

// AdmissionEvidence combines facts from one opened snapshot before admission.
type AdmissionEvidence struct {
	Binding archive.CodexSourceBinding
	Task    OwnTaskFacts
}

// SourceAdmissionEvidence gathers original facts and first-owned task evidence,
// validates supplied immutable constraints, and performs one final shared check.
type SourceAdmissionEvidence interface {
	AdmissionEvidence(context.Context, SourceAdmission) (AdmissionEvidence, error)
}

// SourceRelocation names the physical rollout a snapshot of an unbound
// legacy registration read when it is not the registered locator: Codex
// renamed the rollout (into archived_sessions/, or back) and the lookup found
// it by its thread ID. The registration keeps its legacy representation and
// only follows the locator; a bound registration follows its binding instead.
type SourceRelocation interface {
	RelocatedFrom(registered SourceRef) (SourceRef, bool)
}

// RevisionEvidence identifies meaningful native evidence without teaching shared
// orchestration native record vocabulary. Ownership is checked separately.
type RevisionEvidence interface {
	MeaningfulRevisionRecord(archive.SourceBundle, int) bool
}
