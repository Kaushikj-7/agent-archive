package cli

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"time"
)

func readNativeStartupStatus(view *statusView, cfg config.Config, home string, owners []archive.SessionRegistration, at time.Time) {
	for i := range view.Apps {
		if view.Apps[i].Name != "muse" {
			continue
		}
		observation, err := capture.ReadNativeStartupObservation(context.Background(), home, cfg, owners, at)
		if err != nil {
			observation.Complete = false
			view.Warnings = append(view.Warnings, "Queued Muse startup observations could not be completely read.")
		}
		if observation.Pending > 0 || !observation.Complete {
			view.Apps[i].NativeStartup = &observation
		}
		if observation.Pending > 0 {
			view.Apps[i].HookObserved = true
		}
	}
}
