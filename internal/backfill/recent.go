package backfill

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// Recent narrows a plan built without date filters to what an import with
// --since since (a local date, YYYY-MM-DD; sinceArg is the value as typed when
// it is relative, such as 7d) would import, and keeps only sessions whose
// project is already configured and included: the import adds no project,
// kept-out folder, or app, so a recovery or folder rule that would add a root
// leaves its session out. older counts the sessions the full plan imports
// in included projects that started before since.
//
// Setup uses it so one discovery gives both its recent import and the count
// of older history left to backfill. The narrowed plan records since in its
// filters, so its import record, history, and undo read as backfill --since.
func (p Plan) Recent(cfg config.Config, since, sinceArg string) (recent Plan, older int, err error) {
	if p.Filters.Since != "" || p.Filters.Until != "" {
		return Plan{}, 0, errors.New("the plan is already limited by date")
	}
	recent = p
	recent.Filters.Since, recent.Filters.SinceArg = since, sinceArg
	if _, err := time.Parse(dateLayout, since); err != nil {
		return Plan{}, 0, fmt.Errorf("since must be a date (YYYY-MM-DD), not %q", since)
	}
	start, _ := dateRange(recent.Filters, p.GeneratedAt.Location())
	recent.Candidates = slices.Clone(p.Candidates)
	// Only a project the import adds has folders kept out of it.
	recent.nested = nil
	for i := range recent.Candidates {
		c := &recent.Candidates[i]
		if c.Skip != "" {
			continue
		}
		if !c.ProjectIncluded || !configuredIncluded(cfg, c.ProjectRoot) {
			c.Skip = SkipFilteredOut
			continue
		}
		if c.StartedAt.Before(start) {
			c.Skip = SkipFilteredOut
			older++
			continue
		}
		if c.ProjectResolution != nil {
			// The recovery policy is bound again below, for the narrowed
			// plan; the full plan's resolution is left as it was.
			resolution := *c.ProjectResolution
			c.ProjectResolution = &resolution
		}
	}
	if err := bindRecoveryPolicy(cfg, &recent); err != nil {
		return Plan{}, 0, err
	}
	return recent, older, nil
}

// configuredIncluded reports whether root is a configured, included project.
func configuredIncluded(cfg config.Config, root string) bool {
	for _, project := range cfg.Archive.Projects {
		if project.Included && project.Root == root {
			return true
		}
	}
	return false
}
