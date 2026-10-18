package cli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runBackfillReviewProjects exports private exact assignments after explicit
// terminal review. It never enters the import or configuration transaction.
func runBackfillReviewProjects(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("backfill review-projects", stderr)
	output := fs.String("output", "", "new private mapping file; existing files are refused")
	var harnesses stringList
	fs.Var(&harnesses, "harness", "only review this app; repeatable")
	includeTemp := fs.Bool("include-temp", false, "review temporary-directory candidates")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if *output == "" || *output == "-" {
		fs.usageError("--output needs a new local mapping filename")
		return 2
	}
	fail := func(err error) int {
		terminal.Printf(stderr, "agent-archive: backfill review-projects: %v. No assignments were exported; archive and configuration were unchanged.\n", err)
		return 1
	}
	if !env.interactive(stdin) || !env.interactive(stdout) {
		return fail(errors.New("private project review requires terminal input and output; JSON and piped review are unavailable"))
	}
	path, err := filepath.Abs(*output)
	if err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fail(errors.New("output already exists or cannot be inspected; choose a new filename"))
	}
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
	userHome, err := env.userHomeDir()
	if err != nil {
		return fail(err)
	}
	filters, err := (backfill.Filters{Harnesses: harnesses, IncludeTemp: *includeTemp}).Canonicalize(selectedDiscoveryApps{DiscoveryLookup: env.agentRegistry(), apps: env.backfillDiscoveryAgents()})
	if err != nil {
		return fail(err)
	}
	env.reviewBackfillProjects = true
	plan, ok := planBackfill(env, stdout, stderr, home, userHome, cfg, backfillCommandOptions{filters: filters, dryRun: true}, styleFor(stdout))
	if !ok {
		return 1
	}
	p := newPrompter(stdin, stdout)
	defer p.close()
	if _, err := reviewProjectAssignments(p, cfg, plan.RecoveryReview, path); err != nil {
		return fail(err)
	}
	return 0
}

type recoveryChoice struct {
	root       string
	historical backfill.HistoricalProjectDefinition
}

type recoveryReviewAction string

const (
	recoveryReviewBlank  recoveryReviewAction = ""
	recoveryReviewCancel recoveryReviewAction = "cancel"
	recoveryReviewSkip   recoveryReviewAction = "skip"
	recoveryReviewNew    recoveryReviewAction = "new"
)

func reviewProjectAssignments(p *prompter, cfg config.Config, review *backfill.RecoveryReview, path string) (bool, error) {
	terminal.Println(p.out, "\nPrivate project recovery review: bounded, incomplete coverage of observed candidates.")
	terminal.Println(p.out, "Counts are project-recovery candidates, not promises of importability. Native history, own-task evidence and source limits still apply. No uniqueness is inferred from these windows.")
	if review != nil && review.UnavailableCwdCandidates > 0 {
		terminal.Printf(p.out, "Observed candidates without an exact usable original cwd: %d (no exact assignment available).\n", review.UnavailableCwdCandidates)
	}
	if review != nil && review.UnavailableIdentityCandidates > 0 {
		terminal.Printf(p.out, "Observed candidates with unsupported recorded identity: %d (review unavailable).\n", review.UnavailableIdentityCandidates)
	}
	if review == nil || len(review.Groups) == 0 {
		terminal.Println(p.out, "No missing original cwd groups were observed. No file was written.")
		return false, nil
	}
	terminal.Printf(p.out, "%d groups shown; %d observed candidate records outside these groups (distinct omitted group count unknown; review limit %d).\n", len(review.Groups), review.OmittedCandidates, backfill.RecoveryReviewLimit)
	terminal.Printf(p.out, "%d configured targets and %d automatic checkout evidence rows unlisted. Unlisted choices require a separately reviewed exact mapping file; they were not compared by this review.\n", review.OmittedTargets, review.OmittedWitnesses)
	renderRecoveryWitnesses(p.out, review.Witnesses)
	assignments := backfill.Filters{ProjectMappings: map[string]string{}, HistoricalMappings: map[string]string{}}
	definitions := map[string]backfill.HistoricalProjectDefinition{}
	terminal.Println(p.out, "One decision covers every supported source at an exact cwd, including unlisted identities. Recorded keys are evidence, not assignment selectors; skip skips that whole cwd. A configured choice may recover only some sources. Import still checks each source's restrictions, identity and native evidence.")
	if err := reviewRecoveryCwds(p, cfg, review.Groups, &assignments, definitions); err != nil {
		return false, err
	}
	if len(assignments.ProjectMappings)+len(assignments.HistoricalMappings) == 0 {
		terminal.Println(p.out, "No assignments selected. No file was written.")
		return false, nil
	}
	for _, definition := range definitions {
		assignments.HistoricalProjects = append(assignments.HistoricalProjects, definition)
	}
	data, err := backfill.EncodeMappingFile(assignments)
	if err != nil {
		return false, err
	}
	terminal.Println(p.out, "\nReview exact assignments to export (private metadata):")
	terminal.Print(p.out, string(archive.DisplayJSON(data)))
	answer, err := p.line(fmt.Sprintf("Write these exact assignments to %q? Type export to confirm: ", path))
	if err != nil {
		return false, err
	}
	if answer != "export" {
		terminal.Println(p.out, "Export rejected. No file was written.")
		return false, nil
	}
	if err := writePrivateMappingExclusive(path, data); err != nil {
		return false, err
	}
	terminal.Printf(p.out, "Exported %d exact cwd assignments privately to %q. Review with backfill --dry-run --mapping-file before a separately confirmed import. Configuration and archive were unchanged.\n", len(assignments.ProjectMappings)+len(assignments.HistoricalMappings), path)
	return true, nil
}

