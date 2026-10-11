// Package config is the single local, durable record of how this machine's
// agent-archive is set up: which storage destination it publishes to, which
// projects are included, and whether collection is currently paused. It is
// the one file `agent-archive setup` writes and every other command reads.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// SchemaVersion is bumped only when Config's on-disk shape changes
// incompatibly.
const SchemaVersion = 1

// SkillEvidence controls how much filesystem skill data a source bundle carries.
type SkillEvidence string

const (
	// SkillEvidenceNone omits filesystem skill inventory and snapshots.
	SkillEvidenceNone SkillEvidence = "none"
	// SkillEvidenceMetadata includes names and hashes, without skill bodies.
	SkillEvidenceMetadata SkillEvidence = "metadata"
	// SkillEvidenceBody includes filtered snapshots as well as metadata.
	SkillEvidenceBody SkillEvidence = "body"
)

// EffectiveSkillEvidence preserves the behavior of configs saved before this
// setting existed. Fresh setup persists metadata explicitly.
func (c Config) EffectiveSkillEvidence() SkillEvidence {
	c.SkillEvidence = underlyingSkillEvidence(c.SkillEvidence)
	if c.SkillEvidence == "" {
		return SkillEvidenceBody
	}
	return c.SkillEvidence
}

// ValidSkillEvidence reports whether mode is one of the supported policies.
func ValidSkillEvidence(mode SkillEvidence) bool {
	switch mode {
	case SkillEvidenceNone, SkillEvidenceMetadata, SkillEvidenceBody:
		return true
	}
	return false
}

// CodexNameLookup selects the explicitly configured native metadata transport.
type CodexNameLookup string

const (
	// CodexNameLookupFiles reads settled local metadata without startup.
	CodexNameLookupFiles CodexNameLookup = "files"
	// CodexNameLookupNative explicitly permits bounded native startup.
	CodexNameLookupNative CodexNameLookup = "native"
)

