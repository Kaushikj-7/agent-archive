# Privacy filter rules

> **Status: implemented** (filter 18). The user-facing summary is
> [privacy](../../docs/security/privacy.md); what changed in each filter
> version is in the [filter changelog](privacy-filter-changelog.md).

This is the complete rule set the source filter applies to every archived
session, and the reference for anyone changing it. Any change to what it
keeps, drops, or redacts bumps `FilterVersion`
([versions](../maintainers/versions.md)).

## Rules

Every retained string, at every depth, passes injected-instruction
stripping, value-level redaction, and the 64 KB cap. Tool-argument subtrees
(`input`, `arguments`, `tool_input`) keep every argument name, because the
names belong to the tool, subject to the deny list below.

### Session names and linked pull requests

Typed Claude name and pull-request records are kept (filter 16), each rebuilt
from typed values rather than passed through the key allowlist, so the keys
below are admitted on those records only:

- Claude Code `custom-title` and `ai-title`: `type`, the corresponding
  `customTitle` or `aiTitle`, boolean `isSidechain`, and `sessionId` and
  `timestamp` when they are strings. The title must be a string with
  something in it and passes the value rules like a prompt. Any other key is
  dropped (named in the `unknown_field_omitted` gap); a record without a
  usable title is dropped with an `unsupported_value_omitted` gap. Claude
  Code appends a record for each name a session is given, and every one is
  kept, so a renamed session keeps its earlier names too. Custom titles take
  precedence over generated titles regardless of record order. Present session
  IDs must match the owning native session; malformed or sanitizer-rewritten
  ownership is rejected rather than treated as absent. Legacy missing IDs are
  accepted only within that source. Sidechain titles are excluded from naming. Name
  records do not establish identity, activity or archive eligibility.
- Claude Code `pr-link`: `type`, `prNumber`, `prRepository`, `prUrl`, and
  `sessionId` and `timestamp` when they are strings. `prRepository` is
  `owner/name` with each part matching `git_activity`'s repository pattern;
  `prNumber` is a whole number from 1 to 2^30, read from a string of digits
  or a number and written as a JSON integer; `prUrl` is kept only when it is
  `https://github.com/<prRepository>/pull/<prNumber>`, else dropped with
  its name listed in `unknown_field_omitted`. A record whose repository or
  number is missing or out of shape, or whose repository the value rules
  rewrite, is dropped whole with an `unsupported_value_omitted` gap.
- Cursor chats read from its database: the composer's chat-level `name`,
  written as `name` on the first record, the `session` record:
  `{"type":"session","session_id":…,"timestamp":…,"name":…}`. A name that is
  a string passes the value rules like a prompt and is kept when something is
  left of it; an absent, null, or empty one leaves `name` out; one that is
  not a string is reported as the omitted key `chat.name`.

Claude Code's `agent-name` and `last-prompt` records are still dropped as
unknown record types.

### A subagent's description

A Claude Code subagent's transcript, `subagents/agent-<id>.jsonl`, has a
sibling `agent-<id>.meta.json` holding the name the parent gave the task. It
is not part of the transcript, so it is not filtered as one: the collector
reads it when it filters the transcript of a registered subagent (a
registration with a parent session, whose transcript file is named
`agent-<id>.jsonl`) and passes its bytes to
`ClaudeAdapter.FilterSubagentJSONL`, which writes one record before the
transcript's own (filter 14):

```json
{"type":"subagent-meta","description":"…"}
```

- Only `description` is read, and only when it is a string with something in
  it. Every other key of the file (`worktreePath`, a path on this machine;
  `agentType`; anything else) is never read, never kept, and not reported.
- The text passes the value rules like a prompt, applied to the whole text,
  and is then cut to 512 bytes on a character boundary (a `content_truncated`
  gap records a cut) and checked again, so a secret is redacted before any
  cut falls in it.
- The file is optional. One that is missing, unreadable, a symbolic link
  (never followed, so no file elsewhere is read in its place) or otherwise not
  a regular file, over 16 KB, not a JSON object, or without a usable
  description changes nothing and records no gap.
