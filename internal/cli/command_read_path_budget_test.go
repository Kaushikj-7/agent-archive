package cli

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type readPathListingPrefix string

const (
	readPathCanonicalListing       readPathListingPrefix = "sessions/"
	readPathLegacyCanonicalListing readPathListingPrefix = "sessions"
	readPathV2Listing              readPathListingPrefix = listingindex.V2Prefix
	readPathV3Listing              readPathListingPrefix = listingindex.V3Prefix
)

// These independent fixture plans describe the two explicitly modeled reader
// policies. They never use the production planner or infer budgets from the
// requests being checked. Baseline headers are the cache's immutable warm hints;
// current headers include any newly published revisions inside those boundaries.
func readPathExpectedListingCosts(command readPathCommand, state readPathCacheState, baseline, current []storage.Object, model storagetest.ListingLatencyModel) []storagetest.ListingRequestCost {
	prefixes := []readPathListingPrefix{readPathCanonicalListing}
	if command == readPathListPiped {
		prefixes = append(prefixes, readPathV2Listing, readPathV3Listing)
	}
	warm := state == readPathWarm || state == readPathChanged
	var calls []storagetest.ListingRequestCost
	for _, prefix := range prefixes {
		var hints, headers []storage.Object
		for _, object := range baseline {
			if strings.HasPrefix(object.Key, string(prefix)) {
				hints = append(hints, object)
			}
		}
		for _, object := range current {
			if strings.HasPrefix(object.Key, string(prefix)) {
				headers = append(headers, object)
			}
		}
		var bounds []string
		if warm && model.StopAtBoundary {
			// Post-PR1 caches every key and uses exact 1,000-key stride boundaries.
			for i := 999; i < len(hints)-1; i += 1000 {
				bounds = append(bounds, hints[i].Key)
			}
		} else if warm && command != readPathListPiped && prefix == readPathCanonicalListing {
			// The legacy complete-body paths know all sidecars, with 200-sidecar
			// boundaries. Piped list has only 50 cached sidecars, below its 400 floor.
			var sidecars []string
			for _, hint := range hints {
				if strings.HasSuffix(hint.Key, "/metadata.json") {
					sidecars = append(sidecars, hint.Key)
				}
			}
			if len(sidecars) >= 400 {
				for i := 199; i < len(sidecars)-1; i += 200 {
					bounds = append(bounds, sidecars[i])
				}
			}
		}
		after := ""
		for i := range len(bounds) + 1 {
			through := ""
			if i < len(bounds) {
				through = bounds[i]
			}
			_, cost := storagetest.ModelListingRange(headers, after, through, model)
			calls = append(calls, storagetest.ListingRequestCost{Prefix: string(prefix), After: after, Through: through, Cost: cost})
			after = through
		}
	}
	return calls
}

// This is an analytical page-service schedule for one discovery, rather than
// measured wall time. Canonical/v2/v3 dispatch ordering is deterministic here.
// Legacy canonical ranges own 16 slots; unranged auxiliaries run independently.
// Post-PR1 all prefix ranges use one shared 16-slot pool. Longest single-chain
// pages remain a separate metric. At 1.8k all planned ranged services fit 16 slots.
func readPathModeledListingWaves(calls []storagetest.ListingRequestCost, shared bool) (int64, error) {
	ordered := slices.Clone(calls)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Prefix != ordered[j].Prefix {
			return ordered[i].Prefix < ordered[j].Prefix
		}
		return ordered[i].After < ordered[j].After
	})
	var canonical, v2, v3 []int64
	for _, call := range ordered {
		if call.Cost.Pages < 1 {
			return 0, fmt.Errorf("invalid modeled page service")
		}
		switch readPathListingPrefix(call.Prefix) {
		case readPathCanonicalListing, readPathLegacyCanonicalListing:
			canonical = append(canonical, call.Cost.Pages)
		case readPathV2Listing:
			v2 = append(v2, call.Cost.Pages)
		case readPathV3Listing:
			v3 = append(v3, call.Cost.Pages)
		default:
			return 0, fmt.Errorf("unclassified listing prefix %q", call.Prefix)
		}
	}
	if shared {
		return storagetest.ListingSchedule(append(append(canonical, v2...), v3...), 16), nil
	}
	var auxiliaryV2, auxiliaryV3 int64
	for _, pages := range v2 {
		auxiliaryV2 += pages
	}
	for _, pages := range v3 {
		auxiliaryV3 += pages
	}
	return max(storagetest.ListingSchedule(canonical, 16), auxiliaryV2, auxiliaryV3), nil
}

