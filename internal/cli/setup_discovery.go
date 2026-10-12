package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// configureDiscovery applies explicit source consent; it never widens capture scope.
func configureDiscovery(next *config.Config, setting discoverySetting) error {
	selected := containsString(next.Harnesses, "codex")
	if setting != "" && !selected {
		return errors.New("--codex-discovery requires Codex in --apps")
	}
	switch setting {
	case discoveryOn:
		enableDiscovery(next, true)
	case discoveryOff:
		enableDiscovery(next, false)
	case "":
	default:
		return errors.New("--codex-discovery requires on or off")
	}
	return nil
}

func enableDiscovery(cfg *config.Config, enabled bool) {
	config.SetDiscoveryChoice(cfg, enabled)
}

func promptDiscovery(p *prompter, draft *setupDraft, previous config.Config) error {
	if draft.DiscoveryReviewed || !containsString(draft.Config.Harnesses, "codex") {
		return nil
	}
	draft.DiscoveryReviewed = true
	if draft.NewInstallation {
		enableDiscovery(&draft.Config, true)
		return nil
	}
	if previous.Discovery != nil && (previous.Discovery.Enabled || previous.Discovery.ChoiceRecorded) {
		return nil
	}
	p.note("Automatic Codex discovery can capture supported new tasks without hooks. Existing history stays excluded; an indistinguishable recent copy can also be captured.")
	enabled, err := p.setupYesNo("Enable automatic Codex discovery for the reviewed Codex scope and this destination?", false)
	if err != nil {
		draft.DiscoveryReviewed = false
		return err
	}
	enableDiscovery(&draft.Config, enabled)
	return nil
}

func printDiscoveryConsent(p *prompter, cfg config.Config) {
	for _, row := range codexConsentRows(cfg) {
		p.note(row.label + ": " + strings.Join(row.values, ", ") + ".")
	}
}

func configureCodexCaptureScope(cfg *config.Config, setting string) error {
	if setting == "" {
		return nil
	}
	if !containsString(cfg.Harnesses, "codex") {
		return errors.New("--codex-capture-scope requires Codex in --apps")
	}
	return config.SetCodexCaptureScope(cfg, config.CodexCaptureScope(setting))
}

func promptCodexCaptureScope(p *prompter, cfg *config.Config) error {
	if !containsString(cfg.Harnesses, "codex") {
		return nil
	}
	choice, err := p.setupMenu("Codex capture scope", string(cfg.EffectiveCodexCaptureScope()),
		option{string(config.CodexIncludedProjects), "Included projects only (Codex)"},
		option{string(config.CodexAllProjects), "All current and future projects (Codex only)"})
	if err != nil {
		return err
	}
	return config.SetCodexCaptureScope(cfg, config.CodexCaptureScope(choice))
}

func codexScopeLabel(cfg config.Config) string {
	if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		return "All current and future projects (Codex only)"
	}
	return "Included projects only"
}

func codexOnlyAllProjects(cfg config.Config) bool {
	return len(cfg.Harnesses) == 1 && cfg.Harnesses[0] == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects
}

// setupNeedsProject preserves selected apps' project requirement after review edits.
func setupNeedsProject(cfg config.Config) bool {
	return includedProjects(cfg.Archive.Projects) == 0 && !codexOnlyAllProjects(cfg)
}

func codexConsentRows(cfg config.Config) []reviewRow {
	if !containsString(cfg.Harnesses, "codex") {
		return nil
	}
	mechanism := "Approved hooks only; automatic discovery off"
	if cfg.Discovery != nil && cfg.Discovery.Enabled {
		mechanism = "Automatic discovery plus hooks when approved"
	}
	exceptions := []string{}
	for _, project := range cfg.Archive.Projects {
		if !project.Included {
			exceptions = append(exceptions, "Exclude "+project.Root)
		} else if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
			exceptions = append(exceptions, "Include "+project.Root)
		}
	}
	if len(exceptions) == 0 {
		exceptions = []string{"None configured"}
	}
	sources := []string{"Automatic discovery off"}
	if cfg.Discovery != nil && cfg.Discovery.Enabled {
		sources = cfg.Discovery.CodexHomes
	}
	hooks := "Hook-only capture requires approval; observation is separate"
	if cfg.Discovery != nil && cfg.Discovery.Enabled {
		hooks = "Optional for discovery; approval and observation are separate"
	}
	return []reviewRow{
		{label: "Codex capture", values: []string{mechanism}},
		{label: "Codex scope", values: []string{codexScopeLabel(cfg)}},
		{label: "Exceptions", values: exceptions, detail: "Nearest explicit project rule wins"},
		{label: "Codex sources", values: sources},
		{label: "Starts", values: []string{"After local consent, outside pause or exclusion"}},
		{label: "History", values: []string{"Last 7 days of included projects imported at setup; older sessions require backfill"}},
		{label: "Copies", values: []string{"Qualifying recent native copies may be captured"}},
		{label: "Hooks", values: []string{hooks}},
	}
}

