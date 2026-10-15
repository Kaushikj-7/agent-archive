# Fixture setup receipts

Expensive fixture constructors emit one `CI_FIXTURE_METRIC_V1` JSON receipt per
non-overlapping phase through `testing.TB.Logf`. Go's JSON output supplies the
package and exact test identity. The receipt contains `version`, `fixture`,
`phase`, `elapsed_ns`, and integer `operations`. It never contains metadata,
object keys, fixture bodies, paths, or credentials.

End a phase before invoking another instrumented constructor. Do not emit both
an enclosing total and its child phases. Durations are wall time: parallel test
receipts are not additive, and a process-seed initializer is charged only to its
actual first caller. These measurements explain setup costs; do not subtract
arbitrary fixture durations from overlapping parent-test lifecycles.

Record operation counts at their actual call sites, including explicit zeroes
when a budgeted operation does no work. `Start` accepts optional per-construction
operation ceilings. `Finish` fails the test on an exceeded ceiling or a missing
budget measurement. For example, a publication constructor permits at most one
real commit per input metadata row; it still publishes every row. Constructors
and private-store clones permit one construction or clone per invocation,
counted at the call site. The storage ownership contract separately verifies
that snapshot clones reuse immutable bodies without copying them. These local
ceilings do not approve larger fixture universes or replace the CI gate's trusted
admission policy and reviewed baseline.

The catalog reader seed still exercises the real publication and complete
upgrade once per process. Its frozen `MemorySnapshot` retains immutable bodies
and exact object versions. Every caller receives a private map, writable store,
writer, decoded metadata, snapshot authority, node cache, and lifetime. Avoid
reconstructing immutable object bodies with GET/PUT loops. The qualification
fixture's existing complete-metadata cache identity and 1,400-row retention cap
remain authoritative; large one-use fixtures are not retained.
