package config

// sourceSetUnderlyingConfig removes only the outer source-set writer fence.
// Every lower capability and consent boundary remains part of validation.
func sourceSetUnderlyingConfig(c Config) Config {
	c.SourceSetPublicationProtection = false
	search, indexing := c.SearchLifecycleProtection, c.SearchIndexing
	c = searchUnderlyingConfig(c)
	c.SearchLifecycleProtection, c.SearchIndexing = search, indexing
	if search {
		c.SchemaVersion = 16
	}
	return c
}
