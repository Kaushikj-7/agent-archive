package cli

import (
	"context"
	"errors"
	"io/fs"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/stateerrors"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// issueReasons are the errors whose text status may record as a failed
// session's reason (state.Status.SessionIssueReasons): each is a sentinel
// with fixed text, so its text holds nothing of a transcript, a path, or what
// a credential_process printed, unlike the error that wraps it. The first
// one a session's error wraps is its reason, so the more specific come
// first. Add a sentinel here when a per-session failure is only explained
// by sync's output.
var issueReasons = []error{
	state.ErrDurableStorageCapacity,
	stateerrors.ErrDurableStorageRecovery,
	state.ErrRemovalPending,
	state.ErrDeletionWorkChanged,
	state.ErrGenerationStageUnsettled,
	state.ErrGenerationStageUnsupported,
	state.ErrSessionIndexRecoveryRequired,
	state.ErrSessionIdentityConflict,
	state.ErrSubagentCandidateIncomplete,
	state.ErrSubagentCandidateConflict,
	state.ErrImportPolicyChanged,
	state.ErrSessionNotRegistered,
	state.ErrQuarantined,
	storage.ErrPublicationConflict,
	storage.ErrAtomicCatalogUnqualified,
	storage.ErrVersionedReadUnavailable,
	storage.ErrChecksumMismatch,
	storage.ErrObjectTooLarge,
	storage.ErrPreconditionFailed,
	storage.ErrNotFound,
	catalog.ErrConflict,
	catalog.ErrCommitUnknown,
	catalog.ErrAdmissionClosed,
	catalog.ErrGCRequired,
	collector.ErrNoTranscript,
	collector.ErrSubagentNotCaptured,
	transcriptio.ErrRecordTooLarge,
	transcriptio.ErrNotRegularFile,
	transcriptio.ErrChanged,
	transcriptio.ErrCleanup,
	archive.ErrSourceTooLarge,
	archive.ErrHistoryMutationPending,
	archive.ErrUnsupportedNativeImport,
	agentapi.ErrRawLimit,
	agentapi.ErrReadBudget,
	agentapi.ErrHistorySourceExcluded,
	agentapi.ErrInvalidSourceBinding,
	agentapi.ErrClosed,
	agentmeta.ErrJSONWireLimit,
	agentmeta.ErrJSONWireUnsupported,
	cursorstore.ErrRecordLimit,
	credentials.ErrMissingCredential,
	credentials.ErrUnavailable,
	local.ErrBusy,
	// The operating system's own errors, last: their text is fixed too.
	syscall.ENOSPC,
	syscall.EDQUOT,
	syscall.EMFILE,
	fs.ErrPermission,
	fs.ErrNotExist,
	context.DeadlineExceeded,
	context.Canceled,
}

// sessionIssueReason is the reason status records for one session's
// failure, classified as code: the text of the first of issueReasons it
// wraps, or "" when it wraps none, which status shows as an unrecognized
// error. Of several joined failures (collection and retention both failed),
// only those classified as code are searched, so the reason explains the
// failure status names.
func sessionIssueReason(issue error, code string) string {
	for _, part := range joinedErrors(issue) {
		if sessionIssueCode(part) != code {
			continue
		}
		for _, reason := range issueReasons {
			if errors.Is(part, reason) {
				return reason.Error()
			}
		}
	}
	return ""
}
