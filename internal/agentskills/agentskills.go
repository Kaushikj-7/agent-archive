// Package agentskills is the agent skills setup installs into Claude Code,
// Codex, and Cursor: the Registry of skills, each rendered per destination
// (a Skill), such as `handoff`, which runs `agent-archive handoff --to
// <agent>` from inside an agent. It says what each file holds (Files) and
// plans writing and removing them as setup-journal changes (PlanInstall,
// PlanRemoval). Nothing records the files once setup commits, so a file is
// setup's by its content alone (a marker line), and an installation's by
// the data directory its command names; anything else at the path is left
// alone. Stale finds the files an upgrade has outdated. The plan is
// dev/proposals/implemented/agent-skill.md.
package agentskills

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/skillownership"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// File is one skill file: which skill it is (Skill.Name), where it goes,
// what it holds, and the harnesses (values of config.Config.Harnesses) that
// read it.
type File struct {
	Skill     string
	Harnesses []string
	Path      string
	Content   []byte
	Boundary  string
}

// ports.SkillAgents() is every harness a skill file is written for.

// Files is the skill files of every skill in Registry for harnesses, in
// Registry order (each skill's Claude Code file, then its shared one),
// running executable with AGENT_ARCHIVE_HOME=dataHome, or with none for the
// default data directory (dataHome ""), as the installation's hooks do: the
// agent's environment need not have it. Claude Code reads the skills in its
// configuration directory claudeDir (~/.claude, or $CLAUDE_CONFIG_DIR);
// Codex and Cursor both read ~/.agents/skills (Cursor reads ~/.claude/skills
// too, so the instructions suit any of the three).
func Files(ports agentapi.SkillsLookup, userHome, claudeDir string, harnesses []string, executable, dataHome string) []File {
	return skillFiles(ports, Registry, userHome, claudeDir, harnesses, executable, dataHome)
}

// skillFiles is Files for the skills in registry.
func skillFiles(ports agentapi.SkillsLookup, registry []Skill, userHome, claudeDir string, harnesses []string, executable, dataHome string) []File {
	files, _ := renderedSkillFiles(ports, registry, userHome, claudeDir, harnesses, executable, dataHome)
	return files
}

type skillDestination struct {
	name        string
	destination agentapi.SkillDestination
}

func destinations(ports agentapi.SkillsLookup, userHome, claudeDir string, harnesses []string) []skillDestination {
	var out []skillDestination
	for _, name := range ports.SkillAgents() {
		if harnesses != nil && !contains(harnesses, name) {
			continue
		}
		if provider, ok := ports.LookupSkills(name); ok {
			out = append(out, skillDestination{name: name, destination: provider.Destination(agentapi.SkillLocations{UserHome: userHome, ClaudeDirectory: claudeDir})})
		}
	}
	return out
}

func renderedSkillFiles(ports agentapi.SkillsLookup, registry []Skill, userHome, claudeDir string, harnesses []string, executable, dataHome string) ([]File, error) {
	// An empty selection differs from inventorying every optional provider.
	if len(harnesses) == 0 {
		return nil, nil
	}
	return renderDestinations(registry, destinations(ports, userHome, claudeDir, harnesses), executable, dataHome, true)
}

// renderDestinations renders each template once while keeping every inventory
// ownership alternative. Selected providers alone must agree at shared paths.
func renderDestinations(registry []Skill, providers []skillDestination, executable, dataHome string, merge bool) ([]File, error) {
	var files []File
	for _, skill := range registry {
		indexes := map[string]int{}
		var rendered [2][]byte
		var ready [2]bool
		for _, provider := range providers {
			d := provider.destination
			path := filepath.Join(d.Directory, skill.Name, "SKILL.md")
			dest := Shared
			if d.ClaudeFrontmatter {
				dest = Claude
			}
			if !ready[dest] {
				rendered[dest], ready[dest] = skill.Render(dest, executable, dataHome), true
			}
			content := rendered[dest]
			if merge {
				if i, ok := indexes[path]; ok {
					if !bytes.Equal(files[i].Content, content) {
						return nil, fmt.Errorf("skill %s has conflicting content at %s", skill.Name, path)
					}
					files[i].Harnesses = append(files[i].Harnesses, provider.name)
					continue
				}
				indexes[path] = len(files)
			}
			files = append(files, File{Skill: skill.Name, Harnesses: []string{provider.name}, Path: path, Content: bytes.Clone(content), Boundary: d.Boundary})
		}
	}
	return files, nil
}

