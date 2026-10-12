package backfill

import (
	"context"
	"io"
	"io/fs"
	"os"
	"sort"

	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Per-app recovery isolation. The witness inventory proves a recorded
// repository key unique among the clone roots its evidence names. A gap is
// evidence that could have named one more root. Which gaps block which
// recoveries (dev/specs/backfill.md, "Recorded project recovery"):
//
//   - A gap not attributable to one app (Agent "") blocks every app.
//   - A gap in app A's evidence blocks A's own sessions.
//   - A gap in another app's evidence does not block; the plan reports the
//     sessions recovered without that evidence.
//
// The wrong outcome another app's gap can hide is a second, unconfigured
// checkout of the same repository known only to that app's sessions: the
// session is then recovered into one checkout of its own repository rather
// than left unresolved. That is the residual the inventory already accepts
// for a clone no agent ever ran in, a keyless clone (PR #387) and a
// url.insteadOf origin. Configured roots, including exclusions and nested
// exclusions, are enumerated through Git independently of every app store, so
// no gap can hide capture policy. Output filters do not change this: an app
// --harness leaves out still contributes the witnesses it observed, and its
// gaps block only its own sessions, which the filter hides anyway.

// blocking returns the gap that blocks recorded recovery for a session of h,
// by the diagnostic priority order. h "" (an unknown app) is blocked by every gap.
func (g *recoveryGaps) blocking(h harness) (recoveryGap, bool) {
	var applicable recoveryGaps
	for gap := range *g {
		if h == "" || gap.Agent == "" || gap.Agent == string(h) {
			applicable.add(gap.Agent, gap.Cause)
		}
	}
	cause := applicable.cause()
	if cause == "" {
		return recoveryGap{}, false
	}
	// Prefer the unattributed gap, then the session's own app, for reporting.
	for _, agent := range []string{"", string(h)} {
		if applicable[recoveryGap{Agent: agent, Cause: cause}] {
			return recoveryGap{Agent: agent, Cause: cause}, true
		}
	}
	agents := make([]string, 0, len(applicable))
	for gap := range applicable {
		if gap.Cause == cause {
			agents = append(agents, gap.Agent)
		}
	}
	sort.Strings(agents)
	return recoveryGap{Agent: agents[0], Cause: cause}, true
}

// blockedOutcome is the recovery outcome a blocking gap produces.
func (g recoveryGap) blockedOutcome() sourcefacts.RecoveryOutcome {
	if g.Cause == CauseRecoveryBudget {
		return sourcefacts.RecoveryBudgetExhausted
	}
	return sourcefacts.RecoveryInventoryUnavailable
}

// recoveryFor returns the shared resolver, or, when a gap blocks sessions of
// h, a resolver over the same inventory whose every repository lookup is
// unavailable. Configured-path ownership and keyless recordings resolve as
// before; only recorded-key recovery reports the gap.
func (r *resolver) recoveryFor(ctx context.Context, h harness) (*sourcefacts.RecoveryResolver, *recoveryGap) {
	gap, blocked := r.recoveryGaps.blocking(h)
	if !blocked {
		return r.recovery, nil
	}
	if r.gapRecovery == nil {
		// Fail closed even before the inventory prepared the cache.
		r.gapRecovery = map[bool]*sourcefacts.RecoveryResolver{}
	}
	budget := gap.Cause == CauseRecoveryBudget
	recovery := r.gapRecovery[budget]
	if recovery == nil {
		lookup := func(context.Context, string) sourcefacts.RepositoryIdentity {
			return sourcefacts.RepositoryIdentity{BudgetExhausted: budget}
		}
		recovery = sourcefacts.NewRecoveryResolver(r.gapProjects, r.filters.ProjectMappings, r.env.resolved, lookup, nil)
		recovery.MaxOperations = 1024
		recovery.Validate = r.env.RepositoryIdentityCurrent
		recovery.ResetValidationContext(ctx)
		r.gapRecovery[budget] = recovery
	}
	return recovery, &gap
}

// appendedSource recognizes tentative append-shaped metadata, not proof that
// the inspected bytes are unchanged. Only the complete original-prefix inventory
// check can authorize the associated witness copy or its renewal.
func appendedSource(before, after fs.FileInfo, err error) bool {
	return err == nil && before != nil && after != nil && before.Mode().IsRegular() && after.Mode() == before.Mode() && os.SameFile(before, after) && after.Size() > before.Size()
}

// recoveryEvidenceItems substitutes, for each appended source, an evidence
// copy resolved from its unchanged header. The session itself keeps its empty
// resolution and its source_changed decision; the copy is a non-eligible
// witness whose renewal accepts further appends (stillAppendedOnly).
func recoveryEvidenceItems(items []*work, headersCurrent bool) []*work {
	out := make([]*work, 0, len(items))
	for _, w := range items {
		if headersCurrent && w.appendWitness != nil && w.appendedOnly() {
			evidence := *w
			evidence.res = *w.appendWitness
			evidence.appendWitness = nil
			evidence.appendHeaderVerified = true
			w = &evidence
		}
		out = append(out, w)
	}
	return out
}

// appendedOnly reports append-shaped metadata; it does not grant header authority.
func (w *work) appendedOnly() bool {
	return w.sourceChanged && !w.sourceRewritten && !w.vanished
}

// stillAppendedOnly renews a verified witness only after the coalesced inventory
// rechecked all original header prefixes, and its current metadata still grows.
func (w *work) stillAppendedOnly(env Environment, headersCurrent bool) bool {
	if !headersCurrent || !w.appendHeaderVerified || !w.appendedOnly() || w.t.sourceInfo == nil {
		return false
	}
	current, err := env.lstat(w.t.path)
	return appendedSource(w.t.sourceInfo, current, err)
}

// RecoveryEvidenceGap reports one app's incomplete recovery evidence ("" when
// not attributable to one app) with a fixed cause: how many sessions it kept
// from recorded recovery and how many other apps' sessions were recovered
// without it, by app. Counts only; never paths, IDs or content.
type RecoveryEvidenceGap struct {
	App              string           `json:"app"`
	Cause            DiagnosticDetail `json:"cause"`
	Blocked          map[string]int   `json:"blocked"`
	RecoveredWithout map[string]int   `json:"recovered_without"`
}

// summarizeRecoveryGaps attributes final plan decisions to the gaps; a gap
// that neither blocked nor was bypassed by an imported session is omitted.
func summarizeRecoveryGaps(gaps recoveryGaps, items []*work) []RecoveryEvidenceGap {
	if len(gaps) == 0 {
		return nil
	}
	list := make([]recoveryGap, 0, len(gaps))
	for gap := range gaps {
		list = append(list, gap)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Agent != list[j].Agent {
			return list[i].Agent < list[j].Agent
		}
		return list[i].Cause < list[j].Cause
	})
	out := make([]RecoveryEvidenceGap, len(list))
	index := map[recoveryGap]int{}
	for i, gap := range list {
		out[i] = RecoveryEvidenceGap{App: gap.Agent, Cause: gap.Cause, Blocked: map[string]int{}, RecoveredWithout: map[string]int{}}
		index[gap] = i
	}
	for _, w := range items {
		if w.vanished {
			continue
		}
		app := string(w.t.harness)
		if w.c.Skip == SkipWorktreeUnresolved && w.res.gap != nil {
			out[index[*w.res.gap]].Blocked[app]++
			continue
		}
		if w.c.Skip != "" || w.res.proof == nil || w.res.proof.Method != "recorded_repository" {
			continue
		}
		// Only another app's gap can have been bypassed; the others block.
		for i, gap := range list {
			if gap.Agent != "" && gap.Agent != app {
				out[i].RecoveredWithout[app]++
			}
		}
	}
	kept := out[:0]
	for _, s := range out {
		if len(s.Blocked) > 0 || len(s.RecoveredWithout) > 0 {
			kept = append(kept, s)
		}
	}
	return kept
}

// renderRecoveryEvidenceGaps says which app's evidence was missing and what
// that did, so a partial recovery is never silent.
func renderRecoveryEvidenceGaps(w io.Writer, p Plan) {
	if len(p.RecoveryEvidenceGaps) == 0 {
		return
	}
	terminal.Println(w)
	terminal.Println(w, "Project recovery evidence:")
	for _, g := range p.RecoveryEvidenceGaps {
		source := "every app's evidence"
		if g.App != "" {
			source = agentLabel(g.App) + "'s evidence"
		}
		for _, app := range sortedCountKeys(g.Blocked) {
			terminal.Printf(w, "%4d  %s not recovered: %s is incomplete (%s)\n", g.Blocked[app], sessionNoun(map[string]bool{app: true}, g.Blocked[app]), source, g.Cause)
		}
		for _, app := range sortedCountKeys(g.RecoveredWithout) {
			terminal.Printf(w, "%4d  %s recovered without %s (%s);\n", g.RecoveredWithout[app], sessionNoun(map[string]bool{app: true}, g.RecoveredWithout[app]), source, g.Cause)
			terminal.Printf(w, "      a clone of the same repository known only to %s can't be ruled out.\n", agentLabel(g.App))
		}
	}
}

func sortedCountKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
