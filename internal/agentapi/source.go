package agentapi

import (
	"context"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// Mutation declares the shared rewrite policy.
type Mutation uint8

const (
	// AppendOnly blocks a source which no longer extends retained evidence.
	AppendOnly Mutation = iota
	// ReplaceableSnapshot allows replacement with a cumulative provenance gap.
	ReplaceableSnapshot
)

// SourceSemantics describes observable consistency and rewrite behavior.
type SourceSemantics struct {
	Mutation Mutation
	Provider string
}

// SourceEnvironment supplies narrow read dependencies and local database location.
type SourceEnvironment struct {
	// HistorySourceAllowed vetoes excluded physical dependencies before retention.
	HistorySourceAllowed func(string) bool
	// RequireConfinedHistory is set by capture callers. Read-only source facts
	// remain independent of capture authority and may use explicit inputs.
	RequireConfinedHistory bool

	// LegacyUnboundRegistration is set only for an already admitted ordinary
	// registration without a binding or discovery origin. It grants no creation
	// permission; the provider retains its narrow legacy compatibility checks.
	LegacyUnboundRegistration bool
	ReadBudget                *NativeReadBudget
	CodexRollouts             CodexRolloutLookup
	Files                     transcriptio.Opener
	Policy                    transcriptio.OpenPolicy
	Database                  string
}

// ReadLimits bounds raw values and each native record before filtering.
type ReadLimits struct {
	// ReadBudget optionally shares bounded JSON encoder scratch with the caller.
	ReadBudget *NativeReadBudget
	// Records and FilteredBytes optionally bound changed-history preparation.
	Records          int
	FilteredBytes    int64
	RawBytes         int64
	RecordBytes      int64
	SubagentMetadata bool
}

// SourceSignature is a bounded, provider-qualified equality token.
type SourceSignature struct {
	Version  uint8  `json:"version"`
	Provider string `json:"provider"`
	Token    string `json:"token"`
}

// SourceObservation separates presence, activity and emptiness from equality.
type SourceObservation struct {
	Signature SourceSignature
	Present   bool
	Empty     bool
	Activity  time.Time
	Size      int64
}

// SourceProvider owns native reads but never shared publication policy.
type SourceProvider interface {
	Describe(SourceRef) (SourceSemantics, error)
	OpenPass(context.Context, SourceEnvironment) (SourcePass, error)
}

// SourcePass is serial, lazy, single-owner, and terminal after Close.
type SourcePass interface {
	Signature(context.Context, SourceRef) (SourceObservation, error)
	Read(context.Context, SourceRef, ReadLimits) (SourceSnapshot, error)
	Close() error
}

// SourceSnapshot owns immutable borrowed input valid only during its pass.
type SourceSnapshot interface {
	Observation() SourceObservation
	Input() NativeInput
	Close() error
}

// FileInput exposes verified bounded reads, coverage, and post-read verification.
type FileInput = transcriptio.Input

// NativeRecordKind identifies a provider-owned record frame.
type NativeRecordKind string

// ComposerRecord and BubbleRecord are the Cursor provider's actual ordered frames.
const (
	ComposerRecord NativeRecordKind = "composer"
	BubbleRecord   NativeRecordKind = "bubble"
)

// NativeRecord borrows one immutable ordered native value until snapshot close.
type NativeRecord struct {
	Ordinal uint64
	History *archive.SourceHistory
	Kind    NativeRecordKind
	Key     string
	Raw     []byte
	Missing bool
}

// RecordInput yields provider-declared ordered borrowed frames without an export envelope.
type RecordInput interface {
	Next(context.Context) (NativeRecord, bool, error)
}

// NativeInput is an explicit union; exactly one File or Records is populated.
type NativeInput struct {
	File         FileInput
	Records      RecordInput
	SubagentMeta []byte
}

// FilterContext supplies existing native filtering observations without path access.
type FilterContext struct {
	// Filename is a physical locator observation only; it grants no path access.
	Filename  string
	StartedAt time.Time
	Limits    ReadLimits
}

// TranscriptFilter owns native filtering and retained evidence interpretation.
type TranscriptFilter interface {
	archive.Adapter
	RetainedComparator
	Filter(context.Context, NativeInput, FilterContext) (archive.FilteredTranscript, error)
	Refilter(context.Context, archive.SourceBundle, time.Time) (archive.FilteredTranscript, error)
}

// LeasedTranscriptFilter transfers filtered-output ownership to its caller.
// The caller releases only after all users of the filtered records have ended.
type LeasedTranscriptFilter interface {
	// LeasedFilterFor must attest the injected adapter; embedding must not
	// silently bypass a decorator that overrides its ordinary Filter method.
	LeasedFilterFor(archive.Adapter) bool
	FilterLeased(context.Context, NativeInput, FilterContext, *NativeReadBudget) (archive.FilteredTranscript, func(), error)
}

// LeasedTranscriptRefilter charges retained output incrementally and transfers
// ownership of rows and graph metadata through the same pass ledger.
type LeasedTranscriptRefilter interface {
	RefilterLeased(context.Context, archive.SourceBundle, time.Time, *NativeReadBudget) (archive.FilteredTranscript, func(), error)
}

// BoundedTranscriptRefilter accepts output ceilings before retained records accumulate.
// Exhaustion must return an error; partial output must never be published.
type BoundedTranscriptRefilter interface {
	RefilterBounded(context.Context, archive.SourceBundle, time.Time, ReadLimits) (archive.FilteredTranscript, error)
}

// LocalIdentityResolver owns native filename conventions and identity preference.
// filename is an observation only; implementations must not open it or inspect
// raw ownership SessionIDs. The returned identity must come from safe facts.
type LocalIdentityResolver interface {
	LocalIdentity(archive.FilteredTranscript, string) archive.NativeSessionIdentity
}

// SourcesLookup is the narrow source/filter binding injected into shared readers.
type SourcesLookup interface {
	LookupSources(string) (SourceProvider, TranscriptFilter, bool)
}

// SourceSweeper performs content-free cleanup of abandoned provider resources.
type SourceSweeper interface{ SweepSources() }

// ActivityProvider batches cheap ordering observations without snapshotting or filtering.
type ActivityProvider interface {
	Activities(context.Context, SourceEnvironment, []SourceRef) (map[SourceRef]time.Time, error)
}

// RecoveryReadBudget bounds metadata and raw payload allocations across one
// evidence epoch. It does not measure SQLite page I/O or filesystem stat calls.
type RecoveryReadBudget interface {
	RemainingRows() int
	RemainingBytes() int64
	Charge(int, int64) error
}

// RecoverySourcePass supplies optional bounded in-place recovery evidence.
// It never grants admission identity or changes ordinary Signature/Read behavior.
type RecoverySourcePass interface {
	ReadRecovery(context.Context, SourceRef, ReadLimits, RecoveryReadBudget) (SourceSnapshot, error)
}

// CodexRegisteredRolloutLookup observes a legacy registered ID that does not
// have modern native UUID layout. The implementation checks existing ownership;
// this port never creates or authorizes a registration.
type CodexRegisteredRolloutLookup interface {
	RegisteredThread(context.Context, string) (CodexRolloutSet, error)
}

// ErrHistorySourceExcluded refuses complete import when a required source is excluded.
var ErrHistorySourceExcluded = errors.New("required history source is excluded")
