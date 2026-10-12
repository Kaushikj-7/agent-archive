package stats

import (
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// PrepareOptions fixes the time zone and prices for a prepared snapshot.
// Its zero value uses time.Local and DefaultPriceTable.
type PrepareOptions struct {
	Location   *time.Location
	PriceTable PriceTable
}

// Prepared owns immutable session units that can be reused across windows.
// Prepare again when the metadata, time zone, or prices change.
type Prepared struct {
	location *time.Location
	table    PriceTable
	units    []*unit
}

// Prepare resolves session relationships and token costs once. It retains no
// mutable metadata or price slices owned by the caller.
func Prepare(sessions []archive.Metadata, opts PrepareOptions) *Prepared {
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	table := opts.PriceTable
	if table.Version == "" && len(table.Models) == 0 {
		table = DefaultPriceTable()
	}
	if table.Currency == "" {
		table.Currency = "USD"
	}
	table.Sources = slices.Clone(table.Sources)
	table.Models = slices.Clone(table.Models)
	units := buildUnits(sessions, loc, table.index())
	for _, u := range units {
		// Unit totals and highlight scalars own everything needed by later
		// windows. Drop the temporary relationship pointers into caller metadata.
		u.root = nil
		u.children = nil
	}
	return &Prepared{location: loc, table: table, units: units}
}

// modelLookup remembers each raw identifier for this preparation only.
type modelLookup struct {
	prices     priceIndex
	normalized map[string]string
	entries    map[string]modelPriceLookup
}

func (m *modelLookup) normalize(raw string) string {
	if id, ok := m.normalized[raw]; ok {
		return id
	}
	id := NormalizeModel(raw)
	m.normalized[raw] = id
	return id
}

// modelPriceLookup caches normalized price and family lookups across units.
type modelPriceLookup struct {
	entry  ModelPrice
	label  string
	priced bool
}

func (m *modelLookup) price(id string) modelPriceLookup {
	if entry, ok := m.entries[id]; ok {
		return entry
	}
	entry, ok := m.prices[id]
	result := modelPriceLookup{entry: entry, label: m.prices.label(id), priced: ok}
	m.entries[id] = result
	return result
}
