package stats

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

//go:embed prices.json
var defaultPricesJSON []byte

// PriceTable is a dated, versioned list of per-model list prices, in
// Currency per million tokens. It is what turns token counts into an
// estimate: the estimate is only ever as current as AsOf, and only ever
// covers the models the table names. A model that is not in the table is
// unpriced, and every cost that includes its tokens says so (Cost.Partial);
// the engine never substitutes a similar model's price.
//
// The built-in table is DefaultPriceTable. A person's own prices come from a
// JSON file through ParsePriceTable, and replace or add to the built-in
// entries with WithOverrides.
type PriceTable struct {
	// Version names the table's revision, for example "2026-09.1".
	Version string `json:"version"`
	// AsOf is the date (YYYY-MM-DD) the prices were read from Sources.
	AsOf string `json:"as_of"`
	// Currency is the ISO 4217 code of every price. It defaults to USD.
	Currency string `json:"currency"`
	// Sources are the pages the prices came from.
	Sources []string `json:"sources,omitempty"`
	// Notes states what the prices assume (speed, context, cache tier).
	Notes  string       `json:"notes,omitempty"`
	Models []ModelPrice `json:"models"`
	// Overridden is set by WithOverrides: the table is the built-in one with
	// a person's own entries applied, so its numbers are theirs.
	Overridden bool `json:"overridden,omitempty"`
}

// ModelPrice is one model's price per million tokens. CacheWritePerMTok is
// the rate for writing the prompt cache; a model with no separate cache-write
// charge (OpenAI's) has it 0. Reasoning tokens have no price of their own:
// they are part of the output tokens and cost what output costs.
type ModelPrice struct {
	// ID is the model id as the archive records it, in lower case and
	// without a date suffix ("claude-opus-5-5"); see NormalizeModel.
	ID string `json:"id"`
	// Family is the short label cost-by-model groups the model under
	// ("opus", "gpt-5"). It defaults to ID.
	Family            string  `json:"family,omitempty"`
	InputPerMTok      float64 `json:"input_per_mtok"`
	OutputPerMTok     float64 `json:"output_per_mtok"`
	CacheReadPerMTok  float64 `json:"cache_read_per_mtok"`
	CacheWritePerMTok float64 `json:"cache_write_per_mtok"`
}

// MaxPricePerMTok is the largest price per million tokens a table may carry.
// It is far above any real price in any currency (a million-dollar-a-token
// model does not exist), and low enough that no sum of costs over int64 token
// counts can reach +Inf, which JSON cannot carry.
const MaxPricePerMTok = 1e9

// maxPriceVersionBytes bounds a table's version label, which is printed.
const maxPriceVersionBytes = 64

// isCurrencyCode reports whether s is three ASCII capital letters.
func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// validPrice reports whether a price is a finite number in [0, MaxPricePerMTok].
func validPrice(price float64) bool {
	return price >= 0 && price <= MaxPricePerMTok && !math.IsNaN(price)
}

// priceFile is the wire form of a table: every price is a pointer so a file
// that leaves one out is an error, not a silently free token type.
type priceFile struct {
	Version  string   `json:"version"`
	AsOf     string   `json:"as_of"`
	Currency string   `json:"currency"`
	Sources  []string `json:"sources"`
	Notes    string   `json:"notes"`
	Models   []struct {
		ID                string   `json:"id"`
		Family            string   `json:"family"`
		InputPerMTok      *float64 `json:"input_per_mtok"`
		OutputPerMTok     *float64 `json:"output_per_mtok"`
		CacheReadPerMTok  *float64 `json:"cache_read_per_mtok"`
		CacheWritePerMTok *float64 `json:"cache_write_per_mtok"`
	} `json:"models"`
}

// DefaultPriceTable returns the price table built into this release. Its
// prices are list prices read from the pages in Sources on AsOf; they change
// with releases, not at run time.
func DefaultPriceTable() PriceTable {
	table, err := ParsePriceTable(defaultPricesJSON)
	if err != nil {
		// prices.json is embedded and checked by TestDefaultPriceTableIsValid.
		panic("stats: built-in price table is invalid: " + err.Error())
	}
	return table
}

