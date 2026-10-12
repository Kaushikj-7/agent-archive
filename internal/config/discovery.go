package config

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"reflect"
	"slices"
	"strings"
	"time"
)

const discoveryWriterMarker = "+discovery-v2"

const codexWriterMarker = "+codex-scope-floor-v3"

const legacyCodexWriterMarker = "+codex-scope-v3"

// DiscoveryConfig records consent, roots and current per-project generations.
// Retain it even after disablement: old writers must never flatten the history.
type DiscoveryConfig struct {
	ChoiceRecorded bool                     `json:"choice_recorded,omitempty"`
	Enabled        bool                     `json:"enabled"`
	CodexHomes     []string                 `json:"codex_homes"`
	Authorizations []DiscoveryAuthorization `json:"authorizations"`
}

// SetDiscoveryChoice records proposed setup consent without opening authorization
// intervals. ReconcileDiscovery opens intervals only for the committed scope.
func SetDiscoveryChoice(c *Config, enabled bool) {
	d := DiscoveryConfig{}
	if c.Discovery != nil {
		d = *c.Discovery
	}
	d.Enabled = enabled
	d.ChoiceRecorded = true
	c.Discovery = &d
	_ = prepareDiscoveryConfig(c)
}

// SetSkillEvidence changes the user policy while retaining discovery writer protection.
func SetSkillEvidence(c *Config, mode SkillEvidence) {
	c.SkillEvidence = mode
	_ = prepareDiscoveryConfig(c)
}

// DiscoveryAuthorization is a current permission generation for one scope.
type DiscoveryAuthorization struct {
	Generation string `json:"generation"`
	// NativeStartFloor is immutable within a generation, including while paused.
	// Zero identifies an older empty history whose consent boundary is unknown.
	NativeStartFloor time.Time           `json:"native_start_floor,omitzero"`
	Agent            string              `json:"agent"`
	ProjectRoot      string              `json:"project_root"`
	DestinationID    string              `json:"destination_id"`
	Intervals        []DiscoveryInterval `json:"intervals"`
}

// DiscoveryInterval is a half-open unpaused interval; zero end is open.
type DiscoveryInterval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
}

func underlyingSkillEvidence(mode SkillEvidence) SkillEvidence {
	for {
		clean := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(string(mode), codexWriterMarker), legacyCodexWriterMarker), discoveryWriterMarker)
		if clean == string(mode) {
			return mode
		}
		mode = SkillEvidence(clean)
	}
}

func prepareDiscoveryConfig(c *Config) error {
	if err := ValidateRetentionDays(c.RetentionDays); err != nil {
		return err
	}
	if unsupportedPreparedWriter(*c) {
		return errors.New("configuration requires a newer agent-archive writer")
	}
	if c.Discovery != nil {
		// Migrate only provable retained permission, on a private value retained.
		// Empty old histories have no recoverable floor and remain fail closed.
		d := *c.Discovery
		d.Authorizations = slices.Clone(d.Authorizations)
		for i := range d.Authorizations {
			a := &d.Authorizations[i]
			if a.NativeStartFloor.IsZero() && len(a.Intervals) > 0 {
				a.NativeStartFloor = a.Intervals[0].Start
			}
		}
		c.Discovery = &d
		c.SkillEvidence = SkillEvidence(string(c.EffectiveSkillEvidence()) + discoveryWriterMarker)
		c.SchemaVersion = 2
	}
	if c.CodexCapture != nil {
		p := *c.CodexCapture
		p.Authorization = normalizedAuthorization(p.Authorization)
		p.SourceAuthorization = normalizedAuthorization(p.SourceAuthorization)
		c.CodexCapture = &p
		c.SkillEvidence = SkillEvidence(string(c.EffectiveSkillEvidence()) + codexWriterMarker)
		c.SchemaVersion = 3
	}
	if c.GenerationProtection {
		c.SchemaVersion = 4
	}
	if c.CodexHistoryProtection {
		c.SchemaVersion = 5
	}
	if c.DurableStorageProtection {
		c.SchemaVersion = 7
	}
	if c.RetentionProtection {
		c.SchemaVersion = 8
	}
	if c.ImportStagingProtection {
		c.SchemaVersion = 9
	}
	if c.HistoricalProjectProtection {
		c.SchemaVersion = 10
	}
	return validateDiscoveryConfig(*c)
}

