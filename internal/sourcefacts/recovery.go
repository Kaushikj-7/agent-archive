package sourcefacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// RecoveryOutcome names content-free recovery states, independently of authorization.
type RecoveryOutcome string

// Recovery outcomes distinguish ambiguity, unavailable evidence, and budget retries.
const (
	RecoveryBudgetExhausted       RecoveryOutcome = "project_budget_exhausted"
	RecoveryInventoryUnavailable  RecoveryOutcome = "project_inventory_unavailable"
	RecoveryExcluded              RecoveryOutcome = "project_excluded"
	RecoveryAmbiguous             RecoveryOutcome = "project_ambiguous"
	RecoveryRepositoryUnavailable RecoveryOutcome = "project_repository_unavailable"
	RecoveryMappingConflict       RecoveryOutcome = "project_mapping_conflict"
	RecoverySubtreeUnavailable    RecoveryOutcome = "project_subtree_unavailable"
	RecoveryUnavailable           RecoveryOutcome = "project_unavailable"
)

// RepositoryIdentity is a bounded local Git observation, supplied outside admission locks.
type RepositoryIdentity struct {
	Root         string
	Key          string
	Known        bool
	Dependencies []RepositoryDependency
	// Validation is semantic when Git cannot expose every candidate config path.
	Validation   string
	ObservedRoot string
	// ObservationScope binds temporary evidence to its executable/config environment.
	ObservationScope string
	// BudgetExhausted distinguishes bounded observation failure from unknown evidence.
	BudgetExhausted bool
}

// RepositoryDependency is a local metadata stamp, never Git config contents.
type RepositoryDependency struct {
	Path  string
	Stamp string
}

// RepositoryLookup observes one configured root within the caller deadline.
type RepositoryLookup func(context.Context, string) RepositoryIdentity

// RecoveryInventory carries an unfinished bounded configured-root sweep between discovery passes.
// Completed sweeps are refreshed by the next pass; they are never durable authority.
type RecoveryInventory struct {
	Context string               `json:"context"`
	Cursor  int                  `json:"cursor"`
	Entries []RepositoryIdentity `json:"entries"`
}

// RecoveryResolver only recovers absent checkouts into existing configured roots.
// Live ownership and explicit configured rules must be evaluated before calling Recover.
type RecoveryResolver struct {
	Projects    []archive.ProjectActivation
	Mappings    map[string]string
	ResolvePath func(string) string
	Lookup      RepositoryLookup
	Validate    func(RepositoryIdentity) bool
	// Proposed names inventory roots that are not committed configuration
	// (backfill's prospective roots). See nonOwning.
	Proposed           map[string]bool
	rawResolve         func(string) string
	pathContext        string
	mappedIdentities   map[string]RepositoryIdentity
	MetadataOperations int
	MetadataExhausted  bool
	// prefixValidated counts leading inventory entries already shown current
	// (or freshly observed) in this validation slice; see prepareInventory.
	prefixValidated      int
	sliceValidated       map[string]bool
	observationContext   context.Context
	semanticValidated    bool
	semanticChecked      bool
	semanticObservations map[string]RepositoryIdentity
	semanticOperations   int
	digest               string
	results              map[string]recoveryDecision
	Inventory            *RecoveryInventory
	Context              string
	PolicyContext        string
	Operations           int
	MaxOperations        int
}

type recoveryDecision struct {
	Proof   archive.ProjectResolution
	Outcome RecoveryOutcome
}

// NewRecoveryResolver freezes the policy context; no paths or URLs leave its digest.
func NewRecoveryResolver(projects []archive.ProjectActivation, mappings map[string]string, resolve func(string) string, lookup RepositoryLookup, inventory *RecoveryInventory) *RecoveryResolver {
	if inventory == nil {
		inventory = &RecoveryInventory{}
	}
	r := &RecoveryResolver{Projects: append([]archive.ProjectActivation(nil), projects...), Mappings: maps.Clone(mappings), ResolvePath: resolve, Lookup: lookup, Inventory: inventory, MaxOperations: 128}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Root < r.Projects[j].Root })
	r.rawResolve = resolve
	r.pathContext = RecoveryContext(projects, nil, resolve)
	r.Context = RecoveryContext(projects, mappings, filepath.Clean)
	r.PolicyContext = RecoveryContext(projects, nil, filepath.Clean)
	knownPaths := map[string]string{}
	for _, p := range projects {
		knownPaths[p.Root] = resolve(p.Root)
	}
	r.ResolvePath = func(path string) string {
		if canonical, ok := knownPaths[path]; ok {
			return canonical
		}
		return resolve(path)
	}
	if inventory.Context != r.Context || inventory.Cursor >= len(projects) || inventory.Cursor < 0 || len(inventory.Entries) != inventory.Cursor {
		*inventory = RecoveryInventory{Context: r.Context}
	}
	return r
}