// ParsePriceTable reads a price table from its JSON form (the format of the
// built-in table). It rejects a table with no version (or one over
// maxPriceVersionBytes) or no models, a currency that is not a three-letter
// code, a date
// that is not YYYY-MM-DD, an empty or repeated model id, and any of the four
// prices missing, negative, or above MaxPricePerMTok. Unknown fields are an
// error, so a misspelled price name does not turn into a free token type.
func ParsePriceTable(data []byte) (PriceTable, error) {
	var file priceFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return PriceTable{}, fmt.Errorf("price table is not valid JSON in the expected shape: %w", err)
	}
	if decoder.More() {
		return PriceTable{}, errors.New("price table has content after its JSON object")
	}
	if strings.TrimSpace(file.Version) == "" {
		return PriceTable{}, errors.New(`price table needs a "version"`)
	}
	if len(file.Version) > maxPriceVersionBytes {
		return PriceTable{}, fmt.Errorf(`price table "version" is longer than %d bytes`, maxPriceVersionBytes)
	}
	if currency := strings.ToUpper(strings.TrimSpace(file.Currency)); currency != "" && !isCurrencyCode(currency) {
		return PriceTable{}, fmt.Errorf(`price table "currency" must be a three-letter ISO 4217 code like USD, got %q`, file.Currency)
	}
	if _, err := time.Parse("2006-01-02", file.AsOf); err != nil {
		return PriceTable{}, fmt.Errorf(`price table "as_of" must be a date like 2026-09-29, got %q`, file.AsOf)
	}
	if len(file.Models) == 0 {
		return PriceTable{}, errors.New(`price table needs at least one entry in "models"`)
	}
	table := PriceTable{
		Version: file.Version, AsOf: file.AsOf, Currency: strings.ToUpper(strings.TrimSpace(file.Currency)),
		Sources: file.Sources, Notes: file.Notes,
	}
	if table.Currency == "" {
		table.Currency = "USD"
	}
	seen := map[string]bool{}
	for i, entry := range file.Models {
		id := NormalizeModel(entry.ID)
		if id == "" {
			return PriceTable{}, fmt.Errorf("price table model %d has no id", i+1)
		}
		if seen[id] {
			return PriceTable{}, fmt.Errorf("price table lists model %q twice", id)
		}
		seen[id] = true
		prices := []struct {
			name  string
			value *float64
		}{
			{"input_per_mtok", entry.InputPerMTok}, {"output_per_mtok", entry.OutputPerMTok},
			{"cache_read_per_mtok", entry.CacheReadPerMTok}, {"cache_write_per_mtok", entry.CacheWritePerMTok},
		}
		for _, price := range prices {
			if price.value == nil {
				return PriceTable{}, fmt.Errorf("price table model %q is missing %q (use 0 for a token type that costs nothing)", id, price.name)
			}
			if !validPrice(*price.value) {
				return PriceTable{}, fmt.Errorf("price table model %q has an invalid %q (it must be between 0 and %g)", id, price.name, MaxPricePerMTok)
			}
		}
		family := strings.TrimSpace(entry.Family)
		if family == "" {
			family = id
		}
		table.Models = append(table.Models, ModelPrice{
			ID: id, Family: family, InputPerMTok: *entry.InputPerMTok, OutputPerMTok: *entry.OutputPerMTok,
			CacheReadPerMTok: *entry.CacheReadPerMTok, CacheWritePerMTok: *entry.CacheWritePerMTok,
		})
	}
	return table, nil
}

// WithOverrides returns t with every entry of custom applied on top: an
// entry for a model t already prices replaces it, any other is added. The
// result takes custom's version, date, sources and notes and is marked
// Overridden, so output built from it says the prices are the person's own.
// A custom table in another currency than t replaces t outright: t's numbers
// must not be relabelled as if they were in custom's currency.
func (t PriceTable) WithOverrides(custom PriceTable) PriceTable {
	currency := t.Currency
	if custom.Currency != "" {
		currency = custom.Currency
	}
	if !strings.EqualFold(currency, t.Currency) {
		t = PriceTable{}
	}
	merged := PriceTable{
		Version: custom.Version, AsOf: custom.AsOf, Currency: currency, Sources: custom.Sources,
		Notes: custom.Notes, Overridden: true,
	}
	replaced := map[string]ModelPrice{}
	for _, entry := range custom.Models {
		replaced[NormalizeModel(entry.ID)] = entry
	}
	for _, entry := range t.Models {
		if repl, ok := replaced[NormalizeModel(entry.ID)]; ok {
			merged.Models = append(merged.Models, repl)
			delete(replaced, NormalizeModel(entry.ID))
			continue
		}
		merged.Models = append(merged.Models, entry)
	}
	for _, entry := range custom.Models {
		if _, ok := replaced[NormalizeModel(entry.ID)]; ok {
			merged.Models = append(merged.Models, entry)
		}
	}
	return merged
}

// A date suffix is a plausible calendar date (year 19xx or 20xx, month 01-12,
// day 01-31), not any run of digits: a model id that merely ends in eight
// digits, or in "-1234-56-78", must not be priced as the model before them.
const (
	dateYear  = `(?:19|20)\d{2}`
	dateMonth = `(?:0[1-9]|1[0-2])`
	dateDay   = `(?:0[1-9]|[12]\d|3[01])`
)