// inventorySkillFiles retains each provider's alternative template and ownership
// policy. Different templates at the same path are only a conflict when selected
// together for installation; inventory must still find and remove either one.
func inventorySkillFiles(ports agentapi.SkillsLookup, registry []Skill, userHome, claudeDir, executable, dataHome string) []File {
	files, _ := renderDestinations(registry, destinations(ports, userHome, claudeDir, nil), executable, dataHome, false)
	return files
}

// marker is the line every skill file carries. A file with it is setup's,
// whichever release or executable path wrote it; the person keeps a file of
// their own by deleting the line.
const marker = skillownership.Marker

// dataHomeVariable begins a command line naming a relocated data directory.
const dataHomeVariable = "AGENT_ARCHIVE_HOME="

// commandLine is the command a skill runs, less its arguments.
func commandLine(executable, dataHome string) string {
	if dataHome == "" {
		return shellQuote(executable)
	}
	return dataHomeVariable + shellQuote(dataHome) + " " + shellQuote(executable)
}

// shellQuote quotes s for the shell only when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// owned reports whether content is setup's for the installation in
// dataHome: it carries marker, as what Files renders for any executable
// does, and its command names dataHome, or no data directory for the
// default installation: another installation sharing this HOME, with hook
// files of its own, keeps its file.
func contains(names []string, name string) bool { return slices.Contains(names, name) }

// PlanInstall plans the skill files for harnesses, running executable:
// each is written where there is none and replaced only while it is setup's
// (see owned), and a file of setup's for a harness no longer chosen is
// removed, as is one in previousClaudeDir, where Claude Code's
// configuration was when setup last ran. Any other file, links and
// directories included, is left alone; a wanted path holding one is
// returned in foreign.
func PlanInstall(ports agentapi.SkillsLookup, userHome, claudeDir string, harnesses []string, executable, dataHome, previousClaudeDir string) (changes []hooks.Change, foreign []string, err error) {
	return planInstall(ports, Registry, userHome, claudeDir, harnesses, executable, dataHome, previousClaudeDir)
}

// planInstall is PlanInstall for the skills in registry.
func planInstall(ports agentapi.SkillsLookup, registry []Skill, userHome, claudeDir string, harnesses []string, executable, dataHome, previousClaudeDir string) (changes []hooks.Change, foreign []string, err error) {
	wanted := map[string]bool{}
	observations := skillObservations{}
	files, err := renderedSkillFiles(ports, registry, userHome, claudeDir, harnesses, executable, dataHome)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range files {
		wanted[f.Path] = true
		observed := observations.file(f.Path, true)
		if observed.ReadError != nil {
			return nil, nil, observed.ReadError
		}
		provider, _ := ports.LookupSkills(f.Harnesses[0])
		inspection, err := provider.Inspect(agentapi.SkillInspectionRequest{File: observed, Content: f.Content, DataHome: dataHome})
		if err != nil {
			return nil, nil, err
		}
		if inspection.State == agentapi.HookForeign {
			foreign = append(foreign, f.Path)
			continue
		}
		planned, err := provider.Plan(agentapi.SkillPlanRequest{Action: agentapi.SkillInstall, File: observed, Content: f.Content, DataHome: dataHome})
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, planned...)

	}
	currentFiles := inventorySkillFiles(ports, registry, userHome, claudeDir, "", "")
	previousFiles := currentFiles
	if previousClaudeDir != claudeDir {
		previousFiles = inventorySkillFiles(ports, registry, userHome, previousClaudeDir, "", "")
	}
	for _, f := range append(currentFiles, previousFiles...) {
		if wanted[f.Path] {
			continue
		}
		change, found, err := planRemoval(ports, f, dataHome, observations)
		if err != nil {
			return nil, nil, err
		}
		if found {
			wanted[f.Path] = true // remove each path once, using a provider that owns it
			changes = append(changes, change)
		}
	}
	return changes, foreign, nil
}