// Config is this machine's complete archive configuration. It contains no
// secrets: R2 secrets live in the credential store (the Keychain on macOS, a
// private file elsewhere; see destination.Config.R2CredentialRef)
// and S3 credentials are resolved through the named AWS profile.
type Config struct {
	// CodexNameLookup selects files (default) or explicitly opted-in native startup.
	CodexNameLookup CodexNameLookup     `json:"codex_name_lookup,omitempty"`
	CodexCapture    *CodexCaptureConfig `json:"codex_capture,omitempty"`
	// Discovery carries forward-only authorization; absent means disabled.
	Discovery *DiscoveryConfig `json:"discovery,omitempty"`
	// SpareKeys is the desired unused key count; nil means two.
	SpareKeys *int `json:"spare_keys,omitempty"`
	// SpareCredentialRefs is an advisory index. The issued ledger owns eligibility.
	SpareCredentialRefs []string `json:"spare_credential_refs,omitempty"`
	// CloudflareTokenCommand returns a management token for explicit interactive operations only.
	CloudflareTokenCommand []string `json:"cloudflare_token_command,omitempty"`
	// MachineName is a chosen label, never a detected hostname.
	MachineName string `json:"machine_name,omitempty"`
	// MachineAssignment is locally committed credential provenance for one destination.
	MachineAssignment *MachineAssignment `json:"machine_assignment,omitempty"`
	// RetiredMachineAssignments retains committed dedicated bindings after replacement.
	RetiredMachineAssignments []MachineAssignment        `json:"retired_machine_assignments,omitempty"`
	BucketPrivacy             *destination.PrivacyReport `json:"bucket_privacy,omitempty"`
	RetiredCredentialRefs     []string                   `json:"retired_credential_refs,omitempty"`
	StorageVerifiedAt         time.Time                  `json:"storage_verified_at,omitempty"`
	DestinationSince          time.Time                  `json:"destination_since,omitempty"`
	PreviousDestinations      []destination.Config       `json:"previous_destinations,omitempty"`
	// MCPServerNames supplies display labels for server IDs in stats.
	MCPServerNames map[string]string `json:"mcp_server_names,omitempty"`

	// DurableStorageProtection permanently protects rooted publication obligations.
	DurableStorageProtection bool `json:"durable_storage_protection,omitempty"`
	// CodexHistoryProtection permanently fences writers without revision lifecycle support.
	CodexHistoryProtection bool `json:"codex_history_protection,omitempty"`
	// GenerationProtection permanently fences writers that cannot freeze archive generations.
	GenerationProtection bool               `json:"generation_protection,omitempty"`
	SchemaVersion        int                `json:"schema_version"`
	MachineID            string             `json:"machine_id"`
	Storage              destination.Config `json:"storage"`
	Archive              archive.Config     `json:"archive"`
	// Paused persistently suspends collection, uploads, and remote cleanup
	// without deleting data or existing configuration.
	Paused bool `json:"paused"`
	// PauseGeneration changes with each pause/resume transition. Deferred
	// hook admissions must belong to the same uninterrupted capture window.
	// Empty is the legacy window, valid until the first transition.
	PauseGeneration string `json:"pause_generation,omitempty"`
	// Harnesses lists which applications setup installed hooks for
	// (values match archive.Harness.Name: "codex", "claude", "cursor").
	Harnesses []string `json:"harnesses,omitempty"`
	// ImportedHarnesses lists apps whose sessions were imported by
	// `agent-archive backfill` without their hooks installed. It admits those
	// apps' imported sessions only; hook registrations still need Harnesses.
	// Backfill adds to it and setup carries it over from the committed
	// configuration, dropping an app once its hooks are installed.
	ImportedHarnesses []string `json:"imported_harnesses,omitempty"`
	// DeclinedHarnesses lists apps the user chose to leave out when
	// reconfiguring: declined when setup offered them as found on this
	// computer, or removed from the selection. Setup does not offer them
	// again; an app leaves this list once it is included.
	DeclinedHarnesses []string `json:"declined_harnesses,omitempty"`
	// InstalledExecutable is the executable path setup wrote into the hooks
	// and the LaunchAgent. Status checks the installed hooks against this
	// path rather than whichever path status itself was run through (a
	// symlink, a Homebrew shim, a copied binary); a configuration written
	// before this field existed falls back to the running executable.
	InstalledExecutable string `json:"installed_executable,omitempty"`
	// HookFiles records, per app in Harnesses, the hook configuration file
	// setup installed into, as resolved from the environment setup ran in
	// (CLAUDE_CONFIG_DIR, CODEX_HOME). Status and uninstall read it so they
	// find the files from a shell without those variables. A configuration
	// written before this field existed falls back to the current
	// environment's paths.
	HookFiles map[string]string `json:"hook_files,omitempty"`
	// BackgroundBackend names the scheduler that runs the background collector
	// ("systemd"), as setup recorded it. Status, uninstall, refresh and
	// recovery address the job through this backend and never pick another.
	// Setup leaves out "launchd", and an absent field means launchd on macOS
	// and systemd on Linux, for all time, so a macOS configuration never
	// changes and a binary that rewrites this file without the field cannot
	// change what it means.
	BackgroundBackend string `json:"background_backend,omitempty"`
	// HostID is a digest of the Linux machine ID (local.HostFingerprint) of
	// the machine that set this data directory up, recorded next to MachineID
	// the first time setup runs there. Status and setup compare it with the
	// machine they run on: a different one means the data directory was
	// copied, typically with a cloned VM or container image, and the two
	// machines now claim the same sessions. Empty on macOS (never recorded),
	// and on a Linux system with no machine ID to read. It is local: it is
	// not in any published file.
	HostID string `json:"host_id,omitempty"`
	// AllowNetworkHome records that the person allowed this installation's
	// data directory or systemd unit directory to be on a network
	// filesystem (setup --allow-network-home), which setup and setup
	// --refresh otherwise refuse on Linux, since a home shared between
	// machines shares one machine ID, cannot rely on file locks and runs the
	// background job on every machine. Setup records it only while a
	// directory is on one. Status warns of the network filesystem whether or
	// not it is set. Absent otherwise, and always on macOS.
	AllowNetworkHome bool `json:"allow_network_home,omitempty"`
	// RequireSkillUse opts out of the spec's default (capture sessions with
	// no detected skill use too, to preserve comparison evidence). The zero
	// value (false) matches that default, so a config that predates this
	// field, or one built without setting it, behaves correctly rather than
	// silently declining everything.
	RequireSkillUse bool `json:"require_skill_use"`
	// SkillEvidence controls filesystem skill inventory and snapshot uploads.
	// An absent field is a legacy body policy, not a new-install default.
	SkillEvidence SkillEvidence `json:"skill_evidence,omitempty"`
	// NoSkills records that the person opted out of the agent skills setup
	// installs into their coding agents (setup --no-skills). While it is set,
	// setup installs none and removes the ones it wrote earlier; setup
	// --skills clears it. The zero value installs them, so a configuration
	// from before this field behaves as it did.
	NoSkills bool `json:"no_skills,omitempty"`
	// RetentionDays is whole-session retention, proposed as 90 by setup.
	// Enforcing it is the Retention slice's job, not this package's.
	RetentionDays int `json:"retention_days"`
	// RetentionProtection is sticky: older setup must never reset forever to 90 days.
	ImportStagingProtection bool `json:"import_staging_protection,omitempty"`
	RetentionProtection     bool `json:"retention_protection,omitempty"`
	// Handoff holds preferences for `agent-archive handoff`. Setup never
	// asks for them; they are edited by hand and carried through
	// reconfiguration like the rest of this file.
	Handoff HandoffConfig `json:"handoff,omitzero"`
}

