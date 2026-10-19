package cli

import (
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func skillObserver(env Env, mode config.SkillEvidence) func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
	return newSkillObservationPass(env, mode).Observe
}
