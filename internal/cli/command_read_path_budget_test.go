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

type readPathListingService struct {
	Prefix  readPathListingPrefix
	After   string
	Through string
}

func readPathRequireListingServices(actual, expected []storagetest.ListingRequestCost) error {
	planned := make(map[readPathListingService]int, len(expected))
	service := func(call storagetest.ListingRequestCost) readPathListingService {
		prefix := readPathListingPrefix(call.Prefix)
		if prefix == readPathLegacyCanonicalListing {
			prefix = readPathCanonicalListing
		}
		return readPathListingService{Prefix: prefix, After: call.After, Through: call.Through}
	}
	for _, call := range expected {
		planned[service(call)]++
	}
	for _, call := range actual {
		key := service(call)
		if planned[key] == 0 {
			return fmt.Errorf("unexpected listing service: %+v", key)
		}
		planned[key]--
	}
	for key, remaining := range planned {
		if remaining != 0 {
			return fmt.Errorf("missing required listing service: %+v", key)
		}
	}
	return nil
}

func readPathDiscoveryBudget(measured storagetest.ReadMetrics, actual, expected []storagetest.ListingRequestCost, shared bool) error {
	observed := readPathListingTotals(actual)
	if measured.Lists == 0 || measured.Lists != observed.Lists || measured.ListPages != observed.ListPages || measured.ListKeys != observed.ListKeys || measured.ListBytes != observed.ListBytes || measured.DiscardedListBytes != observed.DiscardedListBytes || measured.LongestListPages != observed.LongestListPages {
		return fmt.Errorf("listing accounting missing requests or costs: measured=%+v recorded=%+v", measured, observed)
	}
	if err := readPathRequireListingServices(actual, expected); err != nil {
		return err
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
				if command == readPathListPiped {
					var omitted []storagetest.ListingRequestCost
					for _, call := range expected {
						if call.Prefix != listingindex.V2Prefix {
							omitted = append(omitted, call)
						}
					}
					if err := readPathDiscoveryBudget(readPathListingTotals(omitted), omitted, expected, shared); err == nil || !strings.Contains(err.Error(), "required listing service") {
						t.Fatal("omitted empty-v2 request escaped gate", state, shared, err)
					}
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

// Successful body bytes are bounded independently of observed GETs. The fixture
// refreshes complete legacy metadata except the newest 50 piped window. At most
// two fallback metadata reads retain the existing GET-count allowance. Handoff
// query spends those two reads on its selected metadata and real source bundle.
func readPathExpectedGETBytes(command readPathCommand, state readPathCacheState, size int, headers []storage.Object) (int64, error) {
	objects := make(map[string]int64, len(headers))
	for _, object := range headers {
		objects[object.Key] = object.Size
	}
	metadata := make([]int64, size)
	var maximum int64
	for i := range size {
		key := readPathFixtureMetadataKey(i + 1)
		n, ok := objects[key]
		if !ok || n <= 0 {
			return 0, fmt.Errorf("missing sized fixture metadata %s", key)
		}
		metadata[i] = n
		maximum = max(maximum, n)
	}
	var total int64
	switch state {
	case readPathCold, readPathNoCache:
		start := 0
		if command == readPathListPiped {
			start = max(0, size-50)
		}
		for _, n := range metadata[start:] {
			total += n
		}
	case readPathChanged:
		if command != readPathListPiped {
			for _, n := range metadata[:max(1, size/100)] {
				total += n
			}
		}
	case readPathWarm:
	default:
		return 0, fmt.Errorf("unknown cache state %q", state)
	}
	if command != readPathHandoffQuery {
		return total + 2*maximum, nil
	}
	// The first fixture session has one real compressed bundle and four one-byte
	// historical placeholders. Its largest source object is the selected bundle.
	var sourceBytes int64
	for key, n := range objects {
		if strings.HasPrefix(key, "sessions/codex/00000001000000000000000000000001/source.") {
			sourceBytes = max(sourceBytes, n)
		}
	}
	if sourceBytes <= 1 {
		return 0, fmt.Errorf("missing sized selected source fixture")
	}
	return total + metadata[0] + sourceBytes, nil
}

func readPathGETByteBudget(command readPathCommand, state readPathCacheState, size int, metrics storagetest.ReadMetrics, headers []storage.Object) error {
	maximum, err := readPathExpectedGETBytes(command, state, size, headers)
	if err != nil {
		return err
	}
	if metrics.Bytes > maximum {
		return fmt.Errorf("successful GET bytes %d exceed fixture ceiling %d", metrics.Bytes, maximum)
	}
	return nil
}

func TestReadPathGETBytesRejectOversizedSuccessfulFallback(t *testing.T) {
	t.Parallel()
	const size = 100
	mem := storagetest.NewMemoryStore()
	storagetest.SeedReadPathArchive(t, mem, size, 0)
	headers, err := mem.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range readPathCommands() {
		for _, state := range []readPathCacheState{readPathCold, readPathWarm, readPathChanged, readPathNoCache} {
			if state == readPathNoCache && !command.supportsNoCache() {
				continue
			}
			limit, err := readPathExpectedGETBytes(command, state, size, headers)
			if err != nil {
				t.Fatal(err)
			}
			if err := readPathGETByteBudget(command, state, size, storagetest.ReadMetrics{Bytes: limit}, headers); err != nil {
				t.Fatal("valid byte boundary rejected", err)
			}
			if err := readPathGETByteBudget(command, state, size, storagetest.ReadMetrics{Bytes: limit + 1}, headers); err == nil {
				t.Fatal("oversized body bytes accepted", command, state)
			}
		}
	}
	limit, err := readPathExpectedGETBytes(readPathShowPicker, readPathWarm, size, headers)
	if err != nil {
		t.Fatal(err)
	}
	const oversizedKey = "unexpected-synthetic-source"
	if err := mem.Put(t.Context(), oversizedKey, make([]byte, limit+1)); err != nil {
		t.Fatal(err)
	}
	measured := storagetest.NewMeasuredStore(mem, 0)
	if _, err := measured.Get(t.Context(), oversizedKey); err != nil {
		t.Fatal(err)
	}
	metrics := measured.Metrics()
	metrics.Lists, metrics.ListPages = 1, 1
	if err := readPathOperationBudget(readPathShowPicker, readPathWarm, size, metrics); err != nil {
		t.Fatal("negative control exceeded existing GET count slack", err)
	}
	if err := readPathGETByteBudget(readPathShowPicker, readPathWarm, size, metrics, headers); err == nil {
		t.Fatal("successful oversized fallback read escaped byte gate")
	}
}

func readPathFixtureMetadataKey(identity int) string {
	harness := "codex"
	if identity%20 == 0 {
		harness = "future-agent"
	}
	return fmt.Sprintf("sessions/%s/%08x%024x/metadata.json", harness, identity, identity)
}

func readPathRequiredGETKeys(command readPathCommand, state readPathCacheState, size int) map[string]bool {
	required := map[string]bool{}
	switch state {
	case readPathCold, readPathNoCache:
		start := 1
		if command == readPathListPiped {
			start = max(1, size-49)
		}
		for identity := start; identity <= size; identity++ {
			required[readPathFixtureMetadataKey(identity)] = true
		}
	case readPathChanged:
		if command != readPathListPiped {
			for identity := 1; identity <= max(1, size/100); identity++ {
				required[readPathFixtureMetadataKey(identity)] = true
			}
		}
	case readPathWarm:
	}
	return required
}

func readPathRequireGETCoverage(command readPathCommand, state readPathCacheState, size int, actual []string, headers []storage.Object) error {
	required := readPathRequiredGETKeys(command, state, size)
	allowed := make(map[string]bool, size+1)
	for identity := 1; identity <= size; identity++ {
		allowed[readPathFixtureMetadataKey(identity)] = true
	}
	if command == readPathHandoffQuery {
		var source string
		var largest int64
		for _, object := range headers {
			if strings.HasPrefix(object.Key, "sessions/codex/00000001000000000000000000000001/source.") && object.Size > largest {
				source, largest = object.Key, object.Size
			}
		}
		if largest <= 1 {
			return fmt.Errorf("missing selected source fixture")
		}
		allowed[source], required[source] = true, true
	}
	seen := make(map[string]bool, len(actual))
	for _, key := range actual {
		if !allowed[key] {
			return fmt.Errorf("unexpected successful body read %s", key)
		}
		seen[key] = true
	}
	for key := range required {
		if !seen[key] {
			return fmt.Errorf("required successful body read missing: %s", key)
		}
	}
	return nil
}

func TestReadPathGETCoverageRejectsInventoryMaskedBySelectedReads(t *testing.T) {
	t.Parallel()
	const size = 200
	mem := storagetest.NewMemoryStore()
	storagetest.SeedReadPathArchive(t, mem, size, 0)
	baseline, err := mem.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	storagetest.SeedReadPathArchive(t, mem, size, 1)
	changed, err := mem.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []readPathCacheState{readPathCold, readPathChanged} {
		headers := baseline
		if state == readPathChanged {
			headers = changed
		}
		// Omit an unrelated required metadata body, retaining the selected
		// first session so its duplicate reads can mask the scalar floor.
		required := readPathRequiredGETKeys(readPathHandoffQuery, state, size)
		missing := readPathFixtureMetadataKey(2)
		var actual []string
		for key := range required {
			if key != missing {
				actual = append(actual, key)
			}
		}
		var source string
		var sourceSize int64
		sizes := map[string]int64{}
		for _, object := range headers {
			sizes[object.Key] = object.Size
			if strings.HasPrefix(object.Key, "sessions/codex/00000001000000000000000000000001/source.") && object.Size > sourceSize {
				source, sourceSize = object.Key, object.Size
			}
		}
		if err := readPathRequireGETCoverage(readPathHandoffQuery, readPathWarm, size, []string{source}, headers); err != nil {
			t.Fatal("verified warm metadata cache reuse rejected", err)
		}
		// The duplicate selected metadata and legitimate source remain within
		// count and byte allowances while leaving the unrelated key unread.
		substitute := readPathFixtureMetadataKey(1)
		actual = append(actual, substitute, source)
		metrics := storagetest.ReadMetrics{Lists: 1, ListPages: 1, Gets: int64(len(actual))}
		for _, key := range actual {
			metrics.Bytes += sizes[key]
		}
		if err := readPathOperationBudget(readPathHandoffQuery, state, size, metrics); err != nil {
			t.Fatal("negative exceeded original scalar gate", state, err)
		}
		if err := readPathGETByteBudget(readPathHandoffQuery, state, size, metrics, headers); err != nil {
			t.Fatal("negative exceeded byte ceiling", state, err)
		}
		if err := readPathRequireGETCoverage(readPathHandoffQuery, state, size, actual, headers); err == nil {
			t.Fatal("selected reads masked missing inventory", state)
		}
		complete := append(slices.Clone(actual), missing)
		if err := readPathRequireGETCoverage(readPathHandoffQuery, state, size, complete, headers); err != nil {
			t.Fatal("complete successful body set rejected", state, err)
		}
	}
	// Piped reads require each newest-window identity; repeated selected reads
	// cannot substitute for another row even when the aggregate GET count is 50.
	required := readPathRequiredGETKeys(readPathListPiped, readPathCold, size)
	var piped []string
	missing := readPathFixtureMetadataKey(size)
	for key := range required {
		if key != missing {
			piped = append(piped, key)
		}
	}
	piped = append(piped, readPathFixtureMetadataKey(size-1))
	if err := readPathRequireGETCoverage(readPathListPiped, readPathCold, size, piped, baseline); err == nil {
		t.Fatal("duplicate piped row masked missing newest identity")
	}
}