- The record, and any gap about it (including the names of keys dropped
  from a description that is JSON text, filtered as a prompt's is), is
  written only when the transcript has records of its own: it does not make
  an empty or unrecognized transcript look captured, and it carries no
  timestamp, session, or agent identity, so it changes none of the
  transcript's.
- A transcript holds one. A `subagent-meta` record in the transcript itself
  (the one a retained snapshot carries when it is filtered again) is rebuilt
  from its `description` alone, other keys named in `unknown_field_omitted`;
  when the file supplies one, or an earlier record already was one, a later
  record is dropped with an `unsupported_value_omitted` gap.
- A transcript that is not a Claude Code subagent's never reads a
  `.meta.json`.
- The collector leaves the record out when it checks that a new snapshot
  extends the last one, since the description is a label the latest read of
  the file replaces, not evidence of the transcript. The file is read when
  the transcript is, so a `.meta.json` that appears or changes beside a
  transcript that does not change is picked up when the transcript next
  changes, or when a new filter or adapter version re-reads the session (a
  new parser version alone re-derives metadata from the retained snapshot).

### Tool-argument deny list

Retaining tool arguments wholesale has two exceptions, applied at every depth
of a tool-argument subtree. In both cases the argument's key name is recorded
in a `sensitive_or_hidden_field_omitted` gap (`omitted tool argument keys: …`,
sorted, capped at 64, names only) and the value is never retained.

- **Typed or submitted text.** An argument named `text`, `value`, `values`,
  `keys`, or `chars` is dropped when the tool's name (`name` or `tool_name`
  beside the argument subtree), split into words at `_`, `-`, `.`, and
  camelCase, holds one of `type`, `typing`, `fill`, `form`, `input`,
  `keyboard`, `key`, `keys`, `press`, `select`, `paste`, `autofill`,
  `credential`, `verification`, `otp`, `password`, `computer`, or `stdin`
  (filter 11; filter 10 matched a few whole names and missed every
  `fill_form` tool). So Playwright's `browser_type`, `browser_fill_form`, and
  `browser_select_option`, chrome-devtools' `fill` and `fill_form`, a
  computer-use tool, and Codex's `write_stdin` all lose what they typed.
  `Edit`, `Write`, and other tools keep their `text`/`value` arguments.
- **A value labelled as a secret** (filter 11). For every tool, `text`,
  `value`, `values`, `keys`, or `chars` is dropped when a key beside it
  (`name`, `label`, `element`, `placeholder`, `field`, `selector`, `id`,
  `key`, `type`, `autocomplete`, `aria_label`, `for`) says the value is a
  password, passcode, PIN, one-time code, secret, token, card number, CVV,
  security or verification code, SSN, or IBAN: a form field
  `{"name": "Password", "value": …}`, an input `{"type": "password", …}`,
  or an environment entry `{"name": "DB_PASSWORD", "value": …}`; or when
  such a label (not `type` or `autocomplete`) names a credential as an
  argument key would (below), as a HAR header or cookie does
  (`{"name": "Authorization", "value": …}`). A two-string array whose first
  string names a credential (`["X-Api-Key", …]`, a header list) loses its
  second.
- **Credential-named arguments.** For every tool, an argument whose key
  names a credential is dropped. The key is split into words the same way,
  and it names a credential when its words hold a term of the credential
  vocabulary, the one list the value-level patterns below use too
  (`api key`, `access key`, `private key`, `secret(s)`, `password(s)`,
  `passwd`, `pass`, `passphrase`, `token`, `authorization`, `bearer`,
  `credential(s)`, `creds`, `cookie(s)`, `auth`, `auth key`, `account key`,
  `shared access key`, `subscription key`, `access key id`, `seed phrase`,
  `recovery phrase`, and a few more; `pwd` only after
  another word). So `X-Api-Key`, `passwd`, `pass`, `private_key`, `auth`,
  `secrets`, and `password_confirmation` are dropped, while budgets such as
  `max_tokens` (a plural is another word) are kept. An
  argument object whose members were all dropped is pruned with them.

A string that holds a JSON object or array (Codex's
`function_call.arguments` and `custom_tool_call.input`, a tool result an MCP
server returned as JSON text, a Cursor result stored as a string) is decoded
and filtered as the object it is (filter 11), with the rules of an argument
subtree: every key name kept, the deny list applied, every string redacted,
binary blocks dropped, and JSON strings inside it decoded in turn. A string
the filter changes is stored re-encoded (compact, keys sorted); one it
leaves alone keeps its bytes. Filter 10 saw such strings only as text, so a
Codex `browser_type` call kept the password it typed. An array of strings
that is a command line (`["mysql", "-pS3cret"]`) has its secret values
redacted by position, like the command-line shapes below.

### Value-level redaction

Every retained string, at every depth, passes these patterns. A match is
replaced with `[REDACTED]` and a `sensitive_content_redacted` gap is recorded.

