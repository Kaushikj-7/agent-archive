package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// setupReview is what the review screen shows beside the configuration
// being saved.
type setupReview struct {
	// existing is the active configuration, which a reconfiguration's
	// changes are marked against.
	existing      config.Config
	reconfiguring bool
	discoveries   map[string]applicationDiscovery
	// hookFiles are the files each app's hooks go into.
	hookFiles hooks.Files
	// installedHookFiles are where setup installed the existing apps'
	// hooks.
	installedHookFiles hooks.Files
	userHome           string
	// sourceRoots captures native capture locations for the same reviewed draft.
	sourceRoots map[string][]string
	// storageUnchecked is used by consent reviews before their connection probe.
	storageUnchecked bool
}

// reviewRow is one labeled line of the review summary. A row with several
// values, such as projects, prints one value per line. Detail follows the
// first value, dim: secondary facts, such as the bucket's region.
type reviewRow struct {
	label  string
	values []string
	detail string
}

// text is the row as one string, for comparing and for "was".
func (r reviewRow) text() string {
	text := strings.Join(r.values, ", ")
	if r.detail != "" {
		text += "  " + r.detail
	}
	return text
}

// reviewRows lists what setup will save, in the order the review shows it.
// The Sessions row appears only when showSessions is set: it is noise while
// every new session is saved, the default.
func reviewRows(cfg config.Config, discoveries map[string]applicationDiscovery, showSessions bool, userHome string) []reviewRow {
	var apps []string
	for _, app := range cfg.Harnesses {
		apps = append(apps, appWithVersion(app, discoveries[app]))
	}
	var projects []string
	for _, project := range cfg.Archive.Projects {
		if project.Included {
			projects = append(projects, displayPath(project.Root, userHome))
		}
	}
	sessions := "All new sessions, with or without skills"
	if cfg.RequireSkillUse {
		sessions = "Only new sessions that use skills"
	}
	rows := []reviewRow{
		{label: "Apps", values: []string{strings.Join(apps, " · ")}},
	}
	if len(cfg.DeclinedHarnesses) > 0 {
		// An app leaves this list only by being included, which changes the
		// Apps row, so the row need not appear when the list is empty.
		rows = append(rows, reviewRow{label: "Skipped", values: []string{appList(cfg.DeclinedHarnesses) + " (setup will not offer again; to add back, choose Apps and projects in agent-archive setup)"}})
	}
	if len(cfg.ImportedHarnesses) > 0 {
		rows = append(rows, reviewRow{label: "Imported", values: []string{friendlyApps(cfg.ImportedHarnesses) + " (sessions imported by backfill stay published; new sessions are not captured)"}})
	}
	rows = append(rows, reviewRow{label: "Projects", values: projects})
	rows = append(rows, codexConsentRows(cfg)...)
	skillScope := string(cfg.EffectiveSkillEvidence())
	if cfg.SkillEvidence == "" {
		skillScope += " (kept from previous setup)"
	}
	rows = append(rows, reviewRow{label: "Skills", values: []string{skillScope}, detail: "User skill roots outside selected projects may be scanned; change with Edit a setting"})
	if showSessions {
		rows = append(rows, reviewRow{label: "Sessions", values: []string{sessions}})
	}
	address, detail := storageAddress(cfg.Storage)
	rows = append(rows, reviewRow{label: "Storage", values: []string{address}, detail: detail})
	if cfg.MachineAssignment != nil && cfg.MachineAssignment.Kind == config.MachineAssignmentR2Own {
		rows = append(rows, reviewRow{label: "R2 keys", values: []string{fmt.Sprintf("Own dedicated key · %d unused spares (target %d)", len(cfg.SpareCredentialRefs), cfg.SpareTarget())}})
	}
	machine := cfg.MachineID
	if cfg.MachineName != "" {
		machine = cfg.MachineName + " · " + machine
	}
	if machine != "" {
		rows = append(rows, reviewRow{label: "Machine", values: []string{machine}})
	}
	days := config.RetentionLabel(cfg.RetentionDays)
	return append(rows, reviewRow{label: "Keep for", values: []string{days}})
}

// storageAddress is where sessions are stored, as one address such as
// s3://bucket/agent-archive/, with what else locates it: the S3 region and
// profile, or the R2 account or endpoint.
func storageAddress(s credentials.Config) (address, detail string) {
	prefix := strings.TrimSuffix(firstNonEmpty(s.Prefix, defaultPrefix), "/") + "/"
	if s.Provider == credentials.ProviderS3 {
		return "s3://" + s.Bucket + "/" + prefix, s.Region + " · profile " + firstNonEmpty(s.AWSProfile, "default")
	}
	if s.R2AccountID != "" {
		return "r2://" + s.Bucket + "/" + prefix, "account " + s.R2AccountID
	}
	return "r2://" + s.Bucket + "/" + prefix, "endpoint " + s.R2Endpoint
}