// RecoveryContext binds proof and continuation to the complete configured inventory, including exclusions.
func RecoveryContext(projects []archive.ProjectActivation, mappings map[string]string, resolve func(string) string) string {
	type rule struct {
		Root      string `json:"root"`
		Canonical string `json:"canonical"`
		Included  bool   `json:"included"`
	}
	rules := make([]rule, 0, len(projects))
	for _, p := range projects {
		rules = append(rules, rule{p.Root, resolve(p.Root), p.Included})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Root < rules[j].Root })
	b, _ := json.Marshal(struct {
		Version  int               `json:"version"`
		Rules    []rule            `json:"rules"`
		Mappings map[string]string `json:"mappings"`
	}{1, rules, mappings})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Recover considers exact mappings and recorded keys without basename inference.
// Unknown inventory entries remain uncertainty, even when the included match looks
// unique; only keyless proposed checkouts are non-owning (see nonOwning).
func (r *RecoveryResolver) Recover(ctx context.Context, cwd, key string) (proof archive.ProjectResolution, outcome RecoveryOutcome) {
	r.observationContext = ctx
	cacheKey := cwd + "\x00" + key + "\x00" + r.Context
	if decision, ok := r.results[cacheKey]; ok {
		if !r.Current(decision.Proof) {
			return decision.Proof, r.staleOutcome()
		}
		return decision.Proof, decision.Outcome
	}
	defer func() {
		if outcome != RecoveryBudgetExhausted && outcome != RecoveryInventoryUnavailable && len(r.results) < 4096 {
			if r.results == nil {
				r.results = map[string]recoveryDecision{}
			}
			r.results[cacheKey] = recoveryDecision{proof, outcome}
		}
	}()
	proof = archive.ProjectResolution{OriginalCwd: cwd, RecordedRepoKey: key, Context: r.Context, PolicyContext: r.PolicyContext}
	if !validRecoveryPath(cwd) {
		return proof, RecoveryUnavailable
	}
	proof.CanonicalCwd = r.rawResolve(cwd)
	if !validRecoveryPath(proof.CanonicalCwd) {
		proof.CanonicalCwd = ""
		return proof, RecoveryUnavailable
	}
	cwd = filepath.Clean(cwd)
	if p, ok := ConfiguredOwner(r.Projects, r.ResolvePath(cwd), r.ResolvePath); ok {
		if !p.Included {
			return proof, RecoveryExcluded
		}
		proof.Root = p.Root
		proof.Method = "configured_path"
		return proof, ""
	}
	target, mapped := r.Mappings[cwd]
	if mapped {
		return r.recoverMapped(ctx, key, target, proof)
	}
	if !archive.IsRepoKey(key) {
		return proof, RecoveryRepositoryUnavailable
	}
	if outcome := r.prepareInventory(ctx); outcome != "" {
		return proof, outcome
	}
	proof.InventoryDigest = r.digest
	proof.ValidationMethod = "dependency_stamps"
	for _, id := range r.Inventory.Entries {
		if id.Validation == "semantic" {
			proof.ValidationMethod = "semantic"
		}
	}
	inv := r.Inventory
	matches := map[string]int{}
	for i, id := range inv.Entries {
		p := r.Projects[i]
		if id.Key != key || id.Root == "" {
			continue
		}
		root := r.ResolvePath(id.Root)
		// An inherited origin from a configured subtree cannot identify the missing subtree.
		if r.ResolvePath(p.Root) != root {
			return proof, RecoverySubtreeUnavailable
		}
		if old, ok := matches[root]; ok && r.Projects[old].Included != p.Included {
			return proof, RecoveryAmbiguous
		}
		matches[root] = i
	}
	if len(matches) != 1 {
		if len(matches) > 1 {
			return proof, RecoveryAmbiguous
		}
		return proof, RecoveryRepositoryUnavailable
	}
	for root, i := range matches {
		p := r.Projects[i]
		if !p.Included {
			return proof, RecoveryExcluded
		}
		for _, nested := range r.Projects {
			if !nested.Included && r.ResolvePath(nested.Root) != root && local.PathWithin(r.ResolvePath(nested.Root), root) {
				return proof, RecoverySubtreeUnavailable
			}
		}
		proof.Root = p.Root
		proof.Method = "recorded_repository"
		return proof, ""
	}
	return proof, RecoveryRepositoryUnavailable
}

