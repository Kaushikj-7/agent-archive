# Changelog

All notable changes are recorded here, by what they mean for you. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

- Capture Codex threads the Codex app opens for a task another thread's agent
  handed off (`thread_source: "agent_created_thread"`). They are top-level
  threads, named and listed by Codex like your own, and were failing on every
  pass with "source unavailable". Rollouts already seen are read again once.
- Follow a Codex rollout that Codex moved into `archived_sessions/` (or back)
  by its thread ID: the session's recorded location moves with it instead of
  leaving a "transcript missing" gap, and unchanged content is not uploaded
  again. This also works while Codex's thread index still names the old
  location.
- Add an explicit Keep forever choice in setup and `--keep-forever` for
  scripted setup; new installations still default to 90 days. Pairing and
  imports preserve this per-machine preference. It disables automatic
  whole-session expiry, while privacy cleanup, explicit deletion and provider
  lifecycle rules still apply. Choosing it installs a permanent compatibility
  fence: older binaries refuse the configuration, even after switching back
  to finite retention. Upgrade installed hooks and collectors before using it.
- Report failed or short JSON output writes, including status and normalized
  transcripts. Reject nonregular or invalid UTF-8 feedback input without
  blocking on named pipes. Publish handoff files privately and atomically,
  preserving existing files on errors and refusing symlink targets.
- Reject invalid purge and machine-listing flags before setup or credentials.
  Remote object reads and listing pages have a two-minute budget covering
  retries and stalled bodies; interactive selection and whole archive scans
  have no shared deadline. Interrupts cancel command reads while the pager
  retains its own signal behavior.
- Protect active and preserved revision sources during purge and selective archive
  cleanup. Invalid or missing history references block deletion; old-filter live
  evidence must be replaced through verified privacy publication first.
