package backfill

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// CheckClock refuses an admission time earlier than the plan, or than the
// boundaries the import's sessions are admitted across: the current
// destination, and each configured project they land in. A clock set back
// would otherwise admit sessions that start after their admission, or that
// the configuration then rejects, silently.
func CheckClock(cfg config.Config, p Plan, admittedAt time.Time) error {
	// The plan judged start_in_future by its own time; an admission before
	// it could admit a session that starts after its admission.
	if !p.GeneratedAt.IsZero() && admittedAt.Before(p.GeneratedAt) {
		return fmt.Errorf("the clock reads %s, earlier than when this plan was made (%s); check the clock and run backfill again",
			admittedAt.Format(time.RFC3339), p.GeneratedAt.Format(time.RFC3339))
	}
	if !cfg.DestinationSince.IsZero() && admittedAt.Before(cfg.DestinationSince) {
		return fmt.Errorf("the clock reads %s, earlier than when this storage destination was set up (%s); check the clock and run backfill again",
			admittedAt.Format(time.RFC3339), cfg.DestinationSince.Format(time.RFC3339))
	}
	targets := map[string]bool{}
	for _, c := range p.Imported() {
		targets[c.ProjectRoot] = true
	}
	for _, project := range cfg.Archive.Projects {
		if project.Included && targets[project.Root] && admittedAt.Before(project.ActivatedAt) {
			return fmt.Errorf("the clock reads %s, earlier than when %s was added (%s); check the clock and run backfill again",
				admittedAt.Format(time.RFC3339), p.display(project.Root), project.ActivatedAt.Format(time.RFC3339))
		}
	}
	return nil
}

// RetentionChange is an import's change to the archive-wide retention: From
// before the import, To after it. An import only ever raises retention (see
// ApplyToConfig); undo puts From back while the retention is still To.
type RetentionChange struct {
	From int `json:"from"`
	To   int `json:"to"`
	// Restored is set once an undo has put From back.
	Restored bool `json:"restored,omitempty"`
}

// ConfigChanges is everything ApplyToConfig changed in the configuration.
// The batch records all of it (Batch.AddChanges), so undo can reverse it;
// TestImportConfigChangesAreRecordedAndUndone holds every configuration
// field an import changes to that.
type ConfigChanges struct {
	// ProjectIDs are the projects added, included.
	ProjectIDs []string
	// Apps are the apps added to ImportedHarnesses.
	Apps []string
	// Retention is set when the plan raised the archive-wide retention.
	Retention *RetentionChange
	// KeptOut are the projects added excluded: folders inside an added
	// project that it would otherwise capture (see nested.go).
	KeptOut []string
}

// ErrRetentionShortened means a plan would lower the archive-wide retention,
// or turn it on while it is off. An import never does: retention applies to
// every session in the archive, and a shorter one deletes sessions already
// archived, hook-captured ones too. Setup is where it is shortened.
var ErrRetentionShortened = errors.New("an import only raises retention; shorten it in setup")

// ApplyToConfig adds what the plan needs to cfg: every project it imports
// into that is not configured yet, included and activated at admittedAt;
// the folders inside those that the plan keeps out, excluded; every app it
// imports that has no hooks to ImportedHarnesses; and the plan's retention
// when it is longer than cfg's. It returns what it changed.
// cfg is left as it was when it returns an error.
func ApplyToConfig(cfg *config.Config, p Plan, admittedAt time.Time) (ConfigChanges, error) {
	var retention *RetentionChange
	if err := config.ValidateRetentionDays(p.RetentionDays); err != nil {
		return ConfigChanges{}, err
	}
	if p.RetentionDays != cfg.RetentionDays {
		if config.RetentionShorter(p.RetentionDays, cfg.RetentionDays) {
			return ConfigChanges{}, fmt.Errorf("%w (from %s to %s)", ErrRetentionShortened, config.RetentionLabel(cfg.RetentionDays), config.RetentionLabel(p.RetentionDays))
		}
		retention = &RetentionChange{From: cfg.RetentionDays, To: p.RetentionDays}
		cfg.RetentionDays = p.RetentionDays
		if p.RetentionDays == 0 {
			cfg.RetentionProtection = true
		}
	}
	changes := ConfigChanges{ProjectIDs: []string{}, Apps: []string{}, KeptOut: []string{}, Retention: retention}
	for _, c := range p.Imported() {
		if c.ProjectIncluded || slices.ContainsFunc(cfg.Archive.Projects, func(existing archive.ProjectActivation) bool { return existing.Root == c.ProjectRoot }) {
			continue
		}
		id := archive.ProjectID(c.ProjectRoot)
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{
			ProjectID: id, Root: c.ProjectRoot, ActivatedAt: admittedAt.UTC(), Included: true,
		})
		changes.ProjectIDs = append(changes.ProjectIDs, id)
	}
	// The folders inside an added plain folder stay out of capture, as they
	// were: an excluded project is the nearest configured one for them.
	for _, s := range p.Projects() {
		if !slices.Contains(changes.ProjectIDs, archive.ProjectID(s.Root)) {
			continue
		}
		for _, folder := range s.KeptOut {
			if slices.ContainsFunc(cfg.Archive.Projects, func(existing archive.ProjectActivation) bool { return existing.Root == folder }) {
				continue
			}
			id := archive.ProjectID(folder)
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{
				ProjectID: id, Root: folder, ActivatedAt: admittedAt.UTC(), Included: false,
			})
			changes.KeptOut = append(changes.KeptOut, id)
		}
	}
	for _, app := range p.AppsWithoutHooks() {
		if !slices.Contains(cfg.ImportedHarnesses, app) {
			cfg.ImportedHarnesses = append(cfg.ImportedHarnesses, app)
			changes.Apps = append(changes.Apps, app)
		}
	}
	return changes, nil
}
