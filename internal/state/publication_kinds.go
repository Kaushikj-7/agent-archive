package state

// PublicationPayloadKind identifies one closed source-byte authority.
type PublicationPayloadKind string

const (
	// PublicationInline selects bytes stored inline.
	PublicationInline PublicationPayloadKind = "inline"
	// PublicationRemote selects an already retained remote reference.
	PublicationRemote PublicationPayloadKind = "remote"
	// PublicationHistoryStage selects the existing history stage.
	PublicationHistoryStage PublicationPayloadKind = "history-stage"
	// PublicationAdmissionStage remains unavailable until staged admission.
	PublicationAdmissionStage PublicationPayloadKind = "admission-stage"
)

// PreparationKind identifies the exact original authority variant.
type PreparationKind string

const (
	// PreparationCapture freezes an ordinary selecting capture.
	PreparationCapture PreparationKind = "capture"
	// PreparationPrivacyCommitted transforms an exact committed selection.
	PreparationPrivacyCommitted PreparationKind = "privacy-committed"
	// PreparationPrivacyPending transforms a retained pending candidate.
	PreparationPrivacyPending PreparationKind = "privacy-pending"
	// PreparationPrivacyPendingAbsent transforms a never-published candidate.
	PreparationPrivacyPendingAbsent PreparationKind = "privacy-pending-absent"
)

// PublicationPhase identifies the closed preparation lifecycle.
type PublicationPhase string

const (
	// PublicationPreparing has no selecting Commit.
	PublicationPreparing PublicationPhase = "preparing"
	// PublicationReady carries a complete selecting Commit.
	PublicationReady PublicationPhase = "ready"
)

// OriginalRoleKind distinguishes the exact original frozen file shape.
type OriginalRoleKind string

const (
	// OriginalSealedPending retains the exact original ready descriptor.
	OriginalSealedPending OriginalRoleKind = "sealed-pending-v1"
	// OriginalHistoryPreparing retains the exact unfinished original descriptor.
	OriginalHistoryPreparing OriginalRoleKind = "history-preparing-v1"
)

// OriginalEvidenceKind identifies the singleton original-evidence protocol.
type OriginalEvidenceKind string

// OriginalEvidence identifies the closed original-evidence envelope.
const OriginalEvidence OriginalEvidenceKind = "publication-original-evidence"

// OrdinaryMigrationMode identifies the existing semantic migration proof.
type OrdinaryMigrationMode string

const (
	// MigrationSamePhysicalCovered proves meaningful same-physical continuity.
	MigrationSamePhysicalCovered OrdinaryMigrationMode = "same-physical-covered"
	// MigrationExactPriorRetained preserves an exact distinct prior reference.
	MigrationExactPriorRetained OrdinaryMigrationMode = "exact-prior-retained"
)

// PublicationRole identifies a source's position in the complete selecting set.
type PublicationRole string

const (
	// PublicationCurrent is the exact selecting current source.
	PublicationCurrent PublicationRole = "current"
	// PublicationPreserved retains a referenced older physical revision.
	PublicationPreserved PublicationRole = "preserved"
)
