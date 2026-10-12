package backfill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Batch is one confirmed import, kept as imports/<id>.json under the archive
// home. It is local bookkeeping and is never uploaded. It holds no native ID
// and no path: projects are named by project ID and sessions by archive
// session ID. Registrations point back to it through ImportBatch.
type Batch struct {
	// ID is <local date>-<n>, n counting that day's imports from 1.
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	// CompletedAt is set once every session of the confirmed plan has been
	// registered or skipped. An import without it was interrupted; the next
	// run with the same filters continues it.
	CompletedAt *time.Time   `json:"completed_at,omitempty"`
	Filters     BatchFilters `json:"filters"`
	// DestinationID names the bucket the import was confirmed for.
	DestinationID string `json:"destination_id"`
	// ProjectsAdded are the project IDs the import added to the
	// configuration; projects that were already included are not listed.
	ProjectsAdded []string `json:"projects_added"`
	// AppsAdded are the apps the import added to ImportedHarnesses.
	AppsAdded []string `json:"apps_added"`
	// ProjectsKeptOut are the projects the import added excluded: folders
	// inside a plain folder it added (repositories, app folders) that the
	// new project would otherwise capture. Undo removes each again once
	// nothing included contains it.
	ProjectsKeptOut []string `json:"projects_kept_out,omitempty"`
	// Sessions are the archive session IDs the import registered.
	Sessions []string `json:"sessions"`
	// StagedSessions records selected durable evidence, independently of
	// registration and remotely verified publication. It contains no locators.
	StagedSessions []string `json:"staged_sessions,omitempty"`
	// Subagents are the archive session IDs given to the imported sessions'
	// subagent transcripts. Each registers when the collector validates it.
	Subagents []string `json:"subagents"`
	// Retention is set when the import raised the archive-wide retention
	// (the prompt's edit): undo restores the earlier value.
	Retention *RetentionChange `json:"retention,omitempty"`
	// UndoneAt is when `backfill undo` last started removing the import. It
	// is written before anything is removed, so an import undone even in
	// part is never continued; history says whether anything of it is left.
	UndoneAt *time.Time `json:"undone_at,omitempty"`
	// ProjectsExcluded are the projects this import's undo has excluded. No
	// undo, of this import or any other, excludes one of them again: once an
	// undo has excluded a project, only setup can include it again, and a
	// project setup included stays included.
	ProjectsExcluded []string `json:"projects_excluded,omitempty"`
	// ProjectsKept are projects undo left included although this import
	// added them (or took them over from an earlier undo that kept them),
	// because another import's sessions were still there. The undo of the
	// last of those imports with sessions there excludes them (see
	// PlanUndo).
	ProjectsKept []string `json:"projects_kept,omitempty"`
	// ProjectsKeptFor names, for each project in ProjectsKept, the imports
	// that still had sessions there when undo kept it: only their undos take
	// it over. A batch written before this field has no entry, and then any
	// import with sessions there takes it over.
	ProjectsKeptFor map[string][]string `json:"projects_kept_for,omitempty"`
}

// BatchFilters are the filters and --include-* flags a batch was run with.
// A --project directory is recorded by its project ID.
type BatchFilters struct {
	MappingDigest  string   `json:"mapping_digest,omitempty"`
	Harnesses      []string `json:"harnesses"`
	ProjectIDs     []string `json:"project_ids"`
	Since          string   `json:"since,omitempty"`
	Until          string   `json:"until,omitempty"`
	SinceArg       string   `json:"since_arg,omitempty"`
	UntilArg       string   `json:"until_arg,omitempty"`
	IncludeHome    bool     `json:"include_home"`
	IncludeTemp    bool     `json:"include_temp"`
	IncludeRemoved bool     `json:"include_removed"`
}

