package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
)

// preSetupLookbackDays is the lookback setup imports (setupImportSince),
// in days, as the status line says it.
const preSetupLookbackDays = 7

// preSetupSessions is what status says about an app's sessions from the 7
// days before discovery's start floor that discovery refused for starting
// before it (start_not_authorized) and that are not archived: people who
// set up before setup imported the last 7 days (setupImportSince) never had
// them imported. It holds no session identities.
type preSetupSessions struct {
	// Sessions counts them: top-level sessions only, as backfill imports a
	// subagent with its parent.
	Sessions int `json:"sessions"`
	// Since is the local day backfill --since takes to import them: the day
	// 7 days before StartFloor. They started on or after its midnight
	// (WindowStart) and before StartFloor.
	Since       string    `json:"since"`
	WindowStart time.Time `json:"window_start"`
	// StartFloor is the earliest start discovery admits for the app
	// (config.DiscoveryStartFloor).
	StartFloor time.Time `json:"start_floor"`
	Command    string    `json:"command"`
}

// readPreSetupSessions fills in each discovery app's pre-setup sessions
// from the record the discovery scans keep (discovery.ReadRefusedStarts),
// less those archived or removed since, so status adds no scan of its own.
func readPreSetupSessions(view *statusView, cfg config.Config, home string, store *state.Store, regs []archive.SessionRegistration, now time.Time) {
	if cfg.Paused || !cfg.Archive.Enabled {
		// backfill refuses while capture is paused.
		return
	}
	refused, err := discovery.ReadRefusedStarts(home)
	if err != nil {
		view.Warnings = append(view.Warnings, unreadableWarning(filepath.Join(home, "discovery-refused-starts.json"), err, "The next discovery scan replaces it."))
		return
	}
	if len(refused) == 0 {
		return
	}
	archived := map[string]bool{}
	for _, reg := range regs {
		archived[reg.Harness.Name+"\x00"+reg.NativeSessionID] = true
	}
	for i := range view.Apps {
		app := &view.Apps[i]
		if app.Discovery == nil || !app.Discovery.Enabled {
			continue
		}
		app.PreSetupSessions = preSetupSessionsFor(app.Name, cfg, refused, archived, store, now)
	}
}

func preSetupSessionsFor(name string, cfg config.Config, refused []discovery.RefusedStart, archived map[string]bool, store *state.Store, now time.Time) *preSetupSessions {
	floor, ok := cfg.DiscoveryStartFloor(name)
	if !ok {
		return nil
	}
	day := floor.AddDate(0, 0, -preSetupLookbackDays).In(now.Location())
	windowStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, now.Location())
	count := 0
	for _, session := range refused {
		if session.Agent != name || session.StartedAt.Before(windowStart) || !session.StartedAt.Before(floor) || archived[session.Agent+"\x00"+session.NativeSessionID] {
			continue
		}
		if _, removed, err := store.Removal(session.Agent, session.NativeSessionID); err != nil || removed {
			continue
		}
		count++
	}
	if count == 0 {
		return nil
	}
	since := day.Format("2006-01-02")
	return &preSetupSessions{Sessions: count, Since: since, WindowStart: windowStart, StartFloor: floor, Command: "agent-archive backfill --since " + since}
}

// preSetupNotes are the lines under an app for its pre-setup sessions.
func (sc statusScreen) preSetupNotes(app appStatus) []statusNote {
	pre := app.PreSetupSessions
	if pre == nil {
		return nil
	}
	sessions, verb, them := plural(pre.Sessions, appName(app.Name)+" session"), "were", "them"
	if pre.Sessions == 1 {
		verb, them = "was", "it"
	}
	return []statusNote{
		{sc.style.warnMark(), fmt.Sprintf("%s from the %d days before setup %s not captured.", sessions, preSetupLookbackDays, verb)},
		{" ", fmt.Sprintf("Import %s with %s.", them, sc.style.cmd(pre.Command))},
	}
}