func validateDiscoveryConfig(c Config) error {
	var err error
	c, err = peelHistoricalProjectProtection(c)
	if err != nil {
		return err
	}
	c, err = peelImportStagingProtection(c)
	if err != nil {
		return err
	}
	if c.RetentionProtection {
		if c.SchemaVersion != 8 {
			return errors.New("retention protection requires schema 8")
		}
		c.RetentionProtection = false
		c.SchemaVersion = 1
		if c.Discovery != nil {
			c.SchemaVersion = 2
		}
		if c.CodexCapture != nil {
			c.SchemaVersion = 3
		}
		if c.GenerationProtection {
			c.SchemaVersion = 4
		}
		if c.CodexHistoryProtection {
			c.SchemaVersion = 5
		}
		if c.DurableStorageProtection {
			c.SchemaVersion = 7
		}
	}
	if c.DurableStorageProtection {
		if c.SchemaVersion != 7 {
			return errors.New("durable storage protection requires schema 7")
		}
		c.DurableStorageProtection = false
		c.SchemaVersion = 1
		if c.Discovery != nil {
			c.SchemaVersion = 2
		}
		if c.CodexCapture != nil {
			c.SchemaVersion = 3
		}
		if c.GenerationProtection {
			c.SchemaVersion = 4
		}
		if c.CodexHistoryProtection {
			c.SchemaVersion = 5
		}
	}
	if c.CodexHistoryProtection {
		if c.SchemaVersion != 5 {
			return errors.New("history protection requires schema 5")
		}
		c.CodexHistoryProtection = false
		c.SchemaVersion = 1
		if c.Discovery != nil {
			c.SchemaVersion = 2
		}
		if c.CodexCapture != nil {
			c.SchemaVersion = 3
		}
		if c.GenerationProtection {
			c.SchemaVersion = 4
		}
	}
	if c.GenerationProtection {
		if c.SchemaVersion != 4 {
			return errors.New("generation protection requires schema 4")
		}
		// The outer writer fence protects generations; underlying policies retain
		// their independently validated v1/v2/v3 consent semantics.
		c.GenerationProtection = false
		c.SchemaVersion = 1
		if c.Discovery != nil {
			c.SchemaVersion = 2
		}
		if c.CodexCapture != nil {
			c.SchemaVersion = 3
		}
	}

	if c.CodexCapture != nil {
		return validateCodexConfig(c)
	}
	marked := strings.HasSuffix(string(c.SkillEvidence), discoveryWriterMarker)
	if marked != (c.Discovery != nil) {
		return errors.New("discovery writer compatibility marker and authorization must be kept together")
	}
	if c.SchemaVersion > 2 {
		return errors.New("configuration requires a newer agent-archive writer")
	}
	if c.Discovery == nil {
		return nil
	}
	if c.SchemaVersion != 2 {
		return errors.New("discovery requires configuration schema 2")
	}
	return validateDiscoveryPayload(c.Discovery)
}

func validateCodexConfig(c Config) error {
	if c.SchemaVersion != 3 || (!strings.HasSuffix(string(c.SkillEvidence), codexWriterMarker) && !strings.HasSuffix(string(c.SkillEvidence), legacyCodexWriterMarker)) {
		return errors.New("codex scope requires protected schema 3")
	}
	if c.CodexCapture.Scope != CodexIncludedProjects && c.CodexCapture.Scope != CodexAllProjects {
		return errors.New("invalid Codex scope")
	}
	if len(c.CodexCapture.Barriers) > 4096 || len(c.CodexCapture.RuleRoots) > 4096 || len(c.Archive.Projects) > 4096 {
		return errors.New("codex exception history limit exceeded")
	}
	for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
		if a == nil {
			continue
		}
		if a.Generation == "" || a.Agent != "codex" || a.ProjectRoot != "" || a.DestinationID == "" || len(a.Intervals) > 256 {
			return errors.New("invalid Codex authorization")
		}
		var prior time.Time
		for i, v := range a.Intervals {
			if v.Start.IsZero() || (!a.NativeStartFloor.IsZero() && v.Start.Before(a.NativeStartFloor)) || (!v.End.IsZero() && !v.End.After(v.Start)) || (i > 0 && (prior.IsZero() || v.Start.Before(prior))) {
				return errors.New("invalid Codex authorization intervals")
			}
			prior = v.End
		}
	}
	for _, b := range c.CodexCapture.Barriers {
		if b.Root == "" || b.Since.IsZero() {
			return errors.New("invalid Codex subtree barrier")
		}
	}
	return validateDiscoveryPayload(c.Discovery)

}