// HandoffConfig is what `agent-archive handoff` launches with by default.
// Agents are named as harnesses are: claude, codex, cursor.
type HandoffConfig struct {
	// Args are arguments given to an agent before any after `--` on the
	// command line, keyed by the destination agent.
	Args map[string][]string `json:"args,omitempty"`
	// DefaultTo is the destination offered first, keyed by the harness of
	// the session being handed off.
	DefaultTo map[string]string `json:"default_to,omitempty"`
}

// validate rejects names handoff would not recognize, so a typo in a
// hand-edited file is reported rather than silently ignored.
func (h HandoffConfig) validateWithCatalog(c agentmeta.Catalog) error {
	for agent, args := range h.Args {
		if !knownAgent(c, agent) {
			return fmt.Errorf("handoff.args: unknown agent %q; use claude, codex, or cursor", agent)
		}
		for _, arg := range args {
			// exec cannot pass a NUL byte, and an empty word is almost
			// always a quoting mistake.
			if arg == "" || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("handoff.args.%s: every argument must be non-empty text without a NUL byte", agent)
			}
		}
	}
	for source, dest := range h.DefaultTo {
		if !knownAgent(c, source) {
			return fmt.Errorf("handoff.default_to: unknown harness %q; use claude, codex, or cursor", source)
		}
		if !knownAgent(c, dest) {
			return fmt.Errorf("handoff.default_to.%s: unknown agent %q; use claude, codex, or cursor", source, dest)
		}
	}
	return nil
}

func path(home string) string { return filepath.Join(home, "config.json") }

// Load reads this machine's configuration. found is false, with a nil error,
// when setup has never run. An error names the file, and for one that no
// longer decodes, the way out: every command needs it, so nothing else can
// say which file stopped it.
func Load(home string) (Config, bool, error) { return LoadWithCatalog(home, agentmeta.Builtins()) }

// LoadWithCatalog reads configuration using the caller's supported identities.
func LoadWithCatalog(home string, c agentmeta.Catalog) (cfg Config, found bool, err error) {
	cfg, found, _, err = loadConfigWithCatalog(home, c)
	return cfg, found, err
}

func loadConfig(home string) (Config, bool, bool, error) {
	return loadConfigWithCatalog(home, agentmeta.Builtins())
}

func loadConfigWithCatalog(home string, c agentmeta.Catalog) (cfg Config, found, fenced bool, err error) {
	defer trace.Start("load config").End()
	data, readErr := os.ReadFile(path(home))
	return decodeLoadedConfig(data, readErr, path(home), c)
}

func decodeLoadedConfig(data []byte, readErr error, name string, c agentmeta.Catalog) (cfg Config, found, fenced bool, err error) {
	err = readErr
	if err == nil {
		fenced, err = decodeConfig(data, &cfg)
	}
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, false, nil
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return Config{}, false, false, fmt.Errorf("%w: %s (%w). Restore it from a backup, or fix the JSON by hand; moving it aside (keep the copy: it records this machine's ID) and running agent-archive setup configures this machine again", ErrUnreadable, name, err)
	}
	if err != nil {
		return Config{}, false, false, fmt.Errorf("read %s: %w", name, err)
	}
	if err := validateDiscoveryConfig(cfg); err != nil {
		return Config{}, false, false, err
	}
	if !ValidSkillEvidence(cfg.EffectiveSkillEvidence()) {
		return Config{}, false, false, fmt.Errorf("read %s: unsupported skill_evidence %q; choose none, metadata, or body", name, cfg.SkillEvidence)
	}
	if err := cfg.ValidateCodexNameLookup(); err != nil {
		return Config{}, false, false, err
	}
	if err := cfg.ValidateCloudflareTokenCommand(); err != nil {
		return Config{}, false, false, err
	}
	if err := cfg.ValidateSpares(); err != nil {
		return Config{}, false, false, err
	}
	if err := cfg.ValidateMachine(); err != nil {
		return Config{}, false, false, err
	}
	if err := normalizeHandoff(&cfg.Handoff, c); err != nil {
		return Config{}, false, false, fmt.Errorf("read %s: %w", name, err)
	}
	return cfg, true, fenced, nil
}

