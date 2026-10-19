// Package builtin composes immutable integrations without host probes.
package builtin

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/agents/nativecapture"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// Integration binds implemented operations to an identity declaration.
type Integration struct {
	Labels          agentapi.LabelProvider
	Descriptor      agentmeta.Descriptor
	Launcher        agentapi.Launcher
	Parser          agentapi.TranscriptParser
	Preview         agentapi.RecordPreviewer
	Sources         agentapi.SourceProvider
	Filter          agentapi.TranscriptFilter
	Runtime         agentapi.RuntimeDetector
	Hooks           agentapi.HookConfigurator
	Decoder         agentapi.HookDecoder
	Skills          agentapi.SkillProvider
	NativeHeaders   agentapi.NativeHeaderInspector
	Evidence        agentapi.CapabilityEvidenceProvider
	Version         agentapi.VersionInspector
	Discovery       agentapi.Discoverer
	DatabaseCatalog agentapi.DatabaseCatalogInspector
	NativePaths     agentapi.NativePathsProvider
	Worktrees       agentapi.MissingWorktreeResolver
	Workspace       agentapi.WorkspaceResolver
	Children        agentapi.ChildDiscoverer
	Imports         agentapi.ImportInspector
}

// Registry holds validated immutable lookups and operation projections.
type Registry struct {
	fixedCapture   *fixedCursorCapture
	catalog        agentmeta.Catalog
	sourceBindings map[string]Integration
	bindings       map[agentmeta.ID]Integration
	supporting     map[agentmeta.Operation][]Integration
	ordered        []Integration
	runtime        []Integration
	sessionKeys    []string
}

