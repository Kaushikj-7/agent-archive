# Search conversation text

Content search finds words in indexed titles and visible user/assistant messages.
It keeps a private local copy of that text. This initial version indexes sessions
only when explicitly requested; it does not install background indexing or
change capture, uploads, `list`, `show`, or `handoff`.

Find a session ID using `agent-archive list`, then index its verified archive
sources, including supported preserved native revisions:

```sh
agent-archive search --refresh --session SESSION_ID --harness codex
agent-archive search "upload checksum"
agent-archive search '"checksum mismatch"' --all-projects
agent-archive search 'check*' --project my-project --since 7d
```

Normal queries read the local index and do not open remote storage or refresh
history. A missing index reports incomplete coverage without creating one.
Results are grouped by session with a matching excerpt. They search the current
repository using the same repository/name/project fallback as `list`; use
`--all-projects` to broaden. Replay and child sessions are hidden by default;
use `--replays include` or `--children include` to include them.

Words are ANDed within one message passage. Quotes match a phrase. Only an
explicit trailing `*` enables prefix matching. Case is ignored; accents are
significant. Punctuation splits words, so filenames and error strings match
word sequences rather than exact byte substrings. This is keyword search;
terms with similar meaning do not automatically match. Queries have a 512-byte
limit. Messages are chunked into 8KiB passages with approximately 1KiB overlap;
very long tokens/phrases across that overlap can be missed.

A body match includes an opaque `ref:` token. Read its exact source text with:

```sh
agent-archive search --read REF --max-bytes 4096
agent-archive search --read REF --offset NEXT_OFFSET --max-bytes 4096
```

Reads verify current source membership and bytes. They print a quoted escaped
text string, plus byte offsets and a continuation offset on stderr. Offset 0 in
the continuation means complete. References can expire after updates, deletion,
privacy/extractor changes or a destination switch; refresh the session and search
again. Title-only results have no passage reference; use `show SESSION_ID`.

`--limit` defaults to 20 and accepts 1–100. A result page may provide a
`Next cursor`; pass it with the same query/filters using `--cursor`. Index writes
expire cursors and require restarting pagination. No long database transaction
is held while you review a page.

Coverage is explicitly partial: sessions not indexed, sources not readable by the
parser, and changes on other machines may be absent. Refreshing a session reads
its latest archive sources; querying does not prove remote freshness. Tools,
reasoning, attachments, generated harness context and copied other-thread history
are excluded according to the existing parser's ownership/visibility rules.
Sources with privacy filters older than version 6, newer than this binary, or
unknown versions report a coverage gap because their visibility rules cannot be
interpreted safely.

The index is stored under `AGENT_ARCHIVE_HOME/search/` (or the normal private
data directory), separate from the metadata cache. It is partitioned by archive
destination, installation, destination activation and current privacy version.
Files are private to the owner. Explicit refresh works while capture is paused.
Uninstall retains this local data, like other archive state. To discard all
local searchable text, stop search commands and remove that `search` directory;
this does not delete archived sources. No downloaded source cache is retained.
See [privacy](../security/privacy.md) for what archive filtering retains and
[engine contracts](../../dev/proposals/content-search-engine.md) for measured
performance, tokenization and history limitations.

Explicit refresh records desired state in the same search database and executes
bounded indexing work. Completed sources survive interruption and are reused on
retry. Unchanged sources, including retained alternatives, need no new download
or parse; changing only titles or display/filter metadata avoids source reads.
Body passages are rewritten only when their projection changes. Changed source
snapshots are still parsed in full; indexing is not an append-only parser.

Refresh reports pending and failed work. Temporary failures use persisted
exponential backoff (one second up to 256 seconds); retry after that delay.
Oversized work stays blocked until its desired snapshot changes. A pass reads at
most 64 sources and 32 MiB compressed data, with a 30-second deadline covering
authoritative metadata reads and indexing, so sessions with many retained
revisions may require another refresh. Each pass can also advance previously
observed pending sessions. There is no automatic archive discovery or scheduled
worker in this release. Cached search results report known pending/failed state,
which does not establish coverage of sessions never observed on this machine.
