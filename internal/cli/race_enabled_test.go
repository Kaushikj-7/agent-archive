//go:build race

package cli

// raceEnabled lets a full-scale test hand its body to the plain restoration
// run: the race detector multiplies its quadratic durable-quota rescans past
// the package timeout without changing what the body proves.
const raceEnabled = true