func appWithVersion(app string, discovery applicationDiscovery) string {
	switch {
	case discovery.Version != "":
		return appName(app) + " " + discovery.Version
	case discovery.VersionState == "absent":
		return appName(app) + " (not found)"
	}
	return appName(app) + " (version not detected)"
}

// reviewDiscoveries returns discoveries as the review shows them. An app
// discovery could not find but whose config directory detection saw is
// reported as installed with an unknown version rather than "not found":
// its CLI may live somewhere discovery does not look. Recorded discoveries
// keep the absent state, which version support relies on.
func reviewDiscoveries(discoveries map[string]applicationDiscovery, detected []string) map[string]applicationDiscovery {
	result := make(map[string]applicationDiscovery, len(discoveries))
	for app, discovery := range discoveries {
		if discovery.VersionState == "absent" && containsString(detected, app) {
			discovery = applicationDiscovery{Installed: true, VersionState: "unknown"}
		}
		result[app] = discovery
	}
	return result
}

// showSetupReview prints the summary of what will be saved, then the
// checklist of what setup found, and reports whether a row of it is ✗,
// which blocks starting. When reconfiguring, a value that differs from the
// active configuration is marked, with the old value beneath it, so only
// those lines need checking.
type setupReviewModel struct {
	implications string
	rows         []reviewRow
	before       map[string]reviewRow
	changed      map[string]bool
	checks       []reviewCheck
	cfg          config.Config
	review       setupReview
}

func buildSetupReviewModel(cfg config.Config, review setupReview, at time.Time) *setupReviewModel {
	showSessions := cfg.RequireSkillUse || review.reconfiguring && review.existing.RequireSkillUse
	m := &setupReviewModel{cfg: cfg, review: review, before: map[string]reviewRow{}, changed: map[string]bool{}, rows: reviewRows(cfg, review.discoveries, showSessions, review.userHome)}
	if review.reconfiguring {
		for _, row := range reviewRows(review.existing, review.discoveries, showSessions, review.userHome) {
			m.before[row.label] = row
		}
	}
	var exclusions []string
	for _, rule := range cfg.Archive.Projects {
		if !rule.Included {
			exclusions = append(exclusions, displayPath(rule.Root, review.userHome))
		}
	}
	if len(exclusions) > 0 {
		m.rows = append(m.rows, reviewRow{label: "Excluded", values: exclusions, detail: "Nearest explicit project rule wins"})
	}
	var oldExclusions []string
	for _, rule := range review.existing.Archive.Projects {
		if !rule.Included {
			oldExclusions = append(oldExclusions, displayPath(rule.Root, review.userHome))
		}
	}
	if len(oldExclusions) > 0 {
		m.before["Excluded"] = reviewRow{label: "Excluded", values: oldExclusions, detail: "Nearest explicit project rule wins"}
		if len(exclusions) == 0 {
			m.rows = append(m.rows, reviewRow{label: "Excluded", values: []string{"none"}})
		}
	}
	// A removed optional setting still needs an old/new row at final review.
	if review.reconfiguring {
		for _, old := range reviewRows(review.existing, review.discoveries, showSessions, review.userHome) {
			present := false
			for _, row := range m.rows {
				present = present || row.label == old.label
			}
			if !present && old.text() != "" {
				m.rows = append(m.rows, reviewRow{label: old.label, values: []string{"none"}})
			}
		}
	}
	for _, row := range m.rows {
		m.changed[row.label] = review.reconfiguring && m.before[row.label].text() != row.text()
	}
	var roots []string
	for _, app := range cfg.Harnesses {
		for _, root := range review.sourceRoots[app] {
			roots = append(roots, appName(app)+": "+displayPath(root, review.userHome))
		}
	}
	if len(roots) > 0 {
		m.rows = append(m.rows, reviewRow{label: "Sources", values: roots})
	}
	var sources []string
	for _, app := range cfg.Harnesses {
		if path := review.hookFiles[app]; path != "" {
			sources = append(sources, appName(app)+": "+displayPath(path, review.userHome))
		}
	}
	if len(sources) > 0 {
		m.rows = append(m.rows, reviewRow{label: "Hook files", values: sources})
	}
	m.checks = reviewChecklist(cfg, review, at)
	return m
}

