# Privacy filter changelog

What each version of the source filter changed, newest first. The current
rules, as a whole, are in the [filter specification](privacy-filter.md);
version numbers and bump rules are in [versions](../maintainers/versions.md). Each archived session
records the filter version that produced it (`filter_version`).

## Source filter version 20

Adapter 0.20.0 accompanies this change.

- **Codex project instructions.** Codex 0.160 sends a project's AGENTS.md as
  the first user message of a thread, and again after compaction, as a
  `# AGENTS.md instructions for DIR` heading followed by an
  `<INSTRUCTIONS>…</INSTRUCTIONS>` block, instead of the `<user_instructions>`
  block earlier releases used. Filter 19 kept it as text the person wrote, so
  it became the session's title and the first prompt in a handoff. The heading
  and block are now dropped with a `hidden_instruction_omitted` gap, like
  `<user_instructions>`. Only a heading at the start of a string, on its own
  line and followed by the tag, counts: a message that quotes it later, or
  indents it as a code example, keeps it. An unclosed block drops the rest of
  the string. Less leaves the machine: nothing new is kept.

## Source filter version 19

Adapter 0.19.0 accompanies the new Muse parser 0.1.0.

- Explicit local Muse files require a complete supported ordinary exec/MSP root,
  exact initial permission checksum and coherent reserved start/workspace.
  Permission profiles, native child JSON, checksums, provider/context bodies and
  hook diagnostics are validation inputs only and are never retained.
- `muse-jsonl` v1 preserves bounded envelope/position and typed conversation joins,
  minimal permission/startup authority, safe own text and names. All retained
  strings pass shared privacy policy; changed identity refuses the file.
  Fixed privacy gaps disclose redaction, hidden-context removal and truncation,
  and survive retained-only privacy refresh without native strings or details.
- Known optional events retain content-free provenance. Unknown authority,
  forks, children, checkpoints, rewrites and incomplete startup refuse the whole
  file. Usage, tools, model switches and child coverage remain unavailable.
  Registered collection refuses Muse before native reading/filtering/publication.

## Source filter version 18

Adapter 0.18.0 and Codex parser 0.25.0 accompany this change.

- The same bounded owning `session_labels` shape accepts the fixed `api` source
  with `codex-api-159.2-v1`. API responses, paths, diagnostics, turns and unrelated
  fields are never retained. Explicit compatible null can clear a name; weaker
  unavailable file fallback cannot clear an API observation.
- Native lookup is explicit opt-in. Its host may write native startup state;
  desktop coexistence and real native authentication remain unverified.

## Source filter version 17

Adapter 0.17.0 and Codex parser 0.24.0 accompany this change.

- Typed `session_labels` evidence keeps only an owning native UUID, present or
  confirmed-absent state, filtered name, source class and pinned contract.
  Unknown fields and other owners are rejected. Generic evidence validates
  bounded safe owner IDs and opaque contracts; the Codex integration separately
  validates native UUID, producer, history and resolver compatibility. Names receive the ordinary
  credential redaction, control-character removal and a 512-byte UTF-8 cap
  before fingerprinting or persistence. Names that would change under another
  privacy pass after presentation normalization are unavailable, preserving
  earlier verified evidence instead of caching an unpublishable observation.
  Native paths and metadata responses
  are omitted. Evidence replacement retains a stable observation timestamp
  when semantic content is unchanged.
- Names remain separate from prompt titles. They do not admit sessions, renew
  capture time, count as conversation activity or extend retention. Related
  Codex histories remain outside this publication path.
- Codex session metadata retains only the `legacy`/`paginated` `history_mode`
  enum, so index-only interpretation cannot mistake a missing paginated
  database for legacy storage. Older retained output lacking this proof defers
  index-only resolution until normal refiltering supplies current context.

## Source filter version 16

Adapter version 0.16.0 goes with it. Claude/common parser 0.21.0 and Codex
parser 0.23.0 derive the corrected labels and prompt fallback.

- **Claude native generated names.** Typed `ai-title` records retain a nonempty
  string `aiTitle`, string `sessionId`/`timestamp`, and boolean `isSidechain`.
  Custom titles preserve the same sidechain flag. Other fields are omitted;
  names receive prompt redaction. Neither title class establishes source
  identity, activity, native start/end, or archive eligibility.
- The latest matching custom title wins over every generated title; otherwise
  the latest matching generated title wins. A present session ID must match
  the owning bundle/verified preview candidate. Legacy records without an ID
  remain usable from that source. Malformed or sanitizer-rewritten ownership
  fields are rejected; inlined sidechain titles are excluded. Title-only
  source publications keep the original capture time and retention basis.
- **Codex open-page context.** The injected `external_codex_apps_open_page`
  leading XML context wrapper is stripped with nested/unclosed-block rules.
  Quoted markup, fenced and indented code examples (including after blank
  lines containing whitespace), and mentions in human prose remain intact. Human
  text outside the block remains a prompt; context-only text is omitted.
  Sources filtered earlier keep the wrapper; Codex parser 0.27.0 applies the
  same rule when deriving their metadata, so a refresh corrects their titles.
- Head/tail previews keep name class precedence and report partial coverage
  when records between their bounded windows were not read.

## Source filter version 15

Adapter version 0.15.0 goes with it; the parser version is unchanged.

