package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

const maxSkillInventoryBytes = 256 << 10

var errSkillInventoryFile = errors.New("installed skill metadata is unavailable or changed")

type confinedSkillInspection struct {
	root      *os.Root
	relative  string
	info      os.FileInfo
	ancestors map[string]os.FileInfo
}

func privateSkillEntry(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0 && skillEntryOwned(info)
}

// inspectConfinedSkill never follows an unqualified child or scans a directory.
// os.Root anchors the subsequent open against concurrent ancestor substitution.
func inspectConfinedSkill(root *os.Root, relative string) (*confinedSkillInspection, error) {
	if !validSkillDeclaration(agentapi.SkillFile{ID: "inventory", Path: relative}) {
		return nil, errSkillInventoryFile
	}
	out := &confinedSkillInspection{root: root, relative: relative, ancestors: map[string]os.FileInfo{}}
	parts := strings.Split(relative, "/")
	for i := range len(parts) {
		name := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !privateSkillEntry(info) {
			return nil, errSkillInventoryFile
		}
		if i == len(parts)-1 {
			if !info.Mode().IsRegular() {
				return nil, errSkillInventoryFile
			}
			out.info = info
		} else {
			if !info.IsDir() {
				return nil, errSkillInventoryFile
			}
			out.ancestors[name] = info
		}
	}
	if !out.stable() {
		return nil, errSkillInventoryFile
	}
	return out, nil
}

func sameSkillEntry(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && privateSkillEntry(b)
}

func (i *confinedSkillInspection) stable() bool {
	for path, prior := range i.ancestors {
		now, err := i.root.Lstat(path)
		if err != nil || !sameSkillEntry(prior, now) {
			return false
		}
	}
	now, err := i.root.Lstat(i.relative)
	return err == nil && sameSkillEntry(i.info, now)
}

func (i *confinedSkillInspection) read() ([]byte, error) {
	if !i.stable() {
		return nil, errSkillInventoryFile
	}
	f, err := openConfinedSkillFile(i.root, i.relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, statErr := f.Stat()
	if statErr != nil || !sameSkillEntry(i.info, opened) {
		return nil, errSkillInventoryFile
	}
	data, err := readSkillHandle(f, i.info.Size(), i.info)
	if err != nil || !i.stable() {
		return nil, errSkillInventoryFile
	}
	return data, nil
}

func inventoryFile(budget *skillReadBudget, root *os.Root, path string) ([]byte, *confinedSkillInspection, error) {
	inspected, err := inspectConfinedSkill(root, path)
	if err != nil {
		return nil, nil, err
	}
	if inspected.info.Size() > maxSkillInventoryBytes {
		return nil, inspected, errSkillBudget
	}
	raw, err := budget.readUsing(inspected.info, true, inspected.read)
	if err != nil || !inspected.stable() {
		return nil, inspected, errSkillInventoryFile
	}
	return raw, inspected, nil
}

// observeInstalledSkills expands only one provider-declared installed registry.
// Raw registry/manifests and private locator digests die with this observation.
func observeInstalledSkills(options SkillOptions, budget *skillReadBudget) []archive.SupplementalEvidence {
	gap := func(status string, complete bool) []archive.SupplementalEvidence {
		return []archive.SupplementalEvidence{inventoryObservation(options.Harness, "user_muse_plugins", status, complete, options.ObservedAt)}
	}
	input := options.InventoryInput
	if !filepath.IsAbs(input.Boundary) || input.Registry != "plugins/installed.json" {
		return gap("unreadable", false)
	}
	baseInfo, err := os.Lstat(input.Boundary)
	if errors.Is(err, os.ErrNotExist) {
		return gap("absent", true)
	}
	if err != nil || !privateSkillEntry(baseInfo) || !baseInfo.IsDir() {
		return gap("unreadable", false)
	}
	base, err := os.OpenRoot(input.Boundary)
	if err != nil {
		return gap("unreadable", false)
	}
	defer func() { _ = base.Close() }()
	opened, err := base.Stat(".")
	if err != nil || !sameSkillEntry(baseInfo, opened) {
		return gap("unreadable", false)
	}
	raw, registry, err := inventoryFile(budget, base, input.Registry)
	if errors.Is(err, os.ErrNotExist) {
		return gap("absent", true)
	}
	if err != nil {
		return gap("unreadable", false)
	}
	catalog, err := options.Inventory.InterpretInventory(raw)
	if err != nil || len(catalog.Packages) > maxSkillsPerRoot {
		return gap("unreadable", false)
	}
	complete := !catalog.Incomplete
	var observations []archive.SupplementalEvidence
	for _, pkg := range catalog.Packages {
		observed, packageComplete := observeInstalledSkillPackage(options, budget, base, pkg)
		if !packageComplete {
			complete = false
		}
		observations = append(observations, observed...)
	}
	// A replacement registry must not authorize mixed-generation package reads.
	// Its original descriptor and all inspected ancestors must still match.
	finalBase, baseErr := os.Lstat(input.Boundary)
	if baseErr != nil || !sameSkillEntry(baseInfo, finalBase) || !registry.stable() {
		return gap("unreadable", false)
	}
	observations = append(observations, gap("present", complete)...)
	return observations
}

func observeInstalledSkillPackage(options SkillOptions, budget *skillReadBudget, base *os.Root, pkg agentapi.SkillInstalledPackage) ([]archive.SupplementalEvidence, bool) {
	input := options.InventoryInput

	if !validSkillDeclaration(agentapi.SkillFile{ID: pkg.ID, Path: pkg.Directory}) {
		return nil, false
	}
	manifest, inspected, readErr := inventoryFile(budget, base, pkg.Directory+"/.muse-plugin/plugin.json")
	if readErr != nil {
		return nil, false
	}
	declared, parseErr := options.Inventory.InterpretManifest(input, pkg, manifest)
	expected := filepath.Join(input.Boundary, filepath.FromSlash(pkg.Directory))
	if parseErr != nil || declared.Layout != agentapi.SkillDeclaredFiles || declared.Path != expected || declared.UserBoundary != input.Boundary || declared.ProjectBoundary != "" || !validSkillScope(declared.Scope) {
		return nil, false
	}
	// The package descriptor is derived from the validated installed record;
	// source.path and arbitrary cache_path values are never read targets.
	packageRoot, openErr := base.OpenRoot(pkg.Directory)
	if openErr != nil {
		return nil, false
	}
	packageInfo, statErr := packageRoot.Stat(".")
	namedInfo, nameErr := base.Lstat(pkg.Directory)
	if statErr != nil || nameErr != nil || !sameSkillEntry(namedInfo, packageInfo) {
		_ = packageRoot.Close()
		return nil, false
	}
	beforeComplete := len(declared.Files) <= budget.declarations
	observed, observeErr := observeRoot(options.Harness, skillRoot{layout: declared.Layout, files: declared.Files, path: declared.Path, scope: declared.Scope, user: declared.UserBoundary, confined: packageRoot}, options.ObservedAt, budget, options.Mode)
	afterPackage, afterErr := base.Lstat(pkg.Directory)
	stable := afterErr == nil && sameSkillEntry(packageInfo, afterPackage) && inspected.stable()
	_ = packageRoot.Close()
	if observeErr != nil || !stable {
		return nil, false
	}
	return observed, beforeComplete
}