func showSetupReview(p *prompter, cfg config.Config, review setupReview) (blocked bool) {
	p.reviewModel = buildSetupReviewModel(cfg, review, p.clock())
	renderSetupReview(p, p.reviewModel, false)
	for _, check := range p.reviewModel.checks {
		if check.mark == symbolFail {
			blocked = true
		}
	}
	return blocked
}

type reviewCompactSetupLabel string

const (
	reviewCompactDestination  reviewCompactSetupLabel = "Destination"
	reviewCompactHookFiles    reviewCompactSetupLabel = "Hook files"
	reviewCompactSources      reviewCompactSetupLabel = "Sources"
	reviewCompactMachine      reviewCompactSetupLabel = "Machine"
	reviewCompactCodexSources reviewCompactSetupLabel = "Codex sources"
	reviewCompactStarts       reviewCompactSetupLabel = "Starts"
	reviewCompactHistory      reviewCompactSetupLabel = "History"
	reviewCompactCopies       reviewCompactSetupLabel = "Copies"
	reviewCompactHooks        reviewCompactSetupLabel = "Hooks"
	reviewCompactExceptions   reviewCompactSetupLabel = "Exceptions"
	reviewCompactR2Keys       reviewCompactSetupLabel = "R2 keys"
	reviewCompactSkipped      reviewCompactSetupLabel = "Skipped"
	reviewCompactImported     reviewCompactSetupLabel = "Imported"
	reviewCompactCodexCapture reviewCompactSetupLabel = "Codex capture"
	reviewCompactApps         reviewCompactSetupLabel = "Apps"
	reviewCompactProjects     reviewCompactSetupLabel = "Projects"
	reviewCompactSkills       reviewCompactSetupLabel = "Skills"
	reviewCompactStorage      reviewCompactSetupLabel = "Storage"
)

func reviewCompactReviewRows(m *setupReviewModel) []reviewRow {
	var rows []reviewRow
	for _, row := range m.rows {
		switch reviewCompactSetupLabel(row.label) {
		case reviewCompactDestination, reviewCompactHookFiles, reviewCompactSources:
			continue
		case reviewCompactMachine:
			if !m.changed[row.label] {
				continue
			}
		case reviewCompactCodexSources, reviewCompactStarts, reviewCompactHistory, reviewCompactCopies, reviewCompactHooks, reviewCompactExceptions, reviewCompactR2Keys, reviewCompactSkipped, reviewCompactImported, reviewCompactCodexCapture:
			if !m.changed[row.label] {
				if reviewCompactSetupLabel(row.label) != reviewCompactImported && reviewCompactSetupLabel(row.label) != reviewCompactSkipped && (reviewCompactSetupLabel(row.label) != reviewCompactCodexCapture || (m.cfg.Discovery != nil && m.cfg.Discovery.Enabled)) {
					continue
				}
			}
		case reviewCompactApps, reviewCompactProjects, reviewCompactSkills, reviewCompactStorage:
			// These essentials stay in the compact review.
		}
		row.values = slices.Clone(row.values)
		if !m.changed[row.label] {
			switch reviewCompactSetupLabel(row.label) {
			case reviewCompactApps:
				row.values = []string{friendlyApps(m.cfg.Harnesses)}
			case reviewCompactProjects:
				if len(row.values) >= 3 {
					row.values = []string{fmt.Sprintf("%d included projects (paths in Details)", len(row.values))}
				}
			case reviewCompactSkills:
				row.values = []string{setupSkillScope(m.cfg)}
				row.detail = ""
			case reviewCompactStorage:
				row.detail = ""
			case reviewCompactDestination, reviewCompactHookFiles, reviewCompactSources, reviewCompactMachine, reviewCompactCodexSources, reviewCompactStarts, reviewCompactHistory, reviewCompactCopies, reviewCompactHooks, reviewCompactExceptions, reviewCompactR2Keys, reviewCompactSkipped, reviewCompactImported, reviewCompactCodexCapture:
				// Preserve the already compact values for other rows.
			}
		}
		if row.label == "Codex scope" && !m.changed[row.label] {
			row.label = "Codex"
		}
		rows = append(rows, row)
	}
	return rows
}

