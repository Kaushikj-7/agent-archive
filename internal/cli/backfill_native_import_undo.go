package cli

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// The caller already holds setup.lock. This route never enters generic removal,
// storage access, retention restoration or project/capture reconfiguration.
func (cmd undoCommand) runNativeImportUndo() int {
	fail := func(err error) int { terminal.Printf(cmd.stderr, "agent-archive: backfill undo: %v\n", err); return 1 }
	plan, err := backfill.PlanNativeImportUndo(context.Background(), state.OpenReadOnly(cmd.home), cmd.batch.ID, cmd.project)
	if err != nil {
		return fail(err)
	}
	defer plan.Close()
	sessions := plan.Sessions()
	absent, independent := plan.Unowned()
	terminal.Printf(cmd.stdout, "Import %s: detach %s.\n", cmd.batch.ID, plural(len(sessions), "exact historical registration membership"))
	for _, session := range sessions {
		status := "attached"
		if session.Detached {
			status = "already detached"
		}
		terminal.Printf(cmd.stdout, "  %s  project %s  %s\n", archive.DisplayLine(session.ArchiveID), archive.DisplayLine(session.ProjectID), status)
	}
	terminal.Printf(cmd.stdout, "%s without an admitted owner; %s preserved.\n", plural(absent, "selected session"), plural(independent, "independent owner"))
	terminal.Println(cmd.stdout, "This stops further registration by this batch. Stored sources, published objects, native files, later requests and configuration are preserved.")
	if !cmd.yes && !cmd.env.interactive(cmd.stdin) {
		terminal.Println(cmd.stderr, "agent-archive: backfill undo: confirming an undo needs a terminal. Nothing was changed. Run again with --yes to undo without asking.")
		return 1
	}
	if !cmd.yes {
		p := newPrompter(cmd.stdin, cmd.stdout)
		defer p.close()
		confirmed, err := p.guidedYesNo("Detach these import memberships?")
		if err != nil {
			return fail(err)
		}
		if !confirmed {
			terminal.Println(cmd.stdout, "Cancelled. Nothing was changed.")
			return 0
		}
	}
	if err := cmd.env.checkpoint("undoing"); err != nil {
		return fail(err)
	}
	count, err := plan.Apply(context.Background(), cmd.env.now())
	if err != nil {
		terminal.Printf(cmd.stderr, "agent-archive: backfill undo: detached %s before stopping: %v. Run undo again to resume.\n", plural(count, "membership"), err)
		return 1
	}
	terminal.Printf(cmd.stdout, "Detached %s. Import %s is stopped.\n", plural(count, "import membership"), cmd.batch.ID)
	return 0
}
