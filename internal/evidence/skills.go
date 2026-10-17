// Package evidence produces bounded, privacy-filtered observations which are
// kept separate from an application's native transcript.
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

const (
	maxSkillsPerRoot = 256
	maxSkillBytes    = 1 << 20
	maxSnapshotBytes = 4 << 20
	// maxSnapshotBodyBytes caps one archived SKILL.md body. Every session's
	// evidence embeds the bodies of the skills installed when it ran, so an
	// uncapped body is repeated in every bundle. The inventory entry (name,
	// sha256, scope) and the recorded original size stay complete, and the
	// sha256 remains the hash of the whole file (redacted, see
	// ObserveSkills). Moving bodies to content-addressed objects is
	// deferred.
	maxSnapshotBodyBytes = 16 << 10
)

// SkillOptions identifies the only filesystem locations a background
// collection pass may inspect. ProjectRoot is already an explicitly included
// project and UserHome is the local user's home; no repository-wide walk is
// performed.
type SkillOptions struct {
	Harness     string
	ProjectRoot string
	UserHome    string
	ObservedAt  time.Time
	Mode        config.SkillEvidence
	Locations   []agentapi.SkillRoot
}

type skillRoot struct {
	layout agentapi.SkillLayout
	files  []agentapi.SkillFile
	path   string
	scope  string
	user   string
	// project is the project root a project-level skill root belongs to, and
	// empty for a user-level root. It bounds where a project skill's SKILL.md
	// may resolve to (see skillBounds).
	project string
}

// One shared allowance and physical read cache covers every root in an observation.
// Manifest/catalog readers added later must reserve from this same allowance.
type skillReadBudget struct {
	remaining    int64
	declarations int
	cache        []cachedSkill
}

type cachedSkill struct {
	info  os.FileInfo
	bytes []byte
}

type skillCandidate struct {
	path      string
	name      string
	id        string
	valid     bool
	directory bool
}

// ObserveSkills inventories immediate SKILL.md children of documented skill
// roots. Filesystem presence proves installation only. It deliberately does
// not claim that the harness discovered, exposed, or invoked a skill in this
// session. The returned evidence has already passed the archive privacy
// filter and is safe to place in a local upload request or source candidate.
//
// A skills directory the collector cannot read is a coverage gap for that
// root, never a reason to withhold the session: an unreadable root yields an
// "unreadable" inventory and an unreadable entry counts as uninspected.
func ObserveSkills(options SkillOptions) ([]archive.SupplementalEvidence, error) {
	if options.ObservedAt.IsZero() {
		return nil, errors.New("skill observation time is required")
	}
	if options.Mode == config.SkillEvidenceNone {
		return nil, nil
	}
	if options.Mode == "" {
		options.Mode = config.SkillEvidenceBody
	}
	if !config.ValidSkillEvidence(options.Mode) {
		return nil, fmt.Errorf("unsupported skill evidence mode %q", options.Mode)
	}
	roots := skillRoots(options)
	budget := skillReadBudget{remaining: maxSnapshotBytes, declarations: maxSkillsPerRoot}
	var observations []archive.SupplementalEvidence
	for _, root := range roots {
		evidence, err := observeRoot(options.Harness, root, options.ObservedAt, &budget, options.Mode)
		if err != nil {
			return nil, err
		}
		observations = append(observations, evidence...)
	}
	filtered, _, err := archive.FilterSupplementalEvidence(observations)
	if err != nil {
		return nil, fmt.Errorf("filter skill evidence: %w", err)
	}
	return filtered, nil
}

