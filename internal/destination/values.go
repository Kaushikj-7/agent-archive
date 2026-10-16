// Package destination holds pure storage destination and privacy observation values.
package destination

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// Config describes one archive storage destination. For S3, AWSProfile is
// mandatory and is loaded deterministically. For R2, R2CredentialRef points
// to an item in the credential store (see OpenDefault) and Endpoint may be
// omitted when AccountID is supplied.
// The JSON tags spell the Go field names, the format already saved in users'
// config files: renaming one would make existing configs unreadable.
type Config struct {
	ArchiveFormat   ArchiveFormat `json:"ArchiveFormat,omitempty"`
	Provider        string        `json:"Provider"`
	Bucket          string        `json:"Bucket"`
	Region          string        `json:"Region"`
	Prefix          string        `json:"Prefix"`
	AWSProfile      string        `json:"AWSProfile"`
	R2CredentialRef string        `json:"R2CredentialRef"`
	R2AccountID     string        `json:"R2AccountID"`
	R2Endpoint      string        `json:"R2Endpoint"`
}

// Config.Provider values.
const (
	// ProviderS3 is Amazon S3, authenticated through a shared AWS profile.
	ProviderS3 = "s3"
	// ProviderR2 is Cloudflare R2, authenticated through a credential store item.
	ProviderR2 = "r2"
)

// PrivacyReport describes native bucket public access controls, not access
// through authorized applications, signed URLs, or downstream copies.
// It contains only fixed diagnostic codes, never provider error strings.
// CheckedAt is nil until an inspection has run, so never-inspected evidence
// omits the field instead of serializing the zero time.
type PrivacyReport struct {
	//lint:ignore LV1001 callers in internal/cli copy State into plain string fields and compare it to literals; a defined type would break them
	State           string     `json:"state"`
	Reason          string     `json:"reason"`
	Scope           string     `json:"scope"`
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
	ConfigurationID string     `json:"configuration_id,omitempty"`
	GuidanceURL     string     `json:"guidance_url"`
	Checks          []string   `json:"checks,omitempty"`
}

// R2Endpoint normalizes the configured HTTPS endpoint without resolving credentials.
func R2Endpoint(endpoint, accountID string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" || strings.ContainsAny(accountID, "/\\ \t\r\n") {
			return "", errors.New("R2 endpoint or account ID is required")
		}
		endpoint = "https://" + accountID + ".r2.cloudflarestorage.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid R2 endpoint")
	}
	return strings.TrimRight(endpoint, "/"), nil
}

// ArchiveFormat selects the destination protocol; absent remains legacy.
type ArchiveFormat string

// Supported archive destination protocols.
const (
	FormatLegacy    ArchiveFormat = "legacy"
	FormatCatalogV4 ArchiveFormat = "catalog-v4"
)

// EffectiveArchiveFormat applies the legacy default to an omitted format.
func (c Config) EffectiveArchiveFormat() ArchiveFormat {
	if c.ArchiveFormat == "" {
		return FormatLegacy
	}
	return c.ArchiveFormat
}
