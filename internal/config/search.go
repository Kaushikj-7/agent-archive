package config

import (
	"errors"

	"github.com/wangjohn/agent-archive/internal/destination"
)

// SetSearchIndexing records local consent and permanently protects lifecycle writers.
func SetSearchIndexing(c *Config, enabled bool) error {
	c.SearchIndexing = enabled
	if enabled {
		c.SearchLifecycleProtection = true
	}
	return prepareDiscoveryConfig(c)
}

// searchUnderlyingConfig removes only the outer lifecycle fence on a value copy.
// The exact lower capabilities, including remembered scoped/settled flags, remain.
func searchUnderlyingConfig(c Config) Config {
	c.SearchLifecycleProtection = false
	c.SearchIndexing = false
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
	if c.HistoricalProjectProtection {
		c.SchemaVersion = 10
	}
	if c.Storage.EffectiveArchiveFormat() == destination.FormatCatalogV4 {
		c.SchemaVersion = 11
	}
	if c.PublicationCompositionProtection {
		c.SchemaVersion = 12
	}
	if c.StagedSelectionProtection {
		c.SchemaVersion = 13
	}
	if c.SettledCursorSelectionProtection {
		c.SchemaVersion = 15
	}
	if c.ScopedPickerProtection {
		c.SchemaVersion = 14
	}
	return c
}

func peelSearchProtection(c Config) (Config, error) {
	if !c.SearchLifecycleProtection {
		if c.SearchIndexing || c.SchemaVersion == 16 {
			return c, errors.New("search indexing requires lifecycle protection")
		}
		return c, nil
	}
	if c.SchemaVersion != 16 {
		return c, errors.New("search lifecycle protection requires schema 16")
	}
	return searchUnderlyingConfig(c), nil
}
