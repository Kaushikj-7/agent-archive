package stats

// Family is the rebuild-per-call baseline for tests and benchmarks; production
// callers use FamilyLookup snapshots. Keeping this adapter test-only preserves
// the current-table mutation control without shipping an unused production API.
//
// Family returns the family label the table groups a model id under in cost by
// model ("opus", "gpt-5"), and whether the table lists the model at all. The id
// is read as the archive records it (any case, with or without a date or
// context suffix). A caller that must show only model names a table lists,
// such as a page that is meant to be shared, asks this of the built-in table:
// a fine-tune id or a custom deployment name is not listed, so it is not shown.
func (t PriceTable) Family(model string) (string, bool) {
	return t.index().family(model)
}