// BatchFilters records the plan's filters without paths. A --project
// directory is named by the project ID of its resolved path, as the plan
// compared it, so the same folder typed two ways continues the same batch.
func (p Plan) BatchFilters() BatchFilters {
	f := p.Filters
	out := BatchFilters{
		Harnesses: []string{}, ProjectIDs: []string{}, MappingDigest: f.mappingDigest(),
		Since: f.Since, Until: f.Until, SinceArg: f.SinceArg, UntilArg: f.UntilArg,
		IncludeHome: f.IncludeHome, IncludeTemp: f.IncludeTemp, IncludeRemoved: f.IncludeRemoved,
	}
	for _, h := range f.Harnesses {
		out.Harnesses = addUnique(out.Harnesses, archive.CanonicalHarness(h))
	}
	sort.Strings(out.Harnesses)
	for _, dir := range p.projectFilter {
		out.ProjectIDs = addUnique(out.ProjectIDs, archive.ProjectID(dir))
	}
	sort.Strings(out.ProjectIDs)
	return out
}

// Reconcile rebuilds the batch's sessions from the local store, which is the
// source of truth: every registration carrying the batch's ID, and every
// imported subagent candidate of those sessions. A crash inside a
// registration hold registers sessions the batch file never heard of.
func (b *Batch) Reconcile(store *state.Store) error {
	regs, err := store.LoadRegistrations()
	if err != nil {
		return err
	}
	if err := b.reconcileStagedSources(store); err != nil {
		return err
	}
	parents := map[string]bool{}
	for _, reg := range regs {
		if !reg.InBatch(b.ID) {
			continue
		}
		if reg.IsChild() {
			b.Subagents = addUnique(b.Subagents, reg.ArchiveSessionID)
		} else {
			b.Sessions = addUnique(b.Sessions, reg.ArchiveSessionID)
			parents[reg.ArchiveSessionID] = true
		}
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if parents[c.ParentArchiveSessionID] && c.Origin == archive.SessionOriginImport {
			b.Subagents = addUnique(b.Subagents, c.ArchiveSessionID)
		}
	}
	return nil
}

// Matches reports whether a run with these filters and destination
// continues the batch.
func (b *Batch) Matches(filters BatchFilters, destinationID string) bool {
	return b.Filters.equal(filters) && b.DestinationID == destinationID
}

// Continues reports whether a run with these filters and destination
// finishes the batch: it was interrupted, not undone since, and matches.
func (b *Batch) Continues(filters BatchFilters, destinationID string) bool {
	return b.CompletedAt == nil && b.UndoneAt == nil && b.Matches(filters, destinationID)
}

func (f BatchFilters) equal(o BatchFilters) bool {
	return f.MappingDigest == o.MappingDigest && slices.Equal(f.Harnesses, o.Harnesses) && slices.Equal(f.ProjectIDs, o.ProjectIDs) &&
		sameBound(f.Since, f.SinceArg, o.Since, o.SinceArg) && sameBound(f.Until, f.UntilArg, o.Until, o.UntilArg) &&
		f.IncludeHome == o.IncludeHome && f.IncludeTemp == o.IncludeTemp && f.IncludeRemoved == o.IncludeRemoved
}

// sameBound reports whether two runs' --since (or --until) values match. Two
// relative values match when typed alike, whichever day they name now, so
// `--since 30d` continues yesterday's interrupted `--since 30d`; otherwise
// the days they name are compared, as for a batch that recorded none.
func sameBound(day, arg, otherDay, otherArg string) bool {
	if arg != "" && otherArg != "" {
		return arg == otherArg
	}
	return day == otherDay
}