func skillRoots(options SkillOptions) []skillRoot {
	var roots []skillRoot
	for _, declared := range options.Locations {
		duplicate := false
		for _, root := range roots {
			if !sameDirectory(root.path, declared.Path) {
				continue
			}
			// A project rooted at home must not repeat an existing user root.
			if declared.Layout == agentapi.SkillDirectory && root.layout == agentapi.SkillDirectory && declared.UserBoundary == "" && root.user != "" {
				duplicate = true
				break
			}
			if root.scope == declared.Scope && root.user == declared.UserBoundary && root.project == declared.ProjectBoundary && root.layout == declared.Layout {
				if root.layout == agentapi.SkillDirectory || len(root.files) <= maxSkillsPerRoot && slices.Equal(root.files, declared.Files) {
					duplicate = true
					break
				}
			}
		}
		if !duplicate {
			roots = append(roots, skillRoot{path: declared.Path, scope: declared.Scope, user: declared.UserBoundary, project: declared.ProjectBoundary, layout: declared.Layout, files: declared.Files})
		}
	}
	return roots
}

func validSkillScope(value string) bool {
	if len(value) == 0 || len(value) > 160 {
		return false
	}
	for _, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}

func validSkillDeclaration(file agentapi.SkillFile) bool {
	if len(file.ID) == 0 || len(file.ID) > 80 || len(file.Path) == 0 || len(file.Path) > 4096 || !utf8.ValidString(file.Path) || filepath.IsAbs(file.Path) || strings.ContainsAny(file.Path, "\\:\x00") {
		return false
	}
	for _, c := range []byte(file.Path) {
		if c < 32 || c == 127 {
			return false
		}
	}
	for _, part := range strings.Split(file.Path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	for i, c := range []byte(file.ID) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func rootCandidates(root skillRoot, budget *skillReadBudget) ([]skillCandidate, int, error) {
	if root.layout == agentapi.SkillDirectory && len(root.files) == 0 {
		entries, err := os.ReadDir(root.path)
		if err != nil {
			return nil, 0, err
		}
		entries, omitted := boundedSkillEntries(entries)
		out := make([]skillCandidate, 0, len(entries))
		for _, entry := range entries {
			out = append(out, skillCandidate{path: filepath.Join(root.path, entry.Name(), "SKILL.md"), name: entry.Name(), valid: true, directory: entry.IsDir() || entry.Type()&os.ModeSymlink != 0})
		}
		return out, omitted, nil
	}
	if root.layout != agentapi.SkillDeclaredFiles {
		return nil, 0, errors.New("invalid skill layout")
	}
	info, err := os.Stat(root.path)
	if err != nil {
		return nil, 0, err
	}
	if !info.IsDir() {
		return nil, 0, errors.New("skill package is not a directory")
	}
	count := min(len(root.files), budget.declarations)
	files := make([]agentapi.SkillFile, 0, count)
	invalid := 0
	for _, file := range root.files[:count] {
		if validSkillDeclaration(file) {
			files = append(files, file)
		} else {
			invalid++
		}
	}
	budget.declarations -= count
	sort.Slice(files, func(i, j int) bool {
		if files[i].ID == files[j].ID {
			return files[i].Path < files[j].Path
		}
		return files[i].ID < files[j].ID
	})
	out := make([]skillCandidate, 0, count)
	seen := map[string]bool{}
	for _, file := range files {
		valid := !seen[file.ID]
		seen[file.ID] = true
		out = append(out, skillCandidate{path: filepath.Join(root.path, filepath.FromSlash(file.Path)), name: file.ID, id: file.ID, valid: valid})
	}
	return out, len(root.files) - count + invalid, nil
}

func boundedSkillEntries(entries []os.DirEntry) ([]os.DirEntry, int) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	omitted := 0
	if len(entries) > maxSkillsPerRoot {
		omitted = len(entries) - maxSkillsPerRoot
		entries = entries[:maxSkillsPerRoot]
	}
	return entries, omitted
}

func observeRoot(harness string, root skillRoot, observedAt time.Time, budget *skillReadBudget, mode config.SkillEvidence) ([]archive.SupplementalEvidence, error) {
	if root.layout == agentapi.SkillDeclaredFiles && !validSkillScope(root.scope) {
		return []archive.SupplementalEvidence{inventoryObservation(harness, "declared_skills", "unreadable", nil, false, observedAt)}, nil
	}
	bounds := newSkillBounds(root)
	if root.user != "" && bounds.root == "" {
		if _, err := os.Lstat(root.path); err == nil {
			return []archive.SupplementalEvidence{inventoryObservation(harness, root.scope, "unreadable", nil, false, observedAt)}, nil
		}
	}
	entries, omittedEntries, err := rootCandidates(root, budget)
	if err != nil {
		// Not a directory, permission denied, or any other read failure: the
		// root exists but its inventory is unknown. Record that instead of
		// failing publication of a session over an unrelated directory.
		rootStatus, complete := "unreadable", false
		if errors.Is(err, os.ErrNotExist) {
			// Root-local absence is observable and lets a later pass record
			// removal of every skill previously seen in this scope.
			// Installed-only coverage still says nothing about what the
			// harness discovered or invoked.
			rootStatus, complete = "absent", true
		}
		return []archive.SupplementalEvidence{inventoryObservation(harness, root.scope, rootStatus, nil, complete, observedAt)}, nil
	}
	provenance := "filesystem:" + strings.ToLower(strings.TrimSpace(harness))
	var inventory []any
	var snapshots []archive.SupplementalEvidence
	omittedSnapshots := 0
	uninspectedEntries := 0
	for _, entry := range entries {
		if !entry.valid {
			uninspectedEntries++
			continue
		}
		// Symlinks are resolved first, and the SKILL.md actually read is the
		// resolved file, which must be a regular file inside the root's bounds.
		path, err := filepath.EvalSymlinks(entry.path)
		if errors.Is(err, os.ErrNotExist) {
			// Some legacy/configured roots contain grouped or plugin-managed
			// subtrees. We do not recursively walk them; record the coverage gap.
			if entry.directory || root.layout == agentapi.SkillDeclaredFiles {
				uninspectedEntries++
			}
			continue
		}
		if err != nil || (root.layout == agentapi.SkillDirectory && !bounds.allow(path)) || (root.layout == agentapi.SkillDeclaredFiles && !bounds.allowDeclared(path)) {
			// Permission denied on the entry or its SKILL.md, or a SKILL.md that
			// resolves outside the root's bounds: coverage gap.
			uninspectedEntries++
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			uninspectedEntries++
			continue
		}
		name := entry.name
		payload := map[string]any{
			"name": name, "coverage": string(archive.SkillCoverageInstalledOnly), "scope": root.scope,
			"uncertainty": "filesystem presence does not prove discovery, eligibility, or invocation",
		}
		installedName := map[string]any{"name": name}
		if entry.id != "" {
			installedName["id"] = entry.id
		}
		if info.Size() > maxSkillBytes {
			inventory = append(inventory, installedName)
			omittedSnapshots++
			continue
		}
		original, err := budget.read(path, info)
		if err != nil {
			if errors.Is(err, errSkillBudget) {
				inventory = append(inventory, installedName)
				omittedSnapshots++
			} else {
				uninspectedEntries++
			}
			continue
		}
		if parsed := frontmatterName(original); parsed != "" {
			name = parsed
			payload["name"] = name
		}
		// The hash is of the whole file with its credentials redacted, not
		// of the original bytes: uploaded beside the redacted snapshot, a
		// hash of the original would let whoever holds the bucket confirm a
		// guess at a redacted secret. A file with nothing to redact hashes
		// to its own SHA-256, so `list --skill-sha256` still matches it.
		digest := sha256.Sum256([]byte(archive.RedactText(string(original))))
		hash := hex.EncodeToString(digest[:])
		payload["sha256"] = hash
		installed := map[string]any{"name": name, "sha256": hash}
		if entry.id != "" {
			installed["id"] = entry.id
			payload["id"] = entry.id
			// Distinct logical declarations sharing bytes must survive snapshot
			// history identity without leaking their relative filesystem paths.
			payload["scope"] = root.scope + ":" + entry.id
		}
		inventory = append(inventory, installed)
		if mode == config.SkillEvidenceMetadata {
			continue
		}
		body := string(original)
		if len(body) > maxSnapshotBodyBytes {
			body = archive.TruncateUTF8(body, maxSnapshotBodyBytes)
			payload["truncated"] = true
		}
		payload["snapshot"] = body
		payload["original_bytes"] = float64(len(original))
		payload["redacted"] = false
		filtered, gaps, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
			Kind: archive.EvidenceKindSkillSnapshot, ObservedAt: observedAt, Provenance: provenance, Payload: payload,
		}})
		if err != nil {
			return nil, err
		}
		if len(filtered) == 0 {
			continue
		}
		archive.AnnotateSupplementalGaps(filtered[0].Payload, gaps)
		snapshots = append(snapshots, filtered[0])
	}
	totalOmittedEntries := omittedEntries + uninspectedEntries
	rootStatus := "present"
	if len(inventory) == 0 && totalOmittedEntries == 0 {
		rootStatus = "empty"
	}
	inventoryPayload := map[string]any{
		"coverage": string(archive.SkillCoverageInstalledOnly), "scope": root.scope, "skills": inventory,
		"inventory_complete": totalOmittedEntries == 0,
		"root_status":        rootStatus,
		"uncertainty":        "root-local filesystem inventory does not prove discovery, eligibility, or invocation; other roots and plugin-managed inventories may not have been inspected",
	}
	if totalOmittedEntries > 0 {
		inventoryPayload["truncated"] = true
		inventoryPayload["omitted_count"] = float64(totalOmittedEntries)
	}
	if omittedSnapshots > 0 {
		inventoryPayload["snapshot_omitted_count"] = float64(omittedSnapshots)
	}
	result := []archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindSkillInventory, ObservedAt: observedAt, Provenance: provenance,
		Payload: inventoryPayload,
	}}
	return append(result, snapshots...), nil
}