// ErrUnreadable is a configuration file that exists but does not decode.
var ErrUnreadable = errors.New("the settings file cannot be read")

// Save durably writes cfg, replacing any prior configuration atomically.
func Save(home string, cfg Config) error { return SaveWithCatalog(home, cfg, agentmeta.Builtins()) }

// SaveWithCatalog validates and writes configuration with injected identities.
func SaveWithCatalog(home string, cfg Config, c agentmeta.Catalog) error {
	previous, found, _, err := loadConfigWithCatalog(home, c)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if found {
		PreserveWriterFence(&cfg, previous)
	}
	if cfg.RetentionDays == 0 {
		cfg.RetentionProtection = true
	}
	if err := prepareDiscoveryConfig(&cfg); err != nil {
		return err
	}
	if err := cfg.ValidateCodexNameLookup(); err != nil {
		return err
	}
	if err := cfg.ValidateCloudflareTokenCommand(); err != nil {
		return err
	}
	if err := cfg.ValidateSpares(); err != nil {
		return err
	}
	if err := cfg.ValidateMachine(); err != nil {
		return err
	}
	if !ValidSkillEvidence(cfg.EffectiveSkillEvidence()) {
		return fmt.Errorf("unsupported skill_evidence %q; choose none, metadata, or body", cfg.SkillEvidence)
	}
	if err := normalizeHandoff(&cfg.Handoff, c); err != nil {
		return err
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = SchemaVersion
	}
	return local.Write(path(home), cfg)
}