var (
	contextSuffix = regexp.MustCompile(`\[[^\]]*\]$`)
	dateSuffix    = regexp.MustCompile(`(?:[-@]` + dateYear + dateMonth + dateDay + `|-` + dateYear + `-` + dateMonth + `-` + dateDay + `)$`)
	// vendorPrefix is Amazon Bedrock's "anthropic." vendor prefix and its
	// cross-region inference prefix ("us.anthropic.", "global.anthropic.").
	vendorPrefix = regexp.MustCompile(`^(?:[a-z0-9-]+\.)?anthropic\.`)
	// platformVersion is Bedrock's "-v1:0" model version suffix (it always has
	// the colon, unlike a name such as "deepseek-v3"). Only Claude models are
	// served under it here, so only a Claude id loses it.
	platformVersion = regexp.MustCompile(`-v\d+:\d+$`)
	// bedrockBareVersion is the one colon-less version Bedrock uses, in
	// "anthropic.claude-opus-4-6-v1". It is dropped only from an id that
	// carried Bedrock's vendor prefix: a bare "claude-opus-5-5-v1" is not a
	// Bedrock id and is left as it is (unpriced).
	bedrockBareVersion = regexp.MustCompile(`-v1$`)
)

// NormalizeModel is the form of a model id the price table is keyed by:
// trimmed, lower case, without a bracketed suffix such as Claude Code's
// "[1m]" context marker and without a trailing date ("-20251001",
// "@20251001", or OpenAI's "-2025-08-07"). It also unwraps the ways a cloud
// platform names the same model: a path ("anthropic/claude-opus-5-5") and
// Amazon Bedrock's vendor, region and version parts
// ("us.anthropic.claude-opus-5-5-20251001-v1:0",
// "anthropic.claude-opus-4-6-v1"). Those name the same model, so they are
// priced at its list price; a platform that bills differently (regional
// endpoints add 10%) is not reflected. It does not map aliases: a
// bare "opus" stays "opus", which no table prices, because it does not say
// which version answered.
//
// It only ever removes a prefix or a suffix, and repeats until nothing more
// comes off, so NormalizeModel(NormalizeModel(id)) is NormalizeModel(id): a
// table entry stored by its normalized id is found under that same key, and
// two entries that passed the duplicate check cannot collide in the index.
func NormalizeModel(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	for {
		next := normalizeModelOnce(id)
		if next == id {
			return id
		}
		id = next
	}
}

func normalizeModelOnce(id string) string {
	if slash := strings.LastIndex(id, "/"); slash >= 0 {
		id = id[slash+1:]
	}
	bedrock := vendorPrefix.MatchString(id)
	id = vendorPrefix.ReplaceAllString(id, "")
	id = contextSuffix.ReplaceAllString(id, "")
	if strings.HasPrefix(id, "claude-") {
		id = platformVersion.ReplaceAllString(id, "")
		if bedrock {
			id = bedrockBareVersion.ReplaceAllString(id, "")
		}
	}
	id = dateSuffix.ReplaceAllString(id, "")
	return strings.TrimSpace(id)
}

// priceIndex is a table's entries by normalized id, for the many lookups one
// Compute makes.
type priceIndex map[string]ModelPrice

// index keys the table's entries by normalized id. An entry with an invalid
// price (a table built by hand rather than by ParsePriceTable) is left out, so
// its model is unpriced and flagged instead of costing NaN or +Inf.
func (t PriceTable) index() priceIndex {
	index := priceIndex{}
	for _, entry := range t.Models {
		if validPrice(entry.InputPerMTok) && validPrice(entry.OutputPerMTok) &&
			validPrice(entry.CacheReadPerMTok) && validPrice(entry.CacheWritePerMTok) {
			if id := NormalizeModel(entry.ID); id != "" {
				index[id] = entry
			}
		}
	}
	return index
}

// Family returns the family label the table groups a model id under in cost by
// model ("opus", "gpt-5"), and whether the table lists the model at all. The id
// is read as the archive records it (any case, with or without a date or
// context suffix). A caller that must show only model names a table lists,
// such as a page that is meant to be shared, asks this of the built-in table:
// a fine-tune id or a custom deployment name is not listed, so it is not shown.
func (t PriceTable) Family(model string) (string, bool) {
	index := t.index()
	id := NormalizeModel(model)
	if _, ok := index[id]; !ok {
		return "", false
	}
	return index.label(id), true
}

// label is the short name a model is grouped under in cost by model: its
// price entry's family, or the normalized id when it is unpriced.
func (p priceIndex) label(model string) string {
	if entry, ok := p[model]; ok {
		if entry.Family != "" {
			return entry.Family
		}
		return entry.ID
	}
	return model
}

func (entry ModelPrice) cost(tokens tokenSet) float64 {
	const perMillion = 1e6
	cost := float64(tokens.fresh)*entry.InputPerMTok +
		float64(tokens.read)*entry.CacheReadPerMTok +
		float64(tokens.write)*entry.CacheWritePerMTok +
		float64(tokens.out)*entry.OutputPerMTok
	return cost / perMillion
}