// skillBounds decides which resolved SKILL.md paths one skill root may read.
// Without it a SKILL.md, or the skill directory holding it, could be a
// symlink to any file the collector can read, and a repository a person
// merely cloned could ship .claude/skills/x/SKILL.md -> ~/.aws/credentials,
// or -> ../../../.env, to have that file archived with every session as a
// skill snapshot.
//
// Symlinks are resolved on both sides.
//
//   - A user-level root may resolve only inside the user's home; its resolved
//     SKILL.md must stay inside the resolved skill root. Links to a skills
//     checkout outside that root are uninspected.
//   - A project-level root belongs to a repository, which controls its skill
//     root as much as its links (.claude/skills -> .. makes the root the
//     project itself), so "inside the skill root" proves nothing for it.
//     Its resolved file must be named SKILL.md and lie inside the project
//     root. A link inside the repository that shares one skills directory
//     between harnesses (.claude/skills/x -> ../../skills/x) keeps working.
//
// A project root that is a user-level root (the session ran
// from the home directory, so the project's .claude/skills is
// ~/.claude/skills) is not observed a second time: see skillRoots.
//
// An entry that resolves outside its bounds counts as uninspected. The
// bounds are checked on the resolved path, and the resolved path is what is
// read. The opened file must still be the inspected inode.
type skillBounds struct {
	// root and project are the resolved skill root and project root, or ""
	// when the directory could not be resolved.
	root    string
	project string
	// projectScoped is set for a project-level root.
	projectScoped bool
}

