package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// CodexCaptureScope is independent of the discovery ingress choice.
type CodexCaptureScope string

// Supported Codex capture scopes select explicit projects or blanket authorization.
const (
	CodexIncludedProjects CodexCaptureScope = "included-projects"
	CodexAllProjects      CodexCaptureScope = "all-projects"
)

// CodexCaptureConfig retains local authorization even after scope reduction.
type CodexCaptureConfig struct {
	Scope               CodexCaptureScope       `json:"scope"`
	Authorization       *DiscoveryAuthorization `json:"authorization,omitempty"`
	SourceAuthorization *DiscoveryAuthorization `json:"source_authorization,omitempty"`
	RuleRoots           map[string]string       `json:"rule_roots,omitempty"`
	Barriers            []CodexRuleBarrier      `json:"barriers,omitempty"`
	Revision            string                  `json:"revision"`
}

// CodexRuleBarrier prevents exception edits from retroactively granting starts.
type CodexRuleBarrier struct {
	Root  string    `json:"root"`
	Since time.Time `json:"since"`
}

// EffectiveCodexCaptureScope never widens legacy consent.
func (c Config) EffectiveCodexCaptureScope() CodexCaptureScope {
	if c.CodexCapture != nil {
		return c.CodexCapture.Scope
	}
	return CodexIncludedProjects
}

// ReconcileCodexCapture opens only locally committed forward authorization.
func ReconcileCodexCapture(next *Config, previous Config, now time.Time) error {
	nativeConsentStart := now.UTC()
	if next.DestinationSince.After(nativeConsentStart) {
		nativeConsentStart = next.DestinationSince
	}

	draft := *next
	if err := reconcileCodexCapture(&draft, previous, now, nativeConsentStart); err != nil {
		return err
	}
	*next = draft
	return nil
}

func reconcileCodexCapture(next *Config, previous Config, now, nativeConsentStart time.Time) error {
	if next.CodexCapture == nil && previous.CodexCapture == nil {
		return nil
	}
	var p CodexCaptureConfig
	if next.CodexCapture != nil {
		p = *next.CodexCapture
	} else {
		p = *previous.CodexCapture
	}
	p.Barriers = slices.Clone(p.Barriers)
	p.RuleRoots = make(map[string]string, len(next.Archive.Projects))
	for _, rule := range next.Archive.Projects {
		canonical, e := filepath.EvalSymlinks(rule.Root)
		if e != nil {
			canonical = canonicalRuleRoot(rule.Root)
		}
		p.RuleRoots[rule.Root] = canonical
	}
	if p.Scope == "" {
		p.Scope = CodexIncludedProjects
	}
	old := previous.CodexCapture
	active := p.Scope == CodexAllProjects && next.Archive.Enabled && slices.Contains(next.Harnesses, "codex")
	keep := active && old != nil && old.Authorization != nil && !discoveryNativeStartFloor(*old.Authorization).IsZero() && old.Scope == CodexAllProjects && previous.Archive.Enabled && slices.Contains(previous.Harnesses, "codex") && previous.DestinationID() == next.DestinationID()
	if keep {
		p.Authorization = normalizedAuthorization(old.Authorization)
		if next.Paused != previous.Paused {
			if err := validateIntervalTransition(*p.Authorization, next.Paused, now.UTC()); err != nil {
				return err
			}
			transitionIntervals(p.Authorization, next.Paused, now.UTC())
		}
	} else if active {
		id, err := local.ID()
		if err != nil {
			return err
		}
		start := nativeConsentStart
		var intervals []DiscoveryInterval
		if !next.Paused {
			intervals = []DiscoveryInterval{{Start: start}}
		}
		a := DiscoveryAuthorization{Generation: id, NativeStartFloor: start, Agent: "codex", DestinationID: next.DestinationID(), Intervals: intervals}
		p.Authorization = &a
	}
	if err := reconcileCodexSource(&p, *next, previous, active, keep, now, nativeConsentStart); err != nil {
		return err
	}
	reconcileCodexBarriers(&p, previous, *next, now)
	b, _ := json.Marshal(next.Archive.Projects)
	sum := sha256.Sum256(b)
	p.Revision = hex.EncodeToString(sum[:])
	next.CodexCapture = &p
	return prepareDiscoveryConfig(next)
}