func reviewRecoveryCwds(p *prompter, cfg config.Config, groups []backfill.RecoveryReviewGroup, assignments *backfill.Filters, definitions map[string]backfill.HistoricalProjectDefinition) error {
	// The planning owner sorts groups by exact assignment cwd, then key.
	for first := 0; first < len(groups); {
		last := first + 1
		for last < len(groups) && groups[last].AssignmentCwd == groups[first].AssignmentCwd {
			last++
		}
		cwdGroups := groups[first:last]
		first = last
		restricted := false
		for _, g := range cwdGroups {
			renderRecoveryGroup(p.out, g)
			if g.SourceRestriction != "" {
				terminal.Printf(p.out, "  Assignment unavailable: source restriction %s.\n", g.SourceRestriction)
				restricted = true
			}
		}
		if restricted {
			terminal.Println(p.out, "  This exact cwd cannot be assigned by this review because an observed group is restricted.")
			continue
		}
		choices := renderRecoveryChoices(p.out, cfg, definitions, recoveryCwdTargets(cwdGroups))
		choice, selected, err := promptRecoveryChoice(p, choices)
		if err != nil {
			return err
		}
		if selected {
			if err := addRecoveryAssignment(assignments, definitions, cwdGroups[0].AssignmentCwd, choice); err != nil {
				return err
			}
		}
	}
	return nil
}

// recoveryCwdTargets offers a configured destination if at least one observed
// group supports it. Each group's incompatible evidence remains visible, and
// import retains its per-source checks. Unlisted identities remain unchecked.
func recoveryCwdTargets(groups []backfill.RecoveryReviewGroup) []backfill.RecoveryTarget {
	targets := slices.Clone(groups[0].Targets)
	for _, group := range groups[1:] {
		for i := range targets {
			other := slices.IndexFunc(group.Targets, func(target backfill.RecoveryTarget) bool { return target.Root == targets[i].Root })
			if other >= 0 && group.Targets[other].Outcome < targets[i].Outcome {
				targets[i].Outcome = group.Targets[other].Outcome
			}
		}
	}
	return targets
}

func renderRecoveryWitnesses(out io.Writer, witnesses []backfill.RecoveryWitness) {
	terminal.Println(out, "Automatic recovery checkout evidence (physical clones remain distinct):")
	for _, w := range witnesses {
		state := "unobserved"
		if w.Observed {
			state = "unresolved"
			if w.Known {
				state = "known; key=" + w.Key
			} else if w.Failure != "" {
				state = string(w.Failure)
			}
		}
		terminal.Printf(out, "  %q physical=%q included=%t %s\n", w.Root, w.PhysicalRoot, w.Included, state)
	}
}

func renderRecoveryGroup(out io.Writer, g backfill.RecoveryReviewGroup) {
	key := g.RecordedRepoKey
	if key == "" {
		key = "unavailable"
	}
	terminal.Printf(out, "\nOriginal cwd: %q\nRecorded repository key: %s\nCandidates: %d\n", g.OriginalCwd, key, g.Candidates)
	if g.AssignmentCwd != g.OriginalCwd {
		terminal.Printf(out, "  Exact assignment spelling: %q\n", g.AssignmentCwd)
	}
	if g.MixedOriginalSpellings {
		terminal.Println(out, "  Multiple original spellings share this exact assignment key; any observed source restriction disables assistance for the group.")
	}
	if g.ParentTraversalOriginal {
		terminal.Println(out, "  Original cwd contains parent traversal and requires separate manual review; this assistance cannot qualify its traversal from a cleaned spelling.")
	}
	for _, reason := range slices.Sorted(maps.Keys(g.PrimaryReasons)) {
		name := string(reason)
		if name == "" {
			name = "project already resolved in current plan"
		}
		terminal.Printf(out, "  Current decision: %s = %d\n", name, g.PrimaryReasons[reason])
	}
	for _, detail := range slices.Sorted(maps.Keys(g.DiagnosticDetails)) {
		terminal.Printf(out, "  Evidence/refusal: %s = %d\n", detail, g.DiagnosticDetails[detail])
	}
	for _, reason := range slices.Sorted(maps.Keys(g.OtherReasons)) {
		terminal.Printf(out, "  Still pending after project assignment: %s = %d\n", reason, g.OtherReasons[reason])
	}
	if g.NativeEvidenceUnresolved > 0 {
		terminal.Printf(out, "  Native history/own-task evidence unresolved or unchecked: %d\n", g.NativeEvidenceUnresolved)
	}
	if g.SourceRestriction == "" {
		for _, target := range g.Targets {
			outcome := string(target.Outcome)
			if outcome == "" {
				outcome = "compatible for this observed group"
			}
			terminal.Printf(out, "  Configured target evidence: %q (%s)\n", target.Root, outcome)
		}
	}
}

