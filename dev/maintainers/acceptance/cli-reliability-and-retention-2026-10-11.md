# CLI reliability and keep forever implementation evidence

Recorded 2026-10-11 against `b4122ea6694dbe679783b724b6adbfdc11047c0d`.
A–G of the [implementation plan](../../proposals/cli-reliability-and-retention.md)
are present on main. This is an implementation and CI record, not release
sign-off. Baseline findings in the plan are historical. Test names below identify
existing regression evidence; inspection of a test is not a new execution result.

| Scope | Owning implementation | Regression evidence |
| --- | --- | --- |
| A | [Purge](../../../internal/purge/purge.go), [cleanup recipes](../../../docs/security/archive-cleanup.md) | [History protection tests](../../../internal/purge/history_test.go): complete active/preserved set, invalid references and references added after planning; [recipe tests](../../../scripts/test_purge_recipe.py). |
| B/C | [CLI reliability](../../../internal/cli/reliability_test.go), [S3 reads](../../../internal/storage/s3.go) | CLI tests cover output failure/short writes, feedback FIFO/substitution/UTF-8, atomic private handoff files, early usage errors and cancellation; [read-budget tests](../../../internal/storage/read_budget_test.go) cover stalled headers/body, interruption and independent page budgets. |
| D | [Retention helpers](../../../internal/config/retention.go), [wire protection](../../../internal/config/wire.go) | [Retention tests](../../../internal/config/retention_test.go), [composed staging-fence tests](../../../internal/config/import_staging_test.go), [published-writer tests](../../../internal/config/published_writer_test.go). |
| E | [Setup/pairing/retention tests](../../../internal/cli/keep_forever_test.go), [pairing tests](../../../internal/pairing/retention_test.go) | Fresh/repeated setup, saved preference, reporting, early flag validation and fenced pairing; [retention lifecycle](../../../internal/retention/retention_test.go) and [backfill](../../../internal/backfill/commit.go) retain explicit deletion and infinity-aware ordering. |
| F | [Revision reader](../../../internal/reader/revisions.go), [CLI selection](../../../internal/cli/revisions.go) | [Reader tests](../../../internal/reader/revisions_test.go), [CLI tests](../../../internal/cli/revisions_test.go): exact referenced UUIDs, provenance, unknown/malformed selection, parent-only consumers and unchanged default. |
| G | [Admission](../../../internal/backfill/register.go), [durable staging](../../../internal/state/import_stage.go), [publication](../../../internal/collector/import_stage.go) | [CLI staging tests](../../../internal/cli/backfill_staging_test.go) cover native deletion after admission, background recovery and dry-run; collector/backfill/state suites cover crash boundaries, privacy refilter, quota, cancellation and read-back before stage release. |

Merge evidence queried from GitHub on 2026-10-11: #423 (`59e9f46f`),
#424 (`851158b5`), #425 (revision selection), #427 (`f49c49ae`), #428 (`6e68662a`) and #426 (`db2ccd3e`)
are merged. Revision implementation `3f4f1aed` and staging implementation
`7e417c9b`, with follow-ups through `930480fb`, are in the inspected ancestry.
Historical open PRs such as #356–#362 and #352 have different live heads; their
open status is not evidence that these integrated features are absent, nor that
those exact PR heads were merged.

## Exact-SHA CI

Read-only GitHub job queries confirmed the following results on 2026-10-11.
All three candidate runs identify
`db2ccd3ef1490970d9c8476041d8056608ed8c2f` as `headSha`.