- Credential assignments (filter 9). The name ends in a term of the
  credential vocabulary (filter 11: `api_key`, `access_key`, `private_key`,
  `private_key_id`, `encryption_key`, `signing_key`, `master_key`,
  `account_key`, `shared_access_key`, `shared_access_signature`, `auth_key`,
  `subscription_key`, `secret(s)`, `password(s)`, `passwd`, `passphrase`,
  `pgpass`, `token`, `authorization`, `bearer`, `credential(s)`,
  `cookie(s)`, `dockerconfigjson`; filter 12: `access_key_id`,
  `seed_phrase`, `recovery_phrase`), with `_`, `-`, `.`, a space, or nothing
  between the parts of a two-word term (`API Key: …`), in any case,
  with anything glued on before it; or in `pass` or `creds` alone or after a separator
  or a camelCase boundary (`DB_PASS`, `redis.pass`, `dbPass`, but not
  `bypass`); or in `pwd` or npm's `_auth` after a separator (`MYSQL_PWD`,
  `DB_PWD`, `:_auth`), since a bare `PWD` or `OLDPWD` is the shell's working
  directory. The trigger may be followed by `key` or `access_key` (`Secret
  Key` too), then `base`, then a number, then `value`. So `DB_PASSWORD`, `AWS_SECRET_ACCESS_KEY`,
  `OPENAI_API_KEY`, `SECRET_KEY_BASE`, `DB_PASSWORD_1`, `accessToken`,
  `PGPASSWORD`, `spring.datasource.password`, and `x-api-key` all match. The
  name may be quoted (`"…"`, `'…'`, or escaped inside a string, `\"…\"`);
  the separator is `=`, `:`, `:=`, `=>`, a full-width colon or equals sign,
  or a tab; a
  `--name value` (or `-name value`) command-line flag counts too. A quoted
  value is taken up to its closing quote (plus anything glued on after it,
  as a shell reads it; filter 10). An unquoted value runs to the end of its
  line (filter 11; filter 10 stopped at whitespace, `,`, `;`, or a quote, so
  `password: correct horse battery staple` kept three words), stopping
  earlier only at the closing quote of a string the assignment sits in, at
  whitespace followed by shell punctuation, a comment, a flag, or another
  `name=` assignment, at `, ` or `; `, and in a URL query at the next `&`.
  A Cookie header's value runs to the end of the line. A value that is only
  `true`, `false`, `null`, `nil`, `none`, or `undefined` is not a secret and
  is kept. The value is replaced and the rest is kept:
  `DB_PASSWORD=[REDACTED]`, `"password": "[REDACTED]"`. An HTTP scheme
  before the value stays: `Authorization: Bearer [REDACTED]`. A single token
  in brackets or braces is a value too (`password=[hunter2]`,
  `token={abc123}`), with anything glued on after it
  (`password=[REDACTED]realsecret` loses `realsecret`, and a glued bracket
  group goes whole). A value may begin with `=` unless whitespace follows
  it; the extra `=` signs stay (`PASSWORD==[REDACTED]`).
- YAML values on the lines below their key (filter 11): a block scalar
  (`password: |` or `>-` and its indented lines) and an indented scalar on
  the next line (`password:` then `  S3cret`) become one `[REDACTED]` line.
  A key whose indented lines are a mapping or a list is a structure, and its
  members are checked on their own.
- Structures and entries (filter 11): every string value inside an object
  or array whose name is a credential (`"secret": {"value": …}`,
  `"passwords": [ … ]`, in JSON, Python, JavaScript, or JSON escaped inside
  a string, across lines) is redacted, keeping keys, numbers, and the value
  of a descriptive key (`type`, `kind`, `name`, `description`, `provider`,
  `scheme`, `alg`, `format`, `encoding`); and the sibling `value:` of a YAML
  entry whose `name:` (or `key:`) is a credential (`- name: DB_PASSWORD`
  then `value: …`, a Kubernetes `env` list), or the `"value"` after a
  credential `"name"` on one line (`{"name": "Authorization", "value":
  …}`).
- Files shown with line numbers (filter 11): the Claude Code Read tool,
  `cat -n`, and `grep -n` number each line. The YAML, entry, and private
  key rules read a line after its number.