// SetPaused updates the Paused flag and rotates PauseGeneration at a state
// transition, preserving the rest of an existing configuration. It fails if
// setup has not run yet: pausing before there is
// anything to pause is not a meaningful state. An optional observation time
// lets command callers commit pause and discovery intervals on their clock.
func SetPaused(home string, paused bool, at ...time.Time) (Config, error) {
	if len(at) > 1 {
		return Config{}, errors.New("pause requires at most one observation time")
	}
	now := time.Now().UTC()
	if len(at) == 1 {
		now = at[0].UTC()
	}
	cfg, found, err := Load(home)
	if err != nil {
		return Config{}, err
	}
	if !found {
		return Config{}, errors.New("not set up yet; run `agent-archive setup` first")
	}
	if cfg.Paused != paused {
		cfg.PauseGeneration, err = local.ID()
		if err != nil {
			return Config{}, fmt.Errorf("generate pause boundary: %w", err)
		}
	}
	if err := transitionDiscoveryPause(&cfg, paused, now.UTC()); err != nil {
		return Config{}, err
	}
	cfg.Paused = paused
	if err := Save(home, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DestinationID identifies a storage destination by its provider, endpoint,
// bucket, and prefix. It never covers credentials or their references. It
// lives here rather than in package archive, which imports no other internal
// package: taking a destination.Config would pull the AWS SDK and cgo into
// archive.
//
// Registrations and batch files store this value, and it decides which bucket
// owns a session. It must never change, not the provider or endpoint
// normalisation, the join, nor the prefix trimming, without a migration of
// every stored ID: otherwise every registration silently belongs to no
// destination. TestDestinationIDIsPinned holds it fixed.
func DestinationID(c destination.Config) string {
	// The provider is compared as storage compares it, case- and
	// space-insensitively; setup always writes it lowercase.
	provider := strings.ToLower(strings.TrimSpace(c.Provider))
	endpoint := ""
	if provider == destination.ProviderR2 {
		endpoint, _ = destination.R2Endpoint(c.R2Endpoint, c.R2AccountID)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{provider, endpoint, c.Bucket, strings.Trim(c.Prefix, "/")}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// DestinationID is the ID of the storage destination configured now.
func (c Config) DestinationID() string { return DestinationID(c.Storage) }

// AcceptSession prevents excluded apps/projects and previous destinations from
// continuing to publish or delete sessions after reconfiguration. The
// destination check is InCurrentDestination. Project activation compares the
// registration's admission, never its start: an imported session began long
// before the project was activated, and is admitted by the import itself.
func (c Config) AcceptSession(r archive.SessionRegistration) bool {
	if !c.InCurrentDestination(r) {
		return false
	}
	if !c.acceptsHarness(r) {
		return false
	}
	admitted := r.Admitted()
	if r.CodexAdmission != nil && r.Harness.Name == "codex" && !r.Imported() && r.CodexAdmission.Generation != "" && r.CodexAdmission.Revision != "" && filepath.IsAbs(r.CodexAdmission.Cwd) && slices.Contains(c.Harnesses, "codex") && c.Archive.Enabled && r.ProjectID == archive.ProjectID(r.ProjectRoot) {
		if c.EffectiveCodexCaptureScope() == CodexIncludedProjects {
			rule, found := c.codexWinningRule(r.ProjectRoot, r.CodexAdmission.Cwd)
			if !found || !rule.Included {
				return false
			}
		}
		return c.CodexContinuationAllowed(r.ProjectRoot, r.CodexAdmission.Cwd)
	}
	if r.Harness.Name == "codex" && c.EffectiveCodexCaptureScope() == CodexAllProjects {
		cwd := r.DiscoveryCwd
		if cwd == "" {
			cwd = r.ProjectRoot
		}
		if !c.CodexContinuationAllowed(r.ProjectRoot, cwd) {
			return false
		}
	}
	for _, p := range c.Archive.Projects {
		if p.Included && p.Root == r.ProjectRoot {
			return p.ActivatedAt.IsZero() || !admitted.Before(p.ActivatedAt)
		}
	}
	return c.CodexCapture == nil && len(c.Archive.Projects) == 0 // older programmatic configurations
}

// InCurrentDestination reports whether a registration published to the
// storage destination configured now, rather than to one it replaced. A
// registration that records its destination's ID is compared by that ID, so
// one admitted into a bucket belongs to it again if the configuration
// switches back. A registration without one (written before the ID existed)
// falls back to time: it belongs here if it was admitted at or after
// DestinationSince. Like AcceptSession it compares the admission, not the
// start: an import published here even though it started before this
// destination was configured.
func (c Config) InCurrentDestination(r archive.SessionRegistration) bool {
	if r.DestinationID != "" {
		return r.DestinationID == c.DestinationID()
	}
	return c.DestinationSince.IsZero() || !r.Admitted().Before(c.DestinationSince)
}

// acceptsHarness reports whether the registration's app may publish. Hook
// registrations need the app's hooks (Harnesses); an import may also come
// from an app that was imported without hooks (ImportedHarnesses). An empty
// Harnesses list is an older programmatic configuration and admits every app.
func (c Config) acceptsHarness(r archive.SessionRegistration) bool {
	if len(c.Harnesses) == 0 || slices.Contains(c.Harnesses, r.Harness.Name) {
		return true
	}
	return r.Imported() && slices.Contains(c.ImportedHarnesses, r.Harness.Name)
}

func knownAgent(c agentmeta.Catalog, name string) bool { _, ok := c.Lookup(name); return ok }

func normalizeHandoff(h *HandoffConfig, c agentmeta.Catalog) error {
	if err := h.validateWithCatalog(c); err != nil {
		return err
	}
	args := make(map[string][]string, len(h.Args))
	for name, words := range h.Args {
		id := agentmeta.Canonical(c, name)
		if _, ok := args[id]; ok {
			return fmt.Errorf("handoff.args: duplicate agent %q", id)
		}
		args[id] = slices.Clone(words)
	}
	defaults := make(map[string]string, len(h.DefaultTo))
	for name, dest := range h.DefaultTo {
		id := agentmeta.Canonical(c, name)
		if _, ok := defaults[id]; ok {
			return fmt.Errorf("handoff.default_to: duplicate harness %q", id)
		}
		defaults[id] = agentmeta.Canonical(c, dest)
	}
	if h.Args != nil {
		h.Args = args
	}
	if h.DefaultTo != nil {
		h.DefaultTo = defaults
	}
	return nil
}

// ValidateCodexNameLookup refuses unknown lookup modes.
func (c Config) ValidateCodexNameLookup() error {
	switch c.CodexNameLookup {
	case "", CodexNameLookupFiles, CodexNameLookupNative:
		return nil
	}
	return fmt.Errorf("unsupported codex_name_lookup %q; choose files or native", c.CodexNameLookup)
}