- Stop reporting Codex sessions whose history coverage is not complete yet,
  or whose source changed while being read, as failed: `sync` counts them by
  reason ("8 waiting for Codex history coverage") and `status` lists them as
  rechecking, while real failures are still listed. `backfill history` counts
  as waiting only sessions not in the archive yet, and counts uploaded ones
  with a re-scan or newer work owed apart (`uploaded · 2 rechecking`). A
  source error can now carry a fixed, content-free cause ("source unavailable:
  Codex history coverage incomplete"). A session still put off after 24
  hours of passes is reported as failed again, until a pass reads it.
- Keep `handoff` within `--max-bytes` for long sessions: once the existing
  trimming is not enough, older exchanges keep only their last agent message,
  then all tool output is dropped, then every exchange but the last is
  condensed to its prompt and counts. Markdown still over the limit because of
  one oversized part, or of several hundred exchanges, is cut at a line with
  a marker, keeping the last exchange.
- Clarify storage onboarding with Account, Storage, and Verification stages.
  Keep valid existing-bucket answers when going back, show S3 archive
  permissions on request, and report privacy separately from connectivity.
- Resume interrupted storage setup from durable resource receipts, prevent
  ambiguous duplicate creation, track manual bucket cleanup, and explicitly
  revoke unused guided keys while protecting active and draft credentials.

- Preserve private, durable bucket creation receipts across interrupted setup and
  abandoned drafts, including uncertain provider outcomes. Archive keys continue
  to use the existing issuance ledger.
- Show pairing delivery and receiver setup as separate progress stages. Closing
  source transfer instructions no longer implies receiver success. Cancellation
  and expiry explain bundle and credential effects, including local S3 profiles.

- Stop titling Codex desktop sessions with injected open-page context such as
  `<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`.
  Sessions captured before that context was filtered now take their title from
  the first real prompt when their metadata refreshes.

- Preserve settled capture and source bytes when cached native naming reads are
  unavailable and the same observation uses an equivalent timezone offset.

- Keep listing revision path components within 255 bytes so capture and index
  rebuild work with filesystem-backed S3 stores such as MinIO. Existing v2/v3
  hints remain readable and are retired by repair or rebuild.

- Add explicit `codex_name_lookup: "native"` opt-in for bounded metadata-only
  Codex naming requests, with guarded file fallback. Files remain the default.
  Native startup can write Codex state; desktop coexistence is unverified.

- Capture Claude Code native generated session names and prefer the latest
  explicit rename. Validate title ownership, preserve partial preview coverage,
  and keep naming bookkeeping out of activity and capture eligibility.
- Remove injected Codex open-page context from prompt fallback while preserving
  the human request outside it.
- Refresh Codex names for already admitted ordinary sessions from verified
  0.159.2 settled storage, including verified legacy database/index fallback. Changes publish filtered
  source evidence without renewing activity or retention; unavailable live-WAL
  storage preserves the last verified name. Deferred lookup targets rotate within
  prioritized homes even after backoff expires. Names that cannot remain stable
  through privacy filtering preserve the last verified name. The default files
  mode starts no native process.


### Added

- List retained native revision IDs with `show SESSION_ID --revisions`, including
  capture times and recorded provenance in human or JSON output. Select one exact
  parent revision with `--revision` in show, archive-only handoff, or a single
  full-detail eval export. Selected summaries and provenance come from verified
  retained evidence; unknown IDs and invalid history fail without an active
  fallback. Selection excludes children, and retained history remains bounded.

- Capture native Codex children without a stop hook or archived parent. Children
  keep their own identity, creation, task evidence, consent and activity. Historical
  imports validate the same native source facts; nested children belong to the new
  import batch, and undo leaves a parent from an earlier batch in place. Backfill
  previews remain read-only and report pending native evidence accurately. Earlier
  captures with a persisted native child binding upgrade their retained ownership
  headers before linking a parent, including preserved revisions.

- An explicitly requested Codex metadata inventory shares the existing discovery
  owner, retains active and archived physical copies and validates bounded
  slices with live native-current checks. Pending-history diagnostics preserve
  the original refusal; ordinary capture keeps its requested lookup lane.
  Conflicting logical or pagination identities for one physical rollout leave
  the inventory incomplete, including UUID spelling variants. Lazy acquisition
  also refuses aliases whose canonical roots changed since owner construction.

- Pairing, handoff, standalone history import and undo, uninstall and purge
  use grouped questions and safe completion receipts. Secondary actions use
  bracketed shortcuts. Destructive defaults, exact purge digest confirmation,
  private pairing code display and structured output contracts are preserved.
  Human machine listings align fields and switch to labeled rows at narrow
  widths; recovery previews group their consequences.

- Setup offers All found projects as the visible default in one shared selector,
  including projects beyond the first page, with bounded discovery coverage,
  observed session counts, Specific selection and preserved capture rules.
  Storage prompts and diagnostics use grouped questions and receipts. A compact
  review keeps essential changes visible with complete settings behind Details.
  Setup completion, app next steps and optional history import are separate;
  machine-transfer recipes are available on request.

- Setup groups the storage provider question and R2 secret fields, replacing
  answered prompts with short receipts when terminal rows can be safely owned.
  `NO_COLOR` keeps live interaction; dumb and redirected terminals use static
  prompts. Hidden fields share buffered input and restore terminal modes on
  EOF, interrupts and suspend/resume. Answers entered while a prompt is being
  written retain safe static history when their echoed rows cannot be proven.

- Readers understand self-contained Codex fork, child and revised histories,
  preserve exact raw ordinal boundaries and count only the chat's own activity.
  Related-history writes and cleanup remain protected pending revision-aware
  lifecycle support. Active captures can finish while a chat keeps appending.
  Concurrent header rewrites defer capture for retry, including ordinary
  admission headers and dependencies with empty captured prefixes.

- Make machine pairing, dedicated R2 keys, provider verification, revocation,
  and guided R2 bucket creation generally available without experimental flags.
  First setup offers pairing from another machine.

- Complete non-live experimental pairing/revocation handling: committed retired
  dedicated-key history, healthy issuer labels and private operator bindings,
  unused spare observations and bounded informational operation progress.
  Pairing preserves standalone exclusions, offers safe scope/delivery/token/app
  recovery and reports success before post-commit cleanup and history import.
  Both peers must be upgraded for standalone exclusions; additional live provider and platform
  coverage remains tracked for release testing.

### Fixed

- Preserve confirmed backfill evidence before registering sessions, including
  selected Codex history and native children. Imports can finish after native
  files disappear; background recovery verifies remote sources and metadata
  before reporting completion. Private filtered staging has a shared 1 GiB
  quota, resumable admission, current-policy filtering, and journalled cancel
  and undo cleanup. History distinguishes staged/admitted from remotely verified
  sessions. Staging rechecks native identity before admission. Dry-run creates
  no staging and reports incomplete JSON writes as failures. Oversized selected
  sources and encoded journals fail before admission, within the collector's
  recovery memory allowance.
  Imports also finish skipped candidates promptly when their project proof or
  authorization changes after identity reservation and before staging.

- Validate a Codex fork's first own task during import and ongoing capture,
  and protect every qualified history import from older writers. Discovery
  stops rereading decisively rejected tasks until their native source changes.
  Undo retention warnings count independent native children with resolved parents.
- Pending publication and historical source staging share a bounded durable
  storage budget. Interrupted or unregistered source stages remain visible as
  recovery work; automatic cleanup retains original evidence. A sticky storage
  writer fence makes older writers refuse these protected configurations.

- Codex current-history validation uses the remaining bounded pass allowance
  for private SQLite queries, avoiding spurious retries after a scheduling delay
  while retaining caller cancellation and native source identity checks.

- First-time backfill can review live repository destinations and recover matching
  deleted worktree sessions in the same import. Hidden clones and unknown or
  excluded roots still prevent automatic attribution; proposed capture projects
  are committed only after confirmation and current-evidence validation.
  Destination witnesses must pass full native validation, including filtered or
  already archived sources; unknown file ownership keeps recovery pending.
  Collector waits retain their existing two-minute allowance before confirmation
  validation starts.

- Deleted-worktree project recovery supports Git installations without optional
  configuration-path introspection, including Git 2.39.2. Bounded semantic
  revalidation preserves clone ambiguity and exclusions, and larger Git config
  inventories no longer consume the short remote-URL output limit.

- Backfill explains deleted-worktree recovery failures with bounded diagnostic
  counts and retry/review actions while preserving existing skip codes. Dry-run
  JSON adds optional diagnostics and physical candidate inventory counts;
  logical history accounting remains pending.

- Pairing accepts relative filenames beginning with `aa-pair`, including existing
  files named `aa-pair1:backup.txt`, instead of mistaking them for pasted pairing text.

- Machine pairing guides file or clipboard transfer, prints a receiver command
  using the transferred filename, and shows the code after transfer. Receivers
  accept file paths or pasted pairing text; spare preparation waits until delivery
  finishes.

- Deleted Codex worktrees can recover an existing configured project from a
  unique recorded repository identity, or an exact historical backfill mapping.
  Excluded clones, ambiguous scope and damaged live checkouts remain pending;
  recovery preserves original creation consent and retained ownership.
  Backfill and automatic discovery retry a source rewritten or replaced during
  header inspection or repository recovery instead of admitting earlier project
  facts. Cached discovery facts for missing directories are reprobed before
  admitting new ownership, including headers without recorded repository keys.
  Configured subdirectory identities invalidate when a nearer repository appears.

- CLI help makes machine pairing discoverable and separates pairing instructions
  from machine-listing options.

- Status distinguishes installed hooks from observed execution and shows Codex
  automatic capture with archived and verified session counts. Optional hook
  evidence and detailed scan diagnostics move to verbose output; capture warnings
  and required hook approval instructions stay visible in normal status.

- Pairing recovery preserves exclusions on other clones and accepts home-prefixed
  manual project paths. Retired keys with exact local deletion confirmations no
  longer block revoking a replacement key after re-pairing; unverified absence
  remains unresolved.

- Codex metadata distinguishes stable threads, root conversations, immediate
  parents, forks and physical rollout segments. Valid child, fork and revised
  histories report that capture support is pending instead of conflicting IDs;
  incomplete related histories are not imported or newly captured.
- Backfill reports a transcript that changed during reading as retryable,
  rather than an unsafe format, while continuing other imports.

## [0.2.0]

### Documentation

- Documentation provides a v0.2.0 quickstart for macOS and Linux,
  explains conversation-only handoff and Codex all-project capture consent,
  and moves the shell cleanup recipe into a focused archive-cleanup guide.

### Fixed

- Automatic Codex discovery admits compatible legacy and paginated JSONL formats
  across versions and prereleases, including 0.155, without a producer-version
  allowlist. Status distinguishes runtime-tested, source-inspected and untested
  compatible session metadata. Creation consent, inherited-history exclusions,
  filtering and publication/read-back checks remain required.

- After local session membership changes, the collector rebuilds a completed
  packed identity index against the current registrations so recovery status
  can return to complete. Status remains unknown until recovery restarts, then
  pending until the authoritative rebuild finishes. Large or frequently changing
  inventories can require multiple sync passes.

- Listing cache lookup selects the current opaque metadata revision before
  opening a body, preserving the combined read limit after normal refreshes.

- Default listing uses session-addressed immutable revision hints to keep
  concurrent cleanup discoverable. Existing v2 entries remain readable;
  `list --rebuild-index` reclaims legacy pointerless hints with bounded cleanup.
  Malformed legacy pointers cannot claim or delete v3 hints.
- Status recognizes valid packed identity-recovery scheduling evidence and
  distinguishes shard application and fallback work without scanning the index.
  Damaged or stale scheduling evidence reports unknown status.

- Existing project-scoped hook sessions decline continuation updates when the
  incoming checkout or a saved scope rule has unknown filesystem identity,
  preserving their transcript locator and lifecycle timestamps.

- The bucket cleanup recipe works after local integrations or configuration
  have been removed. An explicit ordinary uninstall check stops local uploads
  before planning full-prefix deletion; failed stop checks invalidate prior plans.

- Project capture scope resolves symlinked checkout paths even when a nested
  directory is absent, preserving nested exclusions and reinclusions. Existing
  components with another casing or canonically equivalent Unicode spelling
  retain those rules on volumes that treat them as one location. Capture refuses
  unresolved symlink identities and ambiguous absent case or Unicode normalization
  variants instead of falling through to an included ancestor.

- Before-setup handoff launch files use a private temporary namespace per user,
  so users sharing a temporary directory do not block one another. Reuse and
  seven-day cleanup check ownership as well as permissions and reject symlinks.

### Added

- Installers point to `agent-archive backfill` for importing older sessions.
  Installation docs include preview guidance and explain that upgrades keep
  your capture settings.

- Codex setup offers included-project or all-current-and-future-project scope,
  independent of supported-source discovery. Fresh scripts explicitly choose
  both source and scope; omitted reconfiguration preserves consent. Codex-only
  all-mode permits zero explicitly included projects. Discovery does not require hooks;
  existing installations opt in explicitly. Original creation-time consent, pause
  intervals and project/destination scope apply. Indistinguishable recent native
  copies may qualify. Status schema 4 separates discovery health and actual hook
  observation from publication/read-back verification.

- `agent-archive recover SESSION_ID` previews a linked generation for a
  rewritten transcript; `--confirm` preserves the earlier archive and queues
  current activity under a new ID. Each generation keeps its own retention
  age. Recovery permanently requires a generation-aware writer. See the
  [guide](docs/guides/transcript-recovery.md).

- **`agent-archive eval export`** prints archived sessions for an evaluation
  tool: one JSON line per session, in a versioned format
  ([`schemas/eval-export.schema.json`](schemas/eval-export.schema.json)).
  `--detail metadata` reads only metadata sidecars and prints identity,
  commits (`git_head`), counts, tokens, tools, and outcome; `--detail full`
  (the default) adds every filtered human prompt in order, the final
  response, the edited files, and feedback. A session that cannot be
  exported is an error record on its own line and the rest still print.
  `--max-bytes` bounds each record. Read-only and never interactive. See the
  [guide](docs/guides/eval-export.md). Export failures omit private decoder
  details, reject mismatched session identities, and report failed output
  writes; size limits include escaped display controls and preserve whole
  UTF-8 characters at the text floor. Decoded sidecars with missing or null
  model attributes are rejected; model, tool and MCP name limits count
  Unicode characters, matching the published schema. Malformed HTTPS URLs
  in Git activity produce a `read_failed` error at either detail without
  exposing the invalid URL.
- `eval export` also works without setup, on this machine's transcripts:
  `--file PATH --harness NAME` for one, and `--scan` for every transcript
  backfill would find (with its `--harness`, `--project`, `--since`, and
  `--until` filters). It never creates the data directory. `--ids-from -`
  reads session IDs and transcript paths from standard input, and
  `--workers N` exports several sessions at once, writing each record as it
  finishes. Local records carry the transcript's path and project folder,
  and no commit, replay marker, or feedback. Original native start times
  are preserved, native identities come from filtered records, local decoder
  errors omit private details, and failed
  output cancels further source reads.

- **Replay sessions stay out of your history.** A tool that replays archived
  tasks with other agents sets `AGENT_ARCHIVE_REPLAY=<run id>` for the
  agents it runs; the sessions its runs produce are captured as usual but
  marked with `replay` in their metadata (with the run ID when it is a plain
  identifier). `list`, `stats`, and `handoff --latest` and its pickers leave
  them out; `list --replays include|only` and `stats --replays include|only`
  show them, marked `[replay]` in the table. `show ID` opens one as usual,
  and `status --json` counts them per app in `replay_sessions`. See
  [JSON output](docs/reference/json-output.md#replay-sessions).

- **The commit a session started on.** When a session starts in a git
  repository, the hook records the commit checked out in its working
  directory and whether the working tree had uncommitted changes, and each
  stop records the commit checked out then. The metadata carries them as
  `git_head` (`start` with `sha`, `dirty` and `observed_at`; `last` with
  `sha` and `observed_at`), `show` has a `Commit` row, and `show --json` and
  `list --json` include the field. Only full commit names, a yes/no, and
  times are kept: no branch, remote, path, or file name. The hook runs
  `git rev-parse` and `git status` with the same short timeout as the
  repository key, before it takes its lock, and records nothing when git is
  missing, slow, or the directory is not a repository. Sessions registered
  before this release and imported sessions have no starting commit; later
  live stops can still record `last`. Subagents have neither, and nothing
  infers a commit later. See
  [JSON output](docs/reference/json-output.md#show) and
  [privacy](docs/security/privacy.md#what-is-uploaded).

- Codex-only blanket policy and admission consumers retain one explicit scope
  across current and future physical projects, independently of discovery.
  Fresh hook/discovery starts keep immutable local proof and current exclusion/
  destination checks; new projects never grow configuration. Scope-capable
  configuration uses the incompatible `codex-scope-floor-v3` writer fence,
  including disabled history. Scope and source generations retain immutable
  start floors; invalid clock transitions refuse atomically.
  Public selection remains in the activation change.

- Disabled Codex discovery machinery performs bounded source scans before
  storage access and reopens admitted sources within approved roots. Identity
  misses require registration-census recovery before allocation. Startup
  restores surviving owners after both derived indexes are lost, including
  admitted continuations outside a later consent window. Producer activation
  and setup remain a separate implementation phase. Failed directories retry
  without waiting for backlog completion, local recovery failures survive final
  collector status, and existing hook/import origins keep their own locators.

- Shared local discovery foundations: shared project/worktree facts, durable
  authorization intervals, immutable admission, and actual hook observation.
  Parser 0.20.0 records discovery provenance alongside the existing Cursor
  title derivation. Automatic Codex discovery remains disabled pending scanner and onboarding
  integration. Protected discovery configuration makes published older writers
  refuse rather than discard authorization; existing hook-only configs retain
  their numeric schema and behavior. Status uses actual hook observation,
  including hooks that resume imported sessions, with legacy hook-origin fallback.
  Setup drafts preserve protected authorization while changing skill evidence;
  start hooks avoid synchronizing an already persisted observation twice.
  Every permission generation retains its native-start floor even when created
  while paused; updated protected writer fencing refuses older discovery writers.
  Unknown empty older histories require setup renewal before resume.
  Pause/resume rejects clock reversals that would erase consent boundaries;
  shared child materialization retains native transcript start provenance.

- Draft experimental revocation with verified immutable selection, per-key
  recovery journals, serialized issuance selection and independent publication,
  plus transaction-based
  `machines own-key` migration. General availability and first-run pairing
  remain disabled pending combined and live provider acceptance.

- Draft experimental dedicated R2 issuance for `machines add` and guided bucket
  creation: exact immutable provider identities, verified fresh keys, default
  two spare keys (`--spares 0..5`), and an authoritative secret-free slot ledger.
  Creation, reservation, delivery, and uncertain cleanup are tracked durably;
  management tokens are never persisted. This implementation is merged but remains experimentally gated
  pending live provider acceptance and integrated revocation/recovery review.

- Handoff before setup discovers Claude Code and Codex native conversations in
  the current checkout, with filtered batches of 50 previews, explicit older
  loading, native ID selection and modification-time latest selection. It writes
  no archive or configuration; private launch files have seven-day best-effort
  cleanup on later local handoffs. Disposable real-app acceptance is still
  unverified on macOS and Linux.
- Experimental read-only `machines --verify` provider observations, gated by
  `AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_VERIFY=1`, and an interactive argv-based
  `cloudflare_token_command` source shared with guided setup. Provider inventory
  visibility and machine ownership remain explicitly unknown.
- Encrypted machine pairing shared-key beta: explicit R2 key sharing or S3
  profile/settings transfer, destination consent, portable subtree scope,
  staged credential retries, and a secret-free delivery ledger. Shared R2
  recipients cannot be revoked independently; pairing refuses inside agents.

- Linux support with user systemd scheduling and private file credentials,
  alongside macOS support. Release builds cover amd64 and arm64 on both systems.
- Interactive session browsing and search, project-aware listings, richer
  session metadata, and bounded JSON listings.
- Terminal and worktree handoff, installed agent skills, and anonymized HTML
  statistics with estimated costs.
- Guided storage setup and archive indexing. Guided R2 bucket creation remains
  experimental behind `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1`.
- Informational `machines` records, bounded listing, optional first-setup naming
  and local rename. The list shows pairing dates and shared-key claims. Setup
  publishes after commit; the collector independently retries registration and
  refreshes heartbeats at most daily. Records are untrusted bucket claims.

### Fixed

- Guided R2 creation recognizes the management API's observed bucket-name
  conflict (HTTP 409, code 10004) and retries a generated name.
- Second-machine setup commands preserve excluded folders and reincluded
  subtrees, including when a repository checkout moves to another path.
  Scope transfer resolves all rules before applying any inclusion, treats
  symlink aliases as the same scope, and refuses conflicts with saved
  destination exclusions or reinclusions. Distinct clones retain separate
  scopes, unresolved symlinks are refused, and pairing rejects ordinary
  scope-transfer flags. Compatible keyed scopes can be reapplied; equal
  alias decisions coalesce and conflicting aliases are refused. Printed
  scope transfers use stdin to avoid operating-system argument limits, retain
  established repository identities when switching transport, and reject empty
  inputs or companion project flags that could defeat transferred exclusions.
  Saved dangling symlinks now refuse transfer before they can defeat future
  exclusions. Scope input, saved destination scopes, and resulting scopes are
  limited to 4,096 rules during transfer; oversized source rules are never truncated.
- File handoff retrieval commands preserve transcript paths containing shell
  metacharacters, quotes, backslashes, and newlines without expanding them.

- Draft machine revocation rejects forged self labels, leaves registry commands
  usable after progress publication, and preserves unverified request targets.
  Own-key migrations retire completed checkpoints and disclose shared access
  retained through retired local credential aliases. Staged migration retries
  refuse a shared credential reference replaced by later setup, retaining the
  stage for safe cancellation. Pre-slot interruptions can be safely retired
  locally after setup changes, while uncertain issuance and secret cleanup
  remain recorded. Confirmed-deleted own-key stages retry exact local secret
  removal after credential-store recovery without deleting the provider again.
  Own-key commit requires a persisted staged credential even when the shell
  supplies a matching key.

- Native handoff keeps verified IDs selectable when its cumulative label-read
  budget is exhausted, and reuses unused header reservations after inspection.
  Incomplete local `--latest` offers an explicit picker or known-ID recipes
  rather than selecting automatically. Cancellation during canonical checkout
  scoping stops further path resolution.

### Changed

- Interactive storage setup has two provider choices, R2 and S3. Creating a
  bucket is the main flow, with a summary before creation and secondary
  Customize, Use an existing bucket, and Back actions. R2 creation retains
  its experimental flag. Installed storage can be kept without re-entering
  settings. Interactive `r2` and `s3` follow the provider flow;
  `r2-existing` and `s3-existing` jump to existing storage. `setup --yes`
  keeps its existing flags and behavior.

- Bucket cleanup recipes retain private manifests and support reviewed, single-use recovery after partial deletion, including metadata-first session cleanup.
- Move the first admission-intent file synchronization outside the short queue lock and persist a pause generation, preventing delayed hook admission across a pause/resume boundary.
- The README quickstart now covers per-app hook approval and publication/read-back verification. FAQ archive sizing uses an unlimited count across all projects in the configured bucket and prefix, rather than the default 50-row listing.
- Privacy filter 15 (adapters 0.15.0) redacts `aa-pair1:` machine pairing
  bundles from retained transcript text, including truncated payloads, nested
  JSON, tool arguments, and displayed files. Existing sessions are re-filtered
  on the next collector scan.

- Listings default to the current project when available and return at most 50
  sessions. Use `--all-projects` to search the whole archive and `--limit 0` to
  return all matches.
- Scripts written against earlier builds must consume `list --json` schema 4,
  replacing schema 1 and the old `unavailable` field. Check
  `total_matched_known` before treating `total_matched` as exact. For a complete
  whole-archive result, use `agent-archive list --json --all-projects --limit 0`.
- Indexed listing requires every writer to publish index hints before metadata.
  Stop metadata-only development collectors before rebuilding an index. There
  is no legacy collector migration protocol in this launch release.
- Refresh setup and installed `/handoff` skills when adopting this build. Handoff
  offers the current terminal, a new window, or a worktree where supported.

### Fixed

- Guided R2 setup validates pasted replacement management tokens before
  creating a client, matching its environment, command and initial prompt sources.
- Experimental provider verification identifies existing permissions without
  requiring permission to create a new token with them.
- `list`, `show` and `handoff` line up their columns when color is on: a
  dimmed hint such as `· 18 subagents` no longer pushes the rest of its row
  out of line.
- `list` and `show` date a session by when it was last active, as `handoff`
  does, and list the most recently active first: a session `backfill`
  imported shows when it ran, not when it was imported. `list --json` keeps
  its order and fields.
- The `handoff` picker's footer names the subagent sessions it leaves out,
  as `list` and `show` do.
- The Tools row of `show`'s summary wraps between tools and no longer cuts a
  line short with `…`.
- A Cursor session's title (parser 0.19.0) leaves out the `<timestamp>` line
  and `<user_query>` tags Cursor wraps a prompt in.
- Guided R2 setup checks the token before asking for bucket settings, offers
  token replacement or retry on failure, and summarizes the bucket and
  automatic or customized location before creation.
- Guided R2 setup checks the archive-key permission before confirmation and
  bucket creation, so a failed lookup leaves no empty bucket behind.
- R2 setup instructions distinguish custom account tokens from the R2 token
  form, show the dashboard's Edit permission labels, and explain how to use
  a bucket-scoped Object Read & Write key instead.
- HTML statistics exports preserve concurrently created destination files when
  `--force` is absent, including when the filesystem cannot create hard links.
- Clipboard handoff uses `pbcopy` on macOS and installed `wl-copy`, `xclip`, or
  `xsel` providers on Linux. Headless Linux sessions omit copy and offer writing
  the handoff to a file.

<details>
<summary>Detailed development history since v0.1.1</summary>

The entries below record development in sequence; later entries may supersede
intermediate parser versions and interaction details. The summary above describes
the release behavior.

#### Changed

- Scripted setup accepts `--prefix`, `--retention-days`, and explicit skill-use
  capture choices. The command printed for another machine now carries these
  settings, skill evidence, and the agent skill installation policy.

- The handoff picker, `show --json` with no ID, and the pickers for an
  ambiguous `show` or `handoff` query open the browser's alternate screen on a
  terminal, so their list is gone once you choose, as `list`'s is.
  `show "<words>"` with several matches on a terminal browses them (and shows
  the details itself) unless `--json` or `--transcript` asks for one session
  to print. The picker lists every match of an ambiguous query, not the first
  20 (a pipe or an agent still gets 20 and a count). The line-mode prompt says
  words filter, and an answer that is not a row number, an ID, or a command (`q`,
  `n`, `p`, `a`) no longer reports a bad answer but filters.
- `list`, `show`, and handoff keep up to 128 characters of a session's saved
  name or first-prompt preview, instead of 72. Parser `0.17.1` refreshes
  existing metadata from retained source bundles on the next collector scan;
  sessions whose source is unavailable keep their existing preview.

#### Added

- Setup accepts `--project-repo` to match repositories at different paths,
  with bounded header-only history discovery and local exclusions preserved.
  Printed transfer commands use repository keys when available.

- **One session browser, with a filter you type into.** The handoff picker,
  `show --json` with no ID, and a `show` or `handoff` query that matches
  several sessions now open the same browser as `list` and bare `show`
  (`agent-archive list`, `show`, `handoff`), instead of a numbered list you
  answer with a line. Press `/` to narrow the rows as you type, with the words
  `list "<words>"` takes (a topic, a PR number, a branch, a project name):
  the first match is marked `▸`, ↑ and ↓ move the mark, Enter acts on it (shows
  it in `list` and `show`, hands it off in `handoff`), and Esc clears the
  filter. Rows keep their numbers while filtered, and a subagent session that
  matches is shown under its parent. `list "<words>"` and an ambiguous query
  open the browser with the words already in the filter. Where keys cannot be
  read, an answer that is not a row number, an ID, or a command is words to filter
  by, and an empty answer clears them. A handoff picked with the keys still
  reads an answer typed ahead for the `Continue in:` question.
- **Linux is supported for persistent capture** (x86-64 and arm64), on a
  machine with systemd 240 or newer and a user manager (RHEL 8 and its
  rebuilds from 8.3). macOS behavior, its plist, Keychain items and
  `config.json` are unchanged. What you can see on Linux:
  - **Install.** Releases after v0.1.1 carry unsigned static
    `agent-archive-linux-amd64` and `-arm64` binaries, in `SHA256SUMS` and
    attested; `install.sh` installs them, requires the checksum to match, and
    prints the `gh attestation verify` command. v0.1.1 has no Linux binary.
  - **Background collector.** `setup` installs a systemd user timer and
    service (`agent-archive-collector`, every 60 seconds, logs in the data
    directory) in `~/.config/systemd/user`, and records `"background_backend":
    "systemd"` in `config.json`. With no user bus (SSH without
    `pam_systemd`, a container) setup stops before changing anything and says
    to log in properly or run `loginctl enable-linger`; there is no cron
    fallback. `status` and `status --json` (`background_warnings`) note when
    lingering is off or a drop-in overrides the unit; systemd older than 240
    is refused. `setup --refresh` and `uninstall` handle the units and the
    link that enables the timer.
  - **`uninstall --skip-scheduler`** goes on when the scheduler cannot say
    whether the job is loaded: it removes the definition, hooks and skills,
    prints the command that stops the job by hand, and says the collector
    was not verified stopped. Without it, uninstall refuses in that case on
    either system.
  - **Credentials.** There is no Keychain: an R2 key is kept in a 0600 file
    in a 0700 folder of the data directory (not encrypted; an S3 profile is
    recommended on Linux), with the `AGENT_ARCHIVE_R2_*` variables as a
    read-only fallback for containers.
  - **Environment.** `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` are recorded in
    the job when set to an absolute path, `status` warns when your shell's
    differ, and Cursor database copies live under
    `~/.cache/agent-archive/cursor-snapshots` (or `$XDG_CACHE_HOME`), never
    `/tmp`. A user manager can have its own values from outside your shell,
    which `status` cannot see (see [setup on Linux](docs/getting-started/setup.md#setup-on-linux)).
  - **Cloned machines.** `config.json` records `host_id`, a digest of the
    machine ID, on Linux; `status` and `setup` warn when the data directory
    was set up on a different machine (a cloned VM or image). It is best
    effort; see [multiple machines](docs/guides/multiple-machines.md#cloned-machines-on-linux).
  - **Network homes.** `setup` and `setup --refresh` refuse, before any
    question and changing nothing, when the data directory or the systemd unit
    directory is on a network filesystem (NFS, SMB/CIFS, Ceph, sshfs and the
    like; read from `/proc/self/mountinfo`), since machines that share a home
    share one machine ID, cannot rely on file locks and each run the
    collector. Set `AGENT_ARCHIVE_HOME` to local disk, or for a home only one
    machine mounts run `setup --allow-network-home`, which is recorded as
    `allow_network_home` in `config.json`; `status` warns either way. See
    [multiple machines](docs/guides/multiple-machines.md#a-home-directory-shared-across-machines).
  - **Not verified on Linux:** the real Cursor app and `cursor-agent` hooks
    (a Cursor forum report says they may fail silently, so Cursor capture is
    best effort and its version is not detected), the real Claude Code and
    Codex apps, distributions and systemd versions other than Ubuntu 24.04
    with systemd 255 (exercised live on arm64), an amd64 live run, real R2
    and S3 from Linux (the live run used MinIO), a real logout with lingering
    off, a desktop login, and WSL. See [platforms](docs/getting-started/install.md#platforms).
  - **Handoff** opens the new agent in a tmux window on Linux; outside tmux
    it prints the command to run instead.
- The multiple-Macs guide is now [multiple machines](docs/guides/multiple-machines.md)
  (`docs/guides/multiple-macs.md` is gone; update any link to it), with a
  section on cloning Linux machines next to the Migration Assistant and Time
  Machine guidance.
- `agent-archive stats --json --all` lists every project, skill and MCP
  server instead of the top five of each (`--all` is an error without
  `--json`: the web page keeps its top lists). The document is otherwise the
  same, in the same order, and `schema_version` stays 1. The hints under the
  skills and MCP servers now say `+ N more (all in --json --all)`, and the
  one under the projects screen `--json --all` too. See
  [JSON output](docs/reference/json-output.md#stats---json).
- Setup can create an Amazon S3 bucket for you: choose "Amazon S3", then Continue at the storage question. It creates the bucket
  in your own AWS account with the profile you pick (region and name are
  asked, the name suggested as `agent-archive-` and random characters),
  turns on all four Block Public Access settings, and reads them back, then
  prints the least-privilege policy for the new bucket and asks which
  profile archiving should use, recommending a separate narrower one. The profile needs `s3:CreateBucket` and
  `s3:PutBucketPublicAccessBlock`; without them (or when an organization
  policy forbids it) setup says so and lets you pick an existing bucket. If
  Block Public Access can't be turned on, setup offers to retry, or to delete
  the empty bucket once you type its name, and never uploads to it. Only the
  standard AWS regions are supported.
  If setup ends without using a bucket it created, it says so. Setup does
  not create IAM users or keys, and sets no lifecycle rule. The
  manual steps in the bucket guide still work.
- **Experimental:** `setup` can create a Cloudflare R2 bucket for you. Set
  `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1` to enable creation after choosing
  **Cloudflare R2**, then **Continue** at the storage question, then paste one
  Cloudflare API token
  (Workers R2 Storage Write and Account API Tokens Write, or set
  `CLOUDFLARE_API_TOKEN`). Setup creates a new bucket (Cloudflare buckets have
  no public access by default) and a key that can read and write only that
  bucket, checks it, and keeps the key in the Keychain. The token you pasted
  is used during setup and then dropped, never saved, and setup revokes the
  new key's token if it fails its check or can't be stored. It also reads
  whether the bucket's public `r2.dev` URL or a custom domain is on, and if so
  stops and asks: check again, choose other storage (the default, which
  revokes the new key), or continue anyway. Not available with
  `setup --yes`. It has not yet been run against every kind of Cloudflare
  account, which is why it is behind the switch. See [creating a
  bucket](docs/getting-started/bucket.md#let-setup-create-it).
- **`agent-archive stats` is interactive on a terminal.** Plain `stats` opens
  a screen with a bar of keys: `o` `d` `p` `m` `a` switch between the
  overview, detail, projects, models and agents views, `w` cycles the window
  (7, 30, 90 days) instantly from what was already read, the arrows, `j` `k`,
  PgUp/PgDn, space, Home/End and the mouse wheel scroll, `?` lists the keys,
  `h` saves the redacted page as HTML (it asks for a file name and never
  replaces a file) and `q` or Ctrl-C quit, leaving the terminal as it
  was. The bar names the window `w` moves to next. It opens only when
  standard input and output are terminals (not a dumb one) and
  `AGENT_ARCHIVE_NONINTERACTIVE` is off, and not with `--view`, `--detail`,
  `--by`, `--no-pager`, `--json` or `--html`; those print as before. See the
  [stats guide](docs/guides/stats.md#the-interactive-screen).
- `agent-archive stats --json` carries more of what the screen is built from:
  each day's estimated cost (`daily[].cost`, adding up to the overview's) and
  the dearest day (`peak_spend`), the share of tokens that were cache reads
  (`overview.cache_share`), up to three prioritized `heads_up` notes as data
  (subagents using a quarter or more of the tokens, one session costing a
  tenth or more of the spend when the window has more than one session,
  sessions without token data, a low cache-hit rate), and `display_skills`, which lists a plugin's skill once under its
  bare name, with `total_skills`, `total_display_skills` and
  `mcp.total_servers` counting every row. Existing fields and the page are
  unchanged. See [JSON output](docs/reference/json-output.md#stats---json).
- **Handoff without copying.** Continuing a session in another coding agent
  is one step: inside Claude Code, `/handoff codex` opens Codex in a new
  terminal tab or window with the session as its context (in Codex, ask for
  `$handoff`); on a terminal, `agent-archive handoff` picks a session, asks
  where to continue, and starts the agent there. See the
  [handoff guide](docs/guides/handoff.md).
- Metadata may include, from parser `0.15.0`, `git_activity`: the commits,
  pushes, and pull requests created or merged that the session's own tool
  calls confirmed (`git` and `gh` commands, and GitHub MCP tools), each with
  its time and, when known, the commit SHA, branch, `owner/repo`, pull
  request number, and a URL. Only work whose result shows it succeeded is
  recorded, never a failed, rejected, or dry-run attempt. `counts.commits`,
  `counts.pushes`, `counts.prs_created`, and `counts.prs_merged` count it,
  and `show` has a `Git` row. Commit messages and pull request text are not
  kept. Existing sessions gain the fields on the next metadata refresh;
  nothing is re-uploaded but the metadata.
- Setup installs the `handoff` skill for Claude Code
  (`~/.claude/skills/handoff/SKILL.md`) and for Codex and Cursor
  (`~/.agents/skills/handoff/SKILL.md`). It runs
  `agent-archive handoff --to <agent>`, defaulting to another agent than the
  one you are in. Setup leaves a file it did not write, uninstall removes
  only its own, and `status --json` lists them in `agent_skills`. After an
  upgrade, `status` warns about a skill file an earlier release wrote and lists
  it in `agent_skills_out_of_date`; `agent-archive setup --refresh` refreshes it.
- `agent-archive setup --refresh` brings the app hooks, the background
  job's definition, and the skill files up to date for the saved settings and
  the binary you run it from, and changes nothing else. It asks nothing and
  needs no terminal, prints `nothing to refresh` or what it refreshed, and
  refuses (exit 1) before setup has finished, while a setup needs recovery,
  after uninstall, or when another installation's hooks are in the way. It
  also repairs hooks left pointing at a binary that moved. `install.sh` runs it
  when it finds a set-up machine, so upgrading the binary upgrades the hooks and
  skills; if it fails, or the installer runs as root (which would leave
  root-owned files in your home directory), the install still succeeds and
  says how to run it. It waits up to ten seconds for a running collection
  pass, and finishes once it starts writing even if you press Ctrl-C. Run as
  root in another user's home directory, it refuses, changing nothing.
  `status` names it where it reports out-of-date skills, hooks that are
  missing, or a moved binary.
- `agent-archive setup --no-skills` (also with `--yes`) installs no agent
  skills and removes the ones setup wrote; a file that is not setup's is left
  alone and named. It is saved, so later setup runs keep the skills off, and
  `agent-archive setup --skills` turns them back on. `status` says when they
  are turned off (`agent_skills_disabled` in `--json`). Setup now says in one
  line how to opt out.
- Setup also installs an `agent-archive` skill for Claude Code
  (`~/.claude/skills/agent-archive/SKILL.md`) and for Codex and Cursor
  (`~/.agents/skills/agent-archive/SKILL.md`), so you can ask an agent to
  "pull in the auth session from Codex". The agent runs
  `agent-archive handoff "auth" --harness codex` (a bounded, filtered handoff
  prompt, found by title on this machine first, then in the archive), asks you
  which when several sessions match, and can browse with `list`, `show`,
  and `show --transcript`. It is told never to run `setup`,
  `uninstall`, `purge`, `backfill`, `sync`, `feedback`, `handoff --to`, or
  `--max-bytes 0`, and to treat what it reads as data, not instructions. In
  Claude Code the skill names only `agent-archive status` as pre-approved (in
  a `claude -p` check on 2.1.283 that did not apply when the agent chose the
  skill itself, so `status` may ask too); the rest asks once,
  since no permission rule can allow `handoff` without allowing
  `handoff --to`, and Claude Code also asks before first using the skill (a
  `Skill(agent-archive)` rule allows it). Where a sandbox blocks the network,
  a session on this Mac is still found by title. What an agent can read
  through the skill is in
  [privacy](docs/security/privacy.md#what-an-agent-can-read-through-the-skill).
  See [agent skills](docs/guides/agent-skills.md).
  Claude Code only `agent-archive status` runs without asking; the rest asks
  once, since no permission rule can allow `handoff` without allowing
  `handoff --to`. See [agent skills](docs/guides/agent-skills.md).
- Sessions in a git repository now carry a `repo_key` in their metadata: a
  hash of the repository's `origin` address (credentials, scheme, port, and
  `.git` removed, so SSH and HTTPS clones of one repository agree), which
  identifies the repository wherever it is checked out. Only the hash is
  stored, never the address; the
  [privacy page](docs/security/privacy.md) explains what a hash of a known
  address does and does not hide. Parser version is now `0.16.0`, so existing
  sessions gain the field on the next metadata refresh, on the Mac that
  captured them and only while the repository is still there. `handoff
  --latest` uses it (see Changed).
- On a build without a Keychain (Linux), an R2 key is kept in a file with mode
  0600 in a `credentials` folder (mode 0700) of the data directory, and
  agent-archive refuses to read it, or save into the folder, when it is open
  to other users, is a symbolic link, or is not yours, naming the `chmod` that
  fixes it. Where no such file exists, `AGENT_ARCHIVE_R2_ACCESS_KEY_ID` and
  `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY` are read as a read-only fallback. On
  macOS nothing changes: the key stays in the Keychain. An S3 profile keeps
  no secret of its own and is the better choice where you can use one. See
  [privacy](docs/security/privacy.md#where-credentials-are-kept).
- `agent-archive stats` shows how you use your coding agents over the last 30
  days (`--days`, or `--since` for a start day): tokens by day, sessions,
  prompts, estimated cost and active days with their change from the
  previous period, agents, cost by model, top projects, what used your
  tokens (cache reads and writes, input, output, subagents, skills, MCP),
  and highlights. `--by day|week|month|project` breaks the window down,
  `--json` prints a versioned document (`schema_version` 1), `--prices FILE`
  puts your own model prices on top of the built-in table, and `--harness`,
  `--model`, `--imported` and `--hook-captured` filter as `list` does. It
  reads metadata only and prints no prompts or paths. Cost is an estimate at
  list price from a dated price table, unpriced models are left out and
  flagged, and what an agent does not record (Cursor's tokens) reads
  "unknown", never zero. See [stats](docs/guides/stats.md).
- `agent-archive stats --html` writes the same numbers as one self-contained
  web page shaped like the terminal's default view: spend, sessions and
  tokens (with the cache share) at the top, the agents as one bar, a chart of
  daily spend with its dearest day (days that could not be priced are marked,
  not drawn as zero), where the spend went by project and by model in the
  agents', model families' and projects' colors, the skills and MCP servers
  used most, up to three "heads up" notes, and, under a divider, the agents'
  table, a donut of what used your tokens, facts (days active, busiest day,
  tool errors, month rank) and the scope, coverage and price-table notes. The
  change from the previous period is shown only when there was a previous
  period, and subagents are counted as runs, not sessions. It is a single file with
  inline styles and SVG, no script and no request to anything else; it
  follows your light or dark setting, prints, and reads on a phone. Give
  `--output FILE` to save it (mode 0600; an existing file is kept unless
  `--force`; the file is written in one step, never half), or redirect
  standard output. It holds counts and names only, never prompts, paths or
  session IDs, and names each project, skill and MCP server, and each model
  the built-in price table does not list (a fine-tune id, a custom deployment), "project A",
  "skill A", "MCP server A", "model A" and so on unless you pass
  `--include-names`, so the page can be shared. See [stats](docs/guides/stats.md#share-it-as-a-web-page).
- Metadata may include, from parser `0.14.0`, `counts.reasoning_tokens`,
  `counts.tool_errors` (tool results the app flagged as errors; not known
  for Codex), `model_tokens` (token counts split by model, so a session that
  used several models can be costed per model), and `mcp_calls` (MCP calls
  counted by server). Codex's `cache_write_input_tokens` now fills
  `counts.cache_write_tokens`. Existing sessions gain the new fields on the
  next metadata refresh; nothing is re-uploaded but the metadata.
- `AGENT_ARCHIVE_NONINTERACTIVE=1` stops agent-archive from asking anything:
  no session picker or browser, no pager, no confirmation prompt (`purge
  apply` needs `--yes`), even on a terminal. It is on by itself when `CLAUDE_CODE_SESSION_ID`,
  `CODEX_THREAD_ID`, or `CURSOR_AGENT` is set, so a coding agent whose shell is
  a pseudo-terminal never hangs on a prompt; `AGENT_ARCHIVE_NONINTERACTIVE=0`
  turns it off, and a refusal caused by it says so.
- On a terminal, `handoff` asks where to continue once the session is
  chosen: an installed agent (default: `handoff.default_to` in `config.json`,
  else another agent than the session's), print (paged when long), copy to
  the clipboard, or write to a file. Pipes, `--output`, `--format json`, and
  `--no-preamble` print as before, so `codex "$(agent-archive handoff
  --latest)"` still works, and so does a run inside an agent, where
  `handoff` never asks anything.
- `handoff --to claude|codex|cursor` starts that agent without asking, with
  the filtered session record, local or archived. With no session named, run
  inside Claude Code, Codex, or Cursor, it hands off that agent's own
  session; otherwise a terminal gets the picker. The agent reads the record
  from a private file kept 7 days in the data directory's `handoffs/`, so a
  resumed session can read it again (before setup, in a temporary folder the
  system clears), and is told how to get more context with Agent Archive.
  Claude Code gets only that file's folder with `--add-dir`; Codex and
  Cursor get the checkout with `--cd` and `--workspace`. Cursor's `agent`
  CLI is tried before `cursor-agent`. The launched agent does not inherit
  the calling agent's session variables.
- On a terminal the agent runs there. Without one, or inside an agent (or
  with `AGENT_ARCHIVE_NONINTERACTIVE=1`) even when its shell is a
  pseudo-terminal, it opens in a new tmux window, iTerm2 or Ghostty tab, or
  Terminal window, and `handoff` returns; `--here` and `--new-window` choose
  explicitly. Where no window can be opened it prints the command to run
  instead.
- Arguments after `--` go to the launched agent, and `config.json` may set
  per-agent arguments (`handoff.args`) and a default destination per source
  harness (`handoff.default_to`).
- `handoff --worktree` starts the agent in a new git worktree beside the
  checkout, on a new branch (`handoff/<id>`, or `--branch NAME`), with your
  uncommitted changes (staged ones arrive unstaged) and untracked, not
  ignored, files carried over. Your checkout and stash list are left as
  they were. On a terminal `--to` may be left out: the agent chosen at the
  prompt starts in the worktree, and printing, copying, or writing the
  handoff creates none. Without `--worktree`, handing off a session active
  in the last 2 minutes in the same checkout asks on a terminal whether to
  continue there, cancel, or use a worktree, and warns otherwise.
- `show --transcript` prints a session's conversation to read: each prompt,
  the agent's replies, one line per tool call (✗ when it failed), your `!`
  shell and local slash commands, compactions, and app notices such as a
  background task finishing, paged on a terminal (`--no-pager` to print
  directly). `--full` adds tool results and shell output, trimmed.
  `--transcript --json` prints what `--normalized` printed.
  Your prompts are quoted with a `┃` gutter that stays on wrapped lines,
  and the agent's part of each exchange starts with the app's name
  (`Claude Code ›`).
- `show --transcript` prints at most 120,000 bytes (about 30k tokens), like
  `handoff`, so a script or an agent that runs it on a long session is not
  flooded. `--max-bytes N` changes the limit and `0` removes it; it applies
  to `--full` and `--json` too. Over the limit, the oldest tool output, tool
  calls, agent text, and prompts are trimmed first, the newest exchanges are
  kept, and the untrimmed output is saved in the data directory's
  `handoffs/` for 7 days, its path named at the end (`trimmed` in `--json`).
- Browsing on a terminal (`list`, bare `show`) opens a session's summary in
  place of the list, on the terminal's alternate screen: `t` shows its
  transcript, Enter or `b` goes back to the list, and `q` quits. The last
  summary viewed stays in scrollback. Bare `show` now keeps browsing like
  `list` instead of exiting after one pick.
- The session browser and the session pickers (`list`, bare `show`,
  `handoff`, `show --json` without an ID, an ambiguous `show QUERY`) fit the
  window. The browser reads keys as you press them, without Enter: the
  mouse wheel, the arrows, PgUp and PgDn (or space, `n`, `p`), and Home and
  End scroll the list and a long summary at once, with a status line saying
  where you are (Top, a percentage, Bottom) and, in the summary, how many
  lines are above and below. Type a row number or short ID and press Enter
  to open it; in the summary, `t`, `m` (the whole summary in the pager),
  `b`, and `q` act on their own key, and Enter or Backspace go back
  to the list. The wheel's arrows are no longer echoed into the prompt as
  `^[[A`. The other pickers still read a line and show a list taller than
  the terminal a page at a time (`n` and `p` move; any row number or short
  ID still works).
- Metadata may include optional `ended_at` (latest record timestamp),
  `tools_used` (the 10 most-called tools with counts), and
  `counts.files_touched` (distinct files edited; a count only, never
  paths). They arrive with parser `0.13.0`; this release ships `0.16.0`,
  so existing sessions gain them on the next metadata refresh.
- A Claude Code parent session whose subagent's transcript was never written
  now says why the subagent is missing: its metadata carries a
  `subagent_transcript_never_written` capture gap, "Claude Code reported a
  subagent but never wrote its transcript".
- `status --verbose` counts the subagents dropped in the last 7 days because
  Claude Code never wrote their transcripts, by the type Claude Code
  reported ("7 not archived in the last 7 days (Claude Code never wrote
  their transcripts; nothing to do)", then "5 unknown type, 2 Explore"), and
  `status --json` lists them as `collector.expired_subagents`. The type is
  kept on this Mac only and never uploaded. Default `status` still says
  nothing about them.
- `install.sh` and `scripts/install-from-source.sh` now recognise Linux release
  assets (x86_64 and aarch64): the installer selects
  `agent-archive-linux-<arch>`, skips the macOS-only Developer ID check for
  it, and on every OS refuses to install unless the download matches its
  entry in `SHA256SUMS`, which must be exactly one well-formed lowercase
  SHA-256 line; an empty download also stops the install. The macOS Developer
  ID check is unchanged. Other changes you can see on macOS: `sha256sum` is
  preferred over `shasum` when both are present; an unset or empty `HOME` now
  fails with a clear message when no install directory can be chosen
  otherwise; `AGENT_ARCHIVE_VERSION` must look like a release tag (`latest`
  is refused; leave it unset); a relative `AGENT_ARCHIVE_INSTALL_DIR` is
  resolved to an absolute path; the installer refuses to install over a
  directory named `agent-archive` (it used to move the file into it); it
  stages the new binary with `mktemp` and removes it on failure; it prints
  `Downloading from <url>` when `AGENT_ARCHIVE_DOWNLOAD_URL` is set; and it
  reports a missing `curl` ("curl is required") and a failed temporary
  file or directory creation with their own messages.

- `show SESSION_ID`'s summary, `status`, and `purge plan` are paged on a
  terminal, like `list`; `status` and `purge plan` take `--no-pager`, and
  `show`'s `--no-pager` now covers the summary too. Piped output is
  unchanged.

- **`agent-archive list "<words>"` searches.** One or two words find a
  session: every word must appear, in any case, in some field of it (its name,
  title, branch, project name, harness, or the first 4 or more characters of its
  ID), and words may match different fields, so `list "linux 212"` finds the
  session named for Linux that opened PR 212. `#212`, or a bare number of 1 to 6
  digits, also matches a pull request number, any the session linked or created.
  On a terminal it opens the browser over the matches; piped it prints the
  table; `list "<words>" --json` prints the same document, narrowed, with the
  same `scope` object. `handoff "<words>"` and `show "<words>"` use the same
  matcher and the same order, so a person and an agent get the same answer: this
  repository's top-level sessions first, then every project's, then subagent
  sessions (in the repository, then everywhere); the first that has a match
  answers, and a note says how many more match in other projects.
- When several sessions match and nothing can ask (`handoff` or `show` piped,
  or inside a coding agent), the table of candidates gains a PR column, labels
  a subagent `subagent of <parent ID>`, and ends with the exact command to run
  next (`Next: agent-archive handoff d7a77938 --harness claude`, or
  `agent-archive show d7a77938`) and the `list "<words>" --json` that shows
  them as data. The agent skill says so, and that the words may be a topic, a
  PR number, a branch, or a project name.

#### Changed

- **Privacy filter 14: a subagent's task description is now archived.**
  When Claude Code starts a subagent, its parent gives the task a short
  description ("find the retention tests"), which Claude Code keeps in an
  `agent-<id>.meta.json` file beside the subagent's transcript. That
  description is now kept, with the subagent's session, and is its name in
  `list` and `show`. It passes the same redaction as your
  prompts and is cut to 512 bytes. Nothing else in that file is kept (the
  path of a worktree, for one), and a subagent with no such file, or one that
  cannot be read, is archived as before. The next sync re-reads and
  republishes each subagent whose transcript is still on the machine, so it can
  carry its description; a file that appears later is picked up when the
  subagent's transcript next changes. See the
  [filter changelog](dev/specs/privacy-filter-changelog.md) and
  [privacy](docs/security/privacy.md#what-is-uploaded).
- **"Mac" became "machine" wherever the text is not about macOS**, now that
  Linux is supported: in `agent-archive help` and the [CLI
  reference](docs/reference/cli.md) ("sessions already on this machine"), in
  `setup`'s review ("What leaves your machine:") and its next-steps line
  ("To set up another machine with this storage"), in `backfill`, `purge`,
  `uninstall`, `status` and `handoff` messages and in retention's clock
  messages. What is specific to macOS (the Keychain, Time Machine, Migration
  Assistant, macOS's privacy prompts, launchd) keeps its wording, and scripts
  that match these messages should match the new words.
- **Privacy filter 13: a session's name and linked pull request are now
  archived.** Claude Code's session name (the one in its sidebar, set from
  your prompt or by `/rename`) and the pull request a session linked (its
  `owner/repo`, number, and GitHub link) are kept, and so is a Cursor chat's
  name. Every name a Claude Code session was given is kept, so renaming one
  does not remove its earlier names from the archive. Names pass the same
  redaction as your prompts; the link is kept
  only in the exact shape `https://github.com/owner/repo/pull/N`, and a link
  that is not is dropped. Nothing else changes: Claude Code's `agent-name` and
  `last-prompt` records are still dropped. The next sync re-reads and
  republishes each session whose transcript is still on the Mac, so it can
  carry them; `list`, `show`, and the handoff picker show them as described
  below. See the
  [filter changelog](dev/specs/privacy-filter-changelog.md) and
  [privacy](docs/security/privacy.md#what-is-uploaded).
- **`list` and the browser show top-level sessions only.** A session's
  subagents are no longer rows of their own: they are left out before
  `--limit` counts (so `--limit 50` is 50 sessions), the footer says how many
  were hidden (`42 sessions (318 subagent sessions hidden; search to find
  one)`), and a session that has some carries a dim `· 45 subagents` hint, as
  the handoff picker does. A subagent is found by searching. `list --json`
  keeps every row, subagents included, so scripts see what they did. The
  table and the browser now read every session's metadata (the cache keeps it
  quick) instead of the index's newest page, so that subagents can be left out
  before the limit; `list --json` without words keeps the index's page outside
  a project or with `--all-projects`.
- `handoff "<words>"` and `show "<words>"` match words across a session's
  fields (above) where they matched the whole text as a substring of the title
  or the start of an ID. Words that matched a title before still match it,
  except a `#N` that was only the start of a longer number (`#21` no longer
  finds `PR #213`). The start of an ID now needs 4 characters or more, and a PR
  number such as `212` or `#212` never matches one, so it does not also find
  every session whose random ID happens to start with those digits.
  `show "<words>"` now offers a subagent session only when no top-level session
  matches, where it listed both. The table printed for several matches is the
  one described above.
- **`list`, `show`, and `handoff` start from the repository you are in.**
  Run inside a project, `agent-archive list` and `list --json` now return
  that repository's sessions (every checkout and worktree of it, and its
  sessions from other Macs) where they returned all of them. Scripts that
  read every session pass `--all-projects`. The text listing and the
  handoff picker carry a heading that names what is shown, and on a terminal
  `a`, typed alone, switches between the repository and all projects. When
  the repository has no sessions they open on all projects and say so.
  `--project DIR|NAME` (new for `list`, and now for every `handoff`
  selection, not only `--latest`) picks another project by directory or by
  name. `handoff "<title>"` looks in the repository first and says how many
  more match in other projects. Outside any project nothing changes.
  `list --json` gains an optional `scope` object
  (`{"label", "all_projects", "fell_back", "outside_matches"}`) and keeps
  `schema_version` 4.
- The session table and the handoff picker leave out a HARNESS or PROJECT
  column every row shares and name the value in the heading, add a PR column
  (the last pull request the session linked or created) when a row has one, dim the ID
  in the picker, and mark a session active in the last 2 minutes with a dot.
- **Rows and `show` now show the name you gave the session in your agent, its
  branch, and its linked pull requests.** A row in `list`, the handoff picker,
  and the browser shows the session's name (the one in Claude Code's sidebar,
  set from your prompt or by `/rename`, or a Cursor chat's name) where it
  showed a preview of your first prompt, and still shows the preview for a
  session with no name. `show`'s summary uses the name as its heading, with
  the first prompt as a `Prompt` row, and gains `Branch` and `PRs` rows (the
  last git branch the session recorded, and the pull requests it was linked
  to). Metadata from parser `0.17.0` carries them as the optional `name`,
  `branch`, and `pull_requests` fields (see
  [JSON output](docs/reference/json-output.md#show)), so the collector
  refreshes every published session's metadata once, from what is already
  archived; a session gets its name only if it was published by filter 13, which
  the next sync does for sessions whose transcript is still on the Mac. The
  handoff picker's rows for sessions not yet uploaded are cut to 72
  characters like published ones, not by display width.
- `agent-archive stats` has a new default screen: a short summary with the
  headline numbers (estimated spend, sessions, tokens, with the change from the
  previous period only when there was one, and how much of the tokens were
  cache reads), one bar for which agents did the work, a three-row chart of
  each day's spend, where it went by project and model (two columns from 80
  terminal columns, stacked from 60), the skills and MCP servers used most,
  and up to three heads-up notes, in the terminal's 16 colors (`NO_COLOR` and
  pipes are plain; bars have no shaded track). The rest moved behind
  `--detail` (`--view detail`): streaks, the busiest day, the favorite model,
  the tool error rate, the token breakdown, the agents table and the notes on
  what the numbers rest on. `--view projects`, `models` and `agents` list the
  projects and model families (up to 500 each, then `+ N more`) and every
  agent. `--by project` is now `--view projects`, and `--by day`, `week` and
  `month` add their table to the detail screen. It fits
  terminals down to 40 columns. `--json` and `--html` are unchanged.

- **`stats --json` and `--html` rank projects by spend, not tokens.** The
  `projects` list (and the `groups.rows` of `--by project`) used to be ordered
  by tokens and cut to the top five, which cache reads dominate, so a project
  that cost more could be missing from the top five while a cheaper one with
  more cache reads was in. It is now ordered by estimated cost, highest first
  (a project with no priced cost after every one that has it, ties by tokens,
  sessions, then name) and cut after that; the terminal screens already
  ranked this way. The order of the JSON list changes and so does which five
  it keeps; the fields and `schema_version` (1) do not. A partly priced
  project is ranked on the spend it has, so its real cost may be higher than
  its place says, and a project with no priced cost is after every priced one;
  `overview.cost` (`partial`, `unpriced_tokens`) and `models` still say when
  tokens were left out. See
  [JSON output](docs/reference/json-output.md#stats---json).

- `handoff` takes a title as well as a session ID: `handoff "fix the auth
  bug" --harness codex`. It matches as `show` does (a title substring or a
  short session ID; a full ID wins), in this Mac's sessions first, with no
  network, then in the archive. Only the title (the first prompt) is
  matched, never the rest of the conversation; a session on this Mac has its
  title read from its transcript file, which stays on the Mac. Several
  matches (the newest 20 are shown) open the picker on them on a terminal;
  without one they are listed on stderr with exit code 1 instead of
  guessing. Run from inside a Claude Code or Codex session, a title never
  matches that session itself (as `--latest` skips it).
  `handoff` no longer rejects an argument that is not shaped like a session
  ID up front; one that matches nothing says so and points to `list`.
- `handoff --latest` now finds sessions from your other computers even when
  the repository is at a different path. A session matches the current
  directory by path or by repository (the `origin` remote, so SSH and HTTPS
  clones agree), from a subdirectory of the repository too; a directory
  without an `origin` matches by path only, as before, and a fork's `origin`
  is the fork's. A session that ran at the same path always comes first. A
  repository chooses its own `origin`, so when `--latest` can only find a
  session by repository it says so before downloading any of it (machine, project,
  start time, first prompt) and, on a terminal, asks before going on (default
  no); where nothing can be asked (a pipe, or inside a coding agent) it
  refuses, printing only the machine and start time and the `handoff
  SESSION_ID` command for you to run (`agent-archive list` when the ID is not
  a normal one). That refusal slows a steered agent; it does not stop one
  that runs the command. Path matches, explicit session IDs, and the picker
  behave as before. `--to` launches an archived session from another
  computer the same way, and the handoff tells the agent the session was on
  another branch or in another directory when it was. When nothing matches,
  the message says what was tried and how to make a match possible. See the
  [handoff guide](docs/guides/handoff.md#finding-a-session-by-repository) and
  the [threat model](docs/security/privacy.md#threat-model).
- The `handoff` picker also lists this Mac's sessions, including ones not
  yet uploaded (marked so), newest activity first, and still works when the
  archive cannot be read. Sessions with no prompt yet are left out.
- The default pager scrolls on the mouse wheel and names its keys. With no
  `AGENT_ARCHIVE_PAGER` or `PAGER` set, or one set to a bare `less`, `less`
  551 or later runs with `--mouse` (hold Option while dragging to select
  text in iTerm2), and `less` 530 to 550 runs on the alternate screen, where
  the wheel scrolls it too. The prompt reads, for example, "lines 1-48 of
  1210 - arrows/space scroll, / search, q quit" ("q back" from the
  session browser). Any other pager you set runs as given, with `LESS=FRX`
  and `LV=-c` added when those are unset, as git does.
- Ctrl-C while a pager shows `list`, `show`, `status`, or `purge plan` now
  goes to the pager (in `less`, it cancels a search) instead of ending
  agent-archive and leaving the pager on the terminal.
- Off macOS, Cursor's data folder is looked for where VS Code keeps its own,
  `$XDG_CONFIG_HOME/Cursor` (default `~/.config/Cursor`), and the macOS-only
  backfill inputs (Claude and Codex desktop app folders, the privacy-protected
  folders, the `/Applications` probes) are skipped. On macOS nothing changes.
- **Breaking for scripts:** `show SESSION_ID` now prints a readable summary
  (title, when, app, models, activity, skills, subagents, capture gaps)
  instead of JSON. Capture gaps the archive records by design (filtered or
  redacted content, fields the parser does not recognize) are named in one
  dimmed Omitted row; only gaps that may mean content is missing get a
  warning. Pass `--json` for the metadata sidecar, byte for byte what
  `show` printed before.
- `show --normalized` is deprecated in favor of `show --transcript --json`.
  It still works, with unchanged output, and prints a note on stderr.
- `handoff` shows file paths the same way everywhere: relative to the
  workspace root (`./a.go`, `../repo/a.go`, and `/repo/a.go` are all
  `a.go`; a relative path above the root, such as `../other/b.go`, keeps
  that spelling; the root itself is `.`; Windows drive paths use forward
  slashes), and its files-touched list no longer repeats one file under two
  spellings. A search of the workspace root no longer ends in
  `in <root>` (or `in .`). A Codex `local_shell_call` is listed even when
  nothing about it was retained, and `tools_used` counts exactly the calls
  `handoff` lists. It also recognizes Cursor's `edit_file_v2`, `read_file_v2`,
  `ripgrep_raw_search`, and `glob_file_search` tools, so edits in chats
  imported from Cursor's database are listed and counted in
  `counts.files_touched`.
- A first `setup` run inside a Git project, with apps found, asks one
  question for the apps and that project ("Archive Codex and Claude Code
  sessions in ~/src/app?") instead of two; the review still lets you add
  projects, drop apps, or change how long sessions are kept, and setup says
  how many other projects your apps have sessions in. Answer no to choose
  them one at a time as before. Setup no longer pre-selects a repository that
  is your home folder or holds it, or that is a temporary folder (`/tmp`,
  `/private/tmp`, `/var/folders`, `$TMPDIR`) or holds one: it says so and
  asks for the projects instead. A repository inside a temporary folder is
  still offered.
- `setup` offers to import past sessions after it has said how to check that
  capture works, not before, and `setup` ends with the command for another
  Mac after that offer. `setup --yes` is unchanged.
- The storage instructions in `setup` are now two lines and a link to the
  bucket guide, instead of the full steps.
- `setup` checks a new storage key or profile with one small listing before
  it writes its test file, so a wrong account ID, key, or profile fails at
  once with the usual explanation. The full write, read, and delete check
  still decides that storage works.

#### Fixed

- Pager startup failure preserves the complete direct-output fallback, even
  when the pager consumed its input; regression coverage checks partial and
  complete reads and reports output write failures.

- `setup`'s hidden prompt for a secret access key no longer spins at full
  CPU forever on macOS when its terminal goes away without a hangup signal
  (a closed pseudo-terminal, for example). It now ends as every other prompt
  does at the end of input, with "no more input", and so does Ctrl-D on an
  empty answer, which the prompt used to ignore.
- On a busy Mac, setup no longer warns "Could not prune capture diagnostics
  for excluded projects", leaving a project it had just excluded named in the
  local diagnostics file, when a hook recorded a diagnostic at the same time.
  Hooks that fire together are also far less likely to drop each other's
  diagnostics.
  Each writer held the diagnostics lock through its write's disk syncs, which
  could outlast setup's two-second wait for that lock and a hook's 50 ms one.
  Writers now sync before taking the lock and hold it only to reread, check
  and rename the file, and setup waits up to ten seconds for it, since a
  rename alone can stall for over a second while other programs sync.
- The `handoff` picker no longer offers archived subagent sessions. They
  filled the first screen under their orchestrator (one had 45 of them) and
  were counted in "Showing 50 of 659", though only top-level sessions can be
  handed off. The picker now lists top-level sessions only and counts only
  those; `list` and `show` are unchanged.
- A hook that fires while a retention sweep is expiring its session no
  longer fails with "another collector or setup is running" on a busy Mac,
  leaving the session to expire without that turn. Retention wrote the
  session's removal record while holding the lock the hook waits a second
  for, and that write's disk syncs could take longer; it now writes the
  record first and holds the lock only to recheck and forget the session.
- **`agent-archive stats` no longer says `--json` has every row of a list it
  cut.** Under a cut list the screens said `(--json has them all)`, but plain
  `--json` keeps only the top five projects. The projects screen now says `+ N
  more (all in --json --all)` (plain `--json` keeps the top five; `--all`
  lists every project), the models screen `(all in --json)` (`models` is never
  cut), and the detail screen's day, week and month tables `N earlier rows not
  shown (all in --json --by day)`; the interactive screen, which takes no
  command, says to quit first and names the window on show (`+ N more (quit,
  then run agent-archive stats --days 90 --json --all)`). The skills and MCP
  servers say `+ N more (all in --json --all)` too; `stats --help` and the
  guide say the detail screen lists up to 40 of them and the projects and
  models screens up to 500 rows, not "every one".
- **A `kill -QUIT` no longer leaves the terminal raw.** The interactive
  screens (`list`, `show` and `stats`) turn Ctrl-\ off while they read keys,
  but a SIGQUIT sent from outside dumped goroutines and left the terminal on
  the alternate screen without echo. SIGQUIT is now handled like SIGTERM and
  SIGHUP by every command that stops on a signal (those screens, the pager,
  `backfill`, the storage check in `setup`, `stats` while it reads, and
  `setup --refresh`, which absorbs it while it changes files): the terminal is
  restored and the exit status is 131. The collector and the hooks are
  unchanged.
- **`stats` keeps a command and a name with its count together.** A hint such
  as `(all in --json --by project)` was broken after `--by` on a 40-column
  terminal, and a skill or MCP server could be separated from its count; each
  now stays on one line whenever it fits. And the path of a page saved with
  `h` is printed after a signal ends the interactive screen too, as it is
  after a quit.
- A hook no longer fails with "another collector or setup is running", and
  loses that turn's evidence, when the collector, an import, or
  `agent-archive feedback` writes to the same session at the same moment on a
  busy Mac. Those writers held the session's lock, which a hook waits only a
  second for, through the write's disk syncs, which can take longer; they now
  sync first and hold the lock only to check and rename the file. Subagent
  records are written the same way. Forgetting a session also no longer
  waits, under that lock, for a subagent record another process is writing.
- **The `agent-archive` skill no longer claims the session you are in is
  never matched, and `uninstall --help` names both skills.** The skill said
  the calling session is always skipped, but only Claude Code is known to
  expose a session ID to skip, so it now says "skipped where your agent
  reports it". `uninstall --help` listed only the
  `/handoff` skill; it now names the `agent-archive` skill too. A skill file
  installed by an earlier build shows as out of date until `setup --refresh`.
- A Claude Code subagent resumed after it stopped (continued with
  SendMessage) no longer fails `sync` with "subagent transcript has
  incomplete native timestamp provenance" while it runs. Its archive keeps
  the snapshot from its last stop and catches up at its next stop, or, if it
  never stops again (its session was closed while it worked), once its
  transcript has been quiet for 30 minutes. A subagent resumed before the
  collector first saw it is no longer reported as not captured either. A
  subagent record dated more than 30 minutes in the future is still reported
  as a failure rather than waited on. `sync`
  counts these as "still running", and `status --verbose` and `status --json`
  (`collector.running_subagents`) show them. None is a failure.
- A Claude Code subagent whose transcript is never written (Claude Code
  reports some background agents that way) no longer fails every pass:
  `sync` exited 1 and `status` reported "N session(s) need capture or
  publication" for good. Such a subagent now waits up to 30 minutes for its
  transcript, then is dropped and its parent session records the link as
  unavailable. `sync` counts waiting subagents, and ones it decided not to
  capture, without failing ("; N subagent(s) waiting for transcripts",
  "; N subagent(s) not captured"), and `status --verbose` shows how many are
  waiting. A subagent `backfill` found without a transcript is dropped at
  once. A subagent that is lost for another reason (its transcript exists but
  can't be read, or doesn't match its parent) is dropped too, and reported as
  a failed session by that pass only.
- `status` names what kind of failure kept sessions from syncing, and what to
  do about it, instead of an opaque count ("N session(s) need capture or
  publication", or "failed to scan, publish, or clean up" when cleanup also
  failed). The last error now reads, for example, "2 sessions failed to
  reach storage (network or service unavailable) — check the network and
  the storage service, then run agent-archive sync (the next pass also
  retries)", counts subagents apart from sessions, and covers collection
  and retention failures of the same pass in one message. `sync` prints
  each of a session's errors on its own line. Each session's capture gap in
  `status --json` carries its own next step. When no failed session is
  about storage, status no longer leads with "The last sync failed / Check
  storage access": it names the kind ("Some sessions could not be
  captured") and its next step, or leads with no failure, and shows the
  problems as information rather than ✗ rows, when there is nothing to do
  (a subagent that could not be captured, sessions over the transcript
  size limit).
- Recovering an interrupted setup, or rolling back a failed one, no longer
  gets stuck when another installation has taken over this installation's
  background collector label (an older release under a sandboxed `HOME`, or
  an older binary for another data directory). Setup used to try to start the
  collector over the other installation's job, which launchd refuses, and
  reported "launchctl could not restart the background collector" on every
  rerun until `--abandon-recovery`. It now puts the files back, leaves the
  other installation's job running, and finishes; `setup` then explains that
  the job belongs to another installation (uninstall that one, or set
  `AGENT_ARCHIVE_HOME`). The same holds for a job setup had retired (the
  prototype's upload job, or a collector under an earlier label) whose label
  another installation now runs: recovery used to stop there, leaving the
  jobs after it stopped, and now puts its plist back and finishes.
- A Cursor read no longer fails now and then with "lock a Cursor database
  snapshot directory" when a sweep of leftover snapshots (at the start of
  every collector pass and every backfill command) runs at the moment the
  read starts. The sweep checks whether each snapshot is in use by taking
  its lock for an instant; it could take a new snapshot's lock just before
  the read did. A read's lock file now appears already locked, so the
  sweep sees it in use and leaves it alone.
- The `handoff` picker, its filter, and `handoff "<words>"` no longer offer
  an archived session with no prompt, which has nothing to hand off: one
  uploaded before its first prompt showed as an untitled row and was counted
  in the footer. They already passed over such a session on this machine;
  one archived before its first prompt is offered again once its transcript
  here has one. Its session ID still names it, and `list` and `show` are
  unchanged.

#### Changed

- `status` is shorter, and stays the same length however many projects you
  include. Each app has one line with its sessions (subagents counted
  apart), imports and uploads ("212 sessions (+40 subagents) · 48 imported ·
  3 uploading"), and under it each session still uploading, at most five,
  with its project and start time; only an unusual one says more ("waiting
  for its first upload", or a ! row naming the failure). The storage line
  shows the last upload, and a failure the headline already states is no
  longer repeated in the Storage section: a lone storage failure is named
  in the headline ("The last sync failed: storage refused access"). The
  per-project rows, the included projects, skill evidence, the Imported
  line, why bucket privacy couldn't be verified, and the pending count
  moved to `status --verbose`, where the global imports line is now
  "Imported (all destinations)". Hooks read "hooks on", or "hooks
  installed" while an app that needs approval has run none yet; versions
  drop the app's own name; and the footer names the commands to run
  (`agent-archive status --verbose`, `agent-archive status APP`). While the
  headline says the last pass failed on storage, the destination row says
  so ("refused access on the last pass") instead of "reachable". Cursor
  chats with no transcript are counted on a line of their own, not as
  uploading.
- `status APP` (`claude`, `codex` or `cursor`) shows one app in full: every
  uploading session and a table of its projects with the sessions each
  captured, imported and is uploading, and how far read-back has got, then
  the Storage section.
- `status --json` adds, per application, `subagent_sessions`,
  `imported_sessions`, `uploading_sessions`,
  `waiting_for_transcript_sessions`, and `uploading`, the sessions
  not yet uploaded with their project, start time and state
  (`uploading`, `first_upload`, or `failing` with its `issue`). Nothing
  else in it changed.
- `status --json` adds `collector.issue_counts`, the number of sessions per
  failure code. The fallback code in `collector.session_issues` is now
  `capture_failed` (was `capture_or_publication_failed`, which older status
  files still carry and status still reads), and new codes name storage
  credential, storage availability, retention failures, and subagents that
  could not be captured (`subagent_not_captured`).

#### Earlier development changes (previously labeled v0.2.0)

The archive browser now has bounded, readable listings and terminal pickers for
`list`, `show`, and `handoff`. Scripts should update consumers of `list --json`
to schema version 4, which reports whether the total match count is known.

#### Added
- `list --limit N` caps how many sessions are shown (default 50, newest
  first; `0` for all). A truncated text listing reports
  `Showing N or more session(s)` when the count is unknown, or
  `Showing N of M session(s)` when exact; `list --json` is now `schema_version` 4
  and includes `limit`, `returned`, and `total_matched_known`; it includes
  `total_matched` only when the count is exact and `truncated` when the
  result was cut short.
- On a terminal, `list` (text only) pages through `$AGENT_ARCHIVE_PAGER`,
  else `$PAGER`, else `less -FRX`. Use `--no-pager`, or set either env var
  to empty or `cat`, to print directly. `--json` is never paged.
- Default `list` text is title-first: first filtered prompt preview (or a
  short SESSION_ID when none), relative capture time, harness, project, and
  short ID. `--verbose` restores full IDs, absolute times, origin, parser
  status, and all models/skills.
- On an interactive terminal (stdin and stdout), `list` and bare `show`
  offer a numbered picker to print a session's metadata; `q` quits. Piped
  output and `--json` stay non-interactive. Bare `agent-archive` on a TTY
  opens the same interactive list when already set up.
- Bare `handoff` uses the same numbered picker to select an archived session
  on a terminal. Scripts still supply a session ID, `--latest`, or `--file`.
- `show` accepts a title substring (and short SESSION_ID) when the argument
  is not an exact id; multiple matches use the picker on a TTY.
- Human `list` groups rows under project headings when more than one project
  appears in the page.
- Metadata may include optional `title` (preview of the first filtered human
  prompt) and `project_name` (project basename at publish). Parser version
  is now `0.12.0` so existing sessions get titles on the next metadata
  refresh.
- `status --json` has `collector.last_errors`: each problem the last pass
  recorded, one per entry. `collector.last_error` is unchanged (the same
  problems joined with `; `).

#### Changed

- Long-running CLI steps show a short TTY spinner (registering sessions,
  finishing upload, waiting for the collector, scanning, listing, loading a
  session, looking for past sessions, checking storage). Piped and CI output
  stay plain.

#### Fixed

- `status` shows each problem the last pass recorded on its own ✗ row, and
  a storage provider's error message containing `; ` is no longer split in
  two or shown as the wrong cause.
- A sync's problems no longer hide each other in `status`: a read-back or
  retention failure after collection is shown beside the problems the pass
  already recorded (such as a session over the size limit, or retention
  held by the clock) instead of replacing them.
- `status --verbose` prints each of the last pass's problems on its own
  `Last error:` line.

</details>

## [0.1.1] - 2026-09-28

Setup and status UX polish since the first tagged commit. No filter or
bucket-layout changes.

### Added

- A shared terminal style for setup and status: green ✓, yellow !, red ✗,
  cyan commands, dim details, and your home folder shown as `~`. Plain
  output with `NO_COLOR`, `TERM=dumb`, or when piped is unchanged.
- Golden transcripts of the setup and status screens, in color and plain, so
  changes to what you see are reviewed line by line.
- A plain-words diagnosis for storage failures (no credentials, access
  denied, no such bucket, wrong region, network), each with a fix to try.
  The AWS SDK no longer prints its own warnings to the terminal.
- A read-only check of each app's hook settings file that reports the file,
  line, column and reason for JSONC comments, trailing commas and duplicate
  keys, without changing anything.
- `setup` checks before the first question that each app's hook file is
  valid, that `launchctl` responds, and, when R2 is used, that the Keychain
  opens. A problem stops setup with the file and line to fix.
- At the end of `setup`, an offer to import the past sessions of the
  projects you chose, and a copy-paste `agent-archive setup --yes …` line
  for setting up another Mac.

### Changed

- `setup` suggests `AWS_PROFILE` when it is set, marks AWS profiles that
  have no credentials configured, and defaults to S3 when a usable profile
  exists (R2 otherwise).
- `setup` always shows your recent projects with each one's session count,
  with the current repository pre-selected; `a` selects them all, and
  leaving every project out asks again instead of ending setup.
- `setup` lists the chosen AWS profile's buckets to pick from (an
  `agent-archive*` bucket is pre-selected) and uses the bucket's own region.
  A region that isn't shaped like one is refused, and a saved one of that
  kind is dropped on reconfigure.
- When the storage check fails, `setup` shows the cause once with its fix
  (raw error text only with `--verbose`), the menu defaults to fixing the
  answer that failed, and "Continue where you left off" asks that answer
  again instead of re-running the same check.
- `setup --yes` reports every missing or wrong answer together, one per
  line with the flag that fixes it, instead of stopping at the first.
- `setup` questions and their defaults are bold with a `›` cursor, every
  command to type is cyan, and the storage check shows a spinner that ends
  in ✓ or ✗ on the same line (none when a `credential_process` may prompt).
- The `setup` review is a short list of settings (storage as one `s3://` or
  R2 address) followed by a ✓/!/✗ checklist: storage connected, bucket
  private, hook files valid, and each app's next step. A ✗ blocks starting
  until it's fixed.
- `status` is redesigned: a colored state line, the one fix to make on top,
  and grouped Capture and Storage rows with ✓/!/✗, relative times and `~`
  paths. Internal codes and exact times stay in `status --json`, which is
  unchanged.
- `status --verbose` adds a Details section with the internal codes, exact
  times, bucket privacy evidence and per-app notes, and the default view
  names a storage failure's cause in plain words instead of SDK text.
- `setup` polish: "Nothing, exit" leaves no draft behind, a single-area edit
  drops "Step n of 3", the retention warning shows the local date and only
  appears when sessions are affected, hook-file errors name the real
  problem, and `setup --yes` refuses a public bucket like interactive setup.

## [0.1.0] - 2026-09-25

The first release.

### Added

- **Capture** of Claude Code, Codex, and Cursor sessions on macOS through
  each app's lifecycle hooks, for the projects you include. A LaunchAgent
  filters each session, uploads it with its metadata to your own S3 or R2
  bucket, verifies what arrived by its checksum, and deletes sessions past
  the retention period (90 days by default). A long, growing session costs
  a pass a few seconds, and nothing is downloaded to verify it.
- **`setup`**: three steps (apps and projects, storage, review), resumable if
  interrupted, and safe to rerun to change any of them. It offers the
  projects your apps already have sessions in, accepts the R2 bucket URL the
  Cloudflare dashboard shows, and ends with what each app needs next.
  `setup --yes` takes its answers as flags, to script a second Mac. R2
  secrets are kept in the macOS Keychain; S3 uses an AWS profile, and the
  collector runs with the AWS settings files, CA bundle, endpoint overrides,
  proxy settings and `PATH` setup verified it with (never AWS keys or tokens), so a
  `credential_process` like `aws-vault` or `op`, and their settings for
  where credentials live, work in the background too; `status` says when
  they stop working. Setup refuses a temporary
  binary, such as the one `go run` deletes on exit, and after a plain
  `uninstall` it sets up again with your saved answers.
- `agent-archive` with no arguments says when setup hasn't run yet.
- **`status`**: storage, collector, hook, and per-app capture health, with a
  `Next:` line whenever something needs you. `--json` for scripts.
- **`list`, `show`, `handoff`**: browse the archive from any Mac sharing the
  bucket, and print a session as a prompt another agent can continue from.
- **`backfill`**: import sessions already on this Mac after showing a plan;
  `backfill history` and `backfill undo` review and remove imports.
- **`sync`, `pause`, `resume`, `feedback`, `uninstall`**.
- **`install.sh`**, a one-line installer that checks the release checksum
  and Developer ID signature.
- A [CLI reference](docs/reference/cli.md) generated from the CLI, a
  [glossary](docs/reference/glossary.md), and
  [JSON schemas](docs/reference/schemas.md) validated against real output.

### Security

- **Privacy filter 12.** Hidden reasoning, system instructions, binary
  content, and unknown fields are never uploaded. Credentials are redacted
  by name (`DB_PASSWORD=`, `"api_key":`, `--token`, form fields labelled
  password or PIN) and by shape (AWS, GitHub, Slack, Stripe, GitLab, Google,
  Hugging Face, npm, Groq, and xAI tokens; private keys, also
  base64-encoded as in a kubeconfig; Docker registry logins; JWTs; access
  key IDs; wallet seed phrases; passwords in URLs, `curl -u`, `.netrc`, and
  `.pgpass`). Skill hashes are of the redacted text, so they can't confirm a
  guessed secret. See the
  [filter changelog](dev/specs/privacy-filter-changelog.md).
- Hook files are edited in place: only agent-archive's own `hooks` entries
  change, and a file that can't be edited safely is left alone.
- Handoff output is quoted so session text can't escape it or drive your
  terminal.
- Release binaries are built from `main` after the tests pass, then signed,
  notarized, and attested in a separate protected job.

### Known limitations

- macOS only. Redaction is best effort: a secret with no recognizable name
  or shape is archived as it appears.
- R2's S3 keys can't read bucket privacy settings, so `status` reports R2
  privacy as `not_verified`, and setup reminds you to check that public
  access is disabled in the Cloudflare dashboard.
- Sessions are not encrypted by agent-archive; anyone who can read the
  bucket can read them.
- The background collector gets no other shell settings: an S3 profile
  that needs a helper's variables beyond aws-vault's and 1Password's
  reviewed settings, or a proxy with a password in its URL, works for
  `sync` but not in the background; `status` says when the profile's
  `credential_process` fails there. See
  [configuration](docs/reference/configuration.md#environment-variables).

[Unreleased]: https://github.com/wangjohn/agent-archive/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/wangjohn/agent-archive/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/wangjohn/agent-archive/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/wangjohn/agent-archive/releases/tag/v0.1.0