// prepareDiscoveryHomes resolves the proposed source roots before review. A
// committed root remains explicit even when this shell uses another CODEX_HOME.
func prepareDiscoveryHomes(cfg *config.Config, env Env, userHome string) {
	if cfg.Discovery == nil || !cfg.Discovery.Enabled || len(cfg.Discovery.CodexHomes) != 0 {
		return
	}
	files := env.hookFiles(userHome)
	if files["codex"] != "" {
		d := *cfg.Discovery
		d.CodexHomes = []string{filepath.Dir(files["codex"])}
		cfg.Discovery = &d
	}
}

// validateScriptCodexChoices requires separate, explicit source and scope
// choices on a new machine. Omitted reconfiguration flags preserve consent.
func validateScriptCodexChoices(cfg config.Config, opts setupOptions, found bool) error {
	selected := containsString(cfg.Harnesses, "codex")
	if !selected && (opts.codexDiscovery != "" || opts.codexCaptureScope != "") {
		return errors.New("--codex-discovery and --codex-capture-scope require Codex in --apps")
	}
	if selected && !found {
		if opts.codexDiscovery == "" {
			return errors.New("fresh scripted Codex setup requires --codex-discovery on or off")
		}
		if opts.codexCaptureScope == "" {
			return errors.New("fresh scripted Codex setup requires --codex-capture-scope included-projects or all-projects")
		}
	}
	return nil
}

func configurePairedDiscovery(p *prompter, cfg *config.Config, opts setupOptions, previous config.Config, found bool) error {
	if err := configureCodexCaptureScope(cfg, opts.codexCaptureScope); err != nil {
		return err
	}
	if err := configureDiscovery(cfg, discoverySetting(opts.codexDiscovery)); err != nil {
		return err
	}
	if opts.yes {
		if err := validateScriptCodexChoices(*cfg, opts, found && containsString(previous.Harnesses, "codex")); err != nil {
			return err
		}
	}
	if !opts.yes && opts.codexDiscovery == "" {
		draft := setupDraft{Config: *cfg, NewInstallation: !found}
		if err := promptDiscovery(p, &draft, previous); err != nil {
			return err
		}
		*cfg = draft.Config
	}
	return nil
}

// promptCodexExceptions edits explicit project rules without enumerating discovered
// projects. The final review commits these local, forward-only policy changes.
func promptCodexExceptions(p *prompter, cfg *config.Config, userHome string) error {
	for {
		choice, err := p.setupMenu("Codex project exceptions", "done",
			option{"exclude", "Exclude a directory and its descendants"},
			option{"include", "Include a directory, overriding a parent exclusion"},
			option{"done", "Keep these exceptions and return to review"})
		if err != nil || choice == "done" {
			return err
		}
		var root string
		_, err = p.guidedText(promptModel{Question: "Directory", Label: "Answer", Validate: func(path string) error {
			if path == "" {
				return fmt.Errorf("this value is required")
			}
			var err error
			root, err = projectDir(path, userHome)
			return err
		}, ResolveReceipt: func(string) string { return "Directory " + root }})
		if err != nil {
			return err
		}
		included := choice == "include"
		var otherApps []string
		for _, app := range cfg.Harnesses {
			if app != "codex" {
				otherApps = append(otherApps, app)
			}
		}
		if len(otherApps) > 0 {
			p.note("Project rules are shared with " + friendlyApps(otherApps) + ". This rule also changes their capture permission for " + root + " and its descendants.")
			approved, err := p.setupYesNo("Apply this "+choice+" rule to Codex and "+friendlyApps(otherApps)+"?", false)
			if err != nil {
				return err
			}
			if !approved {
				p.note("The rule was not applied. Capture permissions are unchanged.")
				continue
			}
		}
		// A review draft may still share the committed snapshot's slice.
		// Copy only after approval, before editing an existing rule.
		cfg.Archive.Projects = slices.Clone(cfg.Archive.Projects)
		found := false
		for i := range cfg.Archive.Projects {
			if cfg.Archive.Projects[i].Root == root {
				cfg.Archive.Projects[i].Included = included
				found = true
			}
		}
		if !found {
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: included})
		}
		p.note(fmt.Sprintf("%s %s. The nearest explicit directory rule wins; lifting an exclusion admits only eligible future starts.", choice, root))
	}
}
