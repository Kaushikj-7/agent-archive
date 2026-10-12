package cli

import (
	"fmt"
	"io"
	"slices"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type historicalProjectAction string

const (
	historicalProjectList    historicalProjectAction = "list"
	historicalProjectRename  historicalProjectAction = "rename"
	historicalProjectEnable  historicalProjectAction = "enable"
	historicalProjectDisable historicalProjectAction = "disable"
)

func runHistoricalProjects(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("historical-project", stderr)
	if !fs.parse(args) {
		return 2
	}
	args = fs.Args()
	fail := func(message string) int {
		terminal.Printf(stderr, "agent-archive: historical-project: %s\n", message)
		return 1
	}
	if !validHistoricalArguments(args) {
		terminal.Println(stderr, "agent-archive: historical-project: use list, rename ID LABEL, enable ID, or disable ID with an exact project ID and bounded plain-text label")
		return 2
	}
	action := historicalProjectAction(args[0])
	home, err := env.readHome()
	if err != nil {
		return fail(err.Error())
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fail(err.Error())
	}
	if !found {
		return fail(errNotSetUp.Error())
	}
	if action == historicalProjectList {
		for _, project := range cfg.HistoricalProjects {
			enabled, recorded := project.HistoricalPermission(cfg.DestinationID())
			state := "no permission"
			if recorded {
				state = "disabled"
				if enabled {
					state = "enabled"
				}
			}
			terminal.Printf(stdout, "%s  %s  %s (import only, current destination)\n", project.ID, project.Label, state)
		}
		return 0
	}
	if setupjournal.TransactionPending(home) {
		return fail(recoveryPending(home))
	}
	releaseSetup, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return fail("setup or another settings operation is running; retry")
	}
	defer releaseSetup()
	releaseCollector, err := lockCollector(home, "historical-project", env.now())
	if err != nil {
		return fail("collector is running; retry")
	}
	defer releaseCollector()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return fail("a hook is finishing; retry")
	}
	defer releaseHooks()
	cfg, found, err = config.Load(home)
	if err != nil {
		return fail(err.Error())
	}
	if !found {
		return fail(errNotSetUp.Error())
	}
	if setupjournal.TransactionPending(home) {
		return fail(recoveryPending(home))
	}
	index := slices.IndexFunc(cfg.HistoricalProjects, func(p config.HistoricalProject) bool { return p.ID == args[1] })
	if index < 0 {
		return fail("unknown historical project ID; introduce it in a confirmed mapping-file import")
	}
	project := &cfg.HistoricalProjects[index]
	if action == historicalProjectRename {
		project.Label = args[2]
	} else {
		permission := slices.IndexFunc(project.Permissions, func(p config.HistoricalProjectPermission) bool { return p.DestinationID == cfg.DestinationID() })
		if permission < 0 {
			return fail("this project has no permission in the current destination; review a mapping-file import to authorize it here")
		}
		project.Permissions[permission].Enabled = action == historicalProjectEnable
	}
	cfg.HistoricalProjectProtection = true
	if err := config.Save(home, cfg); err != nil {
		return fail(err.Error())
	}
	terminal.Println(stdout, fmt.Sprintf("Historical project %s: %s. Import-only ownership remains bound to its destination.", project.ID, action))
	return 0
}

func validHistoricalArguments(args []string) bool {
	if len(args) == 0 {
		return false
	}
	action := historicalProjectAction(args[0])
	count := 2
	switch action {
	case historicalProjectList:
		count = 1
	case historicalProjectRename:
		count = 3
	case historicalProjectEnable, historicalProjectDisable:
	default:
		return false
	}
	return len(args) == count && (action == historicalProjectList || archive.IsHistoricalProjectID(args[1])) && (action != historicalProjectRename || archive.ValidHistoricalProjectLabel(args[2]))
}
