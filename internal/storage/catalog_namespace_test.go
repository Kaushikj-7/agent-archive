package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/destination"
)

func TestConfiguredCatalogNamespaceBindsActualDestination(t *testing.T) {
	cfg := credentials.Config{Provider: "s3", Bucket: "archive", Prefix: "scope", Region: "us-east-1", AWSProfile: "namespace-fixture", ArchiveFormat: destination.FormatLegacy}
	client := configuredNamespaceFixtureClient(t, cfg, "https://synthetic.invalid")
	same := configuredNamespaceFixtureClient(t, cfg, "https://synthetic.invalid")
	first := configuredCatalogNamespace(cfg, client)
	if first == "" || first != configuredCatalogNamespace(cfg, same) {
		t.Fatal("same immutable destination lacks stable namespace")
	}
	for _, change := range []func(*credentials.Config){
		func(c *credentials.Config) { c.Bucket = "other" },
		func(c *credentials.Config) { c.Prefix = "other" },
	} {
		changed := cfg
		change(&changed)
		if first == configuredCatalogNamespace(changed, client) {
			t.Fatal("configured destination changed without rebinding summaries")
		}
	}
	other := configuredNamespaceFixtureClient(t, cfg, "https://other.invalid")
	if first == configuredCatalogNamespace(cfg, other) {
		t.Fatal("actual SDK endpoint not bound")
	}
	unconfigured, err := NewS3Store(S3StoreOptions{Client: client, Bucket: cfg.Bucket, Prefix: cfg.Prefix})
	if err != nil {
		t.Fatal(err)
	}
	if unconfigured.CatalogNamespace() != "" {
		t.Fatal("arbitrary client asserted configured identity")
	}
	cfg.AWSProfile = "private-profile-reference"
	if strings.Contains(configuredCatalogNamespace(cfg, client), cfg.AWSProfile) {
		t.Fatal("credentials reference retained")
	}
}

// Use the actual restricted loader path, not a manufactured admission marker.
// All credentials and endpoints are private synthetic data; no request is sent.
func configuredNamespaceFixtureClient(t *testing.T, cfg credentials.Config, endpoint string) *s3.Client {
	t.Helper()
	dir := t.TempDir()
	profile := "[profile " + cfg.AWSProfile + "]\nregion = " + cfg.Region + "\nendpoint_url = " + endpoint + "\n"
	credentialsBody := "[" + cfg.AWSProfile + "]\naws_access_key_id = SYNTHETIC\naws_secret_access_key = SYNTHETIC\n"
	configPath := filepath.Join(dir, "config")
	credentialsPath := filepath.Join(dir, "credentials")
	if err := os.WriteFile(configPath, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsPath, []byte(credentialsBody), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsPath)
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("AWS_ENDPOINT_URL_S3", "")
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "false")
	store, err := NewConfiguredStore(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if actual := aws.ToString(store.client.Options().BaseEndpoint); actual != endpoint {
		t.Fatalf("synthetic profile endpoint not loaded: %q", actual)
	}
	return store.client
}

func TestConfiguredCatalogNamespaceRejectsUnprovenClients(t *testing.T) {
	cfg := credentials.Config{Provider: "s3", Bucket: "archive", Region: "us-east-1", AWSProfile: "namespace-fixture", ArchiveFormat: destination.FormatLegacy}
	public := NewClient(aws.Config{Region: cfg.Region}, "https://synthetic.invalid", true, 1)
	if configuredCatalogNamespace(cfg, public) != "" {
		t.Fatal("public client asserted restricted configured-loader provenance")
	}
	configured := configuredNamespaceFixtureClient(t, cfg, "https://synthetic.invalid")
	if configuredCatalogNamespace(cfg, configured) == "" {
		t.Fatal("configured loader failed positive admission control")
	}
	regionalConfig := cfg
	regionalConfig.Region = "eu-west-1"
	regional := configuredNamespaceFixtureClient(t, regionalConfig, "https://synthetic.invalid")
	if configuredCatalogNamespace(cfg, regional) == configuredCatalogNamespace(cfg, configured) {
		t.Fatal("actual SDK region not bound")
	}
	// Replacing a configured client's resolver removes its private admission.
	custom := s3.New(configured.Options(), func(options *s3.Options) {
		options.EndpointResolverV2 = s3.NewDefaultEndpointResolverV2()
	})
	if configuredCatalogNamespace(cfg, custom) != "" {
		t.Fatal("arbitrary resolver asserted configured-loader provenance")
	}
}