func readPathListingTotals(calls []storagetest.ListingRequestCost) storagetest.ReadMetrics {
	var metrics storagetest.ReadMetrics
	for _, call := range calls {
		metrics.Lists++
		metrics.ListPages += call.Cost.Pages
		metrics.ListKeys += call.Cost.Keys
		metrics.ListBytes += call.Cost.Bytes
		metrics.DiscardedListBytes += call.Cost.DiscardedBytes
		metrics.LongestListPages = max(metrics.LongestListPages, call.Cost.Pages)
	}
	return metrics
}

func readPathDiscoveryBudget(measured storagetest.ReadMetrics, actual, expected []storagetest.ListingRequestCost, shared bool) error {
	observed := readPathListingTotals(actual)
	if measured.Lists == 0 || measured.Lists != observed.Lists || measured.ListPages != observed.ListPages || measured.ListKeys != observed.ListKeys || measured.ListBytes != observed.ListBytes || measured.DiscardedListBytes != observed.DiscardedListBytes || measured.LongestListPages != observed.LongestListPages {
		return fmt.Errorf("listing accounting missing requests or costs: measured=%+v recorded=%+v", measured, observed)
	}
	waves, err := readPathModeledListingWaves(actual, shared)
	if err != nil {
		return err
	}
	maxWaves, err := readPathModeledListingWaves(expected, shared)
	if err != nil {
		return err
	}
	if waves > maxWaves {
		return fmt.Errorf("modeled critical-page-waves %d exceed %d", waves, maxWaves)
	}
	limit := readPathListingTotals(expected)
	if observed.Lists > limit.Lists || observed.ListPages > limit.ListPages || observed.ListKeys > limit.ListKeys || observed.ListBytes > limit.ListBytes || observed.DiscardedListBytes > limit.DiscardedListBytes || observed.LongestListPages > limit.LongestListPages {
		return fmt.Errorf("discovery exceeds fixture model: actual=%+v ceiling=%+v", observed, limit)
	}
	return nil
}

func TestReadPathDiscoveryBudgetsRejectDuplicatedWorkAndExtraWaves(t *testing.T) {
	t.Parallel()
	mem := storagetest.NewMemoryStore()
	storagetest.SeedReadPathArchive(t, mem, 1800, 0)
	baseline, err := mem.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	storagetest.SeedReadPathArchive(t, mem, 1800, 1)
	changed, err := mem.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, shared := range []bool{false, true} {
		model := storagetest.ListingLatencyModel{StopAtBoundary: shared}
		for _, command := range readPathCommands() {
			for _, state := range []readPathCacheState{readPathCold, readPathWarm, readPathChanged, readPathNoCache} {
				if state == readPathNoCache && !command.supportsNoCache() {
					continue
				}
				headers := baseline
				if state == readPathChanged {
					headers = changed
				}
				expected := readPathExpectedListingCosts(command, state, baseline, headers, model)
				metrics := readPathListingTotals(expected)
				if err := readPathDiscoveryBudget(metrics, expected, expected, shared); err != nil {
					t.Fatal(command, state, shared, err)
				}
				if command == readPathListPiped && state == readPathCold && (metrics.ListPages != 9 || metrics.ListKeys != 6840) {
					t.Fatal("cold control drifted", metrics)
				}
				duplicated := append(slices.Clone(expected), expected...)
				duplicateMetrics := readPathListingTotals(duplicated)
				if command == readPathListPiped && state == readPathCold && (duplicateMetrics.ListPages != 18 || duplicateMetrics.ListKeys != 13680) {
					t.Fatal("duplicated cold control drifted", duplicateMetrics)
				}
				if err := readPathDiscoveryBudget(duplicateMetrics, duplicated, expected, shared); err == nil {
					t.Fatal("accepted duplicated discovery", command, state, shared)
				}
				extra := slices.Clone(expected)
				longest := 0
				for i := range extra {
					if extra[i].Cost.Pages > extra[longest].Cost.Pages {
						longest = i
					}
				}
				extra[longest].Cost.Pages++
				if err := readPathDiscoveryBudget(readPathListingTotals(extra), extra, expected, shared); err == nil || !strings.Contains(err.Error(), "critical-page-waves") {
					t.Fatal("extra waves escaped their gate", command, state, shared, err)
				}
				missing := metrics
				missing.Lists++
				if err := readPathDiscoveryBudget(missing, expected, expected, shared); err == nil {
					t.Fatal("unrecorded request escaped wave accounting")
				}
			}
		}
	}
}