- Command lines whose secret is a flag's value only for particular programs
  (filter 11): `curl -u user:secret` (and `--user`, `-U`, `--proxy-user`;
  the user name stays), `mysql -psecret`, `sshpass -p`, `docker|podman|helm
  … login -p`, `az login -p`, `redis-cli -a`, `mongosh -p`, `sqlcmd -P`,
  `ldapsearch -w`, `zip -P`, `7z -p`, `ssh-keygen -N`, `keytool
  -storepass`, `openssl -k` and `pass:secret`, macOS `security … -p`/`-w`,
  `gh secret set --body`, `aws configure set NAME value`, `npm|yarn|git
  config set NAME value`, fish `set -gx NAME value`, `setenv NAME value`,
  and PowerShell `ConvertTo-SecureString "…"`. `ssh -p 22` and `mkdir -p`
  are left alone.
- `.netrc` passwords (`machine … login … password secret`, or a `password
  secret` line), XML elements and attributes (`<password>…</password>`,
  `<add key="ApiKey" value="…"/>`), and URL query parameters whose names are
  not credential words (`?key=`, `&sig=`, `X-Amz-Signature=`) (filter 11).
- Machine pairing bundles (filter 15): a word-boundary `aa-pair1:` prefix,
  case-insensitive, followed by one or more ASCII letters, digits, `_`, `-`,
  `+`, `/`, or `=`. The prefix and entire contiguous payload are replaced,
  even when short, truncated, padded, or standard base64 instead of base64url;
  there is no decoding or cryptographic validation. Quotes, backslashes,
  whitespace, and other punctuation stop the match. A bare prefix, a prefix
  inside an ASCII word (`xaa-pair1:abc`, `_aa-pair1:abc`), other versions
  (`aa-pair2:abc`), and an unprefixed ordinary-word pairing code are kept.
  A payload split by whitespace or punctuation is recognized only through
  its first contiguous run; text after that boundary is not inferred to be
  part of the bundle.
- AWS access key IDs (`AKIA…`, and `ASIA…` for temporary STS credentials),
  Anthropic/OpenAI style `sk-` keys, and, since filter 11, the prefixed
  tokens of Stripe (`sk_live_`, `rk_live_`, `whsec_`), GitLab (`glpat-` and
  its siblings), Google (`AIza…`, `GOCSPX-`, `ya29.`), Hugging Face (`hf_`),
  npm (`npm_`), PyPI, SendGrid, Shopify, DigitalOcean, HashiCorp Vault,
  Databricks, Linear, Grafana, Postman, New Relic, Sentry, Atlassian, Figma,
  Doppler, age, Mailgun, Telegram bots, and Azure AD client secrets, and
  since filter 12 Groq (`gsk_`) and xAI (`xai-`) keys; Slack,
  Discord, and Teams incoming-webhook URLs (the path after the host); and a
  `Bearer` token outside a header.
- PEM and PGP private key blocks: `-----BEGIN … PRIVATE KEY-----` (or
  `PRIVATE KEY BLOCK-----`) through the next `-----END … -----` when
  everything between is key body (each line possibly numbered, marked as a
  diff, quote, or comment, or quoted as a string in source code), or holds a
  base64 run of 48 characters or more (and no other BEGIN line); when the END
  line is missing (a key cut off by a truncated record), through the base64
  lines that follow the BEGIN line, a line with a 48-character base64 run
  counting as one (filter 11; filter 10 took everything to the end of the
  string, so source code that names the BEGIN line lost the rest of the
  file). When the BEGIN line is missing (a key read in two parts, or a tool
  result in chunks), the base64 lines right above an END line that starts
  its line are redacted with it. Certificates and public keys are not
  redacted.
- A PEM block encoded in base64 whole (kubeconfig `client-key-data`), from
  its `LS0tLS1CRUdJTi` prefix, certificates included (filter 12).
- Docker `config.json` `"auth"` and `"identitytoken"` values shown as text
  (filter 12).
- `.pgpass` lines (`host:port:database:user:password`): the password, to
  the end of its line, when the port is 5432, 6432, or `*` (a file read by
  the Claude Code Read tool), and with any port in a string that mentions
  `.pgpass` or `PGPASSFILE` (`cat ~/.pgpass`, a heredoc into it) (filter
  12).
- Wallet seed phrases: 12 to 24 words of three to eight letters after a
  name holding `mnemonic` (`MNEMONIC="…"`, `--mnemonic "…"`, a
  comma-separated list or an array), or exactly 12, 15, 18, 21, or 24 after
  one holding `seed` (`SEED=…`, `wallet seed:`) (filter 12).
  `SEED_PHRASE=` and `Secret Recovery Phrase:` are credential names above.