// CodexGeneration checks original start against the committed blanket window.
func (c Config) CodexGeneration(root, cwd string, started, now time.Time) (string, bool) {
	if c.EffectiveCodexCaptureScope() != CodexAllProjects || c.Paused || !c.Archive.Enabled || !slices.Contains(c.Harnesses, "codex") || started.IsZero() || started.After(now.Add(2*time.Minute)) {
		return "", false
	}
	a := c.CodexCapture.Authorization
	if a == nil || discoveryNativeStartFloor(*a).IsZero() || started.Before(discoveryNativeStartFloor(*a)) || a.DestinationID != c.DestinationID() || !c.CodexProjectAllowed(root, cwd, started) {
		return "", false
	}
	for _, v := range a.Intervals {
		if !started.Before(v.Start) && (v.End.IsZero() || started.Before(v.End)) {
			return a.Generation + ":" + c.codexRuleToken(root, cwd), true
		}
	}
	return "", false
}

// CodexProjectAllowed evaluates nearest explicit rules for checkout and main.
func (c Config) CodexProjectAllowed(root, cwd string, boundary time.Time) bool {
	winning, found := c.codexWinningRule(root, cwd)
	if found && (!winning.Included || (!winning.ActivatedAt.IsZero() && boundary.Before(winning.ActivatedAt))) {
		return false
	}
	main, mainFound := c.codexRuleAt(root)
	if mainFound && main.Included && found && !local.PathWithin(winning.Root, main.Root) && boundary.Before(main.ActivatedAt) {
		return false
	}
	if c.CodexCapture != nil {
		for _, b := range c.CodexCapture.Barriers {
			if (local.PathWithin(root, b.Root) || local.PathWithin(cwd, b.Root)) && boundary.Before(b.Since) {
				if winning.Included && winning.Root != b.Root && local.PathWithin(winning.Root, b.Root) && !boundary.Before(winning.ActivatedAt) {
					continue
				}
				return false
			}
		}
	}
	return true
}

