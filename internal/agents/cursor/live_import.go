package cursor

import (
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture"
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture/producer"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// CaptureLiveRequest invokes only the fixed live-image native capture backend.
func CaptureLiveRequest(request *nativefacts.LiveCaptureRequest) (*nativecapture.LiveProof, error) {
	return producer.CaptureLiveCursor(request)
}
