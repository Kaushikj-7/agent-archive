# Configuration

`agent-archive setup` writes the configuration; every other command reads
it. It lives in `config.json` in the data directory (see
[local state](local-state.md)) and holds no secrets. Edit it through setup,
not by hand (except the optional MCP display names and handoff preferences below): setup checks storage, rewrites hooks, and keeps the fields
consistent with each other.

## Fields

| Field | Meaning |
| --- | --- |
| `mcp_server_names` | Optional map, edited by hand; setup keeps it. Exact recorded MCP server IDs mapped to display names, for example `{"opaque-server-id": "GitHub"}`. Stats uses these labels in terminal output and HTML with names included; JSON preserves each original `name` and adds `display_name`. Unknown UUIDs receive neutral labels rather than guessed service names. |
| `schema_version` | Shape of this file. Currently `1`. |
| `machine_id` | This machine's random identity, written into every session it captures. Kept across reconfiguration, and copied with the data directory by Migration Assistant, a backup restore, or a VM or container clone; see [multiple machines](../guides/multiple-machines.md#migration-assistant-and-time-machine-macos) and [on Linux](../guides/multiple-machines.md#cloned-machines-on-linux). |
| `host_id` | Linux only: a digest of the machine ID (`/etc/machine-id`) of the machine setup first ran on, recorded once beside `machine_id` and never uploaded. `status` and `setup` warn when it differs from the machine they run on, which means the data directory was copied (a cloned VM or container image). If the original is retired, or the operating system was reinstalled on the same machine, remove the entry and setup records the current machine. Absent on macOS and where there is no machine ID (or only one made anew at every boot). |
| `allow_network_home` | Linux only: `true` after `agent-archive setup --allow-network-home`, which allows the data directory or the systemd unit directory to be on a network filesystem (NFS, SMB/CIFS and the like), which setup and `setup --refresh` otherwise refuse. Setup records it only while a directory is on one, and drops it when none is. `status` warns of the network filesystem whether or not it is set. Absent otherwise, on macOS always, and in a config from before the field; older binaries ignore it. See [a home directory shared across machines](../guides/multiple-machines.md#a-home-directory-shared-across-machines). |
| `storage.Provider` | `s3` or `r2`. |
| `storage.Bucket`, `storage.Prefix` | The bucket and the folder inside it (`agent-archive/` unless changed in setup). Every object key is under the prefix; see [bucket layout](bucket-layout.md). |
| `storage.Region` | S3 region (from the AWS profile when it has one). |
| `storage.AWSProfile` | S3 only: the named AWS profile credentials are loaded from. No other credential source is used. |
| `storage.R2AccountID`, `storage.R2Endpoint` | R2 only: the account or S3-compatible endpoint. |
| `storage.R2CredentialRef` | R2 only: the reference of the access key in the credential store: a Keychain item (service `agent-archive`) on macOS, or `credentials/<reference>.json` in the data directory on Linux, which has no Keychain. The secret itself is in the store, never in this file. |
| `archive.enabled` | Whether hooks and the collector capture anything. |
| `archive.projects[]` | Included and excluded projects: `root` (resolved path), `project_id` (derived from the root), `included`, and `activated_at`. Only sessions admitted at or after a project's activation are captured for it. |
| `harnesses` | Apps setup installed hooks for: `claude`, `codex`, `cursor`. |
| `imported_harnesses` | Apps whose sessions `backfill` imported without their hooks installed. |
| `declined_harnesses` | Apps you declined when setup offered them; setup doesn't offer them again. |
| `hook_files` | Per app, the hook file setup installed into, resolved from `CLAUDE_CONFIG_DIR` and `CODEX_HOME` at setup time. |
| `installed_executable` | The `agent-archive` path written into the hooks and the background job's definition. |
| `background_backend` | The scheduler that runs the background collector, as setup recorded it: `systemd` on Linux. Status, uninstall, `setup --refresh` and recovery use this one and never pick another. Setup leaves it out for launchd, and an absent field means launchd on macOS and systemd on Linux, for all time, so a macOS `config.json` is what it always was. A build that predates the field ignores it. |
| `retention_days` | Whole-session retention in days: 90 by default; 1 to 36,500 for finite retention, or 0 for **Keep forever**. Missing legacy values default to 90. Forever disables whole-session expiry, while superseded-source/privacy cleanup and explicit deletion continue. Provider lifecycle rules can still delete objects. |
| `require_skill_use` | When `true`, only sessions that used a skill are captured. Default `false`: all sessions. |
| `codex_name_lookup` | Optional `files` (default) or `native`. Native explicitly permits a bounded Codex app-server startup for admitted ordinary session names. Startup can write Codex databases/system skills; desktop coexistence and real authentication are unverified. See [Codex session names](codex-session-names.md). |
| `skill_evidence` | `none` omits filesystem skill inventory and snapshots; `metadata` uploads skill names and hashes of filtered skill text but no body; `body` uploads filtered SKILL.md snapshots. Fresh setup saves `metadata`. A schema 1 config without this field retains `body` and setup labels it “kept from previous setup”. Skill use detected in a native transcript can still satisfy `require_skill_use` with `none`. |
| `no_skills` | `true` after `agent-archive setup --no-skills`: setup installs no agent skills (such as `/handoff`) and removes the ones it wrote, and later setup runs keep it. `setup --skills` removes the field. Absent means skills are installed, as in a config from before the field. |
| `paused` | Set by `pause`, cleared by `resume`. |
| `handoff.args` | Optional, edited by hand; setup keeps it. Per destination (`claude`, `codex`, `cursor`), arguments `handoff --to` passes before any given after `--`, for example `{"codex": ["--model", "o3"]}`. |
| `handoff.default_to` | Optional, edited by hand. Per source harness, the destination `handoff` offers first, for example `{"claude": "codex"}`. Unknown agent names are refused when the file is read. |
| `destination_since`, `previous_destinations` | When the current storage destination was configured, and the ones it replaced. Sessions stay with the destination they were published to. |
| `storage_verified_at`, `bucket_privacy` | The last storage access check and bucket privacy inspection. Evidence, not settings. |
| `retired_machine_assignments` | Up to 128 nonsecret dedicated assignments retired by committed setup, retaining destination, provider, recipient, issuer and slot bindings for explicit ordinary revocation. Never imported from a pairing or draft. |
| `retired_credential_refs` | Credential references (Keychain items, or credential files) of R2 credentials a reconfiguration replaced, kept so `uninstall --delete-local-data` can remove them too. |

## Environment variables

| Variable | Effect |
| --- | --- |
| `AGENT_ARCHIVE_HOME` | Data directory instead of `~/.local/share/agent-archive`. It must not be inside a Git checkout. A non-default directory gets a background job of its own (launchd label `com.agent-archive.collector.<hash>`, systemd unit `agent-archive-collector-<hash>`), and setup writes it into the hook commands, since apps run hooks without your shell's environment. Each installation changes only the hooks that carry its own directory; setup refuses to install beside another installation's hooks, so give a second or test installation its own `HOME` too (or `CLAUDE_CONFIG_DIR` and `CODEX_HOME`). |
| `XDG_CONFIG_HOME` | Linux only. Where Cursor keeps its chat database (`$XDG_CONFIG_HOME/Cursor`, else `~/.config/Cursor`); a relative or empty value counts as unset. Setup records it, when set to an absolute path, in the background job's environment so the scheduled collector and a backfill from your shell look in the same place, and `status` warns when the shell's value later differs from the job's. It does **not** say where the systemd units go: they are always in `~/.config/systemd/user`. A user manager can have its own value from outside your shell (`environment.d`, a desktop session's `import-environment`); if your shell had none when setup ran, the job uses the manager's, and `status`, which compares only your shell's value with the recorded one and cannot see the manager's, cannot warn about a difference. To close that gap, export the same value in your shell and run `agent-archive setup` again. Never recorded on macOS, where Cursor's path ignores it. |
| `XDG_CACHE_HOME` | Linux only. The cache home, under which Cursor database copies live (`$XDG_CACHE_HOME/agent-archive/cursor-snapshots`, else `~/.cache/agent-archive/cursor-snapshots`, where `~` is the account's home from the user database). Recorded in the job's environment and compared by `status` exactly like `XDG_CONFIG_HOME`, with the same limit. |
| `CLAUDE_CONFIG_DIR` | Claude Code's configuration directory, where setup installs hooks (`settings.json`). Read when setup runs; recorded in `hook_files`. |
| `CODEX_HOME` | Codex's home, where setup installs hooks (`hooks.json`). Read when setup runs; recorded in `hook_files`. |
| `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE` | Where the AWS SDK finds profiles, as for the AWS CLI. The background collector runs under launchd or systemd, which pass it none of your shell's environment, so for S3 setup writes the ones set in its shell (as absolute paths) into the collector's job definition (the LaunchAgent's plist, or the systemd service's `Environment=` lines), along with that shell's `PATH`, so a `credential_process` such as `aws-vault`, 1Password's `op`, or `granted` in `/opt/homebrew/bin` runs there too. The `PATH` keeps the shell's entries that are existing directories not writable by every account, then the scheduler's own default (launchd's `/usr/bin:/bin:/usr/sbin:/sbin`; systemd's `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`). Change either and run setup again; `status` warns when the collector's files are gone, its `PATH` no longer finds the profile's `credential_process`, or your shell's files differ from the collector's. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN` are never copied: the profile supplies credentials. |
| `AWS_CA_BUNDLE`, `AWS_ENDPOINT_URL`, `AWS_ENDPOINT_URL_S3`, `AWS_ENDPOINT_URL_STS`, `AWS_ENDPOINT_URL_SSO`, `AWS_ENDPOINT_URL_SSO_OIDC` | A CA bundle (for a network that inspects TLS) and endpoint overrides, as for the AWS CLI. For S3, setup copies the ones set in its shell into the collector's job definition, like the files above (an endpoint URL with a user name or password in it is not copied). |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (either case) | The proxy for S3 and credential requests, as for any Go program. For S3, setup copies the ones set in its shell into the collector's job definition. A proxy URL with a user name or password in it is not copied, and setup warns that the collector runs without it. |
| `AWS_VAULT_BACKEND`, `AWS_VAULT_KEYCHAIN_NAME`, `AWS_VAULT_PROMPT`, `AWS_VAULT_PASS_PREFIX`, `AWS_VAULT_FILE_DIR`; `OP_ACCOUNT`, `OP_CONFIG_DIR` | Where aws-vault and 1Password's `op` find credentials, when a profile's `credential_process` runs one of them. For S3, setup copies the ones set in its shell into the collector's job definition (the directories as absolute paths, or as written when they start with `~/`). The same helpers keep secrets in variables too (`AWS_VAULT_FILE_PASSPHRASE`, `OP_SERVICE_ACCOUNT_TOKEN`, `OP_SESSION_*`); those are never copied. |
| `AGENT_ARCHIVE_R2_ACCESS_KEY_ID`, `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY` | An R2 key. `setup --yes` reads them (see [setup](../getting-started/setup.md#set-up-without-questions)). On Linux, which has no Keychain, they are also a read-only fallback: when no credentials file exists for the saved reference, the R2 key is read from them, which suits a container. The fallback applies to a process that has the variables (a container, a service with `EnvironmentFile=`); a scheduled collector does not inherit an interactive shell's variables, so `setup` stores the key in the credentials file, and does not treat an exported key as one it has already stored (it asks for the key, or takes it from these variables under `setup --yes`, and saves it to the file). They are never written, and a credentials file that exists but is not private is refused rather than replaced by them. On macOS they are never read outside `setup --yes`. |
| `AGENT_ARCHIVE_NONINTERACTIVE` | `1` (also `true`, `yes`, `on`) stops agent-archive from asking anything: no session picker or browser, no confirmation prompt, no pager, no alternate screen or mouse handling, whatever stdin and stdout are. `0` (`false`, `no`, `off`) turns that off. Any case; a value that is neither is refused with a usage error (exit 2) before the command runs. Unset or empty means automatic: on when `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or `CURSOR_AGENT` is set, since a coding agent's shell, which may be a terminal, is running the command, and off otherwise. While it is on, a command behaves as when piped: several matches print the candidates on stderr and exit 1, `handoff` and `show` without a session usage-error, and `setup`, `uninstall`, `backfill`, and `purge apply` refuse to ask (naming the switch and `AGENT_ARCHIVE_NONINTERACTIVE=0`; `--yes` still skips the question, and `setup --yes` never reads the R2 secret from a terminal, only from `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY` or a pipe). Colour and line redrawing follow `NO_COLOR` and the terminal, not this. |
| `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID` | Set by the app for commands it runs; `handoff --latest` skips that session, `handoff --to` with no session named hands it off, and they turn `AGENT_ARCHIVE_NONINTERACTIVE` on. |
| `CURSOR_AGENT` | Set by Cursor's agent for commands it runs; `handoff --to` with no session named hands off the newest Cursor session for the directory, and it turns `AGENT_ARCHIVE_NONINTERACTIVE` on. |
| `NO_COLOR` | Disables colored output. |
| `AGENT_ARCHIVE_TRACE` | `1` (also `true`, `yes`, `on`) prints a timing tree on stderr when a command finishes: each step's start, duration, and counts, with no session content ([A slow list, show, or handoff](../guides/troubleshooting.md#a-slow-list-show-or-handoff)). Any other value leaves it off. The hooks' and collector's internal commands never trace. |
| `AGENT_ARCHIVE_PAGER`, `PAGER` | The pager for long output on a terminal (`list`, `show`, `status`, `purge plan`); `AGENT_ARCHIVE_PAGER` wins. Empty or `cat` disables paging. With neither set, or one set to a bare `less`, `less` with mouse-wheel scrolling and key hints. Any other pager runs as given, with `LESS=FRX` and `LV=-c` added when those are unset ([Scrolling](../guides/list-and-show.md#scrolling)). |
| `AGENT_ARCHIVE_VERSION`, `AGENT_ARCHIVE_INSTALL_DIR` | `install.sh` only: the release and directory to install. |
| `AGENT_ARCHIVE_HOME` (in `install.sh`) | The installer looks for `config.json` here (or in the default directory) to decide whether to run `agent-archive setup --refresh` after installing; see [the install guide](../getting-started/install.md). |

Nothing else from your shell reaches the background collector. An S3 profile
that works only with other variables set, such as another helper's settings
or a proxy that needs a password, passes setup's storage check but fails in
the background. When a profile's `credential_process` fails there, `status`
says so and suggests what to check; what the helper printed is never
recorded, since it can contain credentials. Put settings the collector
doesn't get where the helper reads them without the shell (the AWS profile
itself, or the helper's own configuration file), or run
`agent-archive sync` from your shell.

Cursor's hook file is always `~/.cursor/hooks.json`.

Skill observation scans documented project roots and user-level skill roots,
including roots outside the selected project. It follows user-level links only
within the selected skill root. Change the policy with interactive setup's
“Skill evidence” setting or `setup --yes --skill-evidence none|metadata|body`.
The change applies to future publications, including rebuilt pending work;
it does not delete older local copies or bucket objects. See
[privacy](../security/privacy.md) for cleanup guidance.

## Machine labels and credential provenance

`machine_name` is an optional chosen label (1 to 40 lowercase letters, digits,
or hyphens, starting with a letter or digit). Setup defaults to `unnamed-` plus
four characters of the immutable local `machine_id`, without reading a hostname.
Interactive first setup offers an optional name at final review.
Use `agent-archive machines rename NEW_NAME` to change it safely.

`machine_assignment` is optional nonsecret locally committed provenance for
one `destination_id`. It has `kind` (`aws_profile`, `r2_unknown`, `r2_shared`,
or `r2_own`), `access_key_id`, `recipient_id`, `issuer_id`, `slot_id`,
`shared_with`, `pairing_id`, `paired_from`, and `paired_at` when applicable.
IDs other than the destination digest are canonical 32-character lowercase hex.
No secret, code or bundle belongs here. A different destination invalidates an
old assignment; bucket claims cannot populate or replace this local evidence.
Older configurations without these fields remain readable. The local
`machine-registration.json` acknowledgement binds successful publication to
its destination, stable record fingerprint and last success; failed publication
stays pending and is retried by the collector without another setup.

Resuming an ordinary setup draft keeps the latest committed machine name and
credential provenance for unchanged credentials. Changing the destination or
credential reference clears old provenance; a draft cannot restore it.

### Dedicated key spares

`spare_keys` is an optional integer from 0 through 5; absence means 2. Zero
suppresses spare reservation/refill without deleting existing provider keys.
`spare_credential_refs` contains only opaque `issued-<32 lowercase hex>` references
and is an advisory index. Validated private `issued/slot-<id>.json` records own
eligibility and immutable destination/recipient/issuer/slot lineage. Refilling occurs
only during explicit `machines add` or guided R2 creation while a
management token is available. Listing, collection and revocation never refill.
Dedicated issuance is available without an experimental flag.

## Management token source

`cloudflare_token_command` is an optional argv array, for example
`["op", "read", "op://Private/Cloudflare/agent-archive"]`. Store a reference to
an external secret, never the token itself or a literal secret argument. Guided
R2 creation and `machines --verify` share this source. Both prefer
`CLOUDFLARE_API_TOKEN` and remove that variable before management requests;
removal failure stops the operation. Otherwise an interactive invocation runs
the configured program directly, without a shell, or asks for a hidden token.

The command receives no stdin, has a 20-second deadline and a 4 KiB stdout
limit, and suppresses stderr and failure output. Its environment excludes
credential variables. `--yes`, `--json`, pipes and the noninteractive policy
never run the configured command or prompt; explicit verification in those
modes requires the environment token. Ordinary listing, status and collection
never acquire a management token. See [provider observations](../guides/multiple-machines.md#provider-observations)
for the explicit verification command and limits. This command configuration is local and is not
part of a pairing payload.

`AGENT_ARCHIVE_PAIRING_CODE` supplies the six-word pairing code only to
`setup --pair-file PATH --yes` (or `--pair-file -`). The receiver reads and removes
it from its process environment; it is never saved. Deliver it separately from
the encrypted bundle, and clear it in the parent shell afterward. There is no
code command-line flag. Interactive pairing honors `AGENT_ARCHIVE_NONINTERACTIVE`;
redirected bundle input uses a private terminal for the code and destination review.

## Codex discovery consent

`discovery` records `enabled`, `choice_recorded` (an explicit setup choice),
approved `codex_homes`, and current per-project
`authorizations`. Each authorization has an agent, project root, destination ID,
generation and half-open unpaused intervals. Setup commits these together with
its configuration transaction. Changing effective project scope, destination or
source roots creates a new generation; pause closes the open interval and resume
opens another. A session created before consent or during a closed interval does
not become eligible merely because it is found later.

Use setup's `--codex-discovery on|off` choice instead of editing this ledger.
Disabling discovery retains writer protection and identity history. Protected
configuration uses the schema-2 writer object; explicit Codex capture scope uses
the stronger schema-3 `codex-scope-floor-v3` writer fence so older writers
refuse it instead of removing consent or namespaced identities. Hook-only legacy
configurations remain readable.


`codex_capture.scope` is `included-projects` or `all-projects` and is independent
of `discovery.enabled`. Legacy configurations retain included-project scope.
The blanket authorization window, source authorization window, explicit rule
barriers and rule revision are local permission state; pairing does not import
another machine’s live windows. Discovery-off all-mode can still use approved
hooks. Scope choice and history keep the strongest writer fence after disablement,
rollback, refresh or skill-policy edits; old v2 writers refuse rather than drop it.
## Protected Codex capture scope

The optional local `codex_capture` record separates Codex project permission
from `discovery.enabled`. Its tagged `scope` is `included-projects` or
`all-projects`; absent means included projects. All mode grants only Codex
fresh starts after local consent, including approved hooks when discovery is
off. It never turns an empty project list into permission for another app.
The public setup choice is described in the setup documentation; the policy
and consumers preserve included-project behavior for existing configurations.

One blanket authorization binds a generation to the current destination and
at most 256 half-open unpaused intervals. Discovery also needs its independent
source authorization: disabling and re-enabling discovery or changing approved
homes opens a new source window without withdrawing approved hook permission.
Changing the scope, destination or selected Codex app opens a new blanket
window. Registered sessions keep their original admission, identity, origin and
destination; changing destinations does not move their archives.

Explicit include/exclude rules use the nearest canonical ancestor, with
intentional child inclusions beneath exclusions. Checkout and mapped-main rules are evaluated together. An unrelated positive
checkout rule cannot bypass a mapped-main exclusion; a deliberate child
inclusion beneath the same excluded ancestor keeps its nearest-rule grant.
New Git projects use their physical repository root, validated worktrees use
the main repository, and nonGit projects use their canonical working directory.
Existing native IDs retain their previous configured-owner attribution.
In all mode, legacy registrations also obey current exceptions at their stored
working directory; retaining an included parent does not override an excluded
child. Historical hooks without a stored cwd keep their recorded project root.

Removing or lifting an exclusion records a forward subtree barrier. Retargeting
an excluded path records the same barrier at its previous physical root. Unknown starts
from its excluded period remain ineligible, while unrelated projects retain
their consent window. The policy stores canonical rule roots and at most 4,096
user-edited barriers/rules. Exhausting history fails closed; discovered
projects never append permissions or cause configuration writes.

Scope-capable records use the incompatible writer fence
`{"version":3,"writer":"codex-scope-floor-v3"}` in `schema_version` and a protected
skill-evidence marker, unless a higher protection supplies the outer fence.
Active, disabled and rollback snapshots retain the protection. Older discovery-v2 and numeric writers refuse it before mutation;
removing authorization fields is not a supported downgrade. Pairing transfers
scope preferences, then obtains new local permission, not another machine's
live authorization.

A private registration's immutable `codex_admission` proof is written only
while admitting a fresh authorized hook/discovery start. Publication consumes
that proof for unlisted projects while still checking current exceptions,
selected app and destination. Legacy/import registrations never gain proof
on continuation. An already admitted proof stops while excluded and resumes
when the current exception permits it again; a forward barrier does not revoke
that immutable admission. Its physical identity remains eligible inside a
remaining included parent when scope is reduced. The proof remains local. Filtered source and metadata derivation versions
are unchanged; native provenance already supported by the upstream filtered
source contract remains available to child materialization.

Blanket, source and included-project generations retain an immutable
`native_start_floor`, including when created while paused. The floor is the
latest local reconciliation and destination boundary (and project activation
for included-project scopes). Resume below any floor or the last closed
interval, and pause at or before an open interval's start, refuse the complete
transition without changing configuration. Compaction never resets the floor.
Earlier `codex-scope-v3` documents migrate nonempty histories conservatively
from their earliest retained interval; empty unknown histories cannot resume
or authorize starts until explicit local setup renews them. Every protected
save, nested draft and rollback retains the stronger
`+codex-scope-floor-v3` skill marker and native-floor validation. Without a higher
protection, the outer fence is `codex-scope-floor-v3` (version3). Later sticky
protections supply the outer fence: `archive-generations-v4` for generations,
`codex-history-v5` for preserved history, `durable-storage-v7` for durable local
storage, `retention-forever-v8` for Keep forever, and `import-staging-v9`
(version9) for durable import staging. Confirmed imports install the sticky
staging fence before retaining selected filtered evidence. Earlier retention-only
writers refuse version9 before mutation; upgrade installed hooks and collectors
before importing with this writer. Finite retention changes retain the staging
fence.
Each higher fence preserves the validation and compatibility protections below
it. Older writers must understand the outer writer identity before mutation;
a numeric schema bump alone does not protect against permissive legacy decoders.

Choosing Keep forever installs a sticky compatibility fence: older binaries refuse this configuration before writing. Changing back to finite retention retains that fence; upgrade all installed hooks and collectors before using this option. Retention is per machine; pairing copies the current choice but later changes do not propagate. Already deleted sessions are not restored.
