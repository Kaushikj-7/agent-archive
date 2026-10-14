# Ordinary Codex admission publication space

Fresh ordinary singleton Codex selections leave publication space below the
shared 1 GiB quota. The producer classifies the complete immutable `ImportStage`
after digest/owner/capacity validation. It passes a private headroom policy to
the bounded encoder and the same locked quota census/rooted atomic commit.
No source or release authority is inferred from a path, and no new credit or
reservation codec is introduced.

The class accepts only file imports with a validated Codex binding: non-child,
no parent or subagent/root-ancestor registration facts, no own-history start, CLI producer, and native thread, physical rollout
and root IDs all equal. Stage versions 1 and 2 are supported. Children, preserved
revisions, history, linked sessions, historical projects, frozen captures,
previous generations and Cursor facts are excluded. Unsupported or absent
bindings keep the existing admission policy. The stage selection and its
privacy/owner validation remain unchanged.

Only this class's new/growing stage data uses the lower ceiling. Grouped,
historical, child and other integrations' selections, generic durable writers,
cancellation and release controls retain their full-quota behavior. The check
uses the bounded encoder wire length, existing twice-file growth charge and
new directory's 64 KiB control charge. Shrinking and idempotent stages remain
usable. Unknown-owner refusal and all privacy/ownership proofs are unchanged.
A refusal commits no new selected data, allowing existing foreground OnCapacity
handling to publish an admitted group and retry.

The conservative reserve is 320 MiB + 65,540 charged bytes:

- The complete pending JSON, including Bundle, base64 SourceBytes and
  MetadataBytes, ready/staged selection and descriptors, is at most 160 MiB
  including its encoder newline. `collector.pass.scan` installs
  `SessionReadCeiling = PassReadBudget - PassCacheReserve` (256 - 96 MiB).
  `state.writeDurableGuardHeadroom` reserves scratch, bounds the complete
  wire to scope availability minus one, adds the newline, then reserves it.
  The existing double disk charge is therefore at most 320 MiB.
- Exact typed release receipt and progress are each bounded to 16 KiB plus
  one encoder newline by `state.writeStagedControl`, each charged twice:
  65,540 bytes in total. Progress rewrites need no growth beyond this bound.
  This is additional space; the stage directory's existing 64 KiB control
  allowance is neither borrowed nor reinterpreted.
- These selections have no history source stages, preserved history inputs,
  generation-recovery creation or opaque publication-evidence copies.
  Their only additional quota payload before verified stage release is the
  pending wire and, for typed selections, the bounded release controls.
  Published cache/registration/request files remain outside this quota exactly
  as before; their memory ownership and durable checks still apply.

Twice-file accounting retains atomic-copy allowance, rather than adding a
third uncharged copy. Interrupted files already present count in the next
census before admission. The quota serializes writes and collector ownership
serializes scans. Exact restart uses the same retained pending selection and
readback/release proofs; no native substitution supplies space or authority.

This prevents fresh all-ordinary backfill staging from saturating publication
space through the standard scoped collector, with stable privacy/filter policy.
It does not guarantee provider success or eliminate independent memory/owner
refusals. Mixed homes with other admission classes or unrelated obligations
can still exhaust publication space; all obligations remain fully counted.

This does not migrate or recover homes already saturated under the earlier
policy. Memoryless direct `Store` writers can write pending wires up to 512 MiB
and retain their existing limits; they are outside the scoped collector bound.
Grouped/history imports retain their existing quota behavior and its pre-existing
capacity limitations. History can retain original and replacement compressed
sets, with a transient source written before aggregate validation, so the
ordinary reserve must not be presented as a history recovery bound. Existing
unknown obligations and interruption debris are never deleted or credited from
guessed ownership.

The regression uses canonical Codex sources, real Registration/OnCapacity,
reopened Collector.Run, output readback and verified stage release under the
unchanged quota. A canonical parent plus two retained children also verifies
that the ordinary policy preserves prior grouped admission. These are correctness
checks, not the frozen 1500/1350 complete-command performance acceptance.