func renderSetupReview(p *prompter, m *setupReviewModel, details bool) {
	if details {
		p.setupHeading("Full settings and privacy")
	} else {
		title := "Review and start"
		if m.review.reconfiguring {
			title = "Review your changes"
		}
		p.setupStep(3, title)
	}
	rows := m.rows
	if !details {
		rows = reviewCompactReviewRows(m)
	}
	printSetupReviewRows(p, rows, m)
	if details {
		printReviewChecklist(p, m.checks)
		printReviewNotes(p)
		p.renderer().block(m.implications)
		return
	}
	for _, check := range m.checks {
		if check.mark == symbolOK && check.label != "Storage connected" && check.label != "Bucket is private" {
			continue
		}
		text := check.label
		if check.mark != symbolOK && check.detail != "" {
			text += " · " + check.detail
		}
		more := slices.Clone(check.more)
		if check.link != "" {
			more = append(more, check.link)
		}
		terminal.Println(p.out, p.style.hang("  "+check.mark+" ", text))
		for _, line := range more {
			terminal.Println(p.out, p.style.hang("    ", line))
		}
	}
	p.warn("Setup imports the last 7 days of included projects; older history is separate.")
	p.warn("Sensitive text may remain after filtering.")
	p.renderer().block(m.implications)
}

func printSetupReviewRows(p *prompter, rows []reviewRow, m *setupReviewModel) int {
	width := 0
	for _, row := range rows {
		width = max(width, visibleWidth(row.label))
	}
	cols := p.renderer().capabilities().Width
	if cols <= 0 {
		cols = p.style.width
	}
	if cols <= 0 {
		cols = 80
	}
	changed := 0
	for _, row := range rows {
		mark := "  "
		if m.changed[row.label] {
			changed++
			mark = "* "
		}
		values := row.values
		if len(values) == 0 {
			values = []string{"none"}
		}
		for i, value := range values {
			label := ""
			if i == 0 {
				label = row.label
				if row.detail != "" {
					value += " · " + row.detail
				}
			}
			prefix := mark + label + strings.Repeat(" ", width-visibleWidth(label)) + "  "
			if cols < 60 || visibleWidth(prefix) > cols/2 {
				if label != "" {
					terminal.Println(p.out, mark+label)
				}
				prefix = "    "
			}
			terminal.Println(p.out, p.style.hang(prefix, value))
			mark = "  "
		}
		if m.changed[row.label] {
			was := m.before[row.label].text()
			if was == "" {
				was = "not set"
			}
			terminal.Println(p.out, p.style.hang("    ", "was "+was))
		}
	}
	return changed
}

func showSetupReviewDetails(p *prompter, env Env, errOut io.Writer) error {
	if p.reviewModel == nil {
		return nil
	}
	var out bytes.Buffer
	details := newPrompter(strings.NewReader(""), &out)
	defer details.close()
	details.style = p.style
	details.now = p.now
	renderSetupReview(details, p.reviewModel, true)
	release := p.suspendPrompts(true)
	err := withPager(context.Background(), underlyingWriter(p.out), errOut, env, false, func(w io.Writer) error { _, e := w.Write(out.Bytes()); return e })
	release()
	return err
}

// reviewCheck is one line of the review's checklist: a ✓, ! or ✗, what was
// checked, and what was found, with any further lines under it. A link
// ends the last of those lines.
type reviewCheck struct {
	mark   string
	label  string
	detail string
	more   []string
	link   string
}

// reviewChecklist is what the review found, for cfg: the storage check,
// the bucket's privacy as of at, the hook files, and the step each newly
// included app needs after setup. Ordinary setup follows a successful storage
// check; consent reviews before that probe mark it as not yet checked.
func reviewChecklist(cfg config.Config, review setupReview, at time.Time) []reviewCheck {
	connection := reviewCheck{mark: symbolOK, label: "Storage connected", detail: "write, read, list, delete"}
	if review.storageUnchecked {
		connection = reviewCheck{mark: symbolWarn, label: "Storage not checked yet", detail: "Connection will be checked after you confirm these settings"}
	}
	checks := []reviewCheck{
		connection,
		privacyCheck(cfg, at),
	}
	if len(cfg.Harnesses) > 0 && setupNeedsProject(cfg) {
		checks = append(checks, reviewCheck{mark: symbolFail, label: "Project required", detail: "Use Edit a setting → Projects to include a directory. Only Codex-only all-projects scope permits no included projects."})
	}
	checks = append(checks, hookFilesChecks(cfg.Harnesses, review.hookFiles, review.userHome)...)
	for _, app := range cfg.Harnesses {
		if app == "codex" && cfg.Discovery != nil && cfg.Discovery.Enabled {
			checks = append(checks, reviewCheck{mark: symbolOK, label: "Codex discovery", detail: "supported new tasks do not require hook approval"})
			continue
		}
		// An app whose hooks are installed already has taken its step,
		// unless they move to another file, which it has not approved.
		if review.reconfiguring && containsString(review.existing.Harnesses, app) && review.installedHookFiles[app] == review.hookFiles[app] {
			continue
		}
		if step, ok := appStepAfterSetup[app]; ok {
			checks = append(checks, reviewCheck{mark: symbolWarn, label: appName(app) + " needs one step", detail: step})
		}
	}
	return checks
}

