package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Detect only explicit identities before ordinary backfill's native cleanup.
// Subcommands keep their existing parser and dispatch.
func explicitNativeImport(args []string, env Env) bool {
	if len(args) > 0 && slices.Contains([]string{"projects", "history", "undo"}, args[0]) {
		return false
	}
	for i, arg := range args {
		value, supplied := strings.CutPrefix(arg, "--harness=")
		if !supplied {
			value, supplied = strings.CutPrefix(arg, "-harness=")
		}
		if (arg == "--harness" || arg == "-harness") && i+1 < len(args) {
			value, supplied = args[i+1], true
		}
		if supplied {
			name, known := env.agentRegistry().CanonicalDiscovery(value)
			if known && name == "muse" {
				return true
			}
		}
	}
	return false
}

type nativeImportRow struct {
	Ordinal   int        `json:"entry"`
	NativeID  string     `json:"-"`
	OriginalT *time.Time `json:"original_started_at,omitempty"`
	Project   string     `json:"project"`
	ProjectID string     `json:"project_id"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	selection archive.NativeImportBatchSelection
	candidate state.NativeImportCandidate
	ref       agentapi.SourceRef
	token     string
}

type nativeImportPreview struct {
	Rows          []nativeImportRow `json:"sessions"`
	Complete      bool              `json:"identity_complete"`
	ChildCoverage string            `json:"child_coverage"`
	SinceUTC      string            `json:"since_utc"`
	UntilUTC      string            `json:"until_exclusive_utc"`
	Destination   string            `json:"destination_id"`
	projects      []string
	inventory     *discovery.NativeInventory
	budget        *agentapi.NativeReadBudget
	resume        *backfill.Batch
	charge        int64
}

func (p *nativeImportPreview) close() error {
	inventory := p.inventory
	// End borrowed observations before releasing their inventory/ledger owner.
	p.Rows, p.projects, p.resume, p.inventory = nil, nil, nil, nil
	if p.charge > 0 && p.budget != nil {
		p.budget.ReleaseUnscoped(p.charge)
		p.charge = 0
	}
	p.budget = nil
	if inventory != nil {
		return inventory.Close()
	}
	return nil
}

func nativeImportBounds(opts backfillCommandOptions, now time.Time) (string, string, error) {
	var bounds [2]string
	for i, value := range []string{opts.filters.Since, opts.filters.Until} {
		if value == "" {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02", value, now.Location())
		if err != nil {
			return "", "", err
		}
		if i == 1 {
			at = at.AddDate(0, 0, 1)
		}
		bounds[i] = at.UTC().Format(time.RFC3339Nano)
	}
	return bounds[0], bounds[1], nil
}

func runNativeImportCommand(stdin io.Reader, stdout, stderr io.Writer, env Env, opts backfillCommandOptions) int {
	fail := func(err error) int { terminal.Printf(stderr, "agent-archive: backfill: %v\n", err); return 1 }
	home, err := env.readHome()
	if err != nil {
		return fail(err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(errNotSetUp)
	}
	if !opts.dryRun {
		if refusal := importRefusal(home, cfg); refusal != "" {
			return fail(errors.New(refusal))
		}
		if !opts.yes && !env.interactive(stdin) {
			return fail(errors.New("confirmation needs a terminal; use --dry-run to preview or --yes to confirm this exact selection"))
		}
	}
	ctx, stop := interruptibleContext(env, stderr)
	defer stop()
	preview, err := planNativeImport(ctx, env, home, cfg, opts)
	if err != nil {
		return fail(err)
	}
	defer preview.close()
	if err := renderNativeImport(stdout, preview, opts.jsonOut); err != nil {
		return fail(err)
	}
	if opts.dryRun {
		return 0
	}
	if !preview.Complete {
		return fail(errors.New("native inventory is incomplete; no import was confirmed, run backfill again"))
	}
	selected := 0
	for _, row := range preview.Rows {
		if row.Status == "selected" {
			selected++
		}
	}
	if selected == 0 {
		return 0
	}
	if !opts.yes {
		p := newPrompter(stdin, stdout)
		defer p.close()
		answer, err := p.guidedChoice(promptModel{Question: fmt.Sprintf("Import these %d historical Muse sessions?", selected), Default: "no", Primary: []option{{"yes", "Confirm exactly these sessions"}, {"no", "Cancel"}}})
		if err != nil {
			return fail(err)
		}
		if answer != "yes" {
			terminal.Println(stdout, "Cancelled. Nothing was changed.")
			return 0
		}
	}
	batch, err := commitNativeImportPreview(ctx, env, home, cfg, opts, &preview)
	if err != nil {
		return fail(err)
	}
	terminal.Printf(stdout, "Import %s: %d historical Muse sessions registered.\n", batch.ID, len(batch.Sessions))
	terminal.Printf(stdout, "Exact undo: agent-archive backfill undo %s\n", batch.ID)
	if opts.background {
		terminal.Println(stdout, "The normal collector publishes the frozen stages and verifies readback. Child coverage remains unavailable.")
		return 0
	}
	if err := preview.close(); err != nil {
		return fail(err)
	}
	return runSyncCommand(nil, stdout, stderr, env)
}

func renderNativeImport(out io.Writer, p nativeImportPreview, jsonOut bool) error {
	if jsonOut {
		return writeNativeImportJSON(out, struct {
			Source string `json:"source"`
			nativeImportPreview
		}{"historical-muse-import", p})
	}
	terminal.Printf(out, "Historical Muse import for destination %s\n", archive.DisplayLine(p.Destination))
	since, until := p.SinceUTC, p.UntilUTC
	if since == "" {
		since = "unbounded"
	}
	if until == "" {
		until = "unbounded"
	}
	terminal.Printf(out, "Exact UTC range: %s to %s (exclusive).\n", archive.DisplayLine(since), archive.DisplayLine(until))
	for _, row := range p.Rows {
		started := "unavailable"
		if row.OriginalT != nil {
			started = row.OriginalT.UTC().Format(time.RFC3339Nano)
		}
		terminal.Printf(out, "  entry %d  %s  %s  %s  %s\n", row.Ordinal, started, archive.DisplayLine(row.Project), archive.DisplayLine(row.Status), archive.DisplayLine(row.Reason))
	}
	terminal.Printf(out, "Namespace complete: %t. Child coverage: UNAVAILABLE. No descendant completeness is claimed.\n", p.Complete)
	return nil
}

func nativeImportProjectFilters(env Env, cfg config.Config, opts backfillCommandOptions) ([]string, error) {
	var selected []string
	for _, dir := range opts.filters.Projects {
		if !filepath.IsAbs(dir) {
			cwd, err := env.workingDir()
			if err != nil {
				return nil, err
			}
			dir = filepath.Join(cwd, dir)
		}
		canonical, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, fmt.Errorf("project must name an existing configured physical checkout: %w", err)
		}
		found := false
		for _, project := range cfg.Archive.Projects {
			if project.Included && project.Root == canonical {
				selected = append(selected, project.ProjectID)
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("--project must name a currently included configured physical project; historical mappings/worktrees are unavailable")
		}
	}
	slices.Sort(selected)
	return slices.Compact(selected), nil
}