// New binds each identity exactly once and derives operations from actual ports.
func New(identities agentmeta.Catalog, bindings []Integration) (*Registry, error) {
	if identities == nil || nilImplementation(identities) {
		return nil, fmt.Errorf("missing identity catalog")
	}
	r := &Registry{bindings: make(map[agentmeta.ID]Integration), sourceBindings: map[string]Integration{}, supporting: make(map[agentmeta.Operation][]Integration)}
	for _, b := range bindings {
		d, ok := identities.Lookup(string(b.Descriptor.ID))
		if !ok || d.ID != b.Descriptor.ID {
			return nil, fmt.Errorf("unknown canonical binding %q", b.Descriptor.ID)
		}
		if _, ok := r.bindings[d.ID]; ok {
			return nil, fmt.Errorf("duplicate binding %q", d.ID)
		}
		var err error
		d, err = boundDescriptor(b, d)
		if err != nil {
			return nil, err
		}
		b.Descriptor = d
		r.bindings[d.ID] = b
		for _, name := range append([]string{string(d.ID)}, d.Aliases...) {
			r.sourceBindings[name] = b
		}
	}
	ds := identities.All()
	for i, d := range ds {
		b, ok := r.bindings[d.ID]
		if !ok {
			return nil, fmt.Errorf("missing binding %s", d.ID)
		}
		ds[i] = b.Descriptor
		r.ordered = append(r.ordered, b)
		r.addOperations(b)
	}
	var err error
	r.catalog, err = agentmeta.New(ds)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func nilImplementation(v interface{}) bool {
	rv := reflect.ValueOf(v)
	kind := rv.Kind()
	if kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice {
		return rv.IsNil()
	}
	return false
}

// Catalog returns the immutable derived operation catalog.
func (r *Registry) Catalog() agentmeta.Catalog { return r.catalog }

// Lookup resolves aliases and returns defensive descriptor slices.
func (r *Registry) Lookup(name string) (Integration, bool) {
	d, ok := r.catalog.Lookup(name)
	if !ok {
		return Integration{}, false
	}
	b := r.bindings[d.ID]
	b.Descriptor = d
	return b, true
}

// Supporting returns integrations in stable presentation order with copied metadata.
func (r *Registry) Supporting(op agentmeta.Operation) []Integration {
	out := make([]Integration, 0, len(r.supporting[op]))
	for _, b := range r.supporting[op] {
		entry, _ := r.Lookup(string(b.Descriptor.ID))
		out = append(out, entry)
	}
	return out
}

// NewBuiltins binds the built-in identities to their concrete implementations.
func NewBuiltins() *Registry {
	r, err := New(agentmeta.Builtins(), []Integration{
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Claude}, Launcher: claude.Launcher{}, Skills: claude.Skills(), Imports: claude.Imports(), Evidence: claude.CapabilityEvidence{}, Version: claude.VersionInspector{}, NativeHeaders: claude.NativeHeaders{}, Discovery: claude.NativeHeaders{}, NativePaths: claude.ProjectEvidence{}, Worktrees: claude.ProjectEvidence{}, Children: claude.Children{}, Sources: claude.SourceProvider{}, Filter: claude.Filter{}, Runtime: claude.RuntimeDetector{}, Hooks: claude.Hooks(), Decoder: claude.Decoder(), Parser: claude.Parser{}, Preview: claude.Previewer{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Launcher: codex.Launcher{}, Skills: codex.Skills(), Imports: codex.Imports(), Evidence: codex.CapabilityEvidence{}, Version: codex.VersionInspector{}, NativeHeaders: codex.NativeHeaders{}, Discovery: codex.NativeHeaders{}, NativePaths: codex.ProjectEvidence{}, Sources: codex.SourceProvider{}, Filter: codex.Filter{}, Runtime: codex.RuntimeDetector{}, Hooks: codex.Hooks(), Decoder: codex.Decoder(), Parser: codex.Parser{}, Preview: codex.Previewer{}, Labels: codex.LabelProvider{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Cursor}, Launcher: cursor.Launcher{}, Skills: cursor.Skills(), Imports: cursor.Imports(), Evidence: cursor.CapabilityEvidence{}, Version: cursor.VersionInspector{}, Discovery: cursor.Discovery{}, NativePaths: cursor.ProjectEvidence{}, Workspace: cursor.ProjectEvidence{}, DatabaseCatalog: cursor.DatabaseCatalogInspector{}, Sources: cursor.SourceProvider{}, Filter: cursor.Filter{}, Runtime: cursor.RuntimeDetector{}, Hooks: cursor.Hooks(), Decoder: cursor.Decoder(), Parser: cursor.Parser{}, Labels: cursor.LabelProvider{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Muse}, Decoder: muse.Decoder(), Hooks: muse.Hooks(), NativeHeaders: muse.NativeHeaders{}, Discovery: muse.NativeHeaders{}, Preview: muse.Previewer{}, Runtime: muse.RuntimeDetector{}, Sources: muse.SourceProvider{}, Filter: muse.Filter{}, Parser: muse.Parser{}},
	})
	if err != nil {
		panic(err)
	}
	r.fixedCapture = &fixedCursorCapture{}
	return r
}

// LookupLabels resolves the optional native metadata provider without probing.
func (r *Registry) LookupLabels(name string) (agentapi.LabelProvider, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Labels, ok && b.Labels != nil
}

// LookupParser resolves only the parser needed by retained-source derivation.
func (r *Registry) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	b, ok := r.sourceBindings[strings.ToLower(strings.TrimSpace(name))]
	return b.Parser, ok && b.Parser != nil
}

// LookupPreview resolves a bounded safe-record preview decoder.
func (r *Registry) LookupPreview(name string) (agentapi.RecordPreviewer, bool) {
	b, ok := r.sourceBindings[strings.ToLower(strings.TrimSpace(name))]
	return b.Preview, ok && b.Preview != nil
}

// Launcher resolves the native launch operation without exposing other ports.
func (r *Registry) Launcher(name string) (agentapi.Launcher, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Launcher, ok && b.Launcher != nil
}