func newSkillBounds(root skillRoot) skillBounds {
	projectScoped := root.project != ""
	resolvedRoot, resolvedProject := "", ""
	if resolved, err := filepath.EvalSymlinks(root.path); err == nil {
		resolvedRoot = resolved
	}
	if root.user != "" {
		resolvedUser, err := filepath.EvalSymlinks(root.user)
		rel, relErr := filepath.Rel(root.user, root.path)
		if err != nil || relErr != nil || !local.PathWithin(resolvedRoot, resolvedUser) || resolvedRoot != filepath.Join(resolvedUser, rel) {
			resolvedRoot = ""
		}
	}
	if projectScoped {
		if resolved, err := filepath.EvalSymlinks(root.project); err == nil {
			resolvedProject = resolved
		}
	}
	return skillBounds{
		root:          resolvedRoot,
		project:       resolvedProject,
		projectScoped: projectScoped,
	}
}

// allow reports whether a resolved SKILL.md path is inside the bounds.
func (b skillBounds) allow(resolved string) bool {
	named := strings.EqualFold(filepath.Base(resolved), "SKILL.md")
	if b.projectScoped {
		// The repository controls its skill root as much as its SKILL.md
		// links (.claude/skills -> .. makes the root the project itself), so
		// "inside the skill root" admits nothing more for it: only a file
		// named SKILL.md, inside the project.
		return named && local.PathWithin(resolved, b.project)
	}
	return named && local.PathWithin(resolved, b.root)
}