func (r *RecoveryResolver) recoverMapped(ctx context.Context, key, target string, proof archive.ProjectResolution) (archive.ProjectResolution, RecoveryOutcome) {
	for _, p := range r.Projects {
		if filepath.Clean(p.Root) != filepath.Clean(target) {
			continue
		}
		owner, owned := ConfiguredOwner(r.Projects, r.ResolvePath(p.Root), r.ResolvePath)
		if !p.Included || (owned && !owner.Included) || r.Lookup == nil {
			return proof, RecoveryMappingConflict
		}
		id, cached := r.mappedIdentities[p.Root]
		if !cached {
			// Freeze one planning observation per target so later mappings cannot
			// replace the baseline used to validate earlier candidates.
			if ctx.Err() != nil || r.Operations >= r.MaxOperations {
				return proof, RecoveryBudgetExhausted
			}
			id = safeRepositoryIdentity(r.Lookup(ctx, p.Root))
			r.Operations++
		}
		if ctx.Err() != nil {
			return proof, RecoveryBudgetExhausted
		}
		if id.BudgetExhausted {
			return proof, RecoveryBudgetExhausted
		}
		if !id.Known {
			return proof, RecoveryInventoryUnavailable
		}
		if key != "" && id.Key != key {
			return proof, RecoveryMappingConflict
		}
		if r.mappedIdentities == nil {
			r.mappedIdentities = map[string]RepositoryIdentity{}
		}
		r.mappedIdentities[p.Root] = id
		proof.Root = p.Root
		proof.Method = "explicit_mapping"
		proof.ValidationMethod = "dependency_stamps"
		if id.Validation == "semantic" {
			proof.ValidationMethod = "semantic"
		}
		return proof, ""
	}
	return proof, RecoveryMappingConflict
}

func (r *RecoveryResolver) prepareInventory(ctx context.Context) RecoveryOutcome {
	if r.Lookup == nil || len(r.Projects) > 1024 {
		return RecoveryInventoryUnavailable
	}
	inv := r.Inventory
	// Entries validated or observed earlier in this slice are not charged
	// again: the metadata allowance holds one prefix validation per slice, not
	// one per candidate. Admission (Current) still rechecks every entry, and a
	// failed Current clears this mark so the next candidate revalidates.
	if r.Validate != nil {
		for i := r.prefixValidated; i < len(inv.Entries); i++ {
			if id := inv.Entries[i]; id.Known && !r.identityCurrent(id) {
				if r.MetadataExhausted {
					return RecoveryBudgetExhausted
				}
				r.digest = ""
				r.results = nil
				inv.Cursor = i
				inv.Entries = inv.Entries[:i]
				break
			}
		}
	}
	for inv.Cursor < len(r.Projects) {
		if ctx.Err() != nil || r.Operations >= r.MaxOperations {
			return RecoveryBudgetExhausted
		}
		id := safeRepositoryIdentity(r.Lookup(ctx, r.Projects[inv.Cursor].Root))
		r.Operations++
		if ctx.Err() != nil {
			return RecoveryBudgetExhausted
		}
		// Temporary unavailable answers must be retried, and cannot certify uniqueness.

		inv.Entries = append(inv.Entries, id)
		inv.Cursor++
	}
	r.prefixValidated = len(inv.Entries)
	if r.digest == "" {
		b, _ := json.Marshal(inv)
		sum := sha256.Sum256(b)
		r.digest = hex.EncodeToString(sum[:])
	}
	for i, id := range inv.Entries {
		if id.BudgetExhausted {
			return RecoveryBudgetExhausted
		}
		if !id.Known && !r.nonOwning(i, id) {
			return RecoveryInventoryUnavailable
		}
	}
	return ""
}