// appStepAfterSetup is what an app needs from the user, after setup, before
// it runs the archive hooks. Apps not listed need nothing.
var appStepAfterSetup = map[string]string{
	"codex": "approve the hooks with /hooks after setup",
}

// privacyCheck reports the saved bucket privacy evidence as of at, so the
// review agrees with status output.
func privacyCheck(cfg config.Config, at time.Time) reviewCheck {
	report := currentBucketPrivacy(cfg, at)
	switch {
	case report.State == "verified_private" && report.Reason == "r2_public_domains_disabled":
		return reviewCheck{mark: symbolOK, label: "Bucket is private", detail: "r2.dev off; no enabled custom domains (checked at setup)"}
	case report.State == "verified_private":
		return reviewCheck{mark: symbolOK, label: "Bucket is private", detail: "all public access blocked"}
	case report.State == "public_or_risky" && report.Reason == "r2_public_access_enabled":
		return reviewCheck{mark: symbolWarn, label: "Bucket is public", detail: "you chose to continue", more: []string{"Turn off public access in the Cloudflare dashboard:"}, link: report.GuidanceURL}
	case report.State == "public_or_risky":
		return reviewCheck{mark: symbolFail, label: "Bucket is public", detail: privacyReasonText(report.Reason), more: []string{"Fix its access before archiving:"}, link: report.GuidanceURL}
	case report.Reason == r2PrivacyUnreadable:
		// Every R2 bucket reads this way, and nothing setup can do changes
		// it, so it is a reminder of what to check.
		return reviewCheck{mark: symbolWarn, label: "Bucket privacy unknown", detail: "check public access in the Cloudflare dashboard"}
	}
	return reviewCheck{mark: symbolWarn, label: "Bucket privacy unknown", detail: privacyReasonText(report.Reason), more: []string{"Make sure public access is off:"}, link: report.GuidanceURL}
}

// hookFilesChecks reports on the hook files of apps, checked as they are
// now rather than taken from the checks before setup's first question: a
// file can change while setup asks, and an app chosen since was not
// checked then. Each file setup cannot edit has a ✗ of its own; the rest
// share one ✓. It reports nothing without apps.
func hookFilesChecks(apps []string, files hooks.Files, userHome string) []reviewCheck {
	var bad []reviewCheck
	var paths []string
	fix := func(string) string { return "Setup edits only plain JSON. Fix the file before you start." }
	for _, check := range hookFileChecks(apps, files, userHome, fix) {
		if !check.OK {
			bad = append(bad, reviewCheck{mark: symbolFail, label: appName(check.App) + " hook file is invalid", detail: check.Detail, more: []string{check.Problem, check.Fix}})
			continue
		}
		paths = append(paths, check.Detail)
	}
	if len(paths) == 0 {
		return bad
	}
	label := "Hook files are valid"
	if len(paths) == 1 {
		label = "Hook file is valid"
	}
	return append(bad, reviewCheck{mark: symbolOK, label: label, detail: strings.Join(paths, ", ")})
}

// printReviewChecklist prints the checklist with its details in a column.
// Only the symbol is colored, and a link, which the user opens.
func printReviewChecklist(p *prompter, checks []reviewCheck) {
	paint := map[string]func(string) string{symbolOK: p.style.ok, symbolWarn: p.style.warn, symbolFail: p.style.fail}
	for _, check := range checks {
		mark := paint[check.mark](check.mark)
		terminal.Println(p.out, p.style.hang(fmt.Sprintf("  %s %-22s  ", mark, check.label), p.style.dim(check.detail)))
		for i, line := range check.more {
			if i == len(check.more)-1 && check.link != "" {
				line += " " + p.style.cmd(check.link)
			}
			if line != "" {
				terminal.Println(p.out, p.style.hang("    ", line))
			}
		}
	}
}