// Observations visits the precomputed runtime projection in presentation order.
func (r *Registry) Observations(env agentapi.RuntimeEnvironment) []agentapi.AgentRuntime {
	out := make([]agentapi.AgentRuntime, 0, len(r.runtime))
	for _, b := range r.runtime {
		observation := b.Runtime.Detect(env)
		if observation.PresenceKey != "" {
			out = append(out, agentapi.AgentRuntime{Agent: b.Descriptor.ID, RuntimeObservation: observation})
		}
	}
	return out
}

// SessionEnvironmentKeys returns the defensive cleanup union built at composition.
func (r *Registry) SessionEnvironmentKeys() []string { return slices.Clone(r.sessionKeys) }

// Launchers returns the implemented launch projection in presentation order.
func (r *Registry) Launchers() []agentapi.AgentLauncher {
	bindings := r.supporting[agentmeta.Launch]
	out := make([]agentapi.AgentLauncher, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, agentapi.AgentLauncher{Agent: b.Descriptor.ID, Launcher: b.Launcher})
	}
	return out
}

func (r *Registry) addOperations(b Integration) {
	for _, op := range b.Descriptor.Operations {
		r.supporting[op] = append(r.supporting[op], b)
	}
	if b.Runtime == nil {
		return
	}
	r.runtime = append(r.runtime, b)
	for _, key := range b.Runtime.SessionEnvironmentKeys() {
		if !slices.Contains(r.sessionKeys, key) {
			r.sessionKeys = append(r.sessionKeys, key)
		}
	}
}

// LookupHooks returns only the managed configuration port.
func (r *Registry) LookupHooks(name string) (agentapi.HookConfigurator, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Hooks, ok && b.Hooks != nil
}

// HookAgents returns stable native configuration owners.
func (r *Registry) HookAgents() []string {
	var names []string
	for _, b := range r.supporting[agentmeta.ManagedHooks] {
		names = append(names, string(b.Descriptor.ID))
	}
	return names
}

// LookupDecoder resolves a narrow lifecycle port without host operations.
func (r *Registry) LookupDecoder(name string) (agentapi.HookDecoder, bool) {
	b, ok := r.bindings[agentmeta.ID(agentmeta.Canonical(r.catalog, name))]
	return b.Decoder, ok && b.Decoder != nil
}

// LookupSources resolves only source and filter ports.
func (r *Registry) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	b, ok := r.sourceBindings[strings.ToLower(strings.TrimSpace(name))]
	return b.Sources, b.Filter, ok && b.Sources != nil && b.Filter != nil
}

// SweepSources invokes only provider-declared abandoned-resource cleanup.
func (r *Registry) SweepSources() {
	for _, b := range r.bindings {
		if p, ok := b.Sources.(interface{ Sweep() }); ok {
			p.Sweep()
		}
	}
}

func boundDescriptor(b Integration, d agentmeta.Descriptor) (agentmeta.Descriptor, error) {
	if len(b.Descriptor.Aliases) > 0 || b.Descriptor.DisplayName != "" || len(b.Descriptor.Operations) > 0 {
		return agentmeta.Descriptor{}, fmt.Errorf("binding %s must contain only its canonical ID", d.ID)
	}
	operations, err := implementedOperations(b)
	if err != nil {
		return agentmeta.Descriptor{}, err
	}
	if len(operations) == 0 {
		return agentmeta.Descriptor{}, fmt.Errorf("agent %s has no operations", d.ID)
	}
	if b.Sources != nil || b.Filter != nil {
		if b.Sources == nil || b.Filter == nil {
			return agentmeta.Descriptor{}, fmt.Errorf("agent %s has incomplete source bindings", d.ID)
		}
		if b.Filter.Name() != string(d.ID) {
			return agentmeta.Descriptor{}, fmt.Errorf("agent %s has filter for %s", d.ID, b.Filter.Name())
		}
	}
	d.Operations = operations
	return d, nil
}

// LookupSkills resolves a native skill provider.
func (r *Registry) LookupSkills(name string) (agentapi.SkillProvider, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Skills, ok && b.Skills != nil
}