// nonOwning reports a proposed (uncommitted) root whose lookup completed within
// budget and found a checkout top level, but no normalizable repository key,
// such as a clone whose origin is a local path. Such a root cannot be shown to
// own any recorded key, so it is not uncertainty for other roots' matches; it
// is rechecked with the semantic sweep and must still lack a key at admission.
// Configured roots, and lookups that observed no checkout at all, stay strict.
func (r *RecoveryResolver) nonOwning(i int, id RepositoryIdentity) bool {
	return !id.Known && !id.BudgetExhausted && id.Root != "" && id.Key == "" && i < len(r.Projects) && r.Proposed[r.Projects[i].Root]
}

func safeRepositoryIdentity(id RepositoryIdentity) RepositoryIdentity {
	if id.Key != "" && !archive.IsRepoKey(id.Key) {
		return RepositoryIdentity{}
	}
	if id.Root != "" && (!filepath.IsAbs(id.Root) || len(id.Root) > 4096 || strings.ContainsAny(id.Root, "\x00\r\n")) {
		return RepositoryIdentity{}
	}
	if id.Validation != "" && id.Validation != "semantic" {
		return RepositoryIdentity{}
	}
	if id.Validation == "semantic" && !validRecoveryPath(id.ObservedRoot) {
		return RepositoryIdentity{}
	}
	if len(id.ObservationScope) > 128 {
		return RepositoryIdentity{}
	}
	if len(id.Dependencies) > 128 {
		return RepositoryIdentity{}
	}
	for _, dep := range id.Dependencies {
		if !filepath.IsAbs(dep.Path) || len(dep.Path) > 4096 || len(dep.Stamp) > 128 {
			return RepositoryIdentity{}
		}
	}
	return id
}

// Current rechecks content-free proof dependencies outside admission locks.
// Semantic evidence gets one bounded second sweep; stale evidence stays pending.
// A failure that is not budget exhaustion makes the next Recover revalidate
// the inventory prefix, so later candidates are not planned on stale entries.
// Only this check's own exhaustion counts: an allowance exhausted earlier in
// the slice must not hide a change this check did observe.
func (r *RecoveryResolver) Current(proof archive.ProjectResolution) bool {
	exhausted := r.MetadataExhausted
	r.MetadataExhausted = false
	current := r.current(proof)
	if !current && !r.MetadataExhausted {
		r.prefixValidated = 0
	}
	r.MetadataExhausted = r.MetadataExhausted || exhausted
	return current
}

func (r *RecoveryResolver) current(proof archive.ProjectResolution) bool {
	if proof.ValidationMethod == "semantic" && (r.observationContext == nil || r.observationContext.Err() != nil) {
		r.MetadataExhausted = true
		return false
	}
	if r.Validate == nil && proof.ValidationMethod != "semantic" {
		if proof.Method == "explicit_mapping" {
			id, ok := r.mappedIdentities[proof.Root]
			return ok && r.semanticIdentityCurrent(id)
		}
		return r.semanticCurrent()
	}
	if r.MetadataOperations+len(r.Projects)+1 > r.metadataLimit() {
		r.MetadataExhausted = true
		return false
	}
	r.MetadataOperations += len(r.Projects)
	r.MetadataOperations++
	if proof.CanonicalCwd == "" || r.rawResolve(proof.OriginalCwd) != proof.CanonicalCwd {
		return false
	}
	if RecoveryContext(r.Projects, nil, r.rawResolve) != r.pathContext {
		return false
	}
	if proof.Method == "explicit_mapping" {
		id, ok := r.mappedIdentities[proof.Root]
		return ok && r.semanticIdentityCurrent(id) && r.identityCurrent(id)
	}
	if !r.semanticCurrent() {
		return false
	}
	for i, id := range r.Inventory.Entries {
		// Non-owning entries have no dependency stamps; semanticCurrent rechecked them.
		if !r.nonOwning(i, id) && !r.identityCurrent(id) {
			return false
		}
	}
	return true
}

func (r *RecoveryResolver) identityCurrent(id RepositoryIdentity) bool {
	if r.MetadataOperations+len(id.Dependencies) > r.metadataLimit() {
		r.MetadataExhausted = true
		return false
	}
	r.MetadataOperations += len(id.Dependencies)
	return r.Validate == nil || r.Validate(id)
}

func (r *RecoveryResolver) staleOutcome() RecoveryOutcome {
	if r.MetadataExhausted {
		return RecoveryBudgetExhausted
	}
	return RecoveryInventoryUnavailable
}

// Two maximum-size inventories fit so a resumed complete sweep can both
// validate its prefix and admit at least one source. Further work stays pending.
func (r *RecoveryResolver) metadataLimit() int {
	return max(1024, 2*min(len(r.Projects), 1024)*129)
}