// ReconcileDiscovery creates new generations when effective scope changes.
// Setup calls it before its journal commits the config and all permissions.
func ReconcileDiscovery(next *Config, previous Config, now time.Time) error {
	nativeConsentStart := now.UTC()
	if next.DestinationSince.After(nativeConsentStart) {
		nativeConsentStart = next.DestinationSince
	}
	draft := *next
	if err := reconcileDiscovery(&draft, previous, now, nativeConsentStart); err != nil {
		return err
	}
	*next = draft
	return nil
}

func reconcileDiscovery(next *Config, previous Config, now, nativeConsentStart time.Time) error {
	if err := ReconcileCodexCapture(next, previous, now); err != nil {
		return err
	}
	if next.Discovery == nil {
		next.Discovery = previous.Discovery
	}
	if next.Discovery == nil {
		return nil
	}
	d := *next.Discovery
	d.CodexHomes = slices.Clone(d.CodexHomes)
	d.Authorizations = nil
	if d.Enabled && slices.Contains(next.Harnesses, "codex") && next.Archive.Enabled {
		for _, p := range next.Archive.Projects {
			if !p.Included {
				continue
			}
			var kept *DiscoveryAuthorization
			if previous.Discovery != nil && previous.Discovery.Enabled && slices.Contains(previous.Harnesses, "codex") && previous.Archive.Enabled && sameDiscoveryProjectScope(p.Root, previous, *next) && slices.Equal(previous.Discovery.CodexHomes, d.CodexHomes) {
				for _, a := range previous.Discovery.Authorizations {
					if a.ProjectRoot == p.Root && a.DestinationID == next.DestinationID() && (!a.NativeStartFloor.IsZero() || len(a.Intervals) > 0) {
						a.Intervals = slices.Clone(a.Intervals)
						kept = &a
						break
					}
				}
			}
			if kept == nil {
				id, err := local.ID()
				if err != nil {
					return err
				}
				start := nativeConsentStart
				if p.ActivatedAt.After(start) {
					start = p.ActivatedAt
				}
				var intervals []DiscoveryInterval
				if !next.Paused {
					intervals = []DiscoveryInterval{{Start: start}}
				}
				a := DiscoveryAuthorization{Generation: id, NativeStartFloor: start, Agent: "codex", ProjectRoot: p.Root, DestinationID: next.DestinationID(), Intervals: intervals}
				kept = &a
			}
			d.Authorizations = append(d.Authorizations, *kept)
		}
	}
	next.Discovery = &d
	return prepareDiscoveryConfig(next)
}

func transitionDiscoveryPause(c *Config, paused bool, now time.Time) error {
	if c.Paused == paused {
		return nil
	}
	var scopes []*DiscoveryAuthorization
	if c.CodexCapture != nil {
		scopes = append(scopes, c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization)
	}
	if c.Discovery != nil {
		for i := range c.Discovery.Authorizations {
			scopes = append(scopes, &c.Discovery.Authorizations[i])
		}
	}
	for _, a := range scopes {
		if a != nil {
			if err := validateIntervalTransition(*a, paused, now); err != nil {
				return err
			}
		}
	}
	// Preflight the complete policy, source and project set before copying or
	// changing any shared snapshot. Refusal leaves callers and disk unchanged.
	if c.CodexCapture != nil {
		p := *c.CodexCapture
		p.Authorization = normalizedAuthorization(p.Authorization)
		p.SourceAuthorization = normalizedAuthorization(p.SourceAuthorization)
		c.CodexCapture = &p
		for _, a := range []*DiscoveryAuthorization{p.Authorization, p.SourceAuthorization} {
			if a != nil {
				transitionIntervals(a, paused, now)
			}
		}
	}
	if c.Discovery != nil {
		d := *c.Discovery
		d.Authorizations = slices.Clone(d.Authorizations)
		for i := range d.Authorizations {
			d.Authorizations[i].Intervals = slices.Clone(d.Authorizations[i].Intervals)
			transitionIntervals(&d.Authorizations[i], paused, now)
		}
		c.Discovery = &d
	}
	return nil
}

func normalizedAuthorization(a *DiscoveryAuthorization) *DiscoveryAuthorization {
	if a == nil {
		return nil
	}
	retained := *a
	retained.Intervals = slices.Clone(a.Intervals)
	if retained.NativeStartFloor.IsZero() && len(retained.Intervals) > 0 {
		retained.NativeStartFloor = retained.Intervals[0].Start
	}
	return &retained
}

