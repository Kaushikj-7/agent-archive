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

Build scoped picker indexes in an existing catalog with:

```sh
agent-archive migrate --scoped-picker
```

To construct a new scoped catalog from a legacy archive, add `--scoped-picker`
to the isolated migration command. Both routes require reviewed protocol-11
credential authority; production S3/R2 providers still refuse. Construction
hydrates verified immutable metadata in finite pages and retains original
revisions, receipts, tombstones and source/history bytes. An existing active
catalog stays readable during construction. Every membership is verified before
one head commit makes the complete format visible. Publication stays sealed until
the permanent scoped configuration fence is saved.

Repeat the same command after interruption. Its durable checkpoint identifies
the exact seal owner, captured source roots, candidate roots and progress. An
unknown pending owner must settle through its original frozen journal route;
time and references never grant permission to discard it. Garbage collection
cannot cross an upgrade seal or reclaim candidate roots.

Scoped rollback requires the exact initial activated head and refuses after any
later publication or head change. It retains the original fully indexed data
under head/coordinator protocol 11 and protected configuration version 14; it
seals publication and pauses collection. It never restores an unindexed root or
permits an older writer. For an existing-catalog upgrade, use:

```sh
agent-archive migrate --scoped-picker --rollback
```

Migrating a legacy configuration with settled Cursor protection retains that
permanent flag and every staged/publication/storage protection in scoped
configuration 14. The scoped capability fences older writers; catalog destinations
continue to refuse staged and settled Cursor selection. Rollback preserves all
remembered protections.
