# Reviewed Muse permission-frame excerpts

These data excerpts belong to the [qualification report](../muse-qualification-readiness.md).
See [manifest](manifest.json) for provenance, hashes, scope and expected diagnostics.
They contain no model context and are not complete session fixtures.

The first file is exact native 1.4.8 bytes. The second is a derived checksum mutation.
Do not reserialize the original before using it as native framing evidence.

Repeat the exporter observation with the verified CLI, writing only to temporary storage:

```sh
/private/tmp/agent-archive-muse-qualification-20261014/bin/muse-1.4.8-R6256.1 --version
/private/tmp/agent-archive-muse-qualification-20261014/bin/muse-1.4.8-R6256.1 export --session dev/specs/muse-qualification-fixtures/permission-frame-1.4.8.jsonl --out /private/tmp/muse-permission-valid-export.json
/private/tmp/agent-archive-muse-qualification-20261014/bin/muse-1.4.8-R6256.1 export --session dev/specs/muse-qualification-fixtures/permission-frame-invalid-checksum.jsonl --out /private/tmp/muse-permission-invalid-export.json
```

Check `diagnostics.unparseable_lines`: valid 0, mutation 1. Both commands may exit 0.
These observations do not qualify independent checksum encoding or the archive decoder.
A temporary CLI may disappear; use the same checksum-verified build to reproduce.
