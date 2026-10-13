package cursor

import (
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture"
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture/producer"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// SettledImportFacts retains the exact persisted neutral facts codec.
type SettledImportFacts = nativefacts.SettledImportFacts

// SettledImportEvidence is the opaque native capture proof.
type SettledImportEvidence = nativecapture.Proof

// CaptureSettledRequest invokes only the fixed actual capture backend.
func CaptureSettledRequest(request *nativefacts.CaptureRequest) (*nativecapture.Proof, error) {
	return producer.CaptureSettledCursor(request)
}