- JWTs: three base64url segments, the first beginning with `eyJ`.
- URL userinfo: in `scheme://user:pass@host` (or `scheme://user@host`) the
  userinfo is replaced and the scheme and host are kept. The userinfo ends
  at the last `@` of the authority, and a password holding `/`, `?`, or `#`
  runs to the `@` a host follows (filter 11), unless the part after the
  colon is a port.
- GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`) and
  Slack tokens (`xox[abposre]-`, `xapp-`).

Known false positives. The assignment pattern cannot tell a credential from
code or a path: `token = parse(x)`, `nextToken := lexer.Next()`,
`credentials := loadCreds()`, and `password: required` have their
right-hand side redacted; so do a saved directory in a `*_PWD` variable
(`ORIG_PWD=$(pwd)`), a path to a credentials file
(`GOOGLE_APPLICATION_CREDENTIALS=/path/key.json`), a one-item list
(`password: [required]`, taken for a bracketed value), and the word after a
flag in prose (`pass --token flag`). Every scalar under a YAML key named
like a credential is redacted, so references to secrets lose their names
too: Docker Compose `secrets:` then `- db_password`, or `file:
./db_password.txt` under it. Between a private key's BEGIN and END lines,
any text with a 48-character base64 run is taken as the key, so code
between two constants that name the armor lines is redacted when it holds
a long hash. This is accepted rather
than narrowed, because the cost of a missed credential is higher than the
cost of a redacted identifier in an archived transcript; a reader sees the
`sensitive_content_redacted` gap and can consult the original source if it
still exists. Names with anything after the trigger word other than the
suffix above (`tokens`, `max_tokens`, `token_count`, `secretary`,
`password_policy`, `TOKEN_URL`, `SECRET_NAME`, `--password-stdin`) do not
match, nor do `PWD`, `OLDPWD`, `auth`, comparisons (`token == nil`), or a
name with no value.

Known misses.

- An unquoted value runs to the end of its line (filter 11), so an
  environment prefix before a command loses the command too:
  `TOKEN=abc npm test` becomes `TOKEN=[REDACTED]`. This errs toward the
  secret. A space-separated `-p value` with no program the filter knows
  (`tool -p secret`) is not recognized.
- In JSON escaped more than once inside a string (`\\\"password\\\":…`),
  the value ends at the first escaped quote of any depth, so the tail of a
  value after an escaped quote inside it (`\\\"ab\\\\\\\"cd\\\"`: `cd`) is
  kept. JSON escaped once (`\"password\":\"ab\\\"cd\"`) is handled.
- A value that is an object or array holding whitespace, a comma, a colon,
  or a quote (`"credentials": {"type": …}`, `password: [required, min 8]`)
  is a structure and is not replaced as text. Its string values are
  redacted (above), but an unquoted value inside it (a YAML flow mapping
  `{user: me, pass: x}` aside from its own credential names, a number such
  as a PIN) is caught only by its own name or shape. A YAML mapping or
  sequence written over the lines below a credential key (`secrets:` then
  `github: …`) is redacted value by value (filter 11).
- For the same reason, an unquoted value that begins with `[` or `{` but is
  not a single closed token is kept whole: `password=[Kx9!q2Lm`,
  `password={secret`, `password=[admin:hunter2]`, `password=[a b]realsecret`.
  About 2% of random passwords drawn from the full symbol set begin with a
  bracket or brace.
- A project skill's `SKILL.md` that is a hard link to another file cannot be
  told apart from a real file. A cloned repository cannot create one (git
  does not store hard links); it needs local write access to the project.
- A name that does not end in a trigger word (`DATABASE_URL`, `DSN`,
  `CONNECTION_STRING`) is not redacted by this pattern; its value is
  redacted only if it has a recognizable shape (`AKIA…`/`ASIA…`, URL
  userinfo).
- Access key IDs are redacted (`*_ACCESS_KEY_ID`, filter 12), although an
  ID is not a secret on its own: AWS's were already redacted by their
  `AKIA…` shape and dropped as tool arguments, and an R2 access key ID is
  the ID of the Cloudflare API token behind it. Other key IDs
  (`KMS_KEY_ID`, `private_key_id` aside) are kept.
- **A `.pgpass` line on an unusual port, shown without the file's name
  in the same string, keeps its password.** The Claude Code Read tool
  shows a file's lines with its path in the call, a separate record, so
  `db:15432:app:alice:secret` read that way is kept: five colon-separated
  fields alone are too common a shape (compiler diagnostics, log lines) to
  redact. Lines on port 5432, 6432, or `*` are redacted wherever they
  appear.
