# Native session names in Agent Archive

Status: Implemented for supported ordinary-session publication; history-source name refresh and broader native acceptance remain incomplete. Prepared October 6, 2026; status reconciled October 11, 2026. Claude generated/custom titles, Codex external label evidence and collector refresh, and injected-context cleanup are implemented. Codex defaults to verified settled file lookup; bounded native metadata lookup requires explicit `codex_name_lookup: "native"` opt-in because startup can write native state and desktop coexistence remains unverified. Real sidebar/picker comparisons remain pending in the linked acceptance record. The design below preserves the original plan; current code and user documentation own the shipped contract.

Agent Archive should display the name a person recognizes from Codex or Claude Code, using a cleaned prompt preview only when a native name is unavailable. Capture names during collection, retain the filtered evidence, refresh them after renames, and publish them through the existing archive pipeline. Ordinary `list` must continue to work from archived metadata without launching an agent or reading local conversations.

## User visible behavior

For a Codex thread whose sidebar name is `Clean up subagent sessions`, the archive's `Name` should contain that name and the list should display it. The separate `Title` field should remain the first real human prompt's preview. Names and prompts remain searchable through the existing query fields.

For Claude Code, prefer the latest explicit custom name, then the latest native generated title. Keep a custom name even if a later generated-title record appears. When neither exists, use the cleaned prompt preview. Preserve the existing task-description fallback for Claude subagents, which remain hidden from ordinary text listings.

Names still pass through credential redaction, whitespace normalization, terminal escaping and the existing 128-character storage limit. Matching the native UI means matching its name subject to these archive presentation and privacy rules. A name update becomes visible after a successful collector publication; an archived copy on another machine does not require the originating agent to be installed.

## Producer contracts and current implementation

### Codex

The executable found on this machine reports `codex-cli 0.159.2`. This does not establish the producer version of every saved thread. Use each thread's supported history representation and metadata, and pin runtime acceptance to the actual executable tested.

