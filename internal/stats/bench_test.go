package stats

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// A long-time user's archive: 50,000 sessions. Compute must stay a fraction of
// a second and allocate in proportion to the sessions, since the command runs
// it on every `stats`.
func BenchmarkCompute50kSessions(b *testing.B) {
	sessions, _ := randomArchive(rand.New(rand.NewPCG(7, 7)), 50_000)
	o := Options{Now: time.Date(2026, time.September, 20, 12, 0, 0, 0, newYork), Location: newYork, Days: 400, By: GroupWeek}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		Compute(sessions, o)
	}
}

// The worst case for ranking projects: every session in a project of its own,
// with a spend of its own, so 50,000 projects are ranked (by spend, then the
// tie-breaks) and cut. The list of every project and the default top few cost
// the same to rank.
func BenchmarkCompute50kProjects(b *testing.B) {
	rng := rand.New(rand.NewPCG(7, 7))
	sessions := make([]archive.Metadata, 0, 50_000)
	for i := range 50_000 {
		at := time.Date(2026, time.September, 1+rng.IntN(19), rng.IntN(24), rng.IntN(60), 0, 0, newYork)
		sessions = append(sessions, meta(fmt.Sprintf("s%05d", i), "claude", at, project(fmt.Sprintf("project-%05d", rng.IntN(1_000_000))),
			modelTokens("claude-opus-5-5", rng.IntN(5000), rng.IntN(1000), rng.IntN(5000), rng.IntN(500))))
	}
	for _, allRows := range []bool{false, true} {
		b.Run(fmt.Sprintf("allRows=%v", allRows), func(b *testing.B) {
			o := Options{Now: time.Date(2026, time.September, 20, 12, 0, 0, 0, newYork), Location: newYork, Days: 400, AllRows: allRows, By: GroupProject}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				Compute(sessions, o)
			}
		})
	}
}

// The original accounting and prepared path use the same archive and windows;
// preparation is measured separately and included in the one-shot benchmark.
func BenchmarkPrepared50kSessions(b *testing.B) {
	sessions, _ := randomArchive(rand.New(rand.NewPCG(7, 7)), 50_000)
	opts := Options{Now: time.Date(2026, time.September, 20, 12, 0, 0, 0, newYork), Location: newYork, Days: 400, By: GroupWeek}
	b.Run("original", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			legacyCompute(sessions, opts)
		}
	})
	b.Run("prepare", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			Prepare(sessions, PrepareOptions{Location: newYork})
		}
	})
	b.Run("window", func(b *testing.B) {
		p := Prepare(sessions, PrepareOptions{Location: newYork})
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			p.Compute(opts)
		}
	})
	for _, prepared := range []bool{false, true} {
		b.Run(fmt.Sprintf("repeatedWindows/prepared=%v", prepared), func(b *testing.B) {
			p := Prepare(sessions, PrepareOptions{Location: newYork})
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				for _, days := range []int{7, 30, 90} {
					opts.Days = days
					if prepared {
						p.Compute(opts)
					} else {
						legacyCompute(sessions, opts)
					}
				}
			}
		})
	}
}