// PlanRemoval plans removing every skill file of setup's, for uninstall. A
// file at one of their paths that is not setup's is returned in kept and
// left alone.
func PlanRemoval(ports agentapi.SkillsLookup, userHome, claudeDir, dataHome string) (changes []hooks.Change, kept []string, err error) {
	return planRemovalOf(ports, Registry, userHome, claudeDir, dataHome)
}

// planRemovalOf is PlanRemoval for the skills in registry.
func planRemovalOf(ports agentapi.SkillsLookup, registry []Skill, userHome, claudeDir, dataHome string) (changes []hooks.Change, kept []string, err error) {
	files := inventorySkillFiles(ports, registry, userHome, claudeDir, "", "")
	removed := map[string]bool{}
	observations := skillObservations{}
	for _, f := range files {
		if removed[f.Path] {
			continue
		}
		change, found, err := planRemoval(ports, f, dataHome, observations)
		if err != nil {
			return nil, nil, err
		}
		if found {
			removed[f.Path] = true
			changes = append(changes, change)
		}
	}
	for _, f := range files {
		if !removed[f.Path] && !slices.Contains(kept, f.Path) {
			if observations.file(f.Path, false).Present {
				kept = append(kept, f.Path)
			}
		}
	}
	return changes, kept, nil
}

// planRemoval is the Change deleting path, when it is setup's.
func planRemoval(ports agentapi.SkillsLookup, f File, dataHome string, observations skillObservations) (hooks.Change, bool, error) {
	observed := observations.file(f.Path, true)
	if observed.ReadError != nil {
		return hooks.Change{}, false, observed.ReadError
	}
	provider, ok := ports.LookupSkills(f.Harnesses[0])
	if !ok {
		return hooks.Change{}, false, nil
	}
	planned, err := provider.Plan(agentapi.SkillPlanRequest{Action: agentapi.SkillRemove, File: observed, DataHome: dataHome})
	if err != nil || len(planned) == 0 {
		return hooks.Change{}, false, err
	}
	return planned[0], true, nil
}

// Installed is the skill files of setup's that are there now, for status.
func Installed(ports agentapi.SkillsLookup, userHome, claudeDir, dataHome string) []string {
	return installedOf(ports, Registry, userHome, claudeDir, dataHome)
}