Codex exposes the user-facing name as `thread.name` on metadata responses, including `thread/list` and `thread/read`. A new thread can initially have no name. The API also exposes a distinct `preview`; do not substitute that field for a native name. [Official app-server documentation](https://learn.chatgpt.com/docs/app-server)

Codex 0.159.2 also has `$CODEX_HOME/session_index.jsonl`, containing `id`, `thread_name`, and `updated_at`. Its file order determines the latest update; its native batch resolver keeps the last nonempty name for each requested ID. The producer can rewrite the file when removing entries. This file supplies names, not archive admission or physical rollout selection. [Pinned name-index implementation](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/rollout/src/session_index.rs)

Recent Codex storage distinguishes database `name`, `title`, and `preview`, and legacy and paginated histories have different display rules. Thus a name index alone is not a universal implementation of `thread.name`. Let the native API resolve that distinction; restrict a file-index fallback to explicitly verified producer/history combinations. [Codex storage implementation](https://github.com/openai/codex/blob/main/codex-rs/state/src/runtime/threads.rs)

The repository currently has no Codex name capture. `internal/discovery/index.go` reads a bounded number of SQLite rollout-path hints, deliberately skips live WAL databases, and never selects names. `CodexRolloutLookup` supplies history locators, not labels. Neither should be overloaded to become a name or identity authority.

### Claude Code

The installed CLI reports `2.1.273`. Claude's CLI session picker uses an explicit name when present, otherwise a generated title or other fallback. Unnamed running sessions can have a separate directory-based default display name; that is not the generated session title. CLI, VS Code and desktop histories are distinct surfaces. This release should target the locally captured Claude Code sessions and the CLI/extension names backed by those transcripts, rather than claim support for unrelated desktop-only history. [Official session documentation](https://code.claude.com/docs/en/sessions)

Anthropic's official SDK session reader recognizes `customTitle` and `aiTitle`, preferring custom titles. Its quick reads sample the head and tail, and its summary fallback can use `lastPrompt` and `summary`. Treat the SDK source as evidence of naming semantics, not an invitation to scan arbitrary keys in message/tool payloads. [Official SDK session reader](https://github.com/anthropics/claude-agent-sdk-python/blob/main/src/claude_agent_sdk/_internal/sessions.py)

The generated title is carried by an `ai-title` record with `aiTitle`, while explicit renames use `custom-title` with `customTitle`. Reported native head/tail blind spots can miss an old rename in a long transcript. Retained full-source parsing should preserve explicit-name precedence rather than reproduce that sampling defect. Verify the installed producer during acceptance. [Producer record examples and resolution behavior](https://github.com/anthropics/claude-code/issues/93115)

Current code already filters `custom-title` records in `internal/agents/nativecodec/filter_session_labels.go`, derives their latest value in `parse.go`, and prefers `Metadata.Name` in `internal/archive/session_labels.go`. `ai-title` is not handled by that label filter. The documentation explicitly leaves automatic CLI naming unverified. The existing `TestSessionLabelsLastCustomTitleWins` passes.

## Shared design

Keep `Metadata.Name`, `Metadata.Title`, and `DisplayTitle` as the public display contract. Introduce a narrow optional label-provider capability under `internal/agentapi`, resolved through `internal/agents/builtin`. Shared collector code should not know native database schemas, CLI protocol details, or record field spellings.

The proposed lookup accepts an already admitted registration's canonical harness, native thread ID, and configured source home. Its result distinguishes `present`, `confirmed_absent`, and `unavailable`, and supplies a filtered bounded name, a fixed source identifier, and a revision fingerprint. A budget-limited lookup is unavailable, not confirmed absent. Name evidence cannot create a registration, choose its project, change its parent, or prove which rollout owns its history.

For Codex, implement the provider in `internal/agents/codex` with an injected native process/transport host. For Claude, derive names from the same verified transcript records used by capture; it does not need a second inventory reader or a dependency on Python/Node SDK runtimes.

Retain external Codex names as a new typed `session_labels` supplemental-evidence kind. Extend the current hook-only description of supplemental evidence to include verified native metadata observations; do not invent a lifecycle hook. Its bounded payload contains only native ID, selected name or explicit absence, and fixed provenance/version information. Validate the native ID against the owning source bundle. Filter before local persistence and again before upload. Do not retain the app-server response, database row, index file, unrelated thread names, paths or raw error messages.

Keep a stable observation for an unchanged name. Hash the normalized filtered value and its semantic source state; lookup time must not make every pass appear changed. A new successful name replaces the prior current external-label observation. Unavailable results preserve the last good observation. Only authoritative, complete evidence may clear a name; a missing fallback index entry cannot clear an API-derived name.

This retained evidence allows a parser upgrade to reproduce a Codex name without contacting the local application. New label evidence changes the source checksum: it requires a source publication, even when the transcript is unchanged. Pure re-derivation of existing evidence remains metadata-only. Do not publish a new name solely by overriding metadata while leaving its provenance outside the retained archive.

## Codex collection

### Native API client

Implement a minimal app-server JSON-RPC client used by the collector, not by `list`. Initialize one compatible native process per configured Codex home for a collector pass, reuse it for bounded metadata requests, and close it on completion or cancellation. Use the repository's executable-discovery pattern rather than a hard-coded macOS path. Respect configured homes and keep IDs from different homes separate.

Use `thread/read` for known admitted native IDs with turns/history excluded using the installed protocol's supported parameters. This avoids scanning all projects and avoids the source filters that `thread/list` may apply. If a verified protocol offers a genuinely bounded batch lookup, use it; do not describe a series of `thread/read` calls as one batch RPC. Use `thread/list` only for an explicitly budgeted compatible batch mode with correct pagination and archived/source handling.

Pin the initialize handshake, request shapes and optional name field using the installed binary's generated protocol schema and official source. Accept older compatible responses with missing optional fields. Set limits on startup time, concurrent requests, response bytes, total pass duration and IDs processed; resume deferred IDs with a fair cursor. Discard unsolicited content after bounded decoding. Never start a turn, resume a conversation, set a name, or execute tools to retrieve labels.

Metadata requests do not generate model inference. However, starting an app-server is not a promise that its home is untouched: startup can create state, take locks or migrate storage. Phase 1 must measure those side effects in disposable homes and confirm coexistence with a running desktop app. Do not enable this transport automatically if it disrupts native storage or cannot meet the collector's budgets. In that case, ship only the verified file resolver for its supported history forms and complete the missing storage provider before claiming universal sidebar parity.

### Verified file fallback

Read the existing name index once per home/pass for requested admitted IDs, within byte/record limits. Filter only matching entries and release other values immediately. Resolve the latest usable name by physical file order, not by sorting `updated_at`. Ignore malformed complete records; retry an unfinished trailing record on the next pass. Check opened file identity and handle concurrent appends, truncation and replacement. A partial scan cannot establish absence.

Cache safe matching names and a content-free file signature in archive-owned state. Incremental reads must invalidate on replacement or truncation, and must not make a pruned observation cache look like a complete catalog. An index update for an unrelated ID should not dirty every registration.

Use API-resolved `thread.name` whenever available. Enable fallback only for producer/history combinations proven to mirror native resolution. A stale legacy index must not overwrite a canonical database-backed name. Do not read just the main SQLite database while ignoring its WAL; do not extend the existing discovery hint reader to guess names.

## Claude title capture

Extend the typed label-record rebuild to recognize `ai-title`, keeping only its checked `aiTitle`, session ID and supported identity/timestamp fields. Apply the same redaction as custom titles. Only Claude's adapter may admit these fields. Reject empty/non-string values and preserve existing gap behavior for omitted values. An `aiTitle` string inside a tool input or ordinary user record is not a label.

Track latest custom and generated candidates separately in native parsing and preview accumulation. Select custom over generated regardless of record ordering. Within each candidate class, file order selects the latest usable value. Where a record includes a session ID, require it to match the owning native session; handle legacy missing IDs only under a tested compatibility rule. Do not allow inlined sidechain records or a copied fork prefix to rename the owning session.

Update `PreviewRecord`, `PreviewAccumulator`, and collector previews to carry title class/source so their results follow full-parser precedence. A head/tail-only preview that misses the middle of a large transcript must retain `NameComplete=false`; finding a generated title does not prove there is no custom title in the omitted region. Prefer the archived complete name when available. Do not add unbounded reads to interactive browsing.

Do not reinterpret `lastPrompt` as a session name. Keep `Title` as a first-human-prompt preview. Initially use explicit/generated native titles and the archive's established fallback; if neither exists, exact equivalence with every native picker fallback is outside this release. Add a separate summary-preview design later if needed.

## Prompt fallback cleanup

Classify known Codex app-context blocks such as `external_codex_apps_open_page` as harness metadata. Strip a known context block when mixed with a real request, and skip a context-only message when choosing the first prompt. Apply the same behavior in full parsing and native preview. Preserve a user's discussion or quoted example of such markup under tested rules; do not strip arbitrary XML-like text by heuristic.

For attachment-heavy prompts, use only a verified wrapper to find the request portion. If the wrapper is unfamiliar, preserve the filtered text. This cleanup is the fallback path and must not replace a native name or manufacture one with an LLM.

## Collector change detection and publication

The current `pass.scan` calls `skipUnchanged` before loading published state. `regenerateMetadata` currently runs on parser changes. An external Codex rename therefore needs its own change trigger.

Before per-session skip decisions, perform budgeted label lookups for eligible registrations and record safe results in a small archive-owned cache. Add the effective label fingerprint/provider version to the scan signature or a separate label refresh signature. A changed name owes publication even when transcript stat, parser and adapter versions match. Unavailable lookups do not force a transcript read on every pass; retry label lookup with backoff and preserve the last successful name.

When the transcript is unchanged, build from the retained published bundle plus changed label evidence. Reuse already-filtered native records without reopening the transcript. Publish a new checksum-addressed source followed by metadata through `PendingPublication`, `publishPending`, and `listingindex.PublishRevision`. When conversation content also changed, combine both changes into the normal capture rather than publish twice. Exclude replaceable external label evidence from transcript-extension/rewrite checks; changing a name is not a transcript rewrite.

Name-only refreshes must preserve conversation activity, `CapturedAt`, `StartedAt`, `EndedAt`, ordering and retention basis. Update derivation/publication bookkeeping as required without treating a label's observation time as a new user turn. Add explicit tests for the missing-`EndedAt` fallback, where `CapturedAt` otherwise affects list order. Native Claude bookkeeping records should likewise not extend activity merely because a title was appended.

Retry attempted pending publications with their frozen bytes before incorporating a later rename. Once the pending publication succeeds, the newer fingerprint still owes work. Respect current capture policy, destination, machine ownership and skill rules; labels neither qualify an uncaptured session nor resurrect an expired/deleted one.

Codex history publication now uses verified retained source sets, exact predecessor authority and durable pending journals. Naming work must preserve those lifecycle checks and the permanent config-v5 writer marker. Ship external names for publishable ordinary sessions first; history-source name updates require integration with the revision-aware lifecycle rather than an ordinary metadata override. Do not widen history authority as a side effect of a title feature.

## Migration and compatibility

1. Add reader/filter support and fixtures before enabling the new external-label writer. Update relevant source/metadata schemas if required by the validated evidence shape; optional metadata fields alone do not require an incompatible schema bump.
2. Bump filter and adapter versions for `ai-title` retention and app-context filtering. Bump affected per-agent parser versions for naming, classification and activity changes. Update the filter changelog, version table and goldens.
3. Re-read live Claude transcripts through existing upgrade paths to recover generated titles previously omitted. A parser-only refresh cannot recover an `aiTitle` that an old filter discarded. Re-derive from retained records where evidence already exists.
4. Populate Codex names for existing eligible registrations with the new bounded lookup. Retained sources plus native name metadata can support this when the transcript has rotated, subject to the existing publication/policy checks. Archive-only sessions on another machine keep their recorded fallback until their owning machine can publish name evidence.
5. Refresh stored metadata and listing revisions using the normal journaled writer. Do not perform a destructive purge, import new sessions, or make `list` scan transcripts to repair titles.
6. Change user documentation only when the applicable behavior ships. Describe fallback and missing-source limits accurately. Update list/show, JSON output, privacy, agent capability evidence and maintainer version documentation.

## Implementation sequence

| Phase | Work and repository locations | Exit condition |
| --- | --- | --- |
| 1 | Producer compatibility record and disposable acceptance harness; pin Codex protocol and Claude title records. | Installed Codex API name agrees with the sidebar; Claude custom/generated precedence and asynchronous updates are verified; app-server coexistence and startup effects are understood. |
| 2 | Typed Claude `ai-title` filtering and candidate precedence in `internal/agents/nativecodec`; preview support in `internal/archive/preview.go`; known Codex context classification. | Full parser and previews agree when evidence is complete; generated titles never override explicit names; no broad field allowlisting. |
| 3 | Optional label-provider capability, Codex transport/index provider, typed filtered evidence and archive-owned cache. | Native-ID matching, home isolation, budgets, unsupported formats and unavailable results behave correctly with injected hosts. |
| 4 | Collector label signatures and source-backed refresh in `collector.go`, `skip.go`, `session.go`, `metadata.go`, `publish.go`, and `internal/state`. | Rename-only refresh works, pending retry is durable, list order/retention are preserved, and source/metadata/index stay consistent. |
| 5 | Bounded migration, documentation, performance checks and native acceptance. | Existing representative rows display native names, cold/warm listing budgets hold, and runtime coverage is recorded separately from fixtures. |

Implement sequentially so the producer contract and reader support precede publication. Each phase should have a reviewable change with its relevant validation; no native daemon, framework or second scheduler is needed.

The deferred real UI comparisons have a [manual protocol and pending evidence template](../maintainers/native-session-name-comparisons.md). This follow-up work package does not claim those comparisons have run.

## Tests and release acceptance

Use synthetic fixtures with invented names and IDs. Extend the existing label/privacy/parser tests rather than checking in private transcripts.

- Codex: missing initial name, generated name arriving later, repeated rename with unchanged rollout, API name versus preview, legacy index duplicates and out-of-order timestamps, unfinished append, replacement/truncation, authoritative absence versus unavailable, incompatible protocol, timeout, cancellation, archived IDs and multiple homes. Include database-backed/history cases where the index is absent or stale.
- Claude: generated only, explicit only, both record orders, repeated rename, custom title in the middle of a large transcript, malformed and mismatched IDs, sidechain/fork isolation, missing name, partial preview, and title-only stub. Names alone must not make an empty/non-admitted session eligible.
- Privacy: secrets, control characters, injected context, nested `aiTitle`/`customTitle` in arbitrary tool or message data, unknown keys and overlong names. No unfiltered label appears in local persisted evidence, errors or uploads.
- Publication: no-op repeat scans, no transcript read for external-name-only refresh, unchanged counters/models/activity/retention, retry after each upload stage, intervening rename during pending retry, listing-cache revision replacement, parser upgrade preserving external name evidence, frozen/expired/policy-rejected sessions and the Codex history fence.
- Performance: ordinary `list` opens zero native sources and spawns zero agent processes; collector label requests stay within pass budgets; each index is scanned at most once per pass; unrelated renames do not republish all sessions; safe caches do not accumulate unrelated names.

Run focused package tests first, then repository-required checks. A final live check should compare `agent-archive list` and `show` with the Codex sidebar and Claude session picker, using native IDs rather than name similarity to match rows. Verify a rename without another prompt, delayed automatic naming, custom precedence after further work, restart, and a subsequent unchanged collector pass.

The earlier live archive reads failed with Keychain `OSStatus -50`; archive-backed acceptance must use a working authorized credential environment. Synthetic test success cannot substitute for that check. Creating new inference sessions solely for acceptance is optional; prefer existing sessions and disposable format fixtures, and use paid live runs only when needed and authorized.

## Completion criteria

The feature is complete for a supported producer/storage combination when existing publishable sessions show the native name, later name changes arrive through normal collection, retained evidence reproduces the name, unavailable native metadata preserves a useful fallback, and list performance, privacy, subagent filtering and retention behavior remain correct. Explicitly identify combinations still awaiting native transport or history-lifecycle support instead of declaring universal parity.

Subagent admission/parent-link repair is a separate investigation. Do not infer subagent status from a title or hide a legitimate conversation because its name mentions delegation.
