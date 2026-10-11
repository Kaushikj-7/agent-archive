// Package pairing implements the bounded, authenticated encrypted pairing wire
// protocol. It performs no filesystem, credential-store, or network operations.
package pairing

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/config"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Storage is the portable destination whitelist, without local references.
type Storage struct {
	Provider   string `json:"provider"`
	Bucket     string `json:"bucket"`
	Prefix     string `json:"prefix"`
	R2Account  string `json:"r2_account,omitempty"`
	R2Endpoint string `json:"r2_endpoint,omitempty"`
	AWSProfile string `json:"aws_profile,omitempty"`
	Region     string `json:"region,omitempty"`
}

// Inclusion identifies one portable capture scope. HomePath is optional and
// relative to the receiving home, never an absolute source path.
type Inclusion struct {
	ID       string `json:"id"`
	RepoKey  string `json:"repo_key,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	Label    string `json:"label"`
	HomePath string `json:"home_path,omitempty"`
}

// Exclusion maps a restriction beneath an inclusion or the receiving home.
// Unresolved entries carry affected IDs only and withhold those scopes.
type Exclusion struct {
	InclusionID  string   `json:"inclusion_id,omitempty"`
	Path         string   `json:"path,omitempty"`
	HomeRelative bool     `json:"home_relative,omitempty"`
	Unresolved   bool     `json:"unresolved,omitempty"`
	Affected     []string `json:"affected"`
}

// Payload contains only explicitly transferable settings and provenance.
// R2 secrets remain in memory until a receiver stages them in its store.
type Payload struct {
	Kind            config.MachineAssignmentKind `json:"kind,omitempty"`
	SlotID          string                       `json:"slot_id,omitempty"`
	Version         int                          `json:"version"`
	PairingID       string                       `json:"pairing_id"`
	RecipientID     string                       `json:"recipient_id"`
	IssuerID        string                       `json:"issuer_id"`
	IssuerName      string                       `json:"issuer_name"`
	Name            string                       `json:"name"`
	CreatedAt       time.Time                    `json:"created_at"`
	ExpiresAt       time.Time                    `json:"expires_at"`
	Storage         Storage                      `json:"storage"`
	AccessKeyID     string                       `json:"access_key_id,omitempty"`
	SecretAccessKey string                       `json:"secret_access_key,omitempty"`
	Apps            []string                     `json:"apps"`
	RetentionDays   int                          `json:"retention_days"`
	RequireSkillUse bool                         `json:"require_skill_use"`
	SkillEvidence   string                       `json:"skill_evidence"`
	NoSkills        bool                         `json:"no_skills"`
	HandoffArgs     map[string][]string          `json:"handoff_args,omitempty"`
	HandoffDefault  map[string]string            `json:"handoff_default,omitempty"`
	Inclusions      []Inclusion                  `json:"inclusions"`
	Exclusions      []Exclusion                  `json:"exclusions,omitempty"`
}

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

var repoPattern = regexp.MustCompile(`^repo-[a-f0-9]{16}$`)

// ValidID reports whether an identifier is canonical 128-bit lowercase hex.
func ValidID(value string) bool { return idPattern.MatchString(value) }

// ValidName reports whether a machine label uses the supported bounded alphabet.
func ValidName(value string) bool { return namePattern.MatchString(value) }

// RelativePath validates a portable path without traversal or control text.
func RelativePath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../") && safeText(value) && !strings.Contains(value, ":")
}

func safeText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return len(value) <= 4096
}

// Validate checks every imported setting before a setup transaction can use it.
func (p Payload) Validate() error {
	bad := errors.New("pairing settings are invalid or unsupported; create a new bundle with a current source")
	if p.Version != 1 || !ValidID(p.PairingID) || !ValidID(p.RecipientID) || !ValidID(p.IssuerID) || !ValidName(p.Name) || !ValidName(p.IssuerName) {
		return bad
	}
	if p.CreatedAt.IsZero() || p.ExpiresAt.Before(p.CreatedAt.Add(5*time.Minute)) || p.ExpiresAt.After(p.CreatedAt.Add(24*time.Hour)) {
		return bad
	}
	if err := p.validateStorage(); err != nil {
		return err
	}
	if err := p.validateApplications(); err != nil {
		return err
	}
	return p.validateScopes()
}

func invalidSettings() error {
	return errors.New("pairing settings are invalid or unsupported; create a new bundle with a current source")
}

func (p Payload) validateStorage() error {
	bad := invalidSettings()
	s := p.Storage
	if err := p.validateProvenance(); err != nil {
		return err
	}
	if !slices.Contains([]string{"r2", "s3"}, s.Provider) || s.Bucket == "" || !safeText(s.Bucket) || !safeText(s.Prefix) || strings.HasPrefix(s.Prefix, "/") || strings.Contains(s.Prefix, "\\") || strings.Contains(s.Prefix, "..") {
		return bad
	}
	if s.Provider == "r2" {
		if p.AccessKeyID == "" || p.SecretAccessKey == "" || len(p.AccessKeyID) > 128 || len(p.SecretAccessKey) > 4096 || !safeText(p.AccessKeyID) || !safeText(p.SecretAccessKey) || s.AWSProfile != "" {
			return bad
		}
	} else if s.AWSProfile == "" || !safeText(s.AWSProfile) || p.AccessKeyID != "" || p.SecretAccessKey != "" || s.R2Account != "" || s.R2Endpoint != "" {
		return bad
	}
	return p.validateCaptureAndLocation()
}

func (p Payload) validateApplications() error {
	bad := invalidSettings()
	agents := []string{"claude", "codex", "cursor"}
	seenApps := map[string]bool{}
	for _, app := range p.Apps {
		if !slices.Contains(agents, app) || seenApps[app] {
			return bad
		}
		seenApps[app] = true
	}
	for app, args := range p.HandoffArgs {
		if !slices.Contains(agents, app) || len(args) > 128 {
			return bad
		}
		for _, arg := range args {
			if arg == "" || !safeText(arg) {
				return bad
			}
		}
	}
	for app, dest := range p.HandoffDefault {
		if !slices.Contains(agents, app) || !slices.Contains(agents, dest) {
			return bad
		}
	}
	return nil
}

func (p Payload) validateScopes() error {
	bad := invalidSettings()
	if len(p.Inclusions) > 128 || len(p.Exclusions) > 128 {
		return bad
	}
	scopes := map[string]bool{}
	for _, inc := range p.Inclusions {
		if !ValidID(inc.ID) || scopes[inc.ID] || inc.Label == "" || !safeText(inc.Label) || (inc.RepoKey != "" && !repoPattern.MatchString(inc.RepoKey)) || ((inc.RepoPath != "" && (!RelativePath(inc.RepoPath) || inc.RepoKey == "")) || (inc.HomePath != "" && !RelativePath(inc.HomePath))) {
			return bad
		}
		scopes[inc.ID] = true
	}
	for _, exc := range p.Exclusions {
		if !validExclusion(exc, scopes) {
			return bad
		}
	}
	return nil
}

func (p Payload) validateProvenance() error {
	bad := invalidSettings()
	if p.Storage.Provider == "r2" {
		if p.Kind != "" && p.Kind != config.MachineAssignmentR2Shared && p.Kind != config.MachineAssignmentR2Own {
			return bad
		}
		if p.Kind == config.MachineAssignmentR2Own {
			if !ValidID(p.SlotID) || !ValidID(p.AccessKeyID) {
				return bad
			}
		} else if p.SlotID != "" {
			return bad
		}
	} else if p.SlotID != "" || (p.Kind != "" && p.Kind != config.MachineAssignmentAWSProfile) {
		return bad
	}
	return nil
}

func (p Payload) validateCaptureAndLocation() error {
	bad := invalidSettings()
	s := p.Storage
	if !safeText(s.Region) || !safeText(s.R2Account) || !safeText(s.R2Endpoint) || p.RetentionDays < 0 || p.RetentionDays > 36500 || !slices.Contains([]string{"none", "metadata", "body"}, p.SkillEvidence) || len(p.Apps) == 0 || len(p.Apps) > 3 {
		return bad
	}
	return nil
}

func validExclusion(exc Exclusion, scopes map[string]bool) bool {
	if len(exc.Affected) > 128 || len(exc.Affected) == 0 && !exc.HomeRelative && !exc.Unresolved {
		return false
	}
	for _, id := range exc.Affected {
		if !scopes[id] {
			return false
		}
	}
	if exc.Unresolved {
		if exc.Path != "" || exc.InclusionID != "" || exc.HomeRelative {
			return false
		}
		return true
	}
	if !RelativePath(exc.Path) || (exc.HomeRelative && exc.InclusionID != "") || (!exc.HomeRelative && !scopes[exc.InclusionID]) {
		return false
	}
	return true
}