func renderRecoveryChoices(out io.Writer, cfg config.Config, definitions map[string]backfill.HistoricalProjectDefinition, targets []backfill.RecoveryTarget) []recoveryChoice {
	choices := []recoveryChoice{}
	for _, target := range targets {
		if target.Outcome != "" {
			terminal.Printf(out, "  Rejected configured target: %q (%s)\n", target.Root, target.Outcome)
			continue
		}
		choices = append(choices, recoveryChoice{root: target.Root})
		terminal.Printf(out, "  %d. Configured: %q (compatible exact assignment for at least one observed group; other sources may refuse)\n", len(choices), target.Root)
	}
	historical := cfg.HistoricalProjects[:min(len(cfg.HistoricalProjects), backfill.RecoveryReviewLimit)]
	for _, project := range historical {
		enabled, recorded := project.HistoricalPermission(cfg.DestinationID())
		if recorded && !enabled {
			terminal.Printf(out, "  Rejected historical target: %s %q (disabled for current destination)\n", project.ID, project.Label)
			continue
		}
		choices = append(choices, recoveryChoice{historical: backfill.HistoricalProjectDefinition{ID: project.ID, Label: project.Label}})
		terminal.Printf(out, "  %d. Historical: %s %q (import only)\n", len(choices), project.ID, project.Label)
	}
	if omitted := len(cfg.HistoricalProjects) - len(historical); omitted > 0 {
		terminal.Printf(out, "  %d historical targets unlisted; use a separately reviewed exact mapping file.\n", omitted)
	}
	for _, id := range slices.Sorted(maps.Keys(definitions)) {
		if _, exists := cfg.HistoricalProject(id); !exists {
			choices = append(choices, recoveryChoice{historical: definitions[id]})
			terminal.Printf(out, "  %d. Historical: %s %q (new, import only)\n", len(choices), id, definitions[id].Label)
		}
	}
	return choices
}

func promptRecoveryChoice(p *prompter, choices []recoveryChoice) (recoveryChoice, bool, error) {
	answer, err := p.line("Choose an exact destination number, new historical project, skip, or cancel [skip]: ")
	if err != nil {
		return recoveryChoice{}, false, err
	}
	switch recoveryReviewAction(answer) {
	case recoveryReviewCancel:
		return recoveryChoice{}, false, errors.New("review cancelled")
	case recoveryReviewBlank, recoveryReviewSkip:
		return recoveryChoice{}, false, nil
	case recoveryReviewNew:
		label, err := p.line("Historical project label (import only): ")
		if err != nil {
			return recoveryChoice{}, false, err
		}
		if !archive.ValidHistoricalProjectLabel(label) {
			return recoveryChoice{}, false, errors.New("use a plain-text historical label of 1 to 128 characters")
		}
		id, err := local.ID()
		if err != nil {
			return recoveryChoice{}, false, err
		}
		return recoveryChoice{historical: backfill.HistoricalProjectDefinition{ID: id, Label: label}}, true, nil
	default:
		number, err := strconv.Atoi(answer)
		if err != nil || number < 1 || number > len(choices) {
			return recoveryChoice{}, false, errors.New("choose a displayed destination number, new, skip, or cancel")
		}
		return choices[number-1], true, nil
	}
}

func addRecoveryAssignment(assignments *backfill.Filters, definitions map[string]backfill.HistoricalProjectDefinition, cwd string, choice recoveryChoice) error {
	if choice.root != "" {
		if assignments.HistoricalMappings[cwd] != "" || assignments.ProjectMappings[cwd] != "" && assignments.ProjectMappings[cwd] != choice.root {
			return errors.New("recorded identities at one exact cwd received conflicting assignments; review them together")
		}
		assignments.ProjectMappings[cwd] = choice.root
	} else {
		if assignments.ProjectMappings[cwd] != "" || assignments.HistoricalMappings[cwd] != "" && assignments.HistoricalMappings[cwd] != choice.historical.ID {
			return errors.New("recorded identities at one exact cwd received conflicting assignments; review them together")
		}
		assignments.HistoricalMappings[cwd] = choice.historical.ID
		definitions[choice.historical.ID] = choice.historical
	}
	return nil
}

func writePrivateMappingExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create output exclusively; choose a new writable filename")
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	err = errors.Join(writeErr, file.Close())
	if err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}