func validateIntervalTransition(a DiscoveryAuthorization, paused bool, now time.Time) error {
	if !paused {
		floor := discoveryNativeStartFloor(a)
		if floor.IsZero() {
			return errors.New("discovery consent boundary is unavailable; run setup to renew authorization before resuming")
		}
		if now.Before(floor) {
			return errors.New("clock precedes discovery consent boundary; correct the clock before pausing or resuming")
		}
	}
	if n := len(a.Intervals); n > 0 {
		last := a.Intervals[n-1]
		if (paused && last.End.IsZero() && !now.After(last.Start)) || (!paused && (last.End.IsZero() || now.Before(last.End))) {
			return errors.New("clock precedes discovery consent boundary; correct the clock before pausing or resuming")
		}
	}
	return nil
}

// transitionIntervals requires the caller to preflight every affected scope.
func transitionIntervals(a *DiscoveryAuthorization, paused bool, now time.Time) {
	if paused {
		if n := len(a.Intervals); n > 0 && a.Intervals[n-1].End.IsZero() {
			a.Intervals[n-1].End = now
		}
	} else {
		if len(a.Intervals) >= 256 {
			a.Intervals = a.Intervals[1:]
		}
		a.Intervals = append(a.Intervals, DiscoveryInterval{Start: now})
	}
}

// DiscoveryGeneration authorizes native start evidence under the current
// agent/project/destination generation. Scan time is never a start boundary.
func (c Config) DiscoveryGeneration(agent, root string, started, now time.Time) (string, bool) {
	if c.Discovery == nil || !c.Discovery.Enabled || c.Paused || !c.Archive.Enabled || !slices.Contains(c.Harnesses, agent) || started.IsZero() || started.After(now.Add(2*time.Minute)) {
		return "", false
	}
	if agent == "codex" && c.EffectiveCodexCaptureScope() == CodexAllProjects {
		return c.CodexDiscoveryGeneration(root, root, started, now)
	}
	for _, a := range c.Discovery.Authorizations {
		if a.Agent != agent || a.ProjectRoot != root || a.DestinationID != c.DestinationID() {
			continue
		}
		floor := discoveryNativeStartFloor(a)
		if floor.IsZero() || started.Before(floor) {
			continue
		}
		for _, v := range a.Intervals {
			if !started.Before(v.Start) && (v.End.IsZero() || started.Before(v.End)) {
				return a.Generation, true
			}
		}
	}
	return "", false
}

// DiscoveryStartFloor is the earliest native start discovery admits for
// agent under any current authorization (DiscoveryGeneration and
// CodexDiscoveryGeneration refuse every session that started before it):
// the earliest project floor, or, for Codex in all-projects scope, the
// later of the capture and source floors, since admission needs both. ok
// is false when no current authorization of agent has a known floor.
func (c Config) DiscoveryStartFloor(agent string) (time.Time, bool) {
	if c.Discovery == nil || !c.Discovery.Enabled {
		return time.Time{}, false
	}
	if agent == "codex" && c.EffectiveCodexCaptureScope() == CodexAllProjects {
		capture, source := c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization
		if capture == nil || source == nil || capture.DestinationID != c.DestinationID() || source.DestinationID != c.DestinationID() {
			return time.Time{}, false
		}
		captureFloor, sourceFloor := discoveryNativeStartFloor(*capture), discoveryNativeStartFloor(*source)
		if captureFloor.IsZero() || sourceFloor.IsZero() {
			return time.Time{}, false
		}
		if captureFloor.After(sourceFloor) {
			return captureFloor, true
		}
		return sourceFloor, true
	}
	var earliest time.Time
	for _, a := range c.Discovery.Authorizations {
		if a.Agent != agent || a.DestinationID != c.DestinationID() {
			continue
		}
		if floor := discoveryNativeStartFloor(a); !floor.IsZero() && (earliest.IsZero() || floor.Before(earliest)) {
			earliest = floor
		}
	}
	return earliest, !earliest.IsZero()
}

// Legacy retained history can only narrow permission to its earliest surviving
// interval. Activation or observation times cannot recover an empty history.
func discoveryNativeStartFloor(a DiscoveryAuthorization) time.Time {
	if !a.NativeStartFloor.IsZero() {
		return a.NativeStartFloor
	}
	if len(a.Intervals) > 0 {
		return a.Intervals[0].Start
	}
	return time.Time{}
}

func sameDiscoveryProjectScope(root string, previous, next Config) bool {
	within := func(c Config) []archive.ProjectActivation {
		var relevant []archive.ProjectActivation
		for _, p := range c.Archive.Projects {
			if local.PathWithin(p.Root, root) || local.PathWithin(root, p.Root) {
				relevant = append(relevant, p)
			}
		}
		slices.SortFunc(relevant, func(a, b archive.ProjectActivation) int { return strings.Compare(a.Root, b.Root) })
		return relevant
	}
	return reflect.DeepEqual(within(previous), within(next))
}