// SkillAgents lists implemented skill providers in catalog order.
func (r *Registry) SkillAgents() []string {
	var out []string
	for _, b := range r.supporting[agentmeta.Skills] {
		out = append(out, string(b.Descriptor.ID))
	}
	return out
}

// LookupNativeHeaders resolves bounded read-only identity interpretation.
func (r *Registry) LookupNativeHeaders(name string) (agentapi.NativeHeaderInspector, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.NativeHeaders, ok && b.NativeHeaders != nil
}

// NativeHeaderAgents lists bound inspectors without probing the host.
func (r *Registry) NativeHeaderAgents() []string {
	var out []string
	for _, b := range r.ordered {
		if b.NativeHeaders != nil {
			out = append(out, string(b.Descriptor.ID))
		}
	}
	return out
}

// LookupCapabilityEvidence resolves declared support evidence independently of health.
func (r *Registry) LookupCapabilityEvidence(name string) (agentapi.CapabilityEvidenceProvider, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Evidence, ok && b.Evidence != nil
}

// LookupVersionInspector resolves native installed-version observation.
func (r *Registry) LookupVersionInspector(name string) (agentapi.VersionInspector, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Version, ok && b.Version != nil
}

// VersionAgents lists bound inspectors without eager probes.
func (r *Registry) VersionAgents() []string {
	var names []string
	for _, b := range r.ordered {
		if b.Version != nil {
			names = append(names, string(b.Descriptor.ID))
		}
	}
	return names
}

// LookupDiscovery resolves native read-only discovery.
func (r *Registry) LookupDiscovery(name string) (agentapi.Discoverer, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Discovery, ok && b.Discovery != nil
}

// DiscoveryAgents lists actual discovery bindings in catalog order.
func (r *Registry) DiscoveryAgents() []string {
	var names []string
	for _, b := range r.ordered {
		if b.Discovery != nil {
			names = append(names, string(b.Descriptor.ID))
		}
	}
	return names
}

// LookupDatabaseCatalog resolves native compact catalog interpretation without opening a database.
func (r *Registry) LookupDatabaseCatalog(name string) (agentapi.DatabaseCatalogInspector, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.DatabaseCatalog, ok && b.DatabaseCatalog != nil
}

// LookupNativePaths resolves native path declarations without probing the host.
func (r *Registry) LookupNativePaths(name string) (agentapi.NativePathsProvider, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.NativePaths, ok && b.NativePaths != nil
}

// NativePathAgents lists implemented native path providers in catalog order.
func (r *Registry) NativePathAgents() []string {
	var out []string
	for _, b := range r.ordered {
		if b.NativePaths != nil {
			out = append(out, string(b.Descriptor.ID))
		}
	}
	return out
}

// WorktreeResolvers lists implemented native missing-worktree conventions in catalog order.
func (r *Registry) WorktreeResolvers() []agentapi.MissingWorktreeResolver {
	var out []agentapi.MissingWorktreeResolver
	for _, b := range r.ordered {
		if p := b.Worktrees; p != nil {
			out = append(out, p)
		}
	}
	return out
}

// LookupWorkspace resolves a native workspace metadata interpreter.
func (r *Registry) LookupWorkspace(name string) (agentapi.WorkspaceResolver, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Workspace, ok && b.Workspace != nil
}

// LookupChildren resolves native child discovery without admitting registrations.
func (r *Registry) LookupChildren(name string) (agentapi.ChildDiscoverer, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Children, ok && b.Children != nil
}

// LookupImport resolves native historical observation without admission policy.
func (r *Registry) LookupImport(name string) (agentapi.ImportInspector, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return b.Imports, ok && b.Imports != nil
}