- A seed phrase is recognized only after a `mnemonic` or seed name, on one
  line: a numbered list (`1. legal 2. winner …`), a YAML block sequence
  (`mnemonic:` then `- legal` lines), a phrase in another language, or one
  with words outside three to eight letters is kept.
- A credential in prose (`the password is hunter2`), a Markdown table row
  (`| password | hunter2 |`), a name spelled with full-width letters or a
  zero-width space, or a leetspeak name (`p4ssword=`) is not recognized;
  nor is a secret piped into a command (`echo … | docker login
  --password-stdin`, `sudo -S`), a form label in another language (`Mot de
  passe`). A URL-encoded assignment (`password%3D…`, `token%3A…`) is
  recognized (filter 11), but not one encoded twice (`password%253D…`) or
  in another encoding (base64, HTML entities). A YAML value on the lines
  below its key is recognized (filter 11); when its first line reads as a
  mapping entry (`correct horse: battery`), each value is redacted and the
  keys are kept.
- The middle part of a private key split into three or more strings (no
  BEGIN or END line in it) has no marker and is kept, as is a key in a
  format with no armor lines (PuTTY `.ppk`, a JWK's `"d"`, a raw base64
  file) unless its name or shape is recognized.
- Text glued after a closing quote is taken with the value (filter 10) only
  up to a closing `]`, `}`, or `)`, which usually closes the structure
  around the value (`{"password":"abc"}`, `f(PASSWORD="abc")`) and must stay.
  So in the rare `PASSWORD="abc")realsecret`, `realsecret` is kept.
- A Cursor plain-text transcript has no structure beyond its role headers,
  and its exact format is not pinned by a fixture in this repository. Since
  filter 11 a header is a role and a colon at column 0, written in the case
  of the transcript's first header: lower case (`user:`) or capitalized
  (`User:`), never both in one transcript. A line in the other case is
  content, so in a lower-case transcript capitalized prose such as
  `Analysis: …` hides nothing, and in a capitalized one a YAML `user:` line
  in tool output starts no Person turn. When the transcript separates its
  sections with blank lines, a `user:`, `assistant:`, or `tool:` line that
  does not follow a blank line is content too. What remains: a column-0
  line in the transcript's own header case still starts a section in a
  transcript without blank lines between sections, and after a blank line
  in any transcript; and a column-0 hidden role in that case (`system:`,
  `thinking:`, …) always hides what follows, so nothing that might be a
  hidden section is kept (the `hidden_instruction_omitted` gap counts the
  sections and lines hidden). A transcript whose first line is not a header
  in either case (`USER:`, or text before the first header) is refused as
  a capture gap. Cursor's JSONL transcripts and database chats are not
  affected.

Redaction is best effort in both directions: a legitimate value that looks like
a credential is redacted, and a tool argument that happens to contain one of
the instruction tags above loses that span. Both are recorded as gaps.

Codex history parser 0.22.0 can add the content-free metadata gap
`history_cumulative_tokens_unavailable`: a cumulative counter cannot establish
own usage across inherited history. It retains independently observed own
per-call accounting and reports no invented parent-baseline subtraction.
This changes derivation, not the privacy allowlist or filter version.

Leading native `<external_codex_apps_open_page>` context wrappers follow the
nested and unclosed-block omission rules. User prose, quoted markup and code
examples are preserved, as is human text following a context wrapper.

Verified Codex external names are typed `session_labels` supplemental evidence:
`native_session_id`, `state`, optional `name`, `source`, and `contract` only.
The UUID must match the owning ordinary Codex source. `present` requires a
nonempty filtered name; `confirmed_absent` carries no name. Unknown fields,
malformed IDs and history bundles are refused. Raw names are bounded to 16 KiB,
receive normal credential redaction, normalize whitespace/control characters,
and truncate to 512 bytes on a UTF-8 boundary before hashing or caching.
An unavailable observation is not absence and preserves previously verified
evidence. Native paths, raw database rows and index contents are not retained.
Codex `session_meta` additionally retains only the `legacy` or `paginated`
`history_mode` enum to establish index interpretation. Other values are omitted.

Filter 18 accepts the fixed `api`/`codex-api-159.2-v1` session-label pair in
addition to the file contract. It retains only the same owning ID, filtered
bounded name or explicit absent state, contract and stable observation time.
Native JSON responses and process output are discarded before persistence.