// Flags are the options a run with these filters is typed with, for
// telling the person how to continue an import: each --project is named by
// the configured root with project, the plan's own spelling; ok is false
// when one of them is no longer configured.
func (f BatchFilters) Flags(p Plan, projectRoot func(id string) (string, bool)) (flags string, ok bool) {
	if f.MappingDigest != "" && f.MappingDigest != p.Filters.mappingDigest() {
		return "", false
	}
	var out []string
	if f.MappingDigest != "" {
		if p.Filters.BulkMappings {
			return "", false // Bulk input stays private; rerun with the same reviewed assignments.
		} else {
			keys := make([]string, 0, len(p.Filters.ProjectMappings))
			for old := range p.Filters.ProjectMappings {
				keys = append(keys, old)
			}
			sort.Strings(keys)
			for _, old := range keys {
				out = append(out, "--map-project "+shellWord(old+"="+p.Filters.ProjectMappings[old]))
			}
		}
	}
	for _, h := range f.Harnesses {
		out = append(out, "--harness "+h)
	}
	for _, id := range f.ProjectIDs {
		root, found := projectRoot(id)
		if !found {
			return "", false
		}
		out = append(out, "--project "+shellWord(p.display(root)))
	}
	for _, bound := range []struct {
		flag string
		day  string
		arg  string
	}{{"--since", f.Since, f.SinceArg}, {"--until", f.Until, f.UntilArg}} {
		switch {
		case bound.arg != "":
			out = append(out, bound.flag+" "+shellWord(bound.arg))
		case bound.day != "":
			out = append(out, bound.flag+" "+bound.day)
		}
	}
	for _, include := range []struct {
		flag string
		set  bool
	}{{"--include-home", f.IncludeHome}, {"--include-temp", f.IncludeTemp}, {"--include-removed", f.IncludeRemoved}} {
		if include.set {
			out = append(out, include.flag)
		}
	}
	return strings.Join(out, " "), true
}

