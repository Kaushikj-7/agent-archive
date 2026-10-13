package stats

// price returns USD-like cost for tokens of one model, and whether the model
// is priced.
func (p priceIndex) price(model string, tokens tokenSet) (float64, bool) {
	entry, ok := p[model]
	if !ok {
		return 0, false
	}
	return entry.cost(tokens), true
}