// reviewHookFiles warns when an app's hooks will not go where setup
// installed them last time: this shell's CLAUDE_CONFIG_DIR or CODEX_HOME
// differs from the one setup saw then, and confirming moves the hooks.
// recorded is whether the configuration recorded where setup installed
// them; an earlier release did not, and always used the fixed paths.
func reviewHookFiles(ports agentapi.HooksLookup, p *prompter, apps []string, next, previous hooks.Files, installed []string, recorded bool, userHome string) {
	for _, app := range apps {
		if !containsString(installed, app) || previous[app] == next[app] {
			continue
		}
		var variable string
		if provider, ok := ports.LookupHooks(app); ok {
			if keys := provider.EnvironmentKeys(); len(keys) > 0 {
				variable = strings.Join(keys, ", ")
			}
		}
		if variable == "" {
			p.warn(fmt.Sprintf("%s hooks move to %s from %s.", appName(app), displayPath(next[app], userHome), displayPath(previous[app], userHome)), "The native configuration location differs from when setup last ran.", "To keep them where they are, cancel setup and restore its previous configuration location.")
			continue
		}
		reason := []string{variable + " in this shell differs from when setup last ran.", "To keep them where they are, cancel and run " + p.style.cmd("agent-archive setup")}
		if !recorded {
			reason = []string{"An earlier release installed them at the fixed path,", "and " + variable + " is set in this shell.", "To keep them there, cancel and run " + p.style.cmd("agent-archive setup")}
		}
		p.warn(fmt.Sprintf("%s hooks move to %s from %s.", appName(app), displayPath(next[app], userHome), displayPath(previous[app], userHome)),
			append(reason, "from a shell without "+variable+".")...)
	}
}

// privacyDocURL is the page on what leaves the machine and what filtering
// removes.
const privacyDocURL = "https://github.com/wangjohn/agent-archive/blob/main/docs/security/privacy.md"

// printReviewNotes prints the caveat that always applies, last and dim,
// after the hint on changing what setup chose, when there is one.
func printReviewNotes(p *prompter) {
	terminal.Println(p.out, "")
	if p.reviewHint != "" {
		terminal.Println(p.out, p.style.dim(p.style.hang("  ", p.reviewHint)))
	}
	terminal.Println(p.out, p.style.dim(p.style.hang("  ", "Filtering is best effort; sensitive text may remain in archived sessions.")))
	terminal.Println(p.out, p.style.hang("  ", p.style.dim("What leaves your machine:")+" "+p.style.cmd(privacyDocURL)))
}

// printReviewPrivacy reports the saved bucket privacy evidence as of the
// prompter's clock, so the review screen agrees with status output.
func printReviewPrivacy(p *prompter, cfg config.Config) {
	report := currentBucketPrivacy(cfg, p.clock())
	switch {
	case report.State == "verified_private" && report.Reason == "r2_public_domains_disabled":
		p.item(p.style.ok("✓"), "Bucket privacy: r2.dev off and no enabled custom domains at the last check.", nil)
	case report.State == "verified_private":
		p.item(p.style.ok("✓"), "Bucket privacy: native public access blocked at the last check.", nil)
	case report.State == "public_or_risky" && report.Reason == "r2_public_access_enabled":
		p.warn("Bucket privacy: R2 public access was enabled at the last check.", "Turn it off in the Cloudflare dashboard: "+report.GuidanceURL)
	case report.State == "public_or_risky":
		p.item(p.style.fail("!"), p.style.fail("The bucket looks public ("+privacyReasonText(report.Reason)+")."), []string{"Fix its access before archiving: " + report.GuidanceURL})
	case report.Reason == r2PrivacyUnreadable:
		// Every R2 bucket reads this way, and nothing setup can do changes
		// it, so it is a reminder rather than a warning.
		p.note("Bucket privacy not verified. Check that public access is disabled for the bucket in the Cloudflare dashboard.")
	default:
		p.warn("Bucket privacy not verified: "+privacyReasonText(report.Reason)+".", "Make sure public access is off: "+report.GuidanceURL)
	}
}

// r2PrivacyUnreadable is the privacy reason when R2's object key is the only
// credential available: it cannot read public-access settings.
const r2PrivacyUnreadable = "r2_management_credentials_not_configured"

