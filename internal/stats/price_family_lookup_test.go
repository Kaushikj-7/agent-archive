package stats

import (
	"math"
	"sync"
	"testing"
)

func TestFamilyLookupMatchesCurrentTableAndOwnsItsSnapshot(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	lookup := table.FamilyLookup()
	queries := []string{"", "unknown", "opus", "ft:gpt-4o:acme::abc", "acme-private-model"}
	for _, entry := range table.Models {
		queries = append(queries, entry.ID, "  "+entry.ID+"  ", "vendor/"+entry.ID,
			entry.ID+"-20250101", entry.ID+"[1m]")
	}
	type answer struct {
		family string
		listed bool
	}
	want := make(map[string]answer, len(queries))
	for _, query := range queries {
		family, listed := table.Family(query)
		want[query] = answer{family, listed}
	}
	// Replacing entries, growing their backing slice, and replacing the slice
	// cannot change the snapshot or make a newly private model public.
	for i := range table.Models {
		table.Models[i].Family = "changed"
		table.Models[i].ID = "changed-model"
	}
	table.Models = append(table.Models, ModelPrice{ID: "acme-private-model", Family: "private"})
	if family, listed := table.Family("acme-private-model"); family != "private" || !listed {
		t.Fatal("mutation negative control did not change the original table")
	}
	table.Models = nil
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			for query, expected := range want {
				family, listed := lookup(query)
				if family != expected.family || listed != expected.listed {
					t.Errorf("snapshot Family(%q) = %q,%v; want %q,%v", query, family, listed, expected.family, expected.listed)
				}
			}
		})
	}
	callers.Wait()
}

func TestFamilyLookupPreservesHandBuiltTableRules(t *testing.T) {
	t.Parallel()
	table := PriceTable{Models: []ModelPrice{
		{ID: " CLAUDE-OPUS-5-20250101 ", Family: "first"},
		{ID: "claude-opus-5", Family: "last"},
		{ID: "claude-opus-5[1m]", Family: "invalid later", InputPerMTok: math.NaN()},
		{ID: "No-Family"},
		{ID: "negative", InputPerMTok: -1},
		{ID: "nan", OutputPerMTok: math.NaN()},
		{ID: "infinity", CacheReadPerMTok: math.Inf(1)},
		{ID: "negative-infinity", CacheWritePerMTok: math.Inf(-1)},
		{ID: "too-large", CacheWritePerMTok: MaxPricePerMTok + 1},
		{ID: "at-limit", InputPerMTok: MaxPricePerMTok},
		{ID: " ", Family: "empty"},
	}}
	lookup := table.FamilyLookup()
	for query, expected := range map[string]string{
		"US.ANTHROPIC.CLAUDE-OPUS-5-20250101-v1:0[1m]": "last",
		"no-family": "No-Family", "at-limit": "at-limit", "negative": "",
		"nan": "", "infinity": "", "negative-infinity": "", "too-large": "", "": "", "absent": "",
	} {
		family, listed := lookup(query)
		if family != expected || listed != (expected != "") {
			t.Errorf("snapshot Family(%q) = %q,%v; want %q,%v", query, family, listed, expected, expected != "")
		}
		current, currentListed := table.Family(query)
		if family != current || listed != currentListed {
			t.Errorf("snapshot differs from current table for %q", query)
		}
	}
	if family, listed := (PriceTable{}).FamilyLookup()("anything"); family != "" || listed {
		t.Fatal("empty snapshot listed a model", family, listed)
	}
}

func BenchmarkFamilyLookup(b *testing.B) {
	table := DefaultPriceTable()
	queries := []string{
		"claude-opus-5", "  CLAUDE-OPUS-5-20250101 ", "claude-opus-5[1m]",
		"us.anthropic.claude-opus-5-20250101-v1:0", "gpt-5", "vendor/gpt-5",
		"opus", "ft:gpt-4o:acme::abc", "acme-private-model", "",
	}
	for _, implementation := range []struct {
		name   string
		lookup func(string) (string, bool)
	}{
		{"current-table", table.Family},
		{"snapshot", table.FamilyLookup()},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				implementation.lookup(queries[i%len(queries)])
				i++
			}
		})
	}
}
