package cli

import (
	"flag"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

var commandHelp = map[string]string{
	"recover": `Usage: agent-archive recover SESSION_ID [--confirm]

Preview recovery of a local top-level append-only transcript blocked by a
rewrite. Keep its history, feedback and handoffs; freeze its future native
capture, and queue one new archive generation from the current transcript.
Normal sync publishes it. Each generation expires under normal retention.
Repeated confirmation of the old SESSION_ID returns the same successor. Imports
remain in their original undo batch. Existing subagents keep their parent;
new subagents use the active generation. Direct subagent recovery is not
supported: start a fresh parent session instead. No storage is accessed.
Recovery permanently fences older binaries out of this data directory.
  --confirm    Start or resume the previewed generation without prompting
`,
	"eval": `Usage: agent-archive eval export SESSION_ID... | --ids-from - | --scan
       | --file PATH --harness NAME [--detail metadata|full] [--max-bytes N]

Export sessions for an evaluation tool, one JSON Lines record per session
(schemas/eval-export.schema.json), from the archive or from transcripts on
this machine. Read-only and never interactive.
`,
	"eval export": `Usage: agent-archive eval export SESSION_ID... [--detail metadata|full]
       agent-archive eval export --ids-from - [--detail metadata|full]
       agent-archive eval export --scan [--harness NAME] [--project DIR]
               [--since DATE] [--until DATE] [--detail metadata|full]
       agent-archive eval export --file PATH --harness NAME [--detail ...]
       Any of them also takes [--max-bytes N] [--workers N].

Print one JSON line per session: its identity, commits, counts, tokens and
tools, and with --detail full (the default) its filtered human prompts in
order, final response, edited files and feedback. Archived sessions are
named by full SESSION_ID. Transcripts on this machine (--file, --scan, or
absolute paths with --ids-from) need no setup and are filtered as they
would be before upload. With several workers each record is written as its
session finishes. A session that cannot be exported is an error record on
its own line; the others are still printed, and the exit code is 1.
Nothing is uploaded or written.
  --revision REVISION_ID     With one archive SESSION_ID and full detail,
                             export this exact parent native revision
  --detail metadata|full     metadata prints no conversation text and, for
                             the archive, reads only sidecars (default full)
  --ids-from -               Read session IDs or transcript paths from stdin,
                             one a line
  --scan                     Export the transcripts backfill would find here
  --project DIR              With --scan, only this project; repeatable
  --since DATE|TIME|AGE      With --scan, sessions started on or after this
                             local day (as backfill's --since)
  --until DATE|TIME|AGE      With --scan, sessions started on or before it
  --file PATH                Export one transcript; needs --harness
  --harness NAME             The app: for --file, for a path outside the apps'
                             folders, or a session under two apps
  --workers N                Export N sessions at once (default 0: the number
                             of CPUs, up to 8)
  --max-bytes N              Cut each record's longest texts to fit N bytes
                             (default 120000; 0 for no limit)
`,
	"machines revoke": `Usage: agent-archive machines revoke NAME [--include-issued] [--yes] [--json]
       agent-archive machines revoke --machine-id MACHINE_ID [--yes] [--json]
       agent-archive machines revoke --recipient-id RECIPIENT_ID
       [--yes] [--json]
       agent-archive machines revoke --pairing-id PAIRING_ID [--yes] [--json]
       agent-archive machines revoke --operation-id OPERATION_ID
       [--yes] [--json]

Bucket claims never authorize deletion. Verify immutable ownership through a
local assignment, healthy issuer ledger or independently checked operator file.
Unknown ownership refuses even under --yes. No token records a request only.
A provider 404 remains unknown; a request is never proof of access removal.
Self's active object key is deleted last. Sessions and downloaded data remain.
  --machine-id MACHINE_ID   Select an independently bound immutable machine
  --recipient-id RECIPIENT_ID
                    Select a healthy local issuer's recipient lineage
  --pairing-id PAIRING_ID   Select a healthy local issuer's pairing lineage
  --operation-id OPERATION_ID
                    Retry exactly this local operation and confirmed outcomes
  --binding-file PATH
                    Private independent operator binding; never bucket claims
  --include-issued Check visible issuer descendants, including delivered keys
                    Inventory completeness stays unknown; no refill or mint
  --yes             Never prompt; require environment token for deletion
  --json            Write secret-free per-key outcomes; never prompt
`,
	"machines own-key": `Usage: agent-archive machines own-key [--yes] [--cancel]

Stage and check a dedicated key, then commit through setup's transaction.
Existing machine identity remains. Shared access stays valid for other users.
Remove the old local secret only after commit and only when no other local
destination needs it. Record publication retries independently of commit.
  --yes          Require environment token; never prompt or run a command
  --cancel       Remove only a proven uncommitted staged dedicated key
`,
	"machines add": `Usage: agent-archive machines add [--name NAME] [--share-key] [--spares 0..5]
       [--expires 15m] [--print | --file PATH] [--yes]

Create an encrypted pairing file after checking the source storage.
Interactive setup offers a saved file (recommended) or clipboard transfer,
then shows the receiver command and a six-word code on a temporary screen.
An available management token creates
and checks a fresh key; otherwise use a ledger-backed spare. --yes never shares
implicitly. --share-key explicitly selects shared-key pairing without
independent recipient revocation.
S3 transfers settings and a profile name. Configure the profile on the receiver.
Deliver the bundle and six-word code separately. Pairing refuses in any coding
agent, even with --yes or AGENT_ARCHIVE_NONINTERACTIVE=0.
  --name NAME    Recipient name: 1..40 lowercase letters, digits or hyphens
  --share-key    Explicitly share the active R2 key (shared access)
  --spares N     Save unused R2 key target, 0..5 (default 2)
                 Refill with an available management token
  --expires DURATION  Lifetime from 5m to 24h (default: 15m)
  --print        Print the encrypted bundle in this terminal
  --file PATH    Create a private 0600 bundle file; never overwrite a file
  --yes          Require --name and deliberately print bundle and code
                 (with --file, print only the separately delivered code)
Interactive delivery needs terminal input and output; codes use a cleared
alternate screen. Clipboard contents are
cleared on normal exit only if they still equal the bundle. Interrupted delivery
remains uncertain in the local ledger; dedicated keys are never recycled after
attempted exposure. Issuer-local delivered secrets are removed on exit; lineage
remains. Spare refill failure does not invalidate the delivered pairing.
`,
	"machines": `Usage: agent-archive machines [--json] [--verify] [--yes]
       agent-archive machines add [options]

List or add machine records for this bucket. Records are informational and do
not prove identity, current activity, or access removal.

Pair another machine:
  agent-archive machines add    On this machine
  agent-archive setup --pair    On the other machine

Options:
  --json    Write records and observations as JSON
            Never prompt or run a token command
  --verify  Explicit bounded provider metadata check for this R2 bucket
            Account inventory completeness remains unknown; matching metadata
            never proves ownership or access removal
  --yes     With --verify, require CLOUDFLARE_API_TOKEN and never prompt
`,
	"machines rename": `Usage: agent-archive machines rename [CURRENT_NAME|MACHINE_ID] NEW_NAME

Rename this machine only, keeping its immutable machine and credential IDs.
Use the full MACHINE_ID when a current name is ambiguous. The new name has
1 to 40 lowercase letters, digits, or hyphens, starting with a letter or digit.
Observed duplicate names are refused; concurrent naming can still race.
A failed publication keeps the local name and the collector retries.
`,
	"purge": `Usage: agent-archive purge plan [--mode unreferenced|old-filter]
       [--before-filter VERSION] [--no-pager]
       agent-archive purge apply PLAN [--yes]

Create a private five-minute deletion plan, then review its exact keys.
Plan lists still-current older-filter sessions separately and never proposes
their current sources for deletion. Pause every machine uploading to this prefix
before apply. A versioned bucket keeps noncurrent versions and delete markers.
`,
	"purge plan": `Usage: agent-archive purge plan [--mode unreferenced|old-filter]
       [--before-filter VERSION] [--no-pager]

Read metadata and list source objects without deleting anything. Old-filter
mode selects only unreferenced sources whose filter version is below VERSION;
both modes report still-current older-filter sessions separately. On a
terminal, the plan is paged through $PAGER unless --no-pager.
`,
	"purge apply": `Usage: agent-archive purge apply PLAN [--yes]

Pause every uploading machine first. Confirm the plan digest or use --yes.
Apply rechecks remote metadata before each deletion and writes a resumable
report next to the plan. A plan expires five minutes after creation.
`,
	"setup": `Usage: agent-archive setup [--abandon-recovery] [--verbose]
               [--no-skills | --skills] [--allow-network-home]
       agent-archive setup --yes [--provider r2|s3 ...] [--project DIR ...]
               [--codex-discovery on|off]
               [--codex-capture-scope included-projects|all-projects]
               [--prefix PREFIX] [--retention-days DAYS | --keep-forever]
               [--require-skill-use | --no-require-skill-use]
               [--skill-evidence none|metadata|body] [--no-skills | --skills]
               [--allow-network-home]
       agent-archive setup --refresh [--verbose]

Choose apps and projects, connect storage, then review and enable capture.
Run again to continue saved setup or edit capture, storage, or retention.
Credentials are entered privately; never pass them as command arguments.
Setup asks questions, so it needs a terminal, unless --yes is given.
An interrupted setup is recovered on the next run.
  --pair                Receive a pairing file or pasted text and a hidden code
  --pair-file PATH|-    Read a bounded bundle file or stdin; with --yes read and
                        unset AGENT_ARCHIVE_PAIRING_CODE. Never a code flag.
                        Pairing refuses inside coding agents. --yes refuses a
                        destination change; interactive review requires consent.
  --refresh             After upgrading agent-archive: bring the app hooks, the
                        background job's definition, and the skill files up to
                        date for the saved settings and this executable, and
                        change nothing else. Asks nothing and needs no
                        terminal; the installer runs it. Prints "nothing to
                        refresh" when all is current. It refuses, changing
                        nothing, before setup has finished, while a setup
                        needs recovery, after uninstall, when another
                        installation's hooks are in the way, when this
                        executable is a temporary build, or (Linux) when the
                        data directory or the systemd unit directory is on a
                        network filesystem that --allow-network-home never
                        allowed. It points the hooks at the executable now
                        running, which repairs hooks left pointing at one
                        that moved or was deleted. Takes no other flag than
                        --verbose (which lists the files)
  --abandon-recovery    If recovery stops because a file it changed was
                        edited since, keep every file as it is now and
                        discard the interrupted setup; then run setup again
  --verbose             If the storage check fails, also show the storage
                        provider's own error under the diagnosis
  --yes                 Ask nothing: take the answers below, the saved
                        settings, and the apps found; run the same storage
                        check; and save. Refuses if an answer is missing
  --provider r2|s3      Storage provider (default: the saved storage)
  --bucket NAME         Bucket (R2: or from --r2-account's URL)
  --r2-account ACCOUNT|URL
                        The R2 account, or the bucket URL the dashboard shows
  --r2-access-key-id KEY
                        The R2 access key (or AGENT_ARCHIVE_R2_ACCESS_KEY_ID;
                        default: keep the saved key)
  --aws-profile NAME    S3: the AWS profile with access to the bucket
  --region REGION       S3: the bucket's region (default: the profile's)
  --prefix PREFIX       Folder inside the bucket (default: saved folder, else
                        agent-archive/). May be changed alone with --yes
  --retention-days DAYS Keep sessions for 1 to 36500 days (default: saved,
                        else 90)
  --keep-forever        Disable automatic whole-session expiry; conflicts with
                        --retention-days. Provider lifecycle rules still apply.
  --require-skill-use   Capture only sessions that use skills
  --no-require-skill-use
                        Capture sessions with or without skills (default:
                        saved setting, else capture both)
  --project DIR         Capture this project, besides any saved (repeatable)
  --project-repo KEY    Capture a unique local repo by key (repeatable)
                       Skip ambiguous, excluded, or incomplete matches
  --project-scope JSON  Transfer include/exclude rules together; repository
                        subtrees follow the unique local checkout
  --project-scope-file PATH  Read those rules from a file, or - for stdin
                        Scope transfer accepts at most 4096 rules
                        Scope flags need a nonempty value and cannot mix
                        with --project or --project-repo
  --apps LIST           Apps to capture: codex,claude,cursor (default: the
                        saved apps, else those found on this machine). It must
                        name every app set up now: --yes never removes one
  --no-skills           Install no agent skills (such as /handoff), and
                        remove those setup wrote. Later setup runs keep
                        them off until --skills
  --skills              Turn the agent skills back on and install them
  --allow-network-home  Linux: allow the data directory or the systemd unit
                        directory (under your home) on a network filesystem
                        (NFS, SMB, ...), which setup otherwise refuses,
                        changing nothing, because machines that share a home
                        share one identity, cannot rely on file locks, and
                        each run the background job. Only for a home that one
                        machine ever mounts; recorded while it is needed
  --codex-discovery MODE on or off; fresh scripted Codex setup must choose.
                        Reconfiguration keeps an omitted choice. Discovery
                        finds supported sources; recent native copies may count
  --codex-capture-scope MODE included-projects or all-projects (Codex only).
                        Fresh scripts must choose; default: included projects.
                        Omitted reconfiguration never expands recorded scope.
                        all-projects with discovery off uses approved hooks only
  --skill-evidence MODE none: no filesystem skill evidence; metadata: names
                        and filtered hashes; body: filtered SKILL.md text.
                        Fresh setup defaults to metadata; earlier configs
                        without this field keep body until changed.
With --yes, the R2 secret access key is read from
AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY, or else from standard input.
Example: agent-archive setup
Example: printf '%s\n' "$SECRET" | agent-archive setup --yes --provider r2 \
  --r2-account ACCOUNT_ID --bucket BUCKET --r2-access-key-id KEY_ID \
  --project ~/src/app --apps codex,claude
`,
	"status": `Usage: agent-archive status [APP] [--verbose] [--json] [--no-pager]

Show local capture evidence, background health, and a next step.
No conversations are printed and no cloud request is made.
Each app's line counts its sessions, imports and uploads, with at most
five uploading sessions under it; the screen doesn't grow with projects.
APP (claude, codex or cursor) shows that app in full: every uploading
session and a table of its projects.
--verbose adds each project's progress, skill evidence, imports and
every error, then a Details section with the codes, exact times, full
paths and raw errors behind each line.
On a terminal, status is paged through $PAGER unless --no-pager.
--json prints the same status as a versioned JSON document. In it,
storage_verified_at is when setup's storage check last passed, and
storage_access_confirmed_at is the latest confirmation of access (by
"setup" or the "collector", in storage_access_confirmed_by).
Example: agent-archive status claude
Example: agent-archive status --verbose
Example: agent-archive status --json
`,
	"sync": `Usage: agent-archive sync

Collect and upload once. Failures leave pending work available to retry.
If paused, no work is started; run agent-archive resume first.
Example: agent-archive sync
`,
	"pause": `Usage: agent-archive pause

Persistently pause collection, uploads, and cleanup. Current work must finish
before the command can confirm pause; retry if another operation is running.
Already registered sessions can catch up after resume, including activity
written during the pause. New sessions begun while paused are not imported.
Example: agent-archive pause
`,
	"resume": `Usage: agent-archive resume

Resume scheduled collection, uploads, and cleanup. Already registered sessions
can catch up, including activity written during the pause. For an immediate
pass, run agent-archive sync.
Example: agent-archive resume
`,
	"uninstall": `Usage: agent-archive uninstall [--delete-local-data] [--skip-scheduler] [--yes]

Remove hooks, the agent skills (/handoff and agent-archive), and the
background collector. Keep local evidence,
settings, and credentials by default, so setup can restore the installation.
--delete-local-data also removes owned local files and stored credentials,
including unpublished evidence, after a separate confirmation.
--skip-scheduler goes on when the background scheduler cannot be reached (no
user session bus, for example): it tries to stop the job, removes its
definition and the rest all the same, prints the command that stops the job by
hand, and says the job was not verified stopped. Without it, uninstall stops
there and changes nothing.
--yes skips the confirmations; it is required without a terminal.
Remote archives and unrelated files are always kept.
Example: agent-archive uninstall
`,
	"list": `Usage: agent-archive list [WORDS] [options]

Find sessions using metadata; does not download conversation content.
With WORDS (quote them: one argument), list only the sessions they match.
Every word must appear, in any case, in some field of a session: its name,
title, branch, project name, harness, or the start of its SESSION_ID (4
characters or more). A word like #212 or 212 also matches a pull request
number, and never the start of a SESSION_ID. Words may match different
fields, so "linux 212" finds the session named for Linux that opened PR 212.
Inside a project the search looks at that repository's top-level sessions
first, then at every project's, and only then at subagent sessions, in that
order; the first that has a match answers, and a note says how many more match
in other projects. The words are matched against metadata, never the
conversation. Without WORDS, the table and browser list top-level sessions
only: subagent sessions are left out before --limit counts, and the footer
says how many; a parent shows how many it has. --json without WORDS lists
every session, subagents included.
Run inside a project, it lists that repository's sessions (every checkout and
worktree of it, and its sessions from other machines), with a heading naming the
repository; when there are none, it lists all projects and says so. --project
lists another project's, and --all-projects every project's. Outside any
project it lists every session, grouped by project. On a terminal, the a key,
typed alone, switches between the repository and all projects.
Default text columns: TITLE (the Claude Code or Cursor session name when
available, else a preview of its first filtered prompt, else a short
SESSION_ID prefix; Codex uses the prompt preview), PR (the
last pull request the session linked or created, when any row has one),
relative capture time, harness, project, and a short SESSION_ID. A harness or
project every row shares is left out of the table and named in the heading.
On a terminal with an interactive stdin, list a numbered table and pick a
session to show its summary, then t for its transcript, Enter or b to go
back, or q to quit. Keys act as pressed; the wheel, arrows, and PgUp/PgDn
scroll. Press / to filter the rows as you type, with the words WORDS takes:
the first match is highlighted, the arrows move the highlight, Enter shows it,
and Esc clears the filter. A subagent that matches is shown under its parent,
which is shown too. Rows keep their numbers while filtered. WORDS open the
browser with the filter already filled in, to edit. Where keys cannot be read,
the browser reads lines, and an answer that is not a row number, a
SESSION_ID, or q is words to filter by (an empty answer clears them). Piped
or --json output is never interactive, nor is any run with
AGENT_ARCHIVE_NONINTERACTIVE on, as it is inside coding agents (see the
configuration reference). On a terminal without interactive stdin, text is
paged through $PAGER unless --no-pager.
  --harness codex|claude|cursor   Filter by application
  --model NAME                   Filter by model
  --skill NAME                   Filter by skill
  --skill-sha256 HEX             Filter by exact lowercase skill SHA-256
  --skill-usage used|available
                                 How --skill matches (default used).
                                 Non-use queries are unsupported because no
                                 parser proves complete eligibility and use.
  --since DATE|TIME|AGE          Captured at or after a date (2026-01-31,
                                 from midnight UTC; backfill's --since uses
                                 your local day), an RFC 3339 time, or an
                                 age (7d, 12h)
  --complete                     Require complete parser coverage and no
                                 capture gaps
  --imported                     Only sessions agent-archive backfill imported
  --hook-captured                Only sessions hooks captured as they ran
  --replays hide|include|only    Sessions a replay tool ran (with
                                 AGENT_ARCHIVE_REPLAY set): hidden by default
  --limit N                      Show at most N sessions, newest first
                                 (default 50; 0 for all)
  --project DIR|NAME             List this project's sessions: the
                                 repository of a directory, or a project
                                 name (matched to project_name and the
                                 configured project labels, exactly, ignoring
                                 case). Default: the current directory's
                                 repository
  --all-projects                 List every project's sessions (not with
                                 --project). Scripts that read every session
                                 pass this, since list run inside a project
                                 now lists only its own
  --rebuild-index                Rebuild the listing index from live metadata;
                                 scans the full archive and writes index keys
  --verbose                      Full SESSION_IDs, absolute times, origin,
                                 parser status, all models/skills, and title
  --no-pager                     Print directly; do not page through $PAGER
  --no-cache                     Bypass the local metadata cache; indexed
                                 listing verifies current revision headers
                                 (never conversation content)
  --json                         Print {"schema_version": 4, "sessions": [...],
                                 "limit", "returned", "total_matched_known"}:
                                 "total_matched" is present only when exact;
                                 each session is live metadata. "scope"
                                 ({"label", "all_projects", "fell_back",
                                 "outside_matches"}) appears when the
                                 listing looked at a project's sessions
                                 first. Usage errors print no JSON. Never
                                 paged or interactive.
Example: agent-archive list "flaky retention" --json
Example: agent-archive list --skill review-pr --skill-sha256 HASH --since 7d
`,
	"show": `Usage: agent-archive show [SESSION_ID|WORDS] [options]

Print a readable summary of a session's metadata: title, when, app, models,
activity counts, skills, subagents, and capture gaps. --json prints the
metadata sidecar instead. WORDS also work, as in list: every word must appear
in some field of a session (name, title, branch, project, harness, the start
of its SESSION_ID from 4 characters, or a PR number such as #212), looking at
this repository's sessions first. One match is shown; several on a terminal
open the browser on them, with the words in its filter, and without one they
are listed with the command to run next. With --json or --transcript the
browser picks the one session to print instead.
With no SESSION_ID on a terminal, browse sessions as list does: pick one for
its summary, then t for its transcript, Enter or b to go back, or q to quit,
and press / to filter the rows as you type. With --json, it picks one session
and prints its sidecar.
On a terminal, the summary and transcript are paged; in the default less,
scroll with the mouse wheel, arrows, or space, search with /, and quit with
q. Nothing is asked when AGENT_ARCHIVE_NONINTERACTIVE is on, as it is inside
coding agents: give a SESSION_ID.
  --harness NAME        The session's app, if the same SESSION_ID exists under
                        more than one
  --revisions           List active and preserved native revision IDs, capture
                        times and provenance; --json prints the inventory
  --revision REVISION_ID
                        Read one exact parent revision identifier (see
                        --revisions), without resolving children; summary
                        is rederived
  --transcript          Download and verify the source bundle, and print the
                        conversation: prompts, replies, one line per tool call
  --full                With --transcript, also print tool results and shell
                        command output (trimmed)
  --max-bytes N         With --transcript, the output limit, default 120000
                        (about 30k tokens); 0 for no limit. When trimmed, the
                        full version is saved in the data directory for 7 days
                        and its path is named at the end
  --json                Print JSON: the metadata sidecar, which an imported
                        session extends with origin, imported_at, and
                        started_at_source; with --transcript, then the
                        normalized conversation
  --no-pager            Print the summary or transcript directly; do not
                        page through $PAGER
  --normalized          Deprecated: the same as --transcript --json
Example: agent-archive show SESSION_ID --transcript
`,
	"stats": `Usage: agent-archive stats [options]

Show how you use your coding agents. The default screen is a summary of the
last 30 days: estimated spend, sessions and tokens (with the change from the
30 days before), which agents did the work, daily spend, where it went by
project and model, the skills and MCP servers used most, and anything worth a
look. Reads metadata only; prints numbers and names, never prompts or paths.
Cost is an estimate at list price, not a bill, from a dated price table.
Tokens and cost say "unknown" for sessions that record none (Cursor). A
subagent's tokens count with its session. Sessions are placed by capture
time, so imported sessions appear on the day they were imported.
Two columns of bars from 80 terminal columns, one column from 60, a compact
table below that. Color on a color terminal (16 ANSI colors; NO_COLOR turns it
off). Not a terminal: no color, full layout.
On a terminal (stdin and stdout) it opens an interactive screen, unless
--view, --detail, --by, --no-pager, --json or --html is given, or
AGENT_ARCHIVE_NONINTERACTIVE is on, as it is inside coding agents, or TERM is
dumb: o d p m a switch views, w cycles the window (7d, 30d, 90d, or --days and
--since as a custom one; the bar shows the next) without reading again,
arrows, j, k, PgUp, PgDn, space, Home and End scroll, h saves the redacted
page as HTML (it asks for a file name and never replaces one), ? lists the
keys, q quits. Otherwise text is paged through $PAGER unless --no-pager.
  --view overview|detail|projects|models|agents
                                 Which screen to print (default overview):
                                 detail has the streaks, tool errors, token
                                 breakdown, up to 40 skills and MCP servers
                                 and the notes on what the numbers rest on;
                                 projects and models list up to 500 rows,
                                 agents every agent. Not with --json or
                                 --html
  --detail                       The same as --view detail. Not with --view
  --days N                       Window of N calendar days ending today
                                 (default 30; up to 3660)
  --since DATE|TIME|AGE          Window from this local day through today (a
                                 date, an RFC 3339 time, or an age: 7d, 12h;
                                 a date is a local day here, not UTC as in
                                 list). Not with --days
  --by day|week|month|project    Also break the window down that way:
                                 project is --view projects, and day, week
                                 and month add a table to --view detail
                                 (with --json, groups.rows has every row)
  --harness codex|claude|cursor  Only this application
  --model NAME                   Only sessions that used this model (their
                                 other models count too)
  --imported                     Only sessions agent-archive backfill imported
  --hook-captured                Only sessions hooks captured as they ran
  --replays hide|include|only    Sessions a replay tool ran (with
                                 AGENT_ARCHIVE_REPLAY set): hidden by default
  --prices FILE                  Price tokens with the prices in this JSON file
                                 (the built-in table's format), applied on top
                                 of it; the output says so
  --no-cache                     Download every metadata sidecar instead of
                                 reusing the local metadata cache
  --no-pager                     Print directly; do not page through $PAGER
  --json                         Print a versioned document ({"schema_version":
                                 1, ...}) of the numbers: unknown is null,
                                 never 0. Usage errors print no JSON.
  --all                          With --json, list every project, skill and
                                 MCP server, not the top five of each (models
                                 are always all). Only with --json: the --html
                                 page keeps its top lists
  --html                         Write one self-contained web page (inline
                                 styles and SVG; no script, no requests, works
                                 in light and dark and prints) to stdout, or
                                 to --output. Counts and names only. Not with
                                 --json
  --output FILE                  With --html, write the page to FILE (mode
                                 0600, replaced in one step) instead of stdout
  --force                        With --output, replace FILE if it is an
                                 ordinary file that exists
  --include-names                With --html, name the real projects, skills,
                                 MCP servers and models the built-in price
                                 table does not list; by default the page
                                 says project A, skill A, MCP server A,
                                 model A, ... so it can be shared
Example: agent-archive stats --since 2026-09-01 --by project
Example: agent-archive stats --html --output stats.html
`,
	"handoff": `Usage: agent-archive handoff [SESSION_ID|WORDS|--latest|--file PATH] [options]

Before setup, browse Claude Code and Codex native sessions in this checkout
and its descendants. Local IDs (qualified with --harness) or unique prefixes
select beyond the first 50 filtered previews. o loads another 50 explicitly;
/ filters loaded rows only. Word queries report their bounded preview window.
--project requires a directory; --all-projects explicitly broadens the scope.
Local --latest ranks file modification time and requires complete identity
coverage. Direct --to uses an exact current identity or asks for selection;
it never guesses newest. --source archive requires setup. No capture hooks,
credentials, configuration, registrations or scheduler are created. Private
temporary launch files survive launch; later local handoffs clean owned files
older than seven days, best effort. Cursor discovery is not automatic.


After setup, continue a session in another coding agent. On a terminal, pick
a session
(this machine's, including ones not yet uploaded, and archived ones), then pick
where to continue: an installed agent starts in this terminal with the
session as its context (Enter takes handoff.default_to in config.json, else
Codex for a Claude Code session and Claude Code for the others), or print,
copy to the clipboard, or write to a file. Inside Claude Code, /handoff codex
runs handoff --to codex, which opens Codex in a new terminal tab or window.
The session is filtered as it is for the archive: injected instructions and
credentials removed, tool output trimmed, edit bodies left out. A session on
this machine is read from its transcript now; otherwise it is downloaded from
the archive. Piped, or with --output, --format json, or --no-preamble, it
prints without asking. Without a terminal, give a SESSION_ID or WORDS,
--latest, or --file PATH (or --to, from inside an agent). Inside a coding
agent, or with AGENT_ARCHIVE_NONINTERACTIVE=1, it never asks, even on a
terminal.
After setup, WORDS are matched as list matches them: every word must appear,
in any case,
in some field of a session (its name, title, branch, project name, harness, or
the start of its SESSION_ID, from 4 characters), and a word like #212 or 212
also matches a pull request number, never a SESSION_ID. Quote them as one
argument, and use one or two distinctive words: a topic, a PR number, a
branch, or a project name. They look in this machine's sessions first (no
network), then the archive's; a full SESSION_ID wins, and subagent sessions
answer only when no other session matches. Inside a project, the picker and
WORDS look at that repository's sessions first (every checkout and worktree of
it), then everywhere; a note says how many more match in other projects.
On the picker, the a key, typed alone, switches between the repository and
all projects, and / filters the rows as you type (the arrows move the
highlight, Enter hands off the highlighted session, Esc clears the filter; a
subagent that matches shows under its parent). The heading names what is shown,
and a dot marks a session active in the last 2 minutes. Several matches on a
terminal open the same picker on them, with the words in its filter; without
one, or inside a coding agent, they are listed on
stderr, with a PR column and the exact command to run next, and the command
exits 1, never guessing. WORDS skip the agent session running the command,
unless --to is given.
  --latest              The most recent session for the project: one that ran
                        at this path, else one from another checkout of the
                        same repository (its remote origin), such as on
                        another Mac. A match by repository alone is named
                        before anything is downloaded and, on a terminal, asked
                        about (default no); with no terminal it is refused
                        and the SESSION_ID command printed, since a
                        repository chooses its own origin
  --project DIR|NAME    The project to pick from and search, in place of
                        the current directory's repository: a directory, or
                        a project name (matched to project_name and the
                        configured project labels, exactly, ignoring case).
                        With --latest, the directory it searches, and where
                        the agent starts
  --all-projects        Pick from and search every project
  --harness NAME        claude, codex, or cursor
  --file PATH           Render a native transcript directly (needs --harness);
                        works for sessions the archive never captured
  --revision REVISION_ID
                        Read this exact parent native revision; requires a
                        SESSION_ID and --source archive (no local/latest)
  --source auto|local|archive
                        Where the content comes from: auto (default) reads
                        this machine's transcript when there is one, else the
                        archive; local or archive uses only that one
  --max-bytes N         Output limit, default 120000 (about 30k tokens); 0 for
                        no limit. When trimmed, the full version is saved for
                        7 days and its path is named at the end
  --format markdown|json
                        markdown (default) prints the prompt; json prints
                        the structured handoff document it is rendered from
  --output FILE         Write to FILE (mode 0600) instead of printing it
  --force               With --output, replace FILE if it exists
  --no-preamble         Omit the note addressed to the receiving agent
  --to NAME             Start claude, codex, or cursor without asking (its
                        CLI must be on PATH). It reads a private copy of the
                        handoff, kept 7 days. With no session named, hands
                        off the agent session it runs in, else picks one on
                        a terminal. On a terminal the agent runs there;
                        otherwise, or inside a coding agent, it opens in a
                        new tmux window, iTerm2 or Ghostty tab, or Terminal
                        window, and the command returns. If the session was
                        active in this checkout in the last 2 minutes, a
                        terminal asks first (inside an agent it only warns)
  --here                Run the launched agent in this terminal
  --new-window          Open the launched agent in a new window or tab
  --worktree            Start the agent in a new git worktree beside the
                        checkout, on a new branch at HEAD, carrying
                        uncommitted changes and untracked (not ignored) files
  --branch NAME         With --worktree, the new branch (default: handoff/
                        and the first 8 characters of SESSION_ID)
  -- ARGS               Everything after -- goes to the launched agent, after
                        any arguments set in config.json's handoff.args
Example: agent-archive handoff
Example: agent-archive handoff --to codex
Example: agent-archive handoff "fix the auth bug" --harness codex --to claude
Example: agent-archive handoff "#212"
Example: codex "$(agent-archive handoff --latest --harness claude)"
Example: claude "$(agent-archive handoff --latest --harness codex)"
Example: agent-archive handoff SESSION_ID --to claude --worktree
Example: agent-archive handoff SESSION_ID --to codex -- --model o3
`,
	"backfill": `Usage: agent-archive backfill [options]
       agent-archive backfill history
       agent-archive backfill undo [IMPORT_ID] [--project DIR] [--yes]
                                [--restore-retention]

Import the Claude Code, Codex, and Cursor sessions already on this machine that
the archive has not captured. First shows each project with its session count
per app, and why any session is not imported; nothing is written until you
confirm. Live repositories can be proposed for matching deleted worktrees.
Projects the import needs are added to capture. Prints project
folders, counts, and recovery reasons, never conversation content.
After confirmation, filtered evidence is staged before admission; uploads can
finish after native files disappear. Staging shares a 1 GiB quota with pending
publication. History reports admitted and remotely verified uploads separately.
  --harness NAME        Only claude, codex, or cursor (repeatable)
  --project DIR         Only this project; it need not still exist (repeatable)
  --map-project OLD=ROOT Exact missing cwd to included root (repeatable)
  --since DATE|TIME|AGE Sessions started on or after this local day (list's
                        --since uses UTC): a date (2026-09-01), an RFC 3339
                        time, or an age (7d, 12h); a time or age selects
                        from the start of its local day
  --until DATE|TIME|AGE Sessions started on or before this local day
  --include-home        Include sessions run from the home folder
  --include-temp        Include sessions run from temporary directories
  --include-removed     Include sessions retention or undo removed
  --dry-run             Print the plan and exit; nothing is written
  --json                With --dry-run, print the plan as JSON
  --yes                 Skip the confirmation (required without a terminal)
  --background          Stage and register sessions, then exit; the background
                        collector verifies and uploads them
See agent-archive help backfill history and agent-archive help backfill undo.
Example: agent-archive backfill --dry-run --since 30d
`,
	"backfill history": `Usage: agent-archive backfill history

List past imports, oldest first: each import's IMPORT_ID, when it started,
how many sessions and projects it added, and its upload state (waiting,
uploaded, interrupted, or undone). Waiting counts sessions not in the archive
yet; archived sessions that are rechecking (a re-scan is still owed) or
updating (newer work is still to upload) are counted after it.
Reads this machine's records only.
Example: agent-archive backfill history
`,
	"backfill undo": `Usage: agent-archive backfill undo [IMPORT_ID] [--project DIR] [--yes]
                                [--restore-retention]

Remove the latest import, or the import IMPORT_ID from backfill history: its
sessions are deleted from the bucket and this machine, and the projects it added
are excluded from capture. Shows what it will do and asks first.
Hook-captured sessions and the apps' own files are never touched. If the
import raised retention, undo offers to put the shorter retention back,
saying how many sessions (from anywhere, not only the import) that deletes.
  --project DIR         Only this project's sessions from the import;
                        retention is left alone
  --yes                 Skip the confirmation (required without a terminal);
                        retention is left as it is
  --restore-retention   With --yes, also put back the retention from before
                        the import, deleting sessions older than it
Example: agent-archive backfill undo --project ~/src/old-experiment
`,
	"version": `Usage: agent-archive version

Print the installed version (also: agent-archive --version, -v).
Example: agent-archive --version
`,
	"feedback": `Usage: agent-archive feedback SESSION_ID --file PATH

Attach an explicit user assessment to a locally captured session. The file is
read locally, privacy-filtered, and queued for the next collection pass. Its
path is not archived. While capture is paused, it waits until you resume.
Example: agent-archive feedback SESSION_ID --file /private/path/feedback.txt
`,
}

// Preflight never resolves paths, credentials, or runtime dependencies.
func commandPreflight(args []string, out, errOut io.Writer) (bool, int) {
	cmd := args[0]
	//lint:ignore LV1001 cmd is raw argv; these are the spellings that ask for help
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		if len(args) == 1 {
			terminal.Print(out, usage)
			return true, 0
		}
		if cmd == "help" && (len(args) == 2 || len(args) == 3) {
			if help, ok := commandHelp[strings.Join(args[1:], " ")]; ok {
				terminal.Print(out, help)
				return true, 0
			}
		}
		terminal.Println(errOut, "Use agent-archive help COMMAND.")
		return true, 2
	}
	//lint:ignore LV1001 cmd is raw argv; these are the spellings that ask for the version
	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		if len(args) == 2 && isHelpFlag(args[1]) {
			terminal.Print(out, commandHelp["version"])
			return true, 0
		}
		if len(args) != 1 {
			terminal.Printf(errOut, "agent-archive: version: unexpected argument %q; run agent-archive version --help\n", args[1])
			return true, 2
		}
		return false, 0
	}
	help, public := commandHelp[cmd]
	if !public {
		return false, 0
	}
	// A subcommand (backfill undo, backfill history) has help of its own.
	if len(args) > 1 {
		if sub, ok := commandHelp[cmd+" "+args[1]]; ok {
			help = sub
		}
	}
	if slices.ContainsFunc(args[1:], isHelpFlag) {
		terminal.Print(out, help)
		return true, 0
	}
	// Each command's own flag set is the one source of truth for its
	// arguments, and it validates them before any I/O.
	return false, 0
}

