package cursor

import (
	"context"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// SettledImportFacts retains the exact persisted neutral facts codec.
type SettledImportFacts = nativefacts.SettledImportFacts

// SettledImportEvidence is the opaque native capture proof.
type SettledImportEvidence = nativecapture.Proof

// CaptureSettledRequest invokes only the fixed actual capture backend.
func CaptureSettledRequest(request *nativefacts.CaptureRequest) (*nativecapture.Proof, error) {
	return nativecapture.CaptureSettledCursor(request)
}

// CaptureSettledImport preserves the legacy capture-only API for callers that
// do not save a stage. Saving requires a separately retained host request.
func CaptureSettledImport(ctx context.Context, database string, reg archive.SessionRegistration, reviewed agentapi.DatabaseChat, at time.Time, budget *agentapi.NativeReadBudget) (*SettledImportEvidence, error) {
	request, err := nativefacts.FreezeCaptureRequest(ctx, reg, reviewed, database, at, nativefacts.CapturePolicy{FilterVersion: archive.FilterVersion, SkillEvidence: nativefacts.CaptureSkillNone}, budget)
	if err != nil {
		return nil, err
	}
	proof, err := CaptureSettledRequest(request)
	if err != nil {
		request.Close()
		return nil, err
	}
	// Compatibility proof owns its request until Close; there is no stage expectation getter.
	return proof, nil
}

func validateSettledMetadata(ctx context.Context, key string, raw []byte) error {
	return nativecodec.ValidateSettledCursorMetadata(ctx, key, raw)
}

func validateSettledRoster(ctx context.Context, raw []byte) error {
	return nativecodec.ValidateSettledCursorRoster(ctx, raw)
}