// implementedOperations validates optional ports and derives promises only from
// actual implementations. Multiple project ports contribute one capability.
func implementedOperations(b Integration) ([]agentmeta.Operation, error) {
	ports := []struct {
		name           string
		operation      agentmeta.Operation
		implementation any
	}{
		{"launcher", agentmeta.Launch, b.Launcher},
		{"runtime", agentmeta.Runtime, b.Runtime},
		{"parser", agentmeta.Parse, b.Parser},
		{"preview", "", b.Preview},
		{"labels", "", b.Labels},
		{"sources", agentmeta.Source, b.Sources},
		{"filter", "", b.Filter},
		{"decoder", agentmeta.LifecycleHooks, b.Decoder},
		{"hooks", agentmeta.ManagedHooks, b.Hooks},
		{"skills", agentmeta.Skills, b.Skills},
		{"native headers", agentmeta.NativeInspection, b.NativeHeaders},
		{"version inspector", agentmeta.VersionInspection, b.Version},
		{"evidence provider", "", b.Evidence},
		{"native paths", agentmeta.NativeProjects, b.NativePaths},
		{"worktrees", agentmeta.NativeProjects, b.Worktrees},
		{"workspace", agentmeta.NativeProjects, b.Workspace},
		{"import inspector", agentmeta.HistoricalInspection, b.Imports},
		{"children", agentmeta.ChildDiscovery, b.Children},
		{"database catalog", agentmeta.DatabaseInspection, b.DatabaseCatalog},
		{"discovery", agentmeta.HistoricalDiscovery, b.Discovery},
	}
	var operations []agentmeta.Operation
	seen := map[agentmeta.Operation]bool{}
	for _, port := range ports {
		if port.implementation == nil {
			continue
		}
		if nilImplementation(port.implementation) {
			return nil, fmt.Errorf("typed nil %s for %s", port.name, b.Descriptor.ID)
		}
		if port.operation != "" && !seen[port.operation] {
			seen[port.operation] = true
			operations = append(operations, port.operation)
		}
	}
	return operations, nil
}

// CanonicalDiscovery normalizes a supported external name before planning.
func (r *Registry) CanonicalDiscovery(name string) (string, bool) {
	b, ok := r.sourceBindings[agentmeta.Normalize(name)]
	return string(b.Descriptor.ID), ok && b.Discovery != nil
}

// WithCodexLabelHost returns a narrow pass composition without mutating the registry.
func (r *Registry) WithCodexLabelHost(host agentapi.LabelHostFactory) agentapi.LabelsLookup {
	return nativeLabels{registry: r, host: host}
}

type nativeLabels struct {
	registry *Registry
	host     agentapi.LabelHostFactory
}

func (n nativeLabels) LookupLabels(name string) (agentapi.LabelProvider, bool) {
	if agentmeta.Normalize(name) == "codex" {
		return codex.NativeLabelProvider{Host: n.host}, true
	}
	return n.registry.LookupLabels(name)
}

type fixedCursorCapture struct{}

func (*fixedCursorCapture) capture(request *nativefacts.CaptureRequest) (*nativecapture.Proof, error) {
	return cursor.CaptureSettledRequest(request)
}

// CaptureSettledCursor projects only the private real provider installed by NewBuiltins.
// Arbitrary Registry.New Integration bindings cannot acquire capture authority.
func (r *Registry) CaptureSettledCursor(request *nativefacts.CaptureRequest) (*nativecapture.Proof, error) {
	if r == nil || r.fixedCapture == nil {
		return nil, errors.New("settled capture requires builtin provider")
	}
	return r.fixedCapture.capture(request)
}

func (*fixedCursorCapture) captureLive(request *nativefacts.LiveCaptureRequest) (*nativecapture.LiveProof, error) {
	return cursor.CaptureLiveRequest(request)
}

// CaptureLiveCursor projects only the real provider installed by NewBuiltins.
// Arbitrary integration bindings cannot supply scratch or captured native bytes.
func (r *Registry) CaptureLiveCursor(request *nativefacts.LiveCaptureRequest) (*nativecapture.LiveProof, error) {
	if r == nil || r.fixedCapture == nil {
		return nil, errors.New("live capture requires builtin provider")
	}
	return r.fixedCapture.captureLive(request)
}
