# Muse local handoff

The local reader supports automatic `handoff` discovery for ordinary supported
exec/MSP roots. It uses existing source passes, bounded filesystem discovery and
preview budgets, and the strict whole-file source filter before producing an
artifact or launching an existing destination. It creates no registration,
consent or capture configuration. Native show/search and launch into Muse remain
unavailable.

The command injects one absolute effective XDG data base into `NativeLocations`.
Trimmed nonempty `XDG_DATA_HOME` takes precedence; a relative value resolves
against the injected absolute working directory. Unset or whitespace-only XDG
uses the injected qualified HOME's `.local/share`. Invalid or unqualified values
provide no Muse root; there is no search of both locations or inference from
hook configuration. Muse appends `muse/sessions`, then recognizes exact ordinary
`YYYY/MM/DD/SESSION_ID/session.jsonl` locators. Dates do not establish birth.

This precedence follows the pinned macOS 1.4.9 resolver and three isolated local
functional observations with absolute XDG, unset XDG plus explicit private HOME,
and relative XDG plus explicit CWD. Unicode trimming is static evidence plus
synthetic integration tests. These observations used private environments and a
fake loopback provider; no external OS sandbox or independent global access
or write audit was performed. They do not establish broader platform acceptance.
No additional native process is needed for the production tests, which use
synthetic fixtures only.

Headers retain at most eight projected startup records, sharing the existing
permission-frame checksum/profile, envelope and strict root-prefix grammar.
They corroborate native identity, workspace and reserved logical start, and
require parent-folder identity agreement. The Scan interface normalizes/skips
records and supplies no physical line ordinal. Header facts therefore cannot
certify raw physical first-line grammar. The selected complete source must
still reject leading blank/whitespace, unknown suffixes and unsupported history.
An oversized record larger than the entire header window cannot leave room for
a later startup prefix: that census stays incomplete.

The stateless preview shows qualified automatic generated names, activity or
native IDs. It applies shared privacy, rejects cross-owner names and treats
unsupported schemas/source enums as partial. It never labels an isolated
run-start prompt as human conversation. Full selected interpretation continues
through the retained parser's accepted/start/materialized causal joins.

The existing limits remain 10,000 qualifying files, 256 KiB header/record windows,
64 MiB cumulative header/preview bytes, two workers and 50 labels per explicit
batch. Whole-directory `ReadDir` acquisition remains a limitation of the current
browse path, not qualified bounded capture traversal. Large synthetic inventories
may exhaust the byte budget and report incomplete discovery. Benchmarks report
actual reads, opens, allocations and verified IDs without latency promises or
increased caps. A real injected Filter counter distinguishes census/preview
zero full filters from selected-source exactly one full filter.

Import capability remains separate: the existing selected-discovery projection
limits backfill/project/recovery discovery to providers with real Imports ports.
Explicit Muse or muse-code backfill refuses before home/config/journal/storage
access. Existing stale Cursor-snapshot housekeeping predates argument validation;
this refusal does not claim zero process-wide I/O. Capture remains refused before
native reads/filtering/PUT even for injected registrations.

Selected handoff opens one additional confined verification snapshot on the same
serial source pass after analysis. It compares pathname identity, size and time
with the filtered descriptor, checks both snapshots and closes verification
without reading transcript bytes. This is a point-in-time check, not an atomic
guarantee against a later external pathname change.