func privacyReasonText(reason string) string {
	//lint:ignore LV1001 reason codes come from package storage, and unknown ones are shown as words
	switch reason {
	case r2PrivacyUnreadable:
		return "R2 storage keys cannot read public-access settings"
	case "inspection_unavailable":
		return "public-access settings could not be read"
	case "public_access_controls_not_fully_verified":
		return "not every public-access setting could be confirmed"
	case "inspection_stale":
		return "the last check is more than a day old"
	case "r2_public_access_enabled":
		return "R2 public access is enabled"
	case "r2_public_access_not_fully_checked":
		return "not every R2 public-access setting could be checked"
	case "storage_configuration_changed":
		return "storage changed since the last check"
	case "public_bucket_policy":
		return "a bucket policy allows public access"
	case "public_bucket_acl":
		return "the bucket ACL allows public access"
	}
	return strings.ReplaceAll(reason, "_", " ")
}

type setupReviewAction string

const (
	setupReviewStart      setupReviewAction = "start"
	setupReviewYes        setupReviewAction = "yes"
	setupReviewNo         setupReviewAction = "no"
	setupReviewCancel     setupReviewAction = "cancel"
	setupReviewCheckAlias setupReviewAction = "c"
	setupReviewCheck      setupReviewAction = "check"
	setupReviewEdit       setupReviewAction = "edit"
	setupReviewDetails    setupReviewAction = "details"
	setupReviewMachine    setupReviewAction = "machine"
)

// reviewAction asks the final confirmation, returning start, edit, or cancel.
// y, n, and e still work for scripted input. When the checklist is blocked
// (a row is ✗), starting is neither offered nor accepted: the first choice
// checks again instead, returning check.
func reviewAction(p *prompter, reconfiguring, blocked, offerName bool) (string, error) {
	label, first := "Start archiving?", option{"start", "Start archiving"}
	if reconfiguring {
		label, first = "Save these changes?", option{"start", "Save changes"}
	}
	if blocked {
		label, first = "Fix the blocking checks first.", option{"check", "Check again"}
	}
	secondary := []actionOption{{"details", "d", "Full settings and privacy"}, {"cancel", "q", "Cancel; keep draft"}}
	if offerName {
		secondary = append(secondary, actionOption{"machine", "m", "Name this machine"})
	}
	aliases := []option{{"no", ""}}
	if blocked {
		aliases = append(aliases, option{"c", ""})
	}
	if !blocked {
		aliases = append(aliases, option{"yes", ""})
	}
	choice, err := p.guidedChoice(promptModel{Question: label, Default: first.Key, Primary: []option{first, {"edit", "Edit a setting"}}, Secondary: secondary, Aliases: aliases, ResolveReceipt: func(key string) string {
		switch setupReviewAction(key) {
		case setupReviewYes, setupReviewStart:
			return first.Label
		case setupReviewNo, setupReviewCancel:
			return "Cancel; keep draft"
		case setupReviewCheckAlias, setupReviewCheck:
			return "Check again"
		case setupReviewEdit:
			return "Edit a setting"
		case setupReviewDetails:
			return "Full settings and privacy"
		case setupReviewMachine:
			return "Name this machine"
		}
		return key
	}})
	if choice == "c" {
		choice = "check"
	}
	if choice == "yes" {
		choice = "start"
	}
	if choice == "no" {
		choice = "cancel"
	}
	return choice, err
}

// promptStopImported offers to stop publishing each app that has only
// imported sessions: one backfill imported without its hooks installed, and
// which setup is not installing hooks for now. Stopping removes it from
// ImportedHarnesses when setup commits, so its imports are no longer
// published; sessions already in the bucket stay until retention removes
// them.
func promptStopImported(p *prompter, draft *setupDraft) error {
	for _, app := range draft.Config.ImportedHarnesses {
		if containsString(draft.Config.Harnesses, app) || containsString(draft.StopImported, app) {
			continue
		}
		keep, err := p.setupYesNo(fmt.Sprintf("Keep publishing %s sessions imported by backfill?", appName(app)), true)
		if err != nil {
			return err
		}
		if !keep {
			draft.StopImported = append(draft.StopImported, app)
		}
	}
	return nil
}

// offerStopImported refreshes the draft's list from the committed
// configuration, which is where backfill writes it, then prompts.
func offerStopImported(p *prompter, draft *setupDraft, committed config.Config) error {
	draft.Config.ImportedHarnesses = carriedImportedHarnesses(committed.ImportedHarnesses, draft.Config.Harnesses, draft.StopImported)
	return promptStopImported(p, draft)
}