// shellWord quotes s for a shell when it holds anything but plain path
// characters.
func shellWord(s string) string {
	for _, c := range s {
		if !plainShellRune(c) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// plainShellRune reports whether c needs no quoting in a shell word.
func plainShellRune(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.ContainsRune("~/._-+:@", c)
}

func batchDir(home string) string { return filepath.Join(home, "imports") }

// batchIDPattern is the form OpenBatch gives an import ID: its local date
// and the day's count from 1.
var batchIDPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[1-9][0-9]{0,5}$`)

// ValidBatchID reports whether id has the form OpenBatch gives import IDs.
// An empty ID never does.
func ValidBatchID(id string) bool { return batchIDPattern.MatchString(id) }

// validate checks what LoadBatches relies on in a batch read from the file
// named stem.json: an ID of the form OpenBatch gives, that file's own name,
// and a start time.
func (b *Batch) validate(stem string) error {
	switch {
	case !ValidBatchID(b.ID):
		return errors.New("it has no valid import ID")
	case b.ID != stem:
		return fmt.Errorf("its import ID %q does not match its file name", b.ID)
	case b.StartedAt.IsZero():
		return errors.New("it has no start time")
	}
	return nil
}

func batchPath(home, id string) string { return filepath.Join(batchDir(home), id+".json") }

// LoadBatches returns every readable import batch, oldest first. A batch
// file that cannot be read, or does not hold a valid batch (see validate),
// is left out and named in err, which is returned
// alongside the batches that could be read: a caller that only reports on
// imports can go on, one that must see every batch treats err as fatal.
func LoadBatches(home string) ([]Batch, error) {
	entries, err := os.ReadDir(batchDir(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list imports: %w", err)
	}
	var out []Batch
	var unreadable []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var b Batch
		stem := strings.TrimSuffix(e.Name(), ".json")
		if err := local.Read(filepath.Join(batchDir(home), e.Name()), &b); err != nil {
			unreadable = append(unreadable, fmt.Errorf("read import %q: %w", stem, err))
			continue
		}
		if err := b.validate(stem); err != nil {
			unreadable = append(unreadable, fmt.Errorf("read import %q: %w", stem, err))
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, errors.Join(unreadable...)
}

// SaveBatch durably writes b, replacing any earlier version.
func SaveBatch(home string, b Batch) error {
	if !ValidBatchID(b.ID) {
		return fmt.Errorf("import ID %q is not valid", b.ID)
	}
	if err := local.Write(batchPath(home, b.ID), b); err != nil {
		return fmt.Errorf("save import %s: %w", b.ID, err)
	}
	return nil
}

// ErrUnreadableImport wraps OpenBatch's error when an import's batch file
// can't be read, as opposed to the local store's registrations.
var ErrUnreadableImport = errors.New("an import file can't be read")

// OpenBatch returns the batch a confirmed run records into: the latest one,
// if it was interrupted, not undone, and ran with the same filters and
// destination, or a new one named for now's local date. An interrupted
// import is continued so that its sessions and the projects it added stay
// one import, which undo can reverse as a whole.
//
// A new ID is numbered past every ID already in use: each batch file's, and
// each ID a registration in store carries. Undo selects sessions by the ID
// they carry, so an ID reused while an earlier import's sessions still carry
// it would make undoing the new import remove the earlier one too. That
// happens once a batch file is gone (moved aside, deleted) while its
// sessions are still registered.
func OpenBatch(home string, store *state.Store, filters BatchFilters, destinationID string, now time.Time) (Batch, error) {
	batches, err := LoadBatches(home)
	if err != nil {
		return Batch{}, fmt.Errorf("%w: %w", ErrUnreadableImport, err)
	}
	if n := len(batches); n > 0 {
		last := batches[n-1]
		if last.Continues(filters, destinationID) {
			return last, nil
		}
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		return Batch{}, fmt.Errorf("read registrations: %w", err)
	}
	used := make([]string, 0, len(batches)+len(regs))
	for _, b := range batches {
		used = append(used, b.ID)
	}
	for _, reg := range regs {
		if id := reg.ImportBatch.Recorded(); id != "" {
			used = append(used, id)
		}
	}
	day := now.Format(dateLayout)
	next := 1
	for _, id := range used {
		if rest, ok := strings.CutPrefix(id, day+"-"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n >= next {
				next = n + 1
			}
		}
	}
	return Batch{
		ID: fmt.Sprintf("%s-%d", day, next), StartedAt: now.UTC(), Filters: filters, DestinationID: destinationID,
		ProjectsAdded: []string{}, AppsAdded: []string{}, Sessions: []string{}, Subagents: []string{},
	}, nil
}

// AddChanges records what a run changed in the configuration. A continued
// import keeps the retention it first changed from.
func (b *Batch) AddChanges(c ConfigChanges) {
	b.ProjectsAdded = addUnique(b.ProjectsAdded, c.ProjectIDs...)
	b.AppsAdded = addUnique(b.AppsAdded, c.Apps...)
	b.ProjectsKeptOut = addUnique(b.ProjectsKeptOut, c.KeptOut...)
	if c.Retention != nil {
		change := *c.Retention
		if b.Retention != nil {
			change.From = b.Retention.From
		}
		b.Retention = &change
	}
}

// AddSessions records sessions and subagents a run registered.
func (b *Batch) AddSessions(sessions, subagents []string) {
	b.Sessions = addUnique(b.Sessions, sessions...)
	b.Subagents = addUnique(b.Subagents, subagents...)
}

// addUnique appends the values not already in list.
func addUnique(list []string, values ...string) []string {
	for _, v := range values {
		if !slices.Contains(list, v) {
			list = append(list, v)
		}
	}
	return list
}

func (b *Batch) reconcileStagedSources(store *state.Store) error {
	cfg, found, err := config.Load(store.Home())
	if err != nil {
		return err
	}
	if !found || !cfg.ImportStagingProtection {
		return nil
	}
	for after := ""; ; {
		stages, next, err := store.ImportStages(context.Background(), after, 50)
		if err != nil {
			return err
		}
		for _, stage := range stages {
			if stage.Registration.InBatch(b.ID) {
				b.StagedSessions = addUnique(b.StagedSessions, stage.Registration.ArchiveSessionID)
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	return nil
}