// ProtectIdentityWriter preserves policy while making older writers refuse
// namespaced state. Callers hold hooks.lock after checking setup's journal.
// Disablement is explicit: this never grants discovery consent or intervals.
func ProtectIdentityWriter(home string) error {
	c, found, fenced, err := loadConfig(home)
	if err != nil || !found {
		return err
	}
	if (c.Discovery != nil || c.CodexCapture != nil) && fenced {
		return nil
	}
	if c.Discovery == nil {
		c.Discovery = &DiscoveryConfig{}
	}
	return Save(home, c)
}

func validateDiscoveryPayload(d *DiscoveryConfig) error {
	if d == nil {
		return nil
	}
	if len(d.Authorizations) > 4096 || len(d.CodexHomes) > 16 {
		return errors.New("discovery authorization limits exceeded")
	}
	for _, a := range d.Authorizations {
		if a.Generation == "" || a.Agent != "codex" || a.ProjectRoot == "" || a.DestinationID == "" || len(a.Intervals) > 256 {
			return errors.New("invalid discovery authorization")
		}
		var prior time.Time
		for i, v := range a.Intervals {
			if v.Start.IsZero() || (!a.NativeStartFloor.IsZero() && v.Start.Before(a.NativeStartFloor)) || (!v.End.IsZero() && !v.End.After(v.Start)) || (i > 0 && (prior.IsZero() || v.Start.Before(prior))) {
				return errors.New("invalid discovery intervals")
			}
			prior = v.End
		}
	}
	return nil
}

// peelImportStagingProtection validates the capability before checking every
// underlying policy floor. Staging does not grant legacy permission migration.
func peelImportStagingProtection(c Config) (Config, error) {
	if c.SchemaVersion > 9 || (c.SchemaVersion == 9 && !c.ImportStagingProtection) {
		return c, errors.New("configuration requires a newer agent-archive writer")
	}
	if !c.ImportStagingProtection {
		return c, nil
	}
	if c.SchemaVersion != 9 {
		return c, errors.New("import staging protection requires schema 9")
	}
	c.ImportStagingProtection = false
	c.SchemaVersion = 1
	if c.Discovery != nil {
		c.SchemaVersion = 2
	}
	if c.CodexCapture != nil {
		c.SchemaVersion = 3
	}
	if c.GenerationProtection {
		c.SchemaVersion = 4
	}
	if c.CodexHistoryProtection {
		c.SchemaVersion = 5
	}
	if c.DurableStorageProtection {
		c.SchemaVersion = 7
	}
	if c.RetentionProtection {
		c.SchemaVersion = 8
	}
	return c, nil
}

// peelHistoricalProjectProtection preserves every underlying writer floor.
func peelHistoricalProjectProtection(c Config) (Config, error) {
	if c.SchemaVersion > 10 || c.SchemaVersion == 10 && !c.HistoricalProjectProtection {
		return c, errors.New("configuration requires a newer agent-archive writer")
	}
	if !c.HistoricalProjectProtection {
		return c, nil
	}
	if c.SchemaVersion != 10 {
		return c, errors.New("historical project protection requires schema 10")
	}
	c.HistoricalProjectProtection = false
	c.SchemaVersion = 1
	if c.Discovery != nil {
		c.SchemaVersion = 2
	}
	if c.CodexCapture != nil {
		c.SchemaVersion = 3
	}
	if c.GenerationProtection {
		c.SchemaVersion = 4
	}
	if c.CodexHistoryProtection {
		c.SchemaVersion = 5
	}
	if c.DurableStorageProtection {
		c.SchemaVersion = 7
	}
	if c.RetentionProtection {
		c.SchemaVersion = 8
	}
	if c.ImportStagingProtection {
		c.SchemaVersion = 9
	}
	return c, nil
}

func unsupportedPreparedWriter(c Config) bool {
	return (c.SchemaVersion > 5 && c.SchemaVersion != 7 && c.SchemaVersion != 8 && c.SchemaVersion != 9 && c.SchemaVersion != 10) || (c.SchemaVersion == 10 && !c.HistoricalProjectProtection) || (c.SchemaVersion == 9 && !c.ImportStagingProtection) || (c.SchemaVersion == 8 && !c.RetentionProtection) || (c.SchemaVersion == 7 && !c.DurableStorageProtection) || (c.SchemaVersion == 5 && !c.CodexHistoryProtection) || (c.SchemaVersion == 4 && !c.GenerationProtection) || (c.SchemaVersion == 3 && c.CodexCapture == nil)
}