// isHelpFlag reports whether a raw argument asks for help.
func isHelpFlag(arg string) bool {
	//lint:ignore LV1001 arg is raw argv, checked for a help flag
	return arg == "--help" || arg == "-h" || arg == "-help"
}

// commandFlags is a public command's flag set. Every command reports a bad
// command line the same way, through usageError: one line on stderr naming
// the problem and the command's help, and exit 2. The flag package's own
// messages and usage dump (with single-dash flag names) never reach the
// user.
type commandFlags struct {
	*flag.FlagSet
	errOut  io.Writer
	catalog agentmeta.Catalog
}

// newCommandFlags returns the flag set of command, named as the user types
// it ("backfill undo").
func (e Env) newCommandFlags(command string, errOut io.Writer) *commandFlags {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	flags := &commandFlags{FlagSet: fs, errOut: errOut, catalog: e.agentRegistry().Catalog()}
	if e.observeFlags != nil {
		e.observeFlags(flags)
	}
	return flags
}

// usageError reports a problem with the command line and returns exit
// code 2.
func (f *commandFlags) usageError(format string, args ...any) int {
	terminal.Printf(f.errOut, "agent-archive: %s: %s; run agent-archive %s --help\n", f.Name(), fmt.Sprintf(format, args...), f.Name())
	return 2
}

