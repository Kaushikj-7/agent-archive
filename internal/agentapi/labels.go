package agentapi

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// LabelLookupMode identifies the selected metadata transport.
type LabelLookupMode string

const (
	// LabelLookupFiles selects settled local metadata.
	LabelLookupFiles LabelLookupMode = "files"
	// LabelLookupNative explicitly selects native host startup.
	LabelLookupNative LabelLookupMode = "native"
)

// LabelEnvironment supplies approved native homes without granting admission.
type LabelEnvironment struct {
	Mode             LabelLookupMode `json:"mode"`
	ProviderContract string          `json:"provider_contract"`
	Homes            []string        `json:"homes"`
	// VerifiedLegacyStorageHomes carries positive per-home proof that the producing
	// legacy session used default SQLite placement. Local config absence is not proof.
	VerifiedLegacyStorageHomes []string `json:"verified_legacy_storage_homes"`
	ExternalSQLite             bool     `json:"external_sqlite"`
	// CursorDatabase is Cursor's state.vscdb, which the collector supplies
	// from its own Cursor database setting for each lookup. It is not part of
	// a label's cached scope, so supplying it leaves other harnesses' cached
	// observations in place; "" means no Cursor database is known.
	CursorDatabase string `json:"-"`
}

// LabelRequest identifies an already admitted retained source.
type LabelRequest struct {
	Registration archive.SessionRegistration
	Bundle       archive.SourceBundle
	Context      LabelContext
}

// LabelContext is durable, content-free interpretation of an owning retained source.
type LabelContext struct {
	APICompatible   bool   `json:"api_compatible,omitempty"`
	NativeID        string `json:"native_id"`
	Ordinary        bool   `json:"ordinary"`
	Legacy          bool   `json:"legacy,omitempty"`
	Producer        string `json:"producer,omitempty"`
	PreviewDigest   string `json:"preview_digest,omitempty"`
	PreviewComplete bool   `json:"preview_complete,omitempty"`
	Contract        string `json:"contract"`
}

// LabelContextProvider derives narrow lookup facts once per retained source revision.
type LabelContextProvider interface {
	LabelContext(archive.SourceBundle) LabelContext
	// LabelContextVersion identifies the provider interpretation without performing I/O.
	LabelContextVersion() string
}

// RegistrationLabelContextProvider derives lookup context from the admitted
// registration alone, for a provider whose lookup needs nothing of the
// retained source: the collector then asks without loading the publication,
// so a session too large to load for a context is still named.
type RegistrationLabelContextProvider interface {
	RegistrationLabelContext(archive.SessionRegistration) (LabelContext, bool)
}

// LabelProvider resolves bounded metadata for existing sources without enumeration.
type LabelProvider interface {
	LookupLabels(context.Context, LabelEnvironment, []LabelRequest) map[string]archive.SessionLabel
}

// LabelsLookup projects only the optional native label capability.
type LabelsLookup interface {
	LookupLabels(string) (LabelProvider, bool)
}

// LabelRequestGrouper identifies a provider's shared work unit without I/O.
// The opaque key is bounded to 256 bytes and is hashed by the collector before
// persistence; an empty key defers to ID priority.
type LabelRequestGrouper interface {
	LabelRequestGroup(LabelEnvironment, LabelRequest) string
}

// LabelTransport exchanges bounded newline messages with an injected native host.
// Implementations honor cancellation and close/terminate within a bounded grace.
type LabelTransport interface {
	WriteLine(context.Context, []byte) error
	ReadLine(context.Context) ([]byte, error)
	Close() error
}

// LabelHostFactory starts a host only for an approved, explicitly selected home.
type LabelHostFactory func(context.Context, string) (LabelTransport, error)

// LabelError is a fixed diagnostic reason; raw host text is never an error.
type LabelError uint8

const (
	// ErrLabelHostMissing means executable discovery failed.
	ErrLabelHostMissing LabelError = iota + 1
	// ErrLabelHostUnavailable means startup or stream setup failed.
	ErrLabelHostUnavailable
	// ErrLabelProtocolUnavailable means bounded native metadata could not be decoded.
	ErrLabelProtocolUnavailable
	// ErrLabelBudgetExceeded means a stream exceeded its permitted volume.
	ErrLabelBudgetExceeded
	// ErrLabelHostClosed means cancellation or shutdown closed the host.
	ErrLabelHostClosed
	// ErrLabelRequestRejected means an unsolicited or invalid request was refused.
	ErrLabelRequestRejected
	// ErrLabelTerminationUnavailable means reap did not complete within the grace.
	ErrLabelTerminationUnavailable
)

// Error returns only fixed, content-free diagnostic text.
func (f LabelError) Error() string {
	switch f {
	case ErrLabelHostMissing:
		return "native label executable unavailable"
	case ErrLabelHostUnavailable:
		return "native label host unavailable"
	case ErrLabelProtocolUnavailable:
		return "native label response unavailable"
	case ErrLabelBudgetExceeded:
		return "native label stream budget exceeded"
	case ErrLabelHostClosed:
		return "native label host closed"
	case ErrLabelRequestRejected:
		return "native label request rejected"
	case ErrLabelTerminationUnavailable:
		return "native label termination unavailable"
	default:
		return "native label lookup unavailable"
	}
}
