package cli

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A setup can finish while migration qualifies the archive before taking its
// collector/search fences. The later migration must not overwrite that consent.
func TestMigrationRefusesSearchConsentChangedDuringPreflight(t *testing.T) {
	t.Parallel()
	for _, scoped := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(strconv.FormatBool(scoped)+"/"+strconv.FormatBool(enabled), func(t *testing.T) {
				t.Parallel()
				format := destination.FormatLegacy
				if scoped {
					format = destination.FormatCatalogV4
				}
				cfg := config.Config{RetentionDays: 90, Storage: destination.Config{Provider: "s3", Bucket: "private-source", Prefix: "old", Region: "us-east-1", ArchiveFormat: format}}
				must(t, config.SetSearchIndexing(&cfg, enabled))
				env := pickerOperatorEnv(t, cfg)
				home, err := env.readHome()
				must(t, err)
				source := storagetest.NewMemoryStore()
				target := &migrationCLIStore{&qualifiedCLIStore{storagetest.NewMemoryStore()}}
				picker := &scopedMigrationCLIStore{&qualifiedCLIStore{storagetest.NewMemoryStore()}}
				changed := false
				env.OpenStore = func(c config.Config) (storage.ObjectStore, error) {
					if !changed {
						changed = true
						// Use setup's actual lock order; no migration fence is held yet.
						for _, name := range []string{"setup.lock", "collector.lock", "hooks.lock", "search-worker.lock"} {
							release, e := local.NamedLock(home, name)
							must(t, e)
							defer release()
						}
						current, _, e := config.Load(home)
						must(t, e)
						must(t, config.SetSearchIndexing(&current, !enabled))
						must(t, config.Save(home, current))
					}
					if scoped {
						return picker, nil
					}
					if c.Storage.Bucket == "private-target" {
						return target, nil
					}
					return source, nil
				}
				args := []string{"migrate", "--format", "catalog-v4", "--bucket", "private-target", "--prefix", "new"}
				if scoped {
					args = []string{"migrate", "--scoped-picker"}
				}
				var out, diag bytes.Buffer
				if code := Run(args, nil, &out, &diag, env); code != 1 || !strings.Contains(diag.String(), "retry migration") {
					t.Fatalf("stale migration admitted: code=%d out=%s diagnostics=%s", code, out.String(), diag.String())
				}
				saved, _, err := config.Load(home)
				must(t, err)
				if !changed || saved.SearchIndexing != !enabled || saved.Storage != cfg.Storage {
					t.Fatal("migration replaced current consent or destination", saved)
				}
				if scoped {
					_, err = catalog.PickerUpgradeCheckpoint(t.Context(), picker)
				} else {
					_, err = catalog.MigrationCheckpoint(t.Context(), target)
				}
				if !errors.Is(err, storage.ErrNotFound) {
					t.Fatal("stale migration changed archive before refusal", err)
				}
			})
		}
	}
}

func TestSearchDestinationFenceRefusesChangedAdmissionAuthority(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*config.Config){
		"activation":  func(c *config.Config) { c.DestinationSince = c.DestinationSince.Add(time.Second) },
		"machine":     func(c *config.Config) { c.MachineID = "other-synthetic" },
		"credentials": func(c *config.Config) { c.Storage.AWSProfile = "other-synthetic" },
		"pause":       func(c *config.Config) { c.Paused = true },
		"uninstall":   func(c *config.Config) { c.Archive.Enabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Config{RetentionDays: 90, Archive: archive.Config{Enabled: true}, Storage: destination.Config{Provider: "s3", Bucket: "synthetic", AWSProfile: "synthetic"}}
			env := operatorTestEnv(t, cfg)
			home, err := env.readHome()
			must(t, err)
			expected, _, err := config.Load(home)
			must(t, err)
			current := expected
			change(&current)
			must(t, config.Save(home, current))
			release, err := lockSearchDestination(home, expected)
			if release != nil {
				release()
			}
			if err == nil {
				t.Fatal("changed admission authority accepted")
			}
			// Refusal must also release its lock so a retry can proceed.
			release, err = local.NamedLock(home, "search-worker.lock")
			must(t, err)
			release()
		})
	}
}
