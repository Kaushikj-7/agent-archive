//go:build race

package backfill

// raceEnabled lets a test whose real-size fixture is too slow under the race
// detector skip there; the plain build still runs it.
const raceEnabled = true
