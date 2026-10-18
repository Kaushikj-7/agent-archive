package backfill

import (
	"context"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// RecoveryReviewLimit bounds one initial private review, independently of the
// existing mapping-file limits. Larger, complete reviews require paged planning.
const RecoveryReviewLimit = 128

// RecoveryReview contains observations from this plan, never capture authority.
// Coverage is always incomplete: the current planner has bounded source and
// witness windows and cannot certify that every recoverable session was found.
type RecoveryReview struct {
	Groups                        []RecoveryReviewGroup
	OmittedCandidates             int
	OmittedTargets                int
	OmittedWitnesses              int
	UnavailableCwdCandidates      int
	UnavailableIdentityCandidates int
	Witnesses                     []RecoveryWitness
}

// RecoveryReviewGroup groups exact original cwds and recorded repository keys.
// Counts describe project recovery candidates, not promises of importability.
type RecoveryReviewGroup struct {
	OriginalCwd             string
	AssignmentCwd           string
	MixedOriginalSpellings  bool
	ParentTraversalOriginal bool
	RecordedRepoKey         string
	Candidates              int
	DatabaseOnlyCandidates  int
	PrimaryReasons          map[SkipReason]int
	OtherReasons            map[SkipReason]int
	DiagnosticDetails       map[DiagnosticDetail]int
	// NativeEvidenceUnresolved includes related histories not yet qualified;
	// project failure may have prevented their full source inspection.
	NativeEvidenceUnresolved int
	SourceRestriction        SkipReason
	Targets                  []RecoveryTarget
}

// RecoveryTarget reports the existing exact mapping policy for a configured root.
type RecoveryTarget struct {
	Root    string
	Outcome sourcefacts.RecoveryOutcome
}

// RecoveryWitness is a content-free physical checkout observation already made
// by the planner. An unobserved or unresolved root never proves a non-match.
type RecoveryWitness struct {
	Root         string
	PhysicalRoot string
	Included     bool
	Key          string
	Observed     bool
	Known        bool
	Failure      sourcefacts.RecoveryOutcome
}

func finishPlanReview(ctx context.Context, env Environment, r *resolver, plan Plan, batches ...[]*work) (Plan, error) {
	if env.ReviewProjects {
		plan.RecoveryReview = reviewProjects(ctx, r, batches...)
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
	}
	tracePlanSelection(plan)
	return plan, nil
}

func reviewProjectGroups(ctx context.Context, r *resolver, review *RecoveryReview, batches ...[]*work) {
	type groupKey struct {
		cwd string
		key string
	}
	groups := map[groupKey]*RecoveryReviewGroup{}
	// Fold the owner's existing file/database work slices through one bounded
	// group set, without concatenating or copying the candidate inventories.
	for _, items := range batches {
		for _, w := range items {
			if ctx.Err() != nil {
				return
			}
			cwd := w.t.cwd
			if w.vanished || w.evidenceOnly || w.filtered {
				continue
			}
			if !validMappingPath(cwd) {
				if w.c.Skip == SkipProjectUnknown || w.c.Skip == SkipWorktreeUnresolved {
					review.UnavailableCwdCandidates++
				}
				continue
			}
			if r.env.exists(cwd) {
				continue
			}
			if w.t.repoKey != "" && !archive.IsRepoKey(w.t.repoKey) {
				review.UnavailableIdentityCandidates++
				continue
			}
			key := groupKey{filepath.Clean(cwd), w.t.repoKey}
			g := groups[key]
			if g == nil {
				if len(groups) == RecoveryReviewLimit {
					// Keep the lexically first bounded set independent of source order.
					// Do not build a second inventory to count omitted distinct groups.
					var last groupKey
					for retained := range groups {
						if retained.cwd > last.cwd || retained.cwd == last.cwd && retained.key > last.key {
							last = retained
						}
					}
					if key.cwd > last.cwd || key.cwd == last.cwd && key.key > last.key {
						review.OmittedCandidates++
						continue
					}
					review.OmittedCandidates += groups[last].Candidates
					delete(groups, last)
				}
				g = &RecoveryReviewGroup{OriginalCwd: cwd, AssignmentCwd: key.cwd, RecordedRepoKey: key.key, PrimaryReasons: map[SkipReason]int{}, OtherReasons: map[SkipReason]int{}, DiagnosticDetails: map[DiagnosticDetail]int{}}
				groups[key] = g
			}
			observeRecoveryGroup(r, g, w)
		}
	}
	for _, group := range groups {
		review.Groups = append(review.Groups, *group)
	}
	sort.Slice(review.Groups, func(i, j int) bool {
		a, b := review.Groups[i], review.Groups[j]
		if a.AssignmentCwd != b.AssignmentCwd {
			return a.AssignmentCwd < b.AssignmentCwd
		}
		return a.RecordedRepoKey < b.RecordedRepoKey
	})
}

func observeRecoveryGroup(r *resolver, g *RecoveryReviewGroup, w *work) {
	cwd := w.t.cwd
	if cwd != g.OriginalCwd {
		g.MixedOriginalSpellings = true
		if cwd < g.OriginalCwd {
			g.OriginalCwd = cwd
		}
	}
	restriction := r.historicalSourceRestriction(cwd)
	if restriction == "" && r.hasRepositoryEvidence(cwd) {
		restriction = SkipWorktreeUnresolved
	}
	if strings.Contains(cwd, "/../") || strings.HasSuffix(cwd, "/..") {
		// Existing cwd policy cleans before resolving symlinks. Do not
		// claim raw traversal compatibility from that cleaned spelling.
		g.ParentTraversalOriginal = true
		if restriction == "" {
			restriction = SkipWorktreeUnresolved
		}
	}
	if restriction != "" && (g.SourceRestriction == "" || slices.Index(skipOrder, restriction) < slices.Index(skipOrder, g.SourceRestriction)) {
		g.SourceRestriction = restriction
	}
	g.Candidates++
	if w.c.SourceKind == archive.SourceKindCursorSQLite {
		g.DatabaseOnlyCandidates++
	}
	if w.t.capturePending || (w.c.RelatedHistory || w.c.NativeChild) && !w.validated {
		g.NativeEvidenceUnresolved++
	}
	g.PrimaryReasons[w.c.Skip]++
	if w.c.Diagnostic != nil {
		g.DiagnosticDetails[w.c.Diagnostic.Detail]++
	}
	other := *w
	other.res.skip = ""
	if reason := other.reason(r.env.now()); reason != "" && reason != w.c.Skip {
		g.OtherReasons[reason]++
	}
}

func reviewProjects(ctx context.Context, r *resolver, batches ...[]*work) *RecoveryReview {
	review := &RecoveryReview{}
	reviewProjectGroups(ctx, r, review, batches...)
	if ctx.Err() != nil {
		return review
	}
	mapping := r.mappingRecovery
	if mapping == nil {
		mapping = r.recovery
	}
	targets := mapping.Projects[:min(len(mapping.Projects), RecoveryReviewLimit)]
	review.OmittedTargets = len(mapping.Projects) - len(targets)
	for _, p := range targets {
		if !validMappingPath(p.Root) {
			review.OmittedTargets++
		}
	}
	for i := range review.Groups {
		g := &review.Groups[i]
		for _, p := range targets {
			if ctx.Err() != nil {
				return review
			}
			if !validMappingPath(p.Root) {
				continue
			}
			outcome := sourcefacts.RecoveryUnavailable
			if g.SourceRestriction == "" && g.DatabaseOnlyCandidates < g.Candidates {
				info, err := r.env.stat(p.Root)
				if err != nil || !info.IsDir() {
					outcome = sourcefacts.RecoveryMappingConflict
				} else {
					outcome = mapping.ReviewMapping(ctx, g.OriginalCwd, g.RecordedRepoKey, p.Root)
				}
			}
			g.Targets = append(g.Targets, RecoveryTarget{Root: p.Root, Outcome: outcome})
		}
	}
	witnesses := r.recovery.Projects[:min(len(r.recovery.Projects), RecoveryReviewLimit)]
	review.OmittedWitnesses = len(r.recovery.Projects) - len(witnesses)
	for i, p := range witnesses {
		if !validMappingPath(p.Root) {
			review.OmittedWitnesses++
			continue
		}
		var id sourcefacts.RepositoryIdentity
		observed := i < len(r.recovery.Inventory.Entries)
		if observed {
			id = r.recovery.Inventory.Entries[i]
		}
		review.Witnesses = append(review.Witnesses, RecoveryWitness{Root: p.Root, PhysicalRoot: id.Root, Included: p.Included, Key: id.Key, Observed: observed, Known: id.Known, Failure: id.Failure})
	}
	// Source order is independent of map iteration and retained lookup caches.
	slices.SortFunc(review.Witnesses, func(a, b RecoveryWitness) int {
		if a.Root < b.Root {
			return -1
		}
		if a.Root > b.Root {
			return 1
		}
		return 0
	})
	return review
}