func (b skillBounds) allowDeclared(resolved string) bool {
	return b.root != "" && (!b.projectScoped || local.PathWithin(b.root, b.project)) && local.PathWithin(resolved, b.root)
}

var errSkillBudget = errors.New("skill observation byte budget exhausted")

func (b *skillReadBudget) read(path string, info os.FileInfo) ([]byte, error) {
	for _, cached := range b.cache {
		if os.SameFile(cached.info, info) && cached.info.Size() == info.Size() && cached.info.ModTime().Equal(info.ModTime()) {
			return cached.bytes, nil
		}
	}
	if info.Size() < 0 || info.Size() > b.remaining {
		return nil, errSkillBudget
	}
	// Reserve before I/O; failed reads do not replenish the allowance.
	b.remaining -= info.Size()
	original, err := readBounded(path, info.Size(), info)
	if err == nil {
		b.cache = append(b.cache, cachedSkill{info: info, bytes: original})
	}
	return original, err
}

// sameDirectory reports whether a and b are the same directory, compared
// with symlinks resolved when both resolve and as written otherwise.
func sameDirectory(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		if rb, err := filepath.EvalSymlinks(b); err == nil {
			return ra == rb
		}
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func inventoryObservation(harness, scope, rootStatus string, skills []any, complete bool, observedAt time.Time) archive.SupplementalEvidence {
	if skills == nil {
		skills = []any{}
	}
	return archive.SupplementalEvidence{
		Kind: archive.EvidenceKindSkillInventory, ObservedAt: observedAt,
		Provenance: "filesystem:" + strings.ToLower(strings.TrimSpace(harness)),
		Payload: map[string]any{
			"coverage":           string(archive.SkillCoverageInstalledOnly),
			"scope":              scope,
			"skills":             skills,
			"inventory_complete": complete,
			"root_status":        rootStatus,
			"uncertainty":        "root-local filesystem inventory does not prove discovery, eligibility, or invocation; other roots and plugin-managed inventories may not have been inspected",
		},
	}
}

func readBounded(path string, limit int64, inspected os.FileInfo) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(inspected, opened) || opened.Size() != inspected.Size() || !opened.ModTime().Equal(inspected.ModTime()) || opened.Size() < 0 || opened.Size() > limit {
		return nil, errors.New("skill changed between inspection and read")
	}
	data, err := io.ReadAll(io.LimitReader(f, opened.Size()))
	if err != nil {
		return nil, err
	}
	final, statErr := f.Stat()
	if statErr != nil || int64(len(data)) != opened.Size() || !os.SameFile(opened, final) || final.Size() != opened.Size() || !final.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("skill changed while reading")
	}
	return data, nil
}

func frontmatterName(content []byte) string {
	lines := strings.Split(string(content), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		if !strings.HasPrefix(strings.ToLower(trimmed), "name:") {
			continue
		}
		name := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, trimmed[:5])), "\"'")
		if name != "" && len(name) <= 256 && !strings.ContainsAny(name, "\r\n\x00") {
			return name
		}
	}
	return ""
}