// parse parses the flags in args, leaving positional arguments in Args. It
// reports a bad flag and returns false; the command then exits 2.
func (f *commandFlags) parse(args []string) bool {
	if err := f.Parse(args); err != nil {
		f.usageError("%s", describeFlagError(err))
		return false
	}
	return true
}

// parseFlagsOnly is parse for a command that takes no positional
// arguments: one is reported like a bad flag.
func (f *commandFlags) parseFlagsOnly(args []string) bool {
	if !f.parse(args) {
		return false
	}
	if f.NArg() != 0 {
		f.usageError("unexpected argument %q", f.Arg(0))
		return false
	}
	return true
}

// parseWithArgument is parse for a command that takes at most one
// positional argument (a session ID), before or after its flags: the flag
// package otherwise stops at the first positional value. It returns the
// argument, or "" when there is none.
func (f *commandFlags) parseWithArgument(args []string) (string, bool) {
	argument, _, ok := f.parseWithOptionalArgument(args)
	return argument, ok
}

// parseWithOptionalArgument is parseWithArgument that also reports whether
// the argument was given, so an empty one ("") can be told from none.
func (f *commandFlags) parseWithOptionalArgument(args []string) (argument string, given, ok bool) {
	if !f.parse(args) {
		return "", false, false
	}
	if f.NArg() == 0 {
		return "", false, true
	}
	argument = f.Arg(0)
	if !f.parse(f.Args()[1:]) {
		return "", false, false
	}
	if f.NArg() != 0 {
		f.usageError("unexpected argument %q", f.Arg(0))
		return "", false, false
	}
	return argument, true, true
}

var flagValueError = regexp.MustCompile(`^invalid (?:boolean )?value ("(?:[^"\\]|\\.)*") for (?:flag )?-+([^:]+): (.*)$`)

// describeFlagError rewrites the flag package's error in the CLI's terms,
// with flags spelled the way help shows them (--name).
func describeFlagError(err error) string {
	message := err.Error()
	flagName := func(s string) string { return "--" + strings.TrimLeft(strings.TrimSpace(s), "-") }
	switch {
	case strings.HasPrefix(message, "flag provided but not defined: "):
		return "unknown flag " + flagName(strings.TrimPrefix(message, "flag provided but not defined: "))
	case strings.HasPrefix(message, "flag needs an argument: "):
		return flagName(strings.TrimPrefix(message, "flag needs an argument: ")) + " needs a value"
	case strings.HasPrefix(message, "bad flag syntax: "):
		return "bad flag " + strings.TrimPrefix(message, "bad flag syntax: ")
	}
	if m := flagValueError.FindStringSubmatch(message); m != nil {
		return fmt.Sprintf("invalid value %s for --%s: %s", m[1], m[2], m[3])
	}
	return message
}