// ResetValidation starts a new bounded consumer admission slice. It does not
// discard identity evidence or extend the Git lookup allowance.
func (r *RecoveryResolver) ResetValidation() {
	r.MetadataOperations = 0
	r.MetadataExhausted = false
	r.prefixValidated = 0
	r.sliceValidated = nil
	r.semanticValidated = false
	r.semanticChecked = false
	r.semanticObservations = nil
	r.semanticOperations = 0
}

// CurrentSlice coalesces common inventory validation for one short import hold.
// Call ResetValidation before assembling each slice, outside admission locks.
func (r *RecoveryResolver) CurrentSlice(proof archive.ProjectResolution) bool {
	if proof.ValidationMethod == "semantic" && (r.observationContext == nil || r.observationContext.Err() != nil) {
		r.MetadataExhausted = true
		return false
	}
	if r.Validate != nil || proof.ValidationMethod == "semantic" {
		if r.MetadataOperations >= r.metadataLimit() {
			r.MetadataExhausted = true
			return false
		}
		r.MetadataOperations++
		if proof.CanonicalCwd == "" || r.rawResolve(proof.OriginalCwd) != proof.CanonicalCwd {
			return false
		}
	}
	key := proof.Method
	if proof.Method == "explicit_mapping" {
		key += "\x00" + proof.Root
	}
	if r.sliceValidated[key] {
		return true
	}
	if !r.Current(proof) {
		return false
	}
	if r.sliceValidated == nil {
		r.sliceValidated = map[string]bool{}
	}
	r.sliceValidated[key] = true
	return true
}

func validRecoveryPath(path string) bool {
	return len(path) <= 4096 && filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\r\n")
}

// semanticCurrent performs the second complete semantic sweep once per slice.
// Every configured entry participates, including excluded roots and scratch roots.
// Without semantic evidence, only non-owning proposed entries are observed again.
func (r *RecoveryResolver) semanticCurrent() bool {
	if r.semanticChecked {
		return r.semanticValidated
	}
	semantic, unowned := false, false
	for i, id := range r.Inventory.Entries {
		semantic = semantic || id.Validation == "semantic"
		unowned = unowned || r.nonOwning(i, id)
	}
	if !semantic && !unowned {
		return true
	}
	r.semanticChecked = true
	for i, id := range r.Inventory.Entries {
		if (semantic || r.nonOwning(i, id)) && !r.semanticLookupCurrent(id, r.Projects[i].Root) {
			return false
		}
	}
	r.semanticValidated = true
	return true
}

func (r *RecoveryResolver) semanticIdentityCurrent(id RepositoryIdentity) bool {
	if id.Validation != "semantic" {
		return true
	}
	return r.semanticLookupCurrent(id, id.ObservedRoot)
}

func (r *RecoveryResolver) semanticLookupCurrent(id RepositoryIdentity, root string) bool {
	if fresh, ok := r.semanticObservations[root]; ok {
		return semanticIdentityAgrees(id, fresh)
	}
	ctx := r.observationContext
	if ctx == nil || ctx.Err() != nil || r.semanticOperations >= 1024 {
		r.MetadataExhausted = true
		return false
	}
	r.semanticOperations++
	fresh := safeRepositoryIdentity(r.Lookup(ctx, root))
	if r.semanticObservations == nil {
		r.semanticObservations = map[string]RepositoryIdentity{}
	}
	r.semanticObservations[root] = fresh
	if fresh.BudgetExhausted || ctx.Err() != nil {
		r.MetadataExhausted = true
	}
	return ctx.Err() == nil && semanticIdentityAgrees(id, fresh)
}

// ResetValidationContext renews the consumer context after planning ends.
func (r *RecoveryResolver) ResetValidationContext(ctx context.Context) {
	r.ResetValidation()
	r.observationContext = ctx
}

func semanticIdentityAgrees(planned, fresh RepositoryIdentity) bool {
	if !planned.Known {
		// A non-owning entry must still be a keyless checkout at the same top level.
		return !fresh.Known && !fresh.BudgetExhausted && fresh.Root == planned.Root && fresh.Key == "" && fresh.ObservationScope == planned.ObservationScope
	}
	return fresh.Known && !fresh.BudgetExhausted && fresh.Root == planned.Root && fresh.Key == planned.Key && fresh.Validation == planned.Validation && fresh.ObservationScope == planned.ObservationScope
}
