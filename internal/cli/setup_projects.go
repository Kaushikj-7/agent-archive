package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

type setupProjectAction string

const (
	setupProjectPath     setupProjectAction = "path"
	setupProjectRetry    setupProjectAction = "retry"
	setupProjectNext     setupProjectAction = "next"
	setupProjectPrevious setupProjectAction = "previous"
	setupProjectAll      setupProjectAction = "all"
)

// setupProjectSearch keeps bounded evidence and its coverage for one setup run.
type setupProjectSearch struct {
	env     Env
	home    string
	key     string
	result  backfill.KnownProjectsResult
	scanned bool
}

func (s *setupProjectSearch) projects(cfg config.Config) []backfill.KnownProject {
	key, _ := json.Marshal(struct {
		Apps    []string                    `json:"apps"`
		Rules   []archive.ProjectActivation `json:"rules"`
		Sources map[string][]string         `json:"sources"`
	}{cfg.Harnesses, cfg.Archive.Projects, s.env.nativeSessionDirectories(s.home, cfg)})
	if s.scanned && s.key == string(key) {
		return s.result.Projects
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	environment := s.env.backfillEnvironment(s.home, cfg)
	environment.Discovery = selectedDiscoveryApps{DiscoveryLookup: environment.Discovery, apps: cfg.Harnesses}
	s.result = backfill.KnownProjectsBounded(ctx, environment, cfg, 128)
	s.key = string(key)
	s.scanned = true
	return s.result.Projects
}

func (s *setupProjectSearch) reason() string {
	var reasons []string
	if s.result.TimedOut {
		reasons = append(reasons, "time limit reached")
	}
	if s.result.Capped {
		reasons = append(reasons, "search limit reached")
	}
	if s.result.Unreadable > 0 {
		reasons = append(reasons, fmt.Sprintf("%d unreadable sources", s.result.Unreadable))
	}
	return strings.Join(reasons, "; ")
}

type setupProjectCandidate struct {
	evidence backfill.KnownProject
	selected bool
}

// projectCandidates unions saved rules and discovery without discarding explicit
// nested rules. Session evidence is rolled under displayed parent roots.
func setupProjectCandidates(result, existing []archive.ProjectActivation, known []backfill.KnownProject, current string) []setupProjectCandidate {
	known = slices.Clone(known)
	for i := range known {
		known[i].Root = local.CanonicalPath(known[i].Root)
	}
	var out []setupProjectCandidate
	seen := map[string]bool{}
	add := func(root string) {
		if root == "" {
			return
		}
		root = local.CanonicalPath(root)
		if seen[root] {
			return
		}
		seen[root] = true
		out = append(out, setupProjectCandidate{evidence: backfill.KnownProject{Root: root}})
	}
	add(current)
	for _, rule := range existing {
		add(rule.Root)
	}
	for _, rule := range result {
		add(rule.Root)
	}
	for _, project := range known {
		root := project.Root
		nested := false
		for _, c := range out {
			if root != c.evidence.Root && local.PathWithin(root, c.evidence.Root) {
				nested = true
			}
		}
		for _, other := range known {
			parent := other.Root
			if root != parent && local.PathWithin(root, parent) {
				nested = true
			}
		}
		if !nested {
			add(root)
		}
	}
	selected := map[string]bool{}
	for _, rule := range existing {
		selected[local.CanonicalPath(rule.Root)] = rule.Included
	}
	for _, rule := range result {
		selected[local.CanonicalPath(rule.Root)] = rule.Included
	}
	for i := range out {
		out[i].selected = nearestSetupProjectRule(selected, out[i].evidence.Root)
	}
	for _, project := range known {
		index := -1
		for i, c := range out {
			if local.PathWithin(project.Root, c.evidence.Root) && (index < 0 || len(c.evidence.Root) > len(out[index].evidence.Root)) {
				index = i
			}
		}
		if index >= 0 {
			c := &out[index].evidence
			c.Sessions += project.Sessions
			if project.LastUsed.After(c.LastUsed) {
				c.LastUsed = project.LastUsed
			}
			if project.Root == c.Root {
				c.Kind = project.Kind
			}
		}
	}
	return out
}

func selectSetupProjects(p *prompter, result, existing []archive.ProjectActivation, known []backfill.KnownProject, current, home string, backfilled map[string]bool) ([]archive.ProjectActivation, error) {
	candidates := setupProjectCandidates(result, existing, known, current)
	fresh := len(existing) == 0 && len(result) == 0
	if fresh {
		for i := range candidates {
			candidates[i].selected = true
		}
	}
	page := 0
	incomplete := func() bool { return p.projectScan != nil && p.projectScan.result.Incomplete() }
	refresh := func() {
		if p.projectScan == nil {
			return
		}
		p.projectScan.scanned = false
		known = p.projectScan.projects(p.projectConfig)
		// UI choices are broader than persisted ownership rules: unchecked
		// manual roots and selected inherited children still belong in the
		// selector after Retry. Retain every transient root and exact choice.
		saved := make([]archive.ProjectActivation, len(candidates))
		for i, candidate := range candidates {
			saved[i] = archive.ProjectActivation{Root: candidate.evidence.Root, Included: candidate.selected}
		}
		candidates = setupProjectCandidates(saved, existing, known, current)
		page = 0
	}
	addPath := func() error { return addSetupProjectPath(p, &candidates, home) }
	validate := func(all bool) error { return validateSetupProjects(candidates, home, all) }

	for {
		if len(candidates) == 0 {
			if incomplete() {
				choice, err := p.guidedChoice(promptModel{Question: "Could not finish looking", Helpers: []string{p.projectScan.reason()}, Default: "path", Primary: []option{{"path", "Add a project path"}}, Secondary: []actionOption{{"retry", "r", "Retry search"}}})
				if err != nil {
					return nil, err
				}
				if choice == "retry" {
					refresh()
					continue
				}
			}
			if err := addPath(); err != nil {
				return nil, err
			}
		}
		helpers := projectCandidateLines(candidates, current, home, p.clock(), page, false, incomplete())
		if incomplete() {
			helpers = append(helpers, "Search incomplete: "+p.projectScan.reason()+". Session counts are observed, not complete totals.")
		}
		secondary := []actionOption{{"path", "p", "Add a project path"}}
		if p.projectScan != nil {
			secondary = append(secondary, actionOption{"retry", "r", "Retry search"})
		}
		if len(candidates) > maxKnownProjects {
			if (page+1)*maxKnownProjects < len(candidates) {
				secondary = append(secondary, actionOption{"next", "n", "Next projects"})
			}
			if page > 0 {
				secondary = append(secondary, actionOption{"previous", "b", "Previous projects"})
			}
		}
		choice, err := p.guidedChoice(promptModel{Question: "Which projects?", Helpers: helpers, Default: "all", Primary: []option{{"all", fmt.Sprintf("All %d found projects", len(candidates))}, {"specific", "Choose specific projects"}}, Secondary: secondary, Receipt: "Projects", Validate: func(key string) error {
			if key == "all" {
				return validate(true)
			}
			return nil
		}})
		if err != nil {
			return nil, err
		}
		switch setupProjectAction(choice) {
		case setupProjectPath:
			if err = addPath(); err != nil {
				return nil, err
			}
			continue
		case setupProjectRetry:
			refresh()
			continue
		case setupProjectNext:
			page++
			continue
		case setupProjectPrevious:
			page--
			continue
		case setupProjectAll:
			includeAllSetupProjects(p, candidates, existing)
			return applyProjectCandidates(candidates, existing, backfilled), nil

		}
		return chooseSpecificSetupProjects(p, &candidates, existing, current, home, backfilled, &page, incomplete, refresh, addPath, validate)

	}
}

func includedCandidateCount(candidates []setupProjectCandidate) int {
	n := 0
	for _, c := range candidates {
		if c.selected {
			n++
		}
	}
	return n
}

func projectCandidateLines(candidates []setupProjectCandidate, current, home string, now time.Time, page int, specific, partial bool) []string {
	start := page * maxKnownProjects
	end := min(start+maxKnownProjects, len(candidates))
	if start >= len(candidates) {
		start = 0
		end = min(maxKnownProjects, len(candidates))
	}
	title := fmt.Sprintf("Found %d projects:", len(candidates))
	if partial {
		title = fmt.Sprintf("Found %d projects; search incomplete:", len(candidates))
	}
	lines := []string{title}
	if len(candidates) > maxKnownProjects {
		lines = append(lines, fmt.Sprintf("Showing %d–%d of %d", start+1, end, len(candidates)))
	}
	for i := start; i < end; i++ {
		c := candidates[i]
		mark := ""
		if specific {
			mark = "[ ] "
			if c.selected {
				mark = "[✓] "
			}
		}
		details := projectDetails(c.evidence, current, now)
		if partial && c.evidence.Sessions > 0 {
			details = strings.Replace(details, fmt.Sprintf("%d session", c.evidence.Sessions), fmt.Sprintf("at least %d session", c.evidence.Sessions), 1)
		}
		line := fmt.Sprintf("%d) %s%s", i+1, mark, displayPath(c.evidence.Root, home))
		if details != "" {
			line += " · " + details
		}
		lines = append(lines, line)
	}
	return lines
}

func applyProjectCandidates(candidates []setupProjectCandidate, existing []archive.ProjectActivation, backfilled map[string]bool) []archive.ProjectActivation {
	result := slices.Clone(existing)
	indexes := map[string]int{}
	rules := map[string]bool{}
	newExclusions := map[string]bool{}
	for i, rule := range result {
		root := local.CanonicalPath(rule.Root)
		indexes[root] = i
		rules[root] = rule.Included
	}
	// Apply saved explicit choices before deriving inherited ownership.
	for _, c := range candidates {
		if i, found := indexes[c.evidence.Root]; found {
			newExclusions[c.evidence.Root] = result[i].Included && !c.selected
			result[i].Included = c.selected
			rules[c.evidence.Root] = c.selected
		}
	}
	// Covered selected descendants retain their owner's activation time.
	for _, c := range candidates {
		if _, found := indexes[c.evidence.Root]; found || !c.selected {
			continue
		}
		if included := nearestSetupProjectRule(rules, c.evidence.Root); included {
			continue
		}
		result = append(result, archive.ProjectActivation{Root: c.evidence.Root, ProjectID: archive.ProjectID(c.evidence.Root), Included: true})
		rules[c.evidence.Root] = true
	}
	// An unchecked child must override an included parent. Imported roots
	// retain an explicit exclusion even when no parent currently includes them.
	for _, c := range candidates {
		if _, found := indexes[c.evidence.Root]; found || c.selected {
			continue
		}
		included := nearestSetupProjectRule(rules, c.evidence.Root)
		if included || backfilled[archive.ProjectID(c.evidence.Root)] {
			result = append(result, archive.ProjectActivation{Root: c.evidence.Root, ProjectID: archive.ProjectID(c.evidence.Root), Included: false})
			rules[c.evidence.Root] = false
		}
	}
	// Ordinary projects that were just left out are removed, so including
	// them again starts a new activation window. Keep an exclusion when an
	// included ancestor or imported history requires it, and keep old rules.
	result = slices.DeleteFunc(result, func(rule archive.ProjectActivation) bool {
		root := local.CanonicalPath(rule.Root)
		if !newExclusions[root] || backfilled[archive.ProjectID(root)] {
			return false
		}
		delete(rules, root)
		inherited := nearestSetupProjectRule(rules, root)
		rules[root] = false
		return !inherited
	})
	return result
}

func nearestSetupProjectRule(rules map[string]bool, root string) (included bool) {
	length := -1
	for parent, choice := range rules {
		if len(parent) > length && local.PathWithin(root, parent) {
			included, length = choice, len(parent)
		}
	}
	return included
}

// selectedDiscoveryApps restricts discovery to an operation-specific application set.
type selectedDiscoveryApps struct {
	agentapi.DiscoveryLookup
	apps []string
}

func (s selectedDiscoveryApps) DiscoveryAgents() []string {
	var names []string
	for _, name := range s.DiscoveryLookup.DiscoveryAgents() {
		if slices.Contains(s.apps, name) {
			names = append(names, name)
		}
	}
	return names
}

func addSetupProjectPath(p *prompter, candidates *[]setupProjectCandidate, home string) error {
	var root string
	_, err := p.guidedText(promptModel{Question: "Add a project path", Label: "Path", Validate: func(value string) error {
		if value == "" {
			return fmt.Errorf("enter a project path")
		}
		var e error
		root, e = projectDir(value, home)
		return e
	}, ResolveReceipt: func(value string) string { return "Project " + displayPath(value, home) }})
	if err != nil {
		return err
	}
	for i := range *candidates {
		if (*candidates)[i].evidence.Root == root {
			(*candidates)[i].selected = true
			return nil
		}
	}
	*candidates = append(*candidates, setupProjectCandidate{evidence: backfill.KnownProject{Root: root}, selected: true})
	return nil
}

func validateSetupProjects(candidates []setupProjectCandidate, home string, all bool) error {
	var failures []string
	for _, c := range candidates {
		if all || c.selected {
			if _, err := projectDir(c.evidence.Root, home); err != nil {
				failures = append(failures, err.Error()+". Repair the path or leave it out in Specific selection.")
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "\n"))
	}
	return nil
}

func chooseSpecificSetupProjects(p *prompter, candidates *[]setupProjectCandidate, existing []archive.ProjectActivation, current, home string, backfilled map[string]bool, page *int, incomplete func() bool, refresh func(), addPath func() error, validate func(bool) error) ([]archive.ProjectActivation, error) {
	for {
		lines := projectCandidateLines(*candidates, current, home, p.clock(), *page, true, incomplete())
		lines = append(lines, "Enter numbers to toggle; numbers stay the same across pages.", "[a] Select all", "[p] Add a project path")
		if p.projectScan != nil {
			lines = append(lines, "[r] Retry search")
		}
		lines = append(lines, "[n] Next page · [b] Previous page", "[Enter] Confirm selection")
		answer, e := p.guidedText(promptModel{Question: "Choose specific projects", Helpers: lines, Label: "Projects", ResolveReceipt: func(answer string) string {
			if answer == "" {
				return fmt.Sprintf("Projects %d selected", includedCandidateCount(*candidates))
			}
			return "Project selection updated"
		}, Validate: func(answer string) error {
			switch strings.ToLower(answer) {
			case "":
				if includedCandidateCount(*candidates) == 0 {
					return fmt.Errorf("choose at least one project")
				}
				return validate(false)
			case "r", "retry":
				if p.projectScan == nil {
					return fmt.Errorf("retry is unavailable for this project selection")
				}
				return nil
			case "a", "all", "p", "path", "n", "next", "b", "previous":
				return nil
			default:
				_, ok, inRange := parseNumbers(answer, len(*candidates))
				if !ok || !inRange {
					return fmt.Errorf("enter project numbers from 1 to %d, or p to add a path", len(*candidates))
				}
				return nil
			}
		}})
		if e != nil {
			return nil, e
		}
		switch strings.ToLower(answer) {
		case "":
			return applyProjectCandidates(*candidates, existing, backfilled), nil
		case "a", "all":
			for i := range *candidates {
				(*candidates)[i].selected = true
			}
		case "p", "path":
			if e = addPath(); e != nil {
				return nil, e
			}
		case "r", "retry":
			refresh()
		case "n", "next":
			if (*page+1)*maxKnownProjects < len(*candidates) {
				(*page)++
			}
		case "b", "previous":
			if *page > 0 {
				(*page)--
			}
		default:
			numbers, _, _ := parseNumbers(answer, len(*candidates))
			slices.Sort(numbers)
			for _, n := range slices.Compact(numbers) {
				(*candidates)[n-1].selected = !(*candidates)[n-1].selected
			}
		}
	}
}

func includeAllSetupProjects(p *prompter, candidates []setupProjectCandidate, existing []archive.ProjectActivation) {
	included := map[string]bool{}
	for _, rule := range existing {
		included[local.CanonicalPath(rule.Root)] = rule.Included
	}
	added := 0
	for i := range candidates {
		candidates[i].selected = true
		wasIncluded := nearestSetupProjectRule(included, candidates[i].evidence.Root)
		if !wasIncluded {
			added++
		}
	}
	if len(existing) > 0 && added > 0 {
		p.note(fmt.Sprintf("All adds or re-enables %d projects; review the expanded capture scope before saving.", added))
	}
}