// codexRuleToken includes only relevant exceptions, preserving unrelated delayed starts.
func (c Config) codexRuleToken(root, cwd string) string {
	var relevant []archive.ProjectActivation
	for _, p := range c.Archive.Projects {
		p.Root = c.codexRuleRoot(p.Root)
		if local.PathWithin(root, p.Root) || local.PathWithin(cwd, p.Root) {
			relevant = append(relevant, p)
		}
	}
	var barriers []CodexRuleBarrier
	for _, b := range c.CodexCapture.Barriers {
		if local.PathWithin(root, b.Root) || local.PathWithin(cwd, b.Root) {
			barriers = append(barriers, b)
		}
	}
	b, _ := json.Marshal(struct {
		Rules    []archive.ProjectActivation `json:"rules"`
		Barriers []CodexRuleBarrier          `json:"barriers"`
	}{relevant, barriers})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CodexDiscoveryGeneration requires both original scope and source permission windows.
func (c Config) CodexDiscoveryGeneration(root, cwd string, started, now time.Time) (string, bool) {
	token, ok := c.CodexGeneration(root, cwd, started, now)
	if !ok || c.Discovery == nil || !c.Discovery.Enabled {
		return "", false
	}
	a := c.CodexCapture.SourceAuthorization
	if a == nil || discoveryNativeStartFloor(*a).IsZero() || started.Before(discoveryNativeStartFloor(*a)) || a.DestinationID != c.DestinationID() {
		return "", false
	}
	for _, v := range a.Intervals {
		if !started.Before(v.Start) && (v.End.IsZero() || started.Before(v.End)) {
			return token + ":" + a.Generation, true
		}
	}
	return "", false
}

// PreserveWriterFence protects rollback snapshots without copying authorization.
// A legacy snapshot restored over v3 remains included-project-only and fenced.
func PreserveWriterFence(next *Config, previous Config) {
	next.HistoricalProjectProtection = next.HistoricalProjectProtection || previous.HistoricalProjectProtection
	next.ImportStagingProtection = next.ImportStagingProtection || previous.ImportStagingProtection
	next.RetentionProtection = next.RetentionProtection || previous.RetentionProtection
	next.DurableStorageProtection = next.DurableStorageProtection || previous.DurableStorageProtection
	next.CodexHistoryProtection = next.CodexHistoryProtection || previous.CodexHistoryProtection
	next.GenerationProtection = next.GenerationProtection || previous.GenerationProtection
	if previous.CodexCapture != nil && next.CodexCapture == nil {
		next.CodexCapture = &CodexCaptureConfig{Scope: CodexIncludedProjects, Barriers: slices.Clone(previous.CodexCapture.Barriers), Revision: previous.CodexCapture.Revision}
	}
}

func (c Config) codexRuleRoot(root string) string {
	if c.CodexCapture != nil {
		if canonical, ok := c.CodexCapture.RuleRoots[root]; ok {
			return canonical
		}
	}
	return filepath.Clean(root)
}

func (c Config) codexWinningRule(root, cwd string) (archive.ProjectActivation, bool) {
	checkout, hasCheckout := c.codexRuleAt(cwd)
	main, hasMain := c.codexRuleAt(root)
	if hasCheckout && !checkout.Included {
		return checkout, true
	}
	if hasMain && !main.Included {
		if !hasCheckout || !checkout.Included || !local.PathWithin(checkout.Root, main.Root) {
			return main, true
		}
	}
	if hasCheckout {
		return checkout, true
	}
	return main, hasMain
}

func (c Config) codexRuleAt(path string) (archive.ProjectActivation, bool) {
	best := -1
	var rule archive.ProjectActivation
	for _, p := range c.Archive.Projects {
		canonical := c.codexRuleRoot(p.Root)
		if local.PathWithin(path, canonical) && len(canonical) > best {
			p.Root = canonical
			best = len(canonical)
			rule = p
		}
	}
	return rule, best >= 0
}

// CodexContinuationAllowed applies current exceptions to an immutable admission.
// Forward barriers and activation timestamps constrain new admissions only.
func (c Config) CodexContinuationAllowed(root, cwd string) bool {
	rule, found := c.codexWinningRule(root, cwd)
	return !found || rule.Included
}

func reconcileCodexSource(p *CodexCaptureConfig, next, previous Config, active, keep bool, now, nativeConsentStart time.Time) error {
	old := previous.CodexCapture
	if active && next.Discovery != nil && next.Discovery.Enabled {
		sourceKeep := keep && old.SourceAuthorization != nil && !discoveryNativeStartFloor(*old.SourceAuthorization).IsZero() && previous.Discovery != nil && previous.Discovery.Enabled && slices.Equal(previous.Discovery.CodexHomes, next.Discovery.CodexHomes)
		if sourceKeep {
			p.SourceAuthorization = normalizedAuthorization(old.SourceAuthorization)
			if next.Paused != previous.Paused {
				if err := validateIntervalTransition(*p.SourceAuthorization, next.Paused, now.UTC()); err != nil {
					return err
				}
				transitionIntervals(p.SourceAuthorization, next.Paused, now.UTC())
			}
		} else {
			id, err := local.ID()
			if err != nil {
				return err
			}
			start := nativeConsentStart
			var intervals []DiscoveryInterval
			if !next.Paused {
				intervals = []DiscoveryInterval{{Start: start}}
			}
			a := DiscoveryAuthorization{Generation: id, NativeStartFloor: start, Agent: "codex", DestinationID: next.DestinationID(), Intervals: intervals}
			p.SourceAuthorization = &a
		}
	}
	return nil
}

func reconcileCodexBarriers(p *CodexCaptureConfig, previous, next Config, now time.Time) {
	for _, before := range previous.Archive.Projects {
		if before.Included {
			continue
		}
		// A retargeted path lifts its prior physical exclusion as well.
		unchanged := false
		for _, after := range next.Archive.Projects {
			if after.Root == before.Root && !after.Included && p.RuleRoots[after.Root] == previous.codexRuleRoot(before.Root) {
				unchanged = true
			}
		}
		if !unchanged {
			found := false
			for i := range p.Barriers {
				if p.Barriers[i].Root == previous.codexRuleRoot(before.Root) {
					if now.After(p.Barriers[i].Since) {
						p.Barriers[i].Since = now.UTC()
					}
					found = true
				}
			}
			if !found {
				p.Barriers = append(p.Barriers, CodexRuleBarrier{Root: previous.codexRuleRoot(before.Root), Since: now.UTC()})
			}
		}
	}
}

// Canonicalize the existing ancestor of a removed rule root. Exceptions still
// apply to retained physical facts when the leaf is temporarily unavailable.
func canonicalRuleRoot(root string) string {
	root = filepath.Clean(root)
	for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
		if canonical, err := filepath.EvalSymlinks(parent); err == nil {
			relative, err := filepath.Rel(parent, root)
			if err == nil {
				return filepath.Join(canonical, relative)
			}
		}
		if filepath.Dir(parent) == parent {
			return root
		}
	}
}