| Run | Observed result |
| --- | --- |
| [Test 38158963055](https://github.com/wangjohn/agent-archive/actions/runs/38158963055) | Success: Linux full race/performance/vet/script checks, macOS smoke, lint, cross-build and Git identity matrix. Candidate lint's PR-only new-doc-comment step skipped. |
| [Levenshtein 38158963071](https://github.com/wangjohn/agent-archive/actions/runs/38158963071) | Success: `verify`. |
| [Extended 38158963056](https://github.com/wangjohn/agent-archive/actions/runs/38158963056) | Success: `macos-full`, `fuzz`, `real-systemd`. Duplicate `linux-race` skipped by candidate design; Test's `linux-race` succeeded on the same SHA. |

These results establish the named synthetic/source checks for that candidate.
They do not establish final documentation-head readiness. At this record's
initial inspection, main `b4122ea6` had successful
[Levenshtein 38175358328](https://github.com/wangjohn/agent-archive/actions/runs/38175358328)
and [Extended 38175358295](https://github.com/wangjohn/agent-archive/actions/runs/38175358295)
was still running. A main Extended run ordinarily skips fuzz/real-systemd;
never count a skipped job as a pass. Record fresh Test/Levenshtein/Extended
results for the final integrated SHA under the
[testing campaign](../../contributing/testing.md), retaining this historical
snapshot rather than transferring its verdict to a new head.

## Completion-audit execution limits

Local focused documentation checks on the reconciliation passed with Go 1.27.2:
`internal/doclinks`, `TestCLIReferenceIsCurrent` and
`TestDocsQuoteOnlyRealCommandsAndFlags`. Focused A–C CLI/purge/storage and
D–E config/retention/pairing/state plus selected backfill retention/undo checks
were reported passing at the pinned base. Selected F–G reader/backfill/state/
collector/CLI revision, staging, quota, cancellation and crash-recovery checks
also passed. These are narrower than a full combined race suite.

The initial local broad `internal/backfill` run at `b4122ea6` failed: recovery
fixtures inherited read-only synthetic Git ancestors under `/tmp` and
`/workspace`, and a `0077` umask disagreed with fixtures expecting `0644` files.
An independent repeat at the same code SHA passed the full package with
`-race -p 1 -count=1 -timeout 10m`, a private disposable
`TMPDIR=/var/tmp/cli-forever-retention-full-20261011`, and shell-local
`umask 022` (112.735 seconds). The synthetic ancestors were left unchanged;
no product or test patch was needed. Preserve the original failure separately
from that normalized pass. Focused independent source review found no supported
A–G implementation defects; that verdict and this package repeat do not replace
current-head required CI or live acceptance. Keep readiness pending while any
supported in-scope finding or validation gap remains unresolved.

## Outstanding acceptance

The [published-release sheet](../open-source-acceptance.md) and its
[live retention extension](cli-retention-live-protocol.md) own release sign-off.
All new-release live result cells remain pending. This environment has no
configured disposable S3/R2 credentials, native macOS app accounts or final
signed installer for the new features. It cannot run those checks. Native Linux
architecture/app acceptance is also not implied by cross-build or real-systemd
CI. The latest promoted release queried on 2026-10-11 is v0.1.1 with only
`agent-archive-darwin-amd64`, `agent-archive-darwin-arm64` and `SHA256SUMS`.

The completion review executed a Linux source-built pre-forever binary from
`20f86e5f33fde4c720f3ca3554f4a234828bd7a3` (Go 1.27.2, clean VCS build info,
SHA-256 `1a71da5793c37cd17c9c60d9b7927302b6c2e959cec20f515b1c607056097116`).
`TestPriorRetentionWriterRefusesForeverBeforeMutation` passed its finite and
supported discovery controls and refused forever, sticky finite and staged
forever without changing disposable state. The actual old receiver probe
`TestOldReceiverRejectsForeverBeforeSideEffects` passed both synthetic S3 and R2
payloads without creating receiver state. These `-race -p 1 -count=1` probes used
private temporary homes and service stubs; the writer needed an isolated
`/var/tmp` root outside this environment's injected Git ancestors.

The candidate macOS job's published-old-writer step succeeded; the source-built
checks above do not replace that published/native check or establish every
predecessor fence. Run the documented isolated compatibility probes with the
applicable published and retention-only predecessor binaries before claiming
those remaining matrix cells. Live provider and release cells remain pending.
Do not use an old binary on a live archive or replace absent signed-artifact,
provider, app or platform evidence with unit-test passes.
