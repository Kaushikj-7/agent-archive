package stats

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
)

func TestProjectHeapMatchesFullSortWithTiesAndUnknowns(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 11))
	rows := make([]Project, 1000)
	for i := range rows {
		rows[i] = Project{Name: fmt.Sprintf("project-%04d", i), Sessions: rng.IntN(4)}
		if i%3 != 0 {
			tokens := int64(rng.IntN(5))
			rows[i].Tokens = &tokens
		}
		if i%4 != 0 {
			usd := float64(rng.IntN(3))
			rows[i].Cost = Cost{USD: &usd, Partial: i%2 == 0}
		}
	}
	full := append([]Project(nil), rows...)
	sort.Slice(full, func(i, j int) bool { return projectBefore(full[i], full[j]) })
	for _, limit := range []int{1, 2, 5, 50, 999, 1000, 1001} {
		for range 10 {
			rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
			h := make(projectHeap, 0, min(limit, len(rows)))
			for _, row := range rows {
				h.keep(row, limit)
			}
			sort.Slice(h, func(i, j int) bool { return projectBefore(h[i], h[j]) })
			if !reflect.DeepEqual([]Project(h), full[:min(limit, len(full))]) {
				t.Fatalf("top %d differs from full sort", limit)
			}
		}
	}
}