// editSetupReview asks which setting to change and asks for it again.
// known, when not nil, lists the projects the apps' history mentions.
func editSetupReview(available []string, p *prompter, draft *setupDraft, userHome string, backfilled map[string]bool, known func(config.Config) []backfill.KnownProject) error {
	// Whoever opens the edit menu has found it; the hint about it would be
	// stale beside what they change.
	p.reviewHint = ""
	choices := []option{
		{"apps", "Apps to include"},
		{"projects", "Projects to include"},
		{"sessions", "All sessions or only sessions using skills"},
		{"skills", "Skill evidence: none, metadata, or body"},
		{"retention", "How long sessions are kept"},
		{"storage", "Bucket or credentials"},
		{"prefix", "Folder inside the bucket"},
	}
	if draft.Config.Storage.Provider == credentials.ProviderS3 {
		choices = append(choices, option{"region", "AWS bucket region"})
	}
	if containsString(draft.Config.Harnesses, "codex") {
		choices = append(choices, option{"discovery", "Automatic Codex discovery"}, option{"codex-scope", "Codex capture scope"})
		if draft.Config.EffectiveCodexCaptureScope() == config.CodexAllProjects {
			choices = append(choices, option{"codex-exceptions", "Codex project exceptions"})
		}
	}
	choices = append(choices, option{"back", "Nothing, go back to the review"})
	choice, err := p.setupMenu("What would you like to change?", "back", choices...)
	if err != nil {
		return err
	}
	//lint:ignore LV1001 menu keys are the option keys listed just above
	switch choice {
	case "codex-exceptions":
		err = promptCodexExceptions(p, &draft.Config, userHome)
	case "codex-scope":
		err = promptCodexCaptureScope(p, &draft.Config)
	case "discovery":
		enabled := draft.Config.Discovery != nil && draft.Config.Discovery.Enabled
		enabled, err = p.setupYesNo("Enable automatic Codex discovery? Recent indistinguishable copies may be captured.", enabled)
		if err == nil {
			enableDiscovery(&draft.Config, enabled)
			draft.DiscoveryReviewed = true
		}
	case "apps":
		if err = chooseHarnesses(available, p, nil, &draft.Config); err != nil {
			return err
		}
		err = promptStopImported(p, draft)
	case "projects":
		if codexOnlyAllProjects(draft.Config) {
			p.note("Codex captures all current and future projects except explicit exceptions.")
			return promptCodexExceptions(p, &draft.Config, userHome)
		}
		p.projectConfig = draft.Config
		var offered []backfill.KnownProject
		if known != nil {
			offered = known(draft.Config)
		}
		projects, e := promptProjects(p, draft.Config.Archive.Projects, backfilled, offered, userHome)
		if e != nil {
			return e
		}
		draft.Config.Archive.Projects = projects
	case "sessions":
		all, e := p.setupYesNo("Save sessions even when no skills are used?", !draft.Config.RequireSkillUse)
		if e != nil {
			return e
		}
		draft.Config.RequireSkillUse = !all
	case "skills":
		mode, e := p.setupMenu("What skill evidence may be uploaded? User-level skill roots outside selected projects may be scanned.", string(draft.Config.EffectiveSkillEvidence()),
			option{"none", "None (skill use in transcripts can still be detected)"},
			option{"metadata", "Names and filtered hashes; no SKILL.md body"},
			option{"body", "Filtered SKILL.md snapshots"})
		if e != nil {
			return e
		}
		config.SetSkillEvidence(&draft.Config, config.SkillEvidence(mode))
	case "retention":
		draft.RetentionEdited = true
		draft.Config.RetentionDays, err = p.setupRetentionDays(draft.Config.RetentionDays)
		if draft.Config.RetentionDays == 0 {
			draft.Config.RetentionProtection = true
		}
	case "storage":
		draft.Step = 1
	case "prefix":
		prefix, e := p.guidedText(promptModel{Question: "Folder inside the bucket", Label: "Answer", Default: draft.Config.Storage.Prefix, Receipt: "Folder inside the bucket", Validate: func(prefix string) error {
			if _, err := storage.Prefix(prefix, "test"); err != nil || prefix == "" {
				return fmt.Errorf("use a relative folder name, such as agent-archive/; do not include .. or a leading slash")
			}
			return nil
		}})
		if e != nil {
			return e
		}
		draft.Config.Storage.Prefix = prefix
	case "region":
		draft.Config.Storage.Region, err = promptRegion(p, "Bucket region", draft.Config.Storage.Region)
	}
	return err
}

func setupSkillScope(cfg config.Config) string {
	switch cfg.EffectiveSkillEvidence() {
	case config.SkillEvidenceNone:
		return "None"
	case config.SkillEvidenceBody:
		return "Filtered snapshots, including user folders"
	case config.SkillEvidenceMetadata:
		return "Metadata, including user folders"
	default:
		return "Metadata, including user folders"
	}
}