// installedOf is Installed for the skills in registry.
func installedOf(ports agentapi.SkillsLookup, registry []Skill, userHome, claudeDir, dataHome string) []string {
	var paths []string
	observations := skillObservations{}
	for _, f := range inventorySkillFiles(ports, registry, userHome, claudeDir, "", "") {
		if inspection, err := inspectFile(ports, f, dataHome, observations); err == nil && inspection.State == agentapi.HookOwned && !slices.Contains(paths, f.Path) {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

// Stale is the skill files of setup's that are there but differ from what
// this release renders for executable: written by an earlier release or
// executable, so setup (which replaces a file it owns) refreshes them. It
// is a subset of Installed, for status. With no executable recorded there
// is nothing to compare with, and it reports none.
func Stale(ports agentapi.SkillsLookup, userHome, claudeDir, executable, dataHome string) []string {
	return staleOf(ports, Registry, userHome, claudeDir, executable, dataHome)
}

// staleOf is Stale for the skills in registry.
func staleOf(ports agentapi.SkillsLookup, registry []Skill, userHome, claudeDir, executable, dataHome string) []string {
	if executable == "" {
		return nil
	}
	var paths []string
	fresh := map[string]bool{}
	observations := skillObservations{}
	for _, f := range inventorySkillFiles(ports, registry, userHome, claudeDir, executable, dataHome) {
		if inspection, err := inspectFile(ports, f, dataHome, observations); err == nil && inspection.State == agentapi.HookOwned {
			if inspection.Stale {
				if !slices.Contains(paths, f.Path) {
					paths = append(paths, f.Path)
				}
			} else {
				fresh[f.Path] = true
			}
		}
	}
	paths = slices.DeleteFunc(paths, func(path string) bool { return fresh[path] })
	return paths
}

// RemoveEmptyDirs removes the directories each skill file's path is in,
// deepest first, while they are empty: what writing a file there created,
// once the file is gone. It stops at the home folder, and at Claude Code's
// configuration directory claudeDir; a directory holding anything is kept,
// with every one above it. A link is never removed: os.Remove would unlink
// it whatever it names.
func RemoveEmptyDirs(ports agentapi.SkillsLookup, userHome, claudeDir string) {
	removeEmptyDirs(ports, Registry, userHome, claudeDir)
}

// removeEmptyDirs is RemoveEmptyDirs for the skills in registry.
func removeEmptyDirs(ports agentapi.SkillsLookup, registry []Skill, userHome, claudeDir string) {
	for _, f := range inventorySkillFiles(ports, registry, userHome, claudeDir, "", "") {
		stop := f.Boundary
		for dir := filepath.Dir(f.Path); dir != filepath.Clean(stop) && local.PathWithin(dir, stop); dir = filepath.Dir(dir) {
			if info, err := os.Lstat(dir); err != nil || !info.IsDir() || os.Remove(dir) != nil {
				break
			}
		}
	}
}

// mode is the permissions of the file at path, for a Change replacing or
// removing it, so that a rollback puts them back with its content.
func mode(path string) os.FileMode {
	if info, err := os.Lstat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0600
}

type fileState int

const (
	missing fileState = iota
	regular
	other
)

// read is the regular file at path, never followed through a link: not
// the file's own, nor the skill's directory's (a skill of the person's own
// linked in, which a write would land in). A linked skills directory above
// it is followed, as hook files are through a dotfile manager's links.
func read(path string) ([]byte, fileState, error) {
	if info, err := os.Lstat(filepath.Dir(path)); err == nil && !info.IsDir() {
		return nil, other, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, missing, nil
	}
	if err != nil {
		return nil, other, err
	}
	if !info.Mode().IsRegular() {
		return nil, other, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, other, err
	}
	return data, regular, nil
}

func inspectFile(ports agentapi.SkillsLookup, f File, dataHome string, observations skillObservations) (agentapi.SkillInspection, error) {
	observed := observations.file(f.Path, false)
	provider, _ := ports.LookupSkills(f.Harnesses[0])
	return provider.Inspect(agentapi.SkillInspectionRequest{File: observed, Content: f.Content, DataHome: dataHome})
}

// skillObservations shares a read within one pure planning or inspection
// operation, while every alternative provider still validates its own policy.
type skillObservations map[string]skillObservation

type skillObservation struct {
	file     agentapi.HookFile
	modeRead bool
}

func (observations skillObservations) file(path string, withMode bool) agentapi.HookFile {
	observation, ok := observations[path]
	if !ok {
		content, state, err := read(path)
		observation.file = agentapi.HookFile{Path: path, Bytes: content, Present: state != missing, Regular: state == regular, ReadError: err}
	}
	if withMode && !observation.modeRead && observation.file.ReadError == nil {
		observation.file.Mode = mode(path)
		observation.modeRead = true
	}
	observations[path] = observation
	file := observation.file
	if !withMode {
		file.Mode = 0
	}
	return file
}
