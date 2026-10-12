# Archive listing at scale: implementation plan

Prepared 2026-09-30 against `main` at `dc443c7`. Status: phases 1
and 2 implemented; phase 3 partially covered; phase 4 partially implemented.
Reconciled 2026-10-11 at `db2ccd3e`. The current
[listing contract](../maintainers/list-performance.md) specifies indexed bounded
metadata reads (#320), with 10k/20k correctness and operation-count coverage.
`internal/reader/list_index_bench_test.go` has a 10k benchmark; the full original
1k/10k/100k cold/warm measurement matrix is not recorded. Fresh LIST header
enumeration still grows with archive size, so the original constant-cost index
goal remains open. The phase designs below preserve the original plan.
Each phase lands before the next one starts. Each phase's PR updates its
status line below.

| Phase | What | Status |
| --- | --- | --- |
| 1 | Parallel range listing | Implemented in [#219](https://github.com/wangjohn/agent-archive/pull/219) |
| 2 | `AGENT_ARCHIVE_TRACE` timings | Implemented in [#225](https://github.com/wangjohn/agent-archive/pull/225) |
| 3 | Listing benchmark at 1k, 10k and 100k sessions | Partial: 10k benchmark exists; full measurement matrix pending. |
| 4 | Index read whose cost doesn't grow with the archive (spec first) | Partial: bounded metadata bodies implemented; proportional LIST headers remain. |

## Problem

The handoff picker (`agent-archive handoff` with no arguments) takes 1.4–2.1 s
to show anything on a real archive of 662 sessions in Cloudflare R2.
Measured 2026-09-30 with the installed binary, routing its TLS traffic
through a local proxy that timestamps bytes:

| Step | Time |
| --- | --- |
| Startup, config, Keychain, 662 registrations | ~35 ms |
| SDK setup, TCP and TLS to R2 | ~70 ms |
| `ListObjectsV2` page 1 (1,000 keys, 325 KB) | 470–900 ms |
| Page 2 (1,000 keys) | 470–540 ms |
| Page 3 (~350 keys) | 150–285 ms |
| Re-reading changed sidecars (some runs) | +~185 ms, parallel |
| Decoding 662 cached sidecars | 13 ms |
| Merging local sessions, formatting 50 rows | <1 ms |

The time is almost all `store.List("sessions")` inside
`reader.ListMetadataWithOptions`. It pages through every object below
`sessions/`, one page after another, before it reads anything. Only 662 of the
~2,350 listed keys are metadata sidecars. The rest are source bundles: the
current ones, plus ~1,020 superseded snapshots still inside their grace
period. R2 returns about 1,000 keys per 0.5–0.9 s, so the wait grows with the
total object count, whatever the picker shows. The same scan serves
`handoff "<query>"`, `handoff --latest` when nothing matches locally,
`list --limit 0`, `stats`, and bare `show`. `list` with a limit would use the
[listing index](../maintainers/list-performance.md), but this bucket has never
been marked ready (`listing/v1-ready`), so it falls back to the same scan.

Projected with ~3.5 keys per session:

| Sessions | Keys | Sequential scan | Phase 1 (16 ranges in flight) |
| --- | --- | --- | --- |
| 662 | ~2,350 | ~1.5 s (measured) | ~0.3 s |
| 10,000 | ~35,000 | ~17 s | ~1.5 s |
| 100,000 | ~350,000 | ~3 min | ~11 s |

Parallel listing is the right fix now, but any full scan is linear in the
archive. The picker needs only the newest top-level sessions, so the lasting
fix (phase 4) is a read whose cost depends on what is shown, not on how
much is stored.

## Scope

In scope: how the archive is listed and read for `handoff`, `list`, `show` and
`stats`; measuring that; and the index those reads use.

Out of scope: which sessions the picker shows and how they are labelled.
That includes hiding subagents, native titles, repo scoping, filtering, and
the search matcher. That work is the [session-finding plan](../specs/session-finding.md).
Phase 4 below lists what it needs from the listing.

## Phase 1: parallel range listing

Split one full listing into contiguous key ranges and list them concurrently.
It is one PR: the storage interface is dead code without its caller, and CI's
`deadcode` check fails on that.

- **`storage.RangeLister`** (optional, like `PageLister`):
  `ListRange(ctx, prefix, after, through)` returns the objects under `prefix`
  whose keys are greater than `after` (unless empty) and at most `through`
  (unless empty), in key order. `S3Store` implements it with
  `ListObjectsV2`'s `StartAfter` and stops paging once it passes `through`.
  `storagetest.MemoryStore` implements it with the same prefix rule as its
  `List`.
- **Ranges cover every key.** The boundaries b₁ < … < bₙ split the key space
  into (−∞, b₁], (b₁, b₂], …, (bₙ, +∞). Every key falls in exactly one range,
  whatever its shape: unknown harnesses, IDs that aren't hex. How boundaries
  are chosen affects only balance, never which sessions are found.
- **Boundaries come from the metadata cache.** The cache already names every
  sidecar the last listing saw. Every 200th cached key under the listing
  prefix becomes a boundary. At ~3.5 keys per session that is about 700 keys,
  one page per range. With fewer than two ranges' worth cached (a new
  archive, a cold cache, `--no-cache`), the listing is the single sequential
  one it is today, so small archives make no extra requests.
- **Concurrency:** at most 16 ranges in flight. Results are joined in range
  order, so the listing is in key order exactly as before. The first failing
  range in key order is the error returned. A failure cancels the ranges
  still running.
- **Unchanged:** sidecar reads, cache use and eviction, skipped-sidecar
  reporting, filtering and sort order. Stores without `RangeLister` keep
  using `List`.
- **Tests:**
  - For random key sets (including non-hex IDs, other harnesses, empty and
    single-key ranges, boundaries equal to keys), the joined ranges equal
    `List` exactly.
  - The concurrency bound holds; errors and cancellation propagate.
  - `S3Store.ListRange` sends `start-after` and stops paging, checked against
    a fake S3 endpoint.
- **Live check:** confirm R2 honours `start-after`, then repeat the proxy
  measurement. Target: the picker in ~0.3–0.4 s on the 662-session archive.
- **Cost:** a 662-session listing becomes ~4 requests instead of 3; at
  10,000 sessions about 50 instead of 35. R2 bills listing as Class A
  operations (~$4.50 per million).

Size: ~150 lines of production code, ~300 of tests. One PR.

## Phase 2: `AGENT_ARCHIVE_TRACE`

With `AGENT_ARCHIVE_TRACE=1`, a command prints a timing tree to stderr when it
finishes. It covers config, store open (Keychain included), registrations,
the listing (ranges and keys), sidecar reads (from cache or downloaded), the
indexed listing, local activity, local transcript reads, source loads, and
every storage request with its bytes. Measuring then no longer needs a proxy
and a pseudo-terminal.

- **Process-wide recorder** (`internal/trace`). Library packages record
  spans without a context threaded through every command. So no command's
  code changes, including the handoff picker, which the session-finding work
  owns.
- **Nesting:**
  - spans from different packages nest by time (the innermost span that
    contains them);
  - concurrent work (listing ranges) is attached to its parent explicitly;
  - same-named siblings fold into one line with a count, the time they
    spanned (first start to last end, not the sum), and the longest;
  - a `spinner (until first draw)` span times the loading before a command
    shows anything, apart from the time a person spends in the picker.
- **Requests:** a wrapper on the S3 client's HTTP client, always installed
  (a pointer check while tracing is off). Each request's span lasts until
  its body is closed, so a listing page's download counts.
- **Privacy:** names are fixed strings and details are counts, never keys,
  titles or paths. A test asserts that.
- **Internal commands never trace:** the hooks and the collector (`_hook`,
  `_collect`).
- **Docs:** the troubleshooting guide and the configuration reference.

Size: ~250 lines of production code, ~200 of tests. One PR.

## Phase 3: listing benchmark

A benchmark in `internal/reader` against a store wrapper that behaves like R2
as measured above: ~100 ms per request plus ~0.45 ms per listed key, pages of
1,000. It sleeps at a reduced time scale and reports modeled time. Fixtures:
1k, 10k and 100k sessions, 85% of them subagents, ~1.5 superseded snapshots
per session. For each fixture it measures:

- the sequential full scan
- the phase 1 range listing, cold and warm cache
- the current index read (`ListRecent`), which reads sidecars one at a time

It reports modeled latency, requests, and local decode time. At 100k sessions
the one-file-per-sidecar cache is itself measurable. The 10k and 100k runs
are gated behind `AGENT_ARCHIVE_PERF=1`, like the other full-size
performance tests. The results are recorded in
[list-performance.md](../maintainers/list-performance.md) and become phase 4's
baseline.

Size: ~300 lines, test code only. One PR.

## Phase 4: reads whose cost doesn't grow with the archive (spec first)

Starts after phases 1–3 merge and after the session-finding picker changes
land, since it changes where the picker gets its rows. The first PR is a
spec in `dev/specs`. The build follows in PRs sized by that spec, expected to
be 4–5. The spec must decide:

1. **Index readiness without a manual step.** The collector runs
   `RebuildIndex` once, in the background, on a destination marked as
   needing it, rather than waiting for someone to run `list --rebuild-index`.
2. **Top-level and subagent hints kept apart.** Browsing never pages through
   subagent hints (84% of sessions on the probe archive). Subagent hints are
   grouped by parent, so one parent's children are one listing.
3. **Sidecars served by content hash.** A hint's key already carries its
   sidecar's SHA-256. A cached copy with that hash is valid with no request.
   Misses are fetched in parallel, unlike today's one-at-a-time `ListRecent`
   reads, which cost ~100 ms each on R2.
4. **Superseded hints removed on republish.** Every publication writes a new
   immutable hint, and today nothing removes the previous one while the
   session lives. A busy session can fill the newest page with its own old
   revisions. The publisher deletes the previous hint through the existing
   `listing/by-session` pointer.
5. **Republish bursts.** Filter 13 (native titles, PR links, Cursor names) and
   filter 14 (subagent descriptions) each republish every session whose
   transcript still exists. That is a new sidecar and a new hint per session,
   twice. The spec must keep the index correct and the first listings after
   each one reasonable.
6. **The target:** the picker in one listing request (up to 1,000 hints) plus
   cache hits, about 0.2–0.3 s at any archive size.
7. **Staleness.** Hints are not the authority. Deletion removes the metadata
   first, then the hints, so a stale hint survives only an interrupted
   deletion. Handoff re-reads the chosen session before using it. The spec
   must say what each command verifies before it shows a row.

What the [session-finding plan](../specs/session-finding.md) needs from phase 4:

- **(a) Subagent count per parent,** from the parent's `LinkedSessions`, with
  no scan of subagent hints.
- **(b) One parent's children on demand,** in a single listing, for search
  results and a later drill-in.
- **(c) A scoped window of recent top-level sessions,** keyed on `RepoKey`,
  falling back to `ProjectID` when `RepoKey` is empty (older sessions,
  repositories without an origin).
  - **Options:** filter the newest window and page on until enough rows
    match, or write a second hint per scope, which doubles index writes. The
    spec weighs the two.
- **(d) An unscoped window of recent top-level sessions,** for the picker's
  "all projects" toggle.
- **(e) Search stays exhaustive.** The shared matcher (the picker's `/`,
  `handoff "<words>"`, `list "<words>"`) reads name, title, branch,
  `pull_requests[].number`, the `pr_number` of `git_activity`'s
  `pr_created` events, `project_name` (else `project_id`, for a configured
  project label), harness, session ID and `parent_session_id`. `name`,
  `branch` and `pull_requests` are new optional fields in parser 0.17.0. Search keeps using the phase 1 full scan or the
  local cache. If an index ever carries search data, it must carry all of
  these fields.

Also to consider in the spec: at 100k sessions the one-file-per-sidecar
metadata cache costs seconds to read and decode. Packing it into one file
matters for full scans (search), not for the windowed picker.

Not planned: moving sidecars to their own prefix. It would cut a full scan
~3.5× but needs a storage layout migration, and phase 4 removes the full
scan from the picker anyway.