- **Machine pairing bundles.** Every retained string redacts the `aa-pair1:`
  prefix and its contiguous payload, including short or truncated payloads,
  padding, and standard-base64 characters. This covers prose, tool arguments,
  nested JSON text, and displayed files. Each match records a
  `sensitive_content_redacted` gap. See the [exact boundaries](privacy-filter.md#value-level-redaction).
  The ordinary-word pairing code has no recognizable credential shape.
- Existing sessions are re-filtered under version 15 on the next collector
  scan; older uploaded objects remain subject to the usual purge policy.

## Source filter version 14

Adapter version 0.14.0 goes with it. Parser 0.18.0 is the first to read what
is newly kept: it derives a subagent's `name` from it. What is newly kept is
one string, the description the parent session gave a Claude Code subagent's
task, and nothing else that was dropped.

- **Claude Code's subagent description.** A subagent's transcript,
  `subagents/agent-<id>.jsonl`, has a sibling `agent-<id>.meta.json` whose
  `description` is the name the parent gave the task ("Find the retention
  tests"). The description is not in the transcript, so the collector reads
  that file when it filters the transcript of a subagent (a registration
  with a parent session, whose transcript is named `agent-<id>.jsonl`) and
  `ClaudeAdapter.FilterSubagentJSONL` writes one synthetic record at the front
  of the filtered records:

  ```json
  {"type":"subagent-meta","description":"…"}
  ```

  - The description passes the same value rules as a prompt (injected
    instruction stripping, credential redaction), applied to the whole text
    first, and is then cut to 512 bytes on a character boundary (a
    `content_truncated` gap records the cut) and checked again, so a secret
    is never cut in half and left behind. It is kept when it is a string with
    something in it.
  - Nothing else of the file is kept, and none of it is reported: the file
    is not part of the transcript. `worktreePath` (a path on this machine) and
    `agentType` (local-only state) are never read.
  - The file is optional. One that is missing, unreadable, a symbolic link
    (never followed, so no file elsewhere is read in its place) or otherwise
    not a regular file, larger than 16 KB, not a JSON object, or without a
    description that is a non-blank string changes nothing and records no
    gap.
  - The record, and any gap about it (including the names of keys dropped
    from a description that is JSON text, filtered as a prompt's is), is
    written only when the transcript has records of its own, so it never
    makes an empty or unrecognized transcript look captured, and never
    changes the transcript's timestamps or identities. There is at most
    one: a `subagent-meta` record in the transcript itself (a retained
    snapshot filtered again carries the one written earlier) is rebuilt from
    its description alone, and a second one, or one beside the file's, is
    dropped with an `unsupported_value_omitted` gap.
  - A transcript that is not a Claude Code subagent's never reads a
    `.meta.json`.

The collector does not count a changed description as the transcript being
rewritten: a subagent captured before its `.meta.json` existed, or whose
description changed, still extends its earlier snapshot (the leading
`subagent-meta` record is left out of that comparison). A later scan
republishes the new description only when the transcript has changed: a
`.meta.json` that appears or changes beside a transcript that does not
change is not noticed until the transcript changes or the next filter
or adapter version re-reads it (a parser version alone re-derives metadata
from the retained snapshot). The collector registers a subagent when its
`SubagentStop` hook fires, or when backfill finds it, so the file is normally
beside the transcript the first time it is read.

A filter bump makes the collector read and republish every session whose
transcript still exists, which is how a description reaches subagents
archived before this version. A subagent whose transcript is gone keeps what
it had.

## Source filter version 13

Adapter version 0.13.0 goes with it. Parser 0.17.0 is the first to read what
is newly kept: it derives `name` and `pull_requests` from it. What is newly
kept is the name a session was given and the pull request it is linked to, in
the three places below. Nothing else that was dropped is kept.

- **Claude Code's `custom-title` record.** `type`, `customTitle`, and, when
  they are strings, `sessionId` and `timestamp`. `customTitle` is the name in
  the app's sidebar, set automatically or by `/rename`: text the model wrote
  from the person's prompt, or the person typed. It is kept when it is a
  string with something in it, and passes the same value rules as a prompt
  (injected-instruction stripping, credential redaction, the 64 KB cap). Every
  other key of the record (`uuid`, `cwd`, …) is dropped and its name listed
  in the `unknown_field_omitted` gap. A record with no title, an empty or
  blank one, or one that is not a string is dropped, with an
  `unsupported_value_omitted` gap (`record omitted`). Claude Code appends
  one of these records for each name a session is given, and each is kept:
  renaming a session adds a name and does not remove the earlier ones.
- **Claude Code's `pr-link` record.** `type`, `prNumber`, `prRepository`,
  `prUrl`, and, when they are strings, `sessionId` and `timestamp`.
  - `prRepository` must be `owner/name`, each part matching the pattern
    `git_activity` requires of a repository (letters, digits, `_`, `.`, `-`).
  - `prNumber` must be a whole number from 1 to 2^30, written as a string of
    digits (as Claude Code writes it) or as a number. It is kept as a JSON
    integer either way, so the retained record has one shape.
  - `prUrl` is kept only when it is exactly `https://github.com/<prRepository>/pull/<prNumber>`.
    Any other value (another host, a query string, a fragment, a value
    that is not a string) is dropped, the rest of the record is kept, and
    `prUrl` is listed in the `unknown_field_omitted` gap.
  - A `pr-link` whose repository or number is missing or out of shape is
    dropped whole, with an `unsupported_value_omitted` gap (`record
    omitted`). So is one whose repository the value rules would rewrite.
  - Every other key is dropped and listed in the `unknown_field_omitted` gap.
- **Cursor's chat name.** The composer's chat-level `name` is written as
  `name` on the `session` record of a chat read from Cursor's database, and
  passes the same value rules as a prompt. It is no longer listed as the
  omitted key `chat.name`. A chat Cursor has not named, or whose `name` is
  empty, has no `name`; a `name` that is not a string is still reported as
  `chat.name` in the gap. The session record is the first record; a chat
  named (or renamed) after an earlier snapshot of it was taken is
  republished with its new name, and the collector does not count that as
  Cursor changing the chat's messages (no `cursor_chat_rewritten` gap).

Still dropped: Claude Code's `agent-name` (a copy of the custom title) and
`last-prompt` (derivable from the turns), as unknown record types. The keys
are admitted on those two record types only; `customTitle`, `prNumber`,
`prRepository`, and `prUrl` on any other record are still dropped as unknown
keys.

A filter bump makes the collector read and republish every session whose
transcript still exists, which is how a session's name and pull requests
reach sessions archived before this version. A session whose transcript is
gone keeps what it had.

## Source filter version 12

Adapter version 0.12.0 goes with it; the parser is unchanged.

- **Private keys encoded in base64.** A PEM block encoded in base64 whole,
  as kubeconfig's `client-key-data` holds a client's private key, is
  redacted wherever it appears (`cat ~/.kube/config`, `kubectl config view
  --raw`). Certificates encoded the same way are redacted too.
- **Docker registry logins shown as text.** The `"auth"` and
  `"identitytoken"` values of Docker's `config.json`, base64 of
  `user:password`, were dropped only when the file was parsed as JSON. They
  are now redacted in text as well: a file read with line numbers, or JSON
  following other command output.
- **Groq and xAI keys by their prefix.** `gsk_` followed by 48 or more
  letters and digits (Groq writes 52) and `xai-` followed by 70 or more
  (xAI writes 80) are redacted wherever they appear, whatever holds them
  (`GROQ=gsk_…`). Before, only a credential-named variable (`GROQ_API_KEY=`)
  was caught.
- **Access key IDs by name.** `access key id` joins the credential
  vocabulary, so `R2_ACCESS_KEY_ID=…`, `AWS_ACCESS_KEY_ID=…`,
  `"accessKeyId": …`, and `Access Key ID: …` have their values redacted.
  An access key ID is an identifier rather than a secret, but AWS's were
  already redacted by their `AKIA…` shape and a tool argument named
  `access_key_id` was already dropped; an R2 access key ID, 32 hex
  characters with no prefix, is the ID of the account's API token and
  passed through.
- **`.pgpass` passwords.** The password of a `host:port:database:user:
  password` line is redacted to the end of its line (or its closing quote,
  in `echo '…'`), after a line number or a diff marker too: anywhere when
  the port is 5432, 6432, or `*`, as the Claude Code Read tool shows the
  file, and with any port of two or more digits in a string that mentions
  `.pgpass` or `PGPASSFILE` (`cat ~/.pgpass`, a heredoc into it, grep's
  `.pgpass.bak:` prefix). A host that is all digits (a timestamp) is not a
  line of the file.
- **Wallet seed phrases.** 12 to 24 words of three to eight letters after a
  name holding `mnemonic` (`MNEMONIC="…"`, `mnemonic: …`, `--mnemonic
  "…"`, `"wallet_mnemonic": "…"`), separated by spaces or commas or written
  as an array, are redacted, and so are exactly 12, 15, 18, 21, or 24 such
  words after a name holding `seed` (`SEED=…`, `wallet seed: …`). `seed
  phrase` and `recovery phrase` join the credential vocabulary
  (`SEED_PHRASE=…`, `Secret Recovery Phrase: …`), so their values are
  redacted whatever they hold, and tool arguments with those names are
  dropped.
- **Skill hashes are of the redacted text.** A skill snapshot's `sha256`,
  and the inventory's, was the SHA-256 of the original `SKILL.md`, uploaded
  beside its redacted body, so whoever holds the bucket could confirm a
  guess at a redacted secret. It is now the SHA-256 of the whole file with
  its credentials redacted, which for a file with nothing to redact is its
  own SHA-256. Sessions archived earlier keep the old hashes.
- **No crash on a YAML entry with two credential names.** `name:
  DB_PASSWORD` then `name: API_TOKEN` then `value: …` found the one value
  twice, and redacting it twice over panicked.
- **Faster URL scan, same output.** Finding the userinfo of URLs glued
  into one long token (`http://x` repeated with no space) read the rest of
  the token again for each URL: 1.6 MB took about a minute. It now reads
  the text once. The redacted output is unchanged, which a test checks
  against the previous scan.

## Source filter version 11

Filter 11 closes the shapes next to ones filter 9 and 10 fixed, from the
second staff review (P-20 to P-26, A-20). Adapter version 0.11.0 and parser
version 0.11.0 go with it. Every format changes.

- **JSON inside a string is filtered as JSON.** Codex writes a function
  call's arguments as a JSON string, so the tool-argument deny list never saw
  their keys: a Codex `browser_type` call kept the password it typed, which
  the same call from Claude Code dropped. Any string that holds a JSON object
  or array is now decoded and filtered as a tool-argument subtree (every key
  name kept, the deny list applied, every string redacted, binary blocks
  dropped, JSON strings inside it decoded in turn), wherever it sits: Codex
  arguments, custom tool input, and outputs; tool results an MCP server
  returned as JSON text; Cursor JSONL; and Cursor database results stored as
  strings, which kept their base64 images and cookies (P-25). A string the
  filter changes is stored re-encoded (compact, keys sorted, no HTML
  escaping); one it does not change keeps its bytes. A command-line array
  (`["mysql", "-pS3cret"]`) has its secret values redacted by position.
- **Typing tools by any common name, and secret labels.** Typed input is
  dropped for any tool whose name holds a typing word (`type`, `fill`,
  `form`, `input`, `press`, `select`, `keys`, `stdin`, …), which covers
  `browser_fill_form`, chrome-devtools `fill_form`, `select_option`, and
  Codex's `write_stdin`. For every tool, a `value` or `text` beside a label
  that says it is a password, PIN, one-time code, card number, CVV, or
  similar (`{"name": "Password", "value": …}`, `{"type": "password"}`,
  `{"name": "DB_PASSWORD", "value": …}`) is dropped (P-21), and so is one
  beside a label naming a credential as a key would, such as a HAR header
  or cookie (`{"name": "Authorization", "value": …}`); a `[name, value]`
  pair whose name is a credential (`["X-Api-Key", …]`) loses its value.
- **One credential vocabulary.** Argument names and the text patterns now
  use one word list (P-24). Arguments named `X-Api-Key`, `passwd`, `pass`,
  `private_key`, or `auth` are dropped; `max_tokens` is no longer dropped
  (the rule matches words, and a plural is another word; `secrets`,
  `passwords`, and `creds` are words of their own). In text a name may be
  written with spaces (`API Key: …`, `Secret Key = …`), may end in `value`
  (`CLIENT_SECRET_VALUE`), and may be followed by a full-width `＝`;
  `AUTH_KEY` and `Ocp-Apim-Subscription-Key` are names too.
- **Structures and entries in text.** A credential name whose value is an
  object or array (`"secret": {"value": …}`, `"passwords": [ … ]`, in
  JSON, Python, JavaScript, or JSON escaped in a string, over several lines)
  has every string value in it redacted, keeping keys and descriptive
  values such as `type`. So does a YAML mapping or sequence under a
  credential key (`secrets:` then `db: …`, `password:` then `value: …`,
  `passwords:` then `- …`), at any depth, keeping its keys and comments. A
  URL-encoded assignment in a nested query string or form body
  (`password%3D…`, `api_token%3A…`) has its value redacted up to an encoded
  `&` or `,`. A YAML entry whose `name:` is a credential has its
  sibling `value:` redacted (a Kubernetes `env` list), as does a one-line
  `{"name": "Authorization", "value": …}`. Structured data already dropped
  these whole.
- **Files shown with line numbers.** Agents read most files through a tool
  that numbers the lines (the Claude Code Read tool, `cat -n`). The YAML
  block and next-line rule, the entry rule, and the private key rule read
  such lines after their numbers.
- **More credential shapes** (P-22): command-line flags that carry a secret
  for particular programs (`curl -u user:secret`, `mysql -psecret`,
  `sshpass -p`, `docker login -p`, `redis-cli -a`, `sqlcmd -P`, `keytool
  -storepass`, macOS `security -p`/`-w`, `openssl pass:`, `aws configure
  set`, `npm config set`, fish `set -gx`, and more); `.netrc` passwords;
  Cookie and Set-Cookie headers; `DB_PASS`, `REDIS_PASS`, `PGPASS`,
  `dbPass`; `private_key_id`; Azure `AccountKey=`, `SharedAccessKey=`, and
  SAS `sig=`; XML `<password>…</password>` and `<add key="ApiKey"
  value="…"/>`; `api_key<TAB>value`; `?key=`; PGP private key blocks; and
  the prefixed tokens of Stripe, GitLab, Google, Hugging Face, npm, PyPI,
  SendGrid, Shopify, DigitalOcean, Vault, Databricks, Linear, Grafana,
  Postman, New Relic, Sentry, Atlassian, Figma, Doppler, age, Mailgun,
  Telegram, and Azure AD; Slack, Discord, and Teams webhook URLs; and a bare
  `Bearer` token.
- **The whole value** (P-23). An unquoted value runs to the end of its line
  instead of the first space, `,`, `;`, or quote, so `password: correct
  horse battery staple` and `DB_PASSWORD=Xk9;mP2vQ7zR` are redacted whole. It
  stops earlier at the closing quote of a string it sits in, at whitespace
  followed by shell punctuation, a comment, a flag, or another assignment,
  at `, ` or `; `, and in a URL query at the next `&`. A YAML block value
  (`password: |`) and a value on the line below its key are redacted whole.
  URL userinfo ends at the last `@` before the host, and a password holding
  `/` is covered.
- **A PEM BEGIN line without an END line** takes only the base64 body that
  follows it (P-26). Filter 10 took everything to the end of the string, so
  source code that named the BEGIN line lost the rest of the file. A key
  between BEGIN and END lines is taken when its lines are key body, read
  through the decoration a display adds (line numbers from the Claude Code
  Read tool, `cat -n`, or `grep -n`; diff, quote, and comment markers;
  string quotes in source code), or, failing that, when it holds a base64
  run of 48 characters or more (unless another BEGIN line, such as a
  certificate's, is between); code between two constants that name the
  armor lines is kept. A key cut short keeps no line with a 48-character
  base64 run (`cat -A`, `grep -rn` output) and no body written on its
  BEGIN line. A key split between two strings (a file read in two parts,
  a tool result in chunks) has the body above its END line redacted in the
  second string too. Certificates and public keys are kept.
- **A flag at the start of a later line** (`mysql \` then `--password x`)
  is redacted; filter 10's flag pattern matched only at the start of the
  string or after a space.
- **Cursor text headers in one case** (A-20). A role header is a role and a
  colon at column 0, in the case of the transcript's first header: lower
  case (`user:`) or capitalized (`User:`). A line in the other case is
  content, so `Analysis: …` in a lower-case transcript hides nothing and a
  YAML `user:` line in a capitalized one starts no turn. (The real Cursor
  format is not pinned by a fixture; accepting both cases keeps capture
  going whichever it is.) When the transcript separates sections with blank
  lines, a visible role line that does not follow one is content, so YAML
  in tool output cannot start a Person turn. A hidden role line always hides what
  follows (fail closed), and the gap now counts the hidden sections and
  lines. A header-shaped line inside a retained section is indented by one
  space, so the handoff reads the retained text back exactly as it was
  filtered.

Known trade-offs, chosen toward the secret:
- An environment prefix before a command loses the command with the value:
  `TOKEN=abc npm test` becomes `TOKEN=[REDACTED]`.
- A value labelled as a secret is dropped even when it is not one (a
  `{"name": "token_type", "value": "Bearer"}` pair).
- A key or tool name that merely contains a vocabulary word loses its value
  (`password_policy` as a tool argument, `auth_mode`), as filter 10's
  substring rule did.
- A space-separated `-p value` after a program the filter does not know is
  kept, since `-p` is a port or a flag to most programs.
- A Cursor text transcript whose first header is neither lower case nor
  capitalized (`USER:`) is refused as a capture gap rather than guessed at;
  one without blank lines between sections is read as before.
- A string holding JSON is filtered as a record is, wherever it sits, so a
  pasted chat log in a prompt loses its system and reasoning messages
  (`{"role": "system", …}`, with a `hidden_instruction_omitted` gap) and
  its credential-named keys. JSON text that names a key twice is stored
  re-encoded from the last value of each key, which is all the filter
  reads.
- Every string value inside a credential-named structure is redacted
  whatever it is (`"token": {"expires": "[REDACTED]"}`), except under a
  descriptive key (`type`, `kind`, `name`, `description`, `provider`, …).
- Benign text is pinned by `TestFilterV11LeavesBenignTextUnchanged`: code
  that names a password, prose, hashes, UUIDs, git SHAs, `ssh -p 22`,
  `mkdir -p`, and URLs with an `@` in the path are left alone.

The patterns run line by line, and only on lines holding a word every
match needs (a credential word, a program name, a token prefix), which
keeps filtering a large transcript within about twice filter 10's time
(filter 11 as first written was about eight times slower). A fuzz target
checks that the whole redaction, gated this way, equals the same redaction
run over the whole string with no gate
(`FuzzGatedRedactionMatchesWhole`), and a test proves from each parsed
pattern that every match holds one of its needles
(`TestPatternNeedlesAreRequired`).

Snapshots filtered before the upgrade stay in the bucket until the session
expires; see [after a filter upgrade](../../docs/security/privacy.md#after-a-filter-upgrade) to
delete them.

## Source filter version 10

Filter 10 fixes two things. Adapter version 0.10.0 goes with it.

- **Role headers only at column 0.** Filter 9 treated any line whose trimmed
  text began with `user:`, `system:`, or another role name as a role header.
  An indented YAML key in tool output (`    user: …` in a docker-compose
  file) therefore started a new section, and `  system: linux` hid
  everything after it as if it were a system prompt. A header is now only a
  role name and a colon at the start of the line (in any case), followed by
  a space or the end of the line. An indented role word is content: it is
  retained, sanitized like the rest of its section, and never hides or
  reveals anything. Only Cursor plain-text transcripts change.
- **Text glued after a closing quote.** Filter 9 ended a quoted credential
  value at its closing quote, so `PASSWORD="abc"realsecret`, which a shell
  reads as the value `abcrealsecret`, became `PASSWORD="[REDACTED]"realsecret`,
  and `.PWD=='0'0` kept its trailing `0`. Whatever a shell would read as the
  same word after the closing quote (more text, or more closed quoted
  segments: `TOKEN='a'"b"c`) is now part of the value and is redacted with
  it: `PASSWORD="[REDACTED]"`. It stops at whitespace, `,`, `;`, a closing
  `]`, `}`, or `)` (so `{"password":"abc"}` keeps its brace), and shell
  punctuation (`&`, `|`, `<`, `>`). Redacting twice still changes nothing.
  Every format with text changes.

## Source filter version 9

Filter 9 closes four ways content left the machine that the filter was meant
to stop. Adapter version 0.9.0 goes with it.

- **Credential assignments by any common name.** Filter 8 required the
  trigger word to stand alone, and since `_` is part of a word that missed
  every snake_case or SCREAMING_CASE name (`DB_PASSWORD=`,
  `AWS_SECRET_ACCESS_KEY=`, `OPENAI_API_KEY=`, `GITHUB_TOKEN=`); a quote
  between the name and the separator missed every JSON key
  (`"password": "…"`). A credential name is now any name ending in a trigger
  word, optionally followed by `key` or `access_key`, `base`, and a number,
  in any case and with anything glued on before it (`db_password`,
  `accessToken`, `PGPASSWORD`, `SECRET_KEY_BASE`, `DB_PASSWORD_1`,
  `spring.datasource.password`), quoted or not; the separator is `=`, `:`,
  `:=`, or `=>`, and `--name value` command-line flags are covered too. Only
  the value is replaced now, and a quoted value keeps its quotes:
  `DB_PASSWORD=[REDACTED]`, `"password": "[REDACTED]"`,
  `Authorization: Bearer [REDACTED]`. Filter 8 replaced the name as well. See
  [Value-level redaction](privacy-filter.md#value-level-redaction) for the full rule and its
  false positives.
- **Images, documents, and other binary blocks are dropped.** A pasted
  screenshot or PDF, or an image a tool read, arrives as a content block
  whose data is base64. Its key names (`type`, `source`, `data`) were all on
  the allowlist, so filter 8 kept it. Now a block of type `image`,
  `document`, `input_image`, `input_file`, `input_audio`, or `image_url`, a
  `file` block with inline data, and any block whose `source` has type
  `base64` are dropped whole, at any depth (a tool result keeps its
  `tool_use_id`), with a `binary_content_omitted` gap naming the kind. A
  base64 `data:` URL inside a string keeps its media type and loses its
  payload (`data:image/png;base64,[OMITTED]`), with the same gap. `data` is
  no longer an allowed key anywhere: nothing the filter keeps needs it.
- **Structured Cursor tool results are sanitized.** A Cursor tool result or
  error that is an object, not a string, was encoded to one string before
  the sanitizer saw it, so the key rules never ran on it. It is now sanitized
  first, the way a tool call's arguments are (every key name kept,
  `blockedKeys` and the credential-named deny list applied, every string
  redacted, binary blocks dropped), then encoded. Keys dropped by the deny
  list are named in `sensitive_or_hidden_field_omitted` as
  `omitted tool result keys: …`. An empty result stays `{}`; one the
  sanitizer leaves nothing of is dropped and named as `tool.result` (or
  `tool.error`, `toolResult.result`) in `unknown_field_omitted`.
- **Skill snapshots stay inside their skill root.** A skill's `SKILL.md` is
  now resolved through symlinks before it is read, and the resolved file is
  what is read; it must be a regular file. A user-level skill's file must lie
  inside the skill root or be itself named `SKILL.md` (a linked skill, not an
  arbitrary file under a skill's name), so a skill directory linked into a
  skills checkout elsewhere (`~/.claude/skills/x -> ~/src/skills/x`) keeps
  working. A project-level skill's file must be named `SKILL.md` and lie
  inside the project: a repository controls its skill root as much as its
  links (`.claude/skills -> ..`), so being inside that root proves nothing.
  A cloned repository can therefore ship neither
  `.claude/skills/x/SKILL.md -> ~/.aws/credentials`, nor `-> ../../../.env`,
  nor a linked skill root with `x/SKILL.md -> ../master.key`, to have that
  file archived; a skills directory the repository shares between harnesses
  keeps working. When a session runs from
  the home directory, its project skill directory is the user's own; it is
  observed once, under the user scope. Any other entry counts as
  uninspected in the root's inventory.
- **Truncation keeps characters whole.** The 64 KB string cap and the 16 KB
  skill-snapshot cap cut on a UTF-8 character boundary; filter 8 could split
  a multi-byte character and leave invalid UTF-8.

Every filter-8 rule still applies.

## Source filter version 8

Filter 8 adds one source format, `cursor-composer`: a Cursor chat read from
Cursor's own database (`state.vscdb`) rather than from a hook's transcript.
JSONL and text output are byte-identical to filter 7. A chat is a
`composerData` value and one row per message; the filter builds records from
them through an allowlist and then passes every record through the same
sanitizer as a JSONL record.

- **Kept:** one session record (the chat's ID and creation time) and, per
  message, its role (user or assistant), ID, creation time, request ID, start
  and completion times, model name, input and output token counts (an
  all-zero count is left out), text, and its tool call: the tool's name and
  call ID, its arguments, and its result or, for a failed call, its error
  (as text, whatever its shape) with `is_error`. Tool arguments pass the same
  deny list and redaction as in a JSONL transcript.
- **Dropped by design:** every context payload Cursor attaches to a message
  (`codebaseContextChunks`, `attachedCodeChunks`, `originalFileStates`,
  `diffHistories`, `images`, `consoleLogs`, `recentlyViewedFiles`, and the
  like), named in a `cursor_context_omitted` gap; the model's reasoning
  (`thinking`), as `hidden_instruction_omitted`; and the chat's settings and
  bookkeeping (`lastUpdatedAt`, `modelConfig`, `usageData`,
  `workspaceIdentifier`, `blobEncryptionKey`, UI state). Every other key not
  kept, including a tool call's `toolCallBinary`, `userDecision`, and
  `additionalData`, and a result recorded beside an error, is named in
  `unknown_field_omitted` with its level: `chat.`, `message.`, `tool.`
  (`toolFormerData`), `toolResult.` (`toolResults` entries), `model.`,
  `tokens.`, or `record.` (a key the shared sanitizer omitted from a record
  this filter built). Names only, never values.
- **Tool arguments.** Arguments come from `rawArgs` when it decodes to a
  non-empty object, else from `params`. A string argument that is itself
  JSON (an object or array), at any depth, is dropped rather than kept as
  text the argument rules never saw, and the nearest argument name is given
  in `cursor_tool_argument_omitted`; objects and arrays it leaves empty are
  removed. When neither source yields arguments, a source that did not
  decode is named in the same gap under a separate detail, so Cursor's own
  `rawArgs` and `params` are never confused with a tool's argument names. An
  empty argument object is dropped silently.
- **Fail closed.** Only the format versions seen in a real database are read:
  chat `_v` 18 and message `_v` 3. Any other version, older or newer, refuses
  the whole chat as an unsafe format. So do malformed JSON and a message list
  that does not match the chat's headers. Older chats kept their messages
  inline in `conversation` and are refused by version; inline messages in a
  chat that is read are not read either, and add a
  `cursor_inline_conversation_omitted` gap.
- **Counted, not guessed.** A message with no row, or a row belonging to
  another message, adds `cursor_bubble_missing` (and, for the second,
  `cursor_bubble_id_mismatch`); content kept in blobs, which are never read,
  adds `cursor_blob_content_unavailable`; an unknown or inconsistent message
  type adds `cursor_message_type_unknown`. Their details give counts out of
  all the chat's messages, never an ID.
- **Output stops early rather than skipping.** Output stops at the first
  message whose row is missing, belongs to another message, or disagrees with
  its header about its type, and at the first message still in flight: one
  the chat lists in `generatingBubbleIds`; one whose tool call's status is
  anything but `completed`, `error`, or `cancelled` (or `canceled`); or, as a
  backstop, the chat's last message when the chat's `status` is anything but
  `completed`, `none`, `aborted`, `cancelled` (or `canceled`), `error`, or
  empty. That message and everything after it wait for a later pass, counted
  in `cursor_incomplete_tail_omitted`. A completion time is not required:
  most finished replies have none. A chat with no kept message has no
  records at all.
- **Rewrites.** Stopping early keeps a chat's records append-only while it
  grows, but Cursor can still rewrite a message it has finished (late token
  counts, an edited prompt, a checkpoint restore). The filter then produces
  different records; the collector, not the filter, handles a rewritten
  Cursor database chat: it publishes the chat as it now is in place of the
  earlier snapshot (which retention then removes as superseded) and keeps
  one `cursor_chat_rewritten` gap, which names no content, counting the
  rewrites.

Every filter-7 rule still applies.

## Source filter version 7

Filter 7 changes only how a Cursor plain-text transcript is filtered; JSONL
output is byte-identical to filter 6. Older Cursor versions give the hook a
plain-text transcript made of role sections: a `user:`, `assistant:`, or
`tool:` line and the continuation lines under it.

- **Sanitized per section.** Each visible section is sanitized on its own, so
  injected-instruction stripping, credential redaction, and the 64 KB string
  cap apply per message, as they do to a JSONL record. Filter 6 sanitized the
  whole transcript as one string, so any transcript over 64 KB was cut to its
  first 64 KB. A section over 64 KB is now truncated alone, with a
  `content_truncated` gap, and every other section is kept.
- **Structure kept.** Sections stay in their original order, each line as it
  was, so the retained text is read back by the same role prefixes. Hidden
  sections (`system:`, `developer:`, `thinking:`, `analysis:`) and their
  continuation lines are still omitted.
- **Gaps recorded once.** Each gap is recorded once per transcript. Filter 6
  added a `hidden_instruction_omitted` gap for every hidden section.
- **Size.** The transcript is bounded by the record size limit (64 MiB, the
  collector's transcript cap) instead of 2 MB. Over the limit it is a
  `record_size_limit` capture gap rather than a refusal on every pass.

Every filter-6 rule still applies.

## Source filter version 6

Filter 6 is filter 5 plus two narrow retentions and one more injected block.

- **Retained:** Claude Code's `origin`, reduced to `{kind: <string>}`, and
  `promptSource`, only as a string. A person's prompt carries
  `origin.kind: "human"`; a background-task completion carries
  `"task-notification"` and `promptSource: "system"`; only that known kind is
  treated as harness-written, and any other kind stays a prompt. Every other member of
  `origin` is omitted and its name reported in the `unknown_field_omitted`
  gap; an `origin` that is not an object, or has no string `kind`, and a
  `promptSource` that is not a string, are omitted whole. The kept strings
  still pass value redaction.
- **Stripped:** Codex's `<recommended_plugins>…</recommended_plugins>` block,
  a catalog of uninstalled plugins the harness prepends to the first user
  message, is removed like `<environment_context>`, with the same
  `hidden_instruction_omitted` gap.

Every filter-5 rule still applies.

## Source filter version 5

Filter 5 is filter 4 plus what a parser needs to recognize a Claude Code
compaction. After `/compact` or auto-compaction, Claude Code writes a system
record with `subtype: "compact_boundary"`, then a user record marked
`isCompactSummary: true` (usually also `isVisibleInTranscriptOnly: true`)
whose text is a model-written summary of the conversation so far.

- **Retained from the boundary:** `type`, `subtype`, `uuid`, `parentUuid`,
  `logicalParentUuid` (the last record before the compaction), `sessionId`,
  `timestamp`, and `isSidechain`, and nothing else. The record is rebuilt from
  those typed values rather than passed through the ordinary filter, since a
  system record is otherwise hidden whole. An id is kept only when it looks
  like one (a string of at most 256 bytes with no whitespace and no tag
  brackets, which the credential redaction would leave unchanged, or
  null), the timestamp only when it parses, and `isSidechain` only as a
  boolean.
  Everything else — the boundary's text, `compactMetadata` (trigger and token
  count), `level`, `cwd`, `userType`, `version` — is dropped, and its key
  names are listed in the `unknown_field_omitted` gap. Only the exact
  `system`/`compact_boundary` shape is admitted, and only from Claude Code;
  every other system record is still omitted whole.
- **Retained on the summary:** `isCompactSummary` and
  `isVisibleInTranscriptOnly`, as booleans only (the rule filter 4 applies to
  `isMeta`; any other value under those names is omitted).
- **Kept, unlike an `isMeta` record:** the summary's text. It is model output
  describing the session, which is what a handoff to another agent needs. It
  passes every value rule in [privacy](privacy-filter.md#rules) (injected-instruction stripping, credential
  redaction, the 64 KB cap) like any other message.

The shape is taken from Claude Code's behavior; no transcript on the
development machine has contained a compaction yet.

## Source filter version 4

Filter 4 is filter 3 plus one rule for harness-written records. Claude Code
marks user records it writes itself — an expanded skill or custom slash
command, the caveat it adds before local-command output — with
`isMeta: true`. Their text is instruction text the harness injected, the same
kind as a `<system-reminder>` block, not something the person wrote.

- **Retained:** the `isMeta` flag, only as a boolean (a string or object
  under that key is prose the allowlist never kept and is still omitted), and
  the record itself with its ids (`uuid`, `parentUuid`, `sessionId`, and the
  rest of the ordinary allowlist), so parent chains through the record
  survive and a parser can tell the record is not a human prompt.
- **Stripped:** the record's string content and every text block in it, at
  every depth, before any other filtering, with a `hidden_instruction_omitted`
  gap whose detail is `meta record text omitted`. A non-text block (for
  example a tool result) keeps its shape and identifiers (`tool_use_id`,
  `is_error`) but loses its own `text` and its nested content text, and is
  then left to the ordinary rules. A record whose `isMeta` is absent or false
  is untouched.

The typed slash command itself (`<command-name>/review-pr</command-name>`) and
local-command output (`<local-command-stdout>`) are not `isMeta` and are kept
as before; they are what the person did and what the command printed. Every
filter-3 rule still applies.

## Source filter version 3

The source filter decides what leaves this machine. Filter 3 keeps the tool
evidence a reader needs and removes instruction text the harness injected into
a message. It does not relax any value rule: `blockedKeys`, `sensitiveValue`
redaction, the 64 KB string cap, the hidden role/channel rules, and the
exclusion of `encrypted_content`, `base_instructions`, `state`,
`thread_settings`, `rate_limits`, and `attachment` all still apply inside every
newly retained subtree.

Retained, in addition to filter 2:

- **Tool arguments.** Inside a tool-argument subtree — `input`, `arguments`,
  `tool_input`, and Codex's `payload.input` — every argument name is retained,
  because the names belong to the tool and no allowlist can anticipate them.
  This is what restores Edit `old_string`/`new_string`, Agent `prompt`, Skill
  `args`, Grep `pattern`, Bash `timeout`, and MCP tool arguments. Values are
  sanitized exactly as before, so a credential inside an Edit body is still
  redacted and an oversized argument is still capped.
- **Tool-result linkage and turn identity.** `tool_use_id`, `is_error`,
  `stop_reason`, `sessionId`, `requestId`, and `gitBranch`.
- **Token accounting, numbers only.** `usage` (Claude) and Codex's `info`,
  `total_token_usage`, `last_token_usage`, `turn_token_usage`,
  `thread_token_usage`, `last_agent_message`, `thread_id`, `root_turn_id`,
  `started_at_ms`, and `completed_at_ms`. Inside the four `*_token_usage`
  subtrees and `usage`, anything that is not a number is omitted.
- **Record types.** Codex `token_usage_record` and Cursor `turn_ended`.

`toolUseResult` is deliberately not retained: it duplicates the tool result
already kept in the message content.

Stripped:

- **Injected instruction blocks.** Claude Code wraps CLAUDE.md, hook output,
  and memory in `<system-reminder>…</system-reminder>` inside user content, and
  Codex writes AGENTS.md inside `<user_instructions>…</user_instructions>` and
  machine details inside `<environment_context>…</environment_context>`. Those
  blocks are removed from string content wherever they appear and a
  `hidden_instruction_omitted` gap is recorded; the rest of the message is
  kept. An opening tag whose block never closed drops everything after it.
  Untagged instruction text is not guessed at.

Omissions are now visible. A filtered transcript records one
`unknown_field_omitted` gap whose detail lists the distinct key names the
filter could not keep — names only, never values — sorted and capped at 64,
with a note when more were seen.

Skill snapshots embedded in supplemental evidence keep their inventory entry
(name, sha256 of the whole original file, scope) but the archived body is
capped at 16 KB, with `original_bytes` recording the real size and `truncated`
marking the cut. Moving bodies to content-addressed objects is deferred.
