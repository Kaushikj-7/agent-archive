# Migrate the archive

`agent-archive migrate` copies an existing archive into a separate catalog
bucket or prefix and verifies metadata and active or preserved source bytes
before selecting the new destination. Specify `--format catalog-v4` and an
isolated `--prefix`, `--bucket`, or both. At least one destination option is
required; the destination must remain separate from the original archive.

Catalog migration is currently disabled for production S3/R2 destinations.
It requires reviewed provider atomic and clock qualification, revoked old write
grants, and complete writer drain evidence. The command refuses when that
qualification or cutover authority is unavailable, before changing local
configuration. There is no `--force` bypass.

The command's syntax is:

```sh
agent-archive migrate --format catalog-v4 --prefix NEW_PREFIX
agent-archive migrate --format catalog-v4 --bucket NEW_BUCKET
```

An interrupted copy resumes its durable checkpoint when run with the same
destination. The original archive remains read-only. A successful activation
selects the catalog destination only after exhaustive metadata, source, and
preserved-history verification.

`--rollback` returns to the retained source only while the catalog has not
changed since activation. Rollback leaves collection paused:

```sh
agent-archive migrate --format catalog-v4 --prefix NEW_PREFIX --rollback
```

Provider and cutover checks still apply to rollback. These commands document
the interface; they do not establish that a live destination is qualified.
