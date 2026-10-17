# Prepare Muse replay launch artifacts

An external evaluation runner can use `agent-archive eval replay-plan --harness muse` to prepare a private launch plan. The command reads one versioned JSON request from stdin and writes one JSON response. It uses the production Muse hook planner, reads no settings or archive state, and changes no files. Each complete request and response is limited to 64 KiB, including base64 and repeated settings snapshots; a larger document is refused.

Plans currently report `qualified:false` and `preparation_only:true`. Native forwarding, permission behavior and complete descendant lifetime still require mode-specific qualification. Planning does not authorize capture or make ordinary handoff a replay. The external runner retains normal setup/project consent and process supervision responsibilities.

The repository's `scripts/muse_replay_launch.py` provides `prepare_from_archive(executable, request)`. It invokes the selected built executable as an argv array, with bounded stdin, stdout/stderr and time, then consumes the actual production planner response. It does not invoke a shell, Go toolchain or native agent. The executable must be an explicit absolute path and match the request's executable. A response digest records integrity; the qualification packet independently pins the executable and source.

The closed request has these fields:

| Field | Value |
| --- | --- |
| `version` | `1` |
| `executable`, `data_home` | Explicit canonical absolute archive executable and archive data directory |
| `default_data_home` | Observed canonical default archive directory, or an empty string when no default-location ownership is involved |
| `ownership_locations` | At most 32 observed absolute path-to-canonical-path equalities, covering the data home, any nonempty default home and every ownership location referenced by the existing hook inspector |
| `private_directory` | New absolute private launch directory; no traversal, NUL or newline |
| `control` | Exactly `{"launch_id":"opaque-token","replay":{"run_id":"opaque-token"}}`, or `replay:{}` for an anonymous replay |
| `settings`, `managed` | File observations described below |

Both IDs use the existing replay identifier grammar: 1–128 ASCII letters, digits, `.`, `_`, `:` or `-`, beginning with a letter or digit. Omission, null, duplicate/case-variant/unknown control fields and raw malformed marker text are refused. Anonymous replay uses one fixed safe invalid sentinel in the private runner; its stored marker remains `{}`.

Each file observation contains exactly `path`, `present`, `bytes`, `mode` and `regular`. `bytes` is base64. A present file requires an absolute canonical path, regular-file observation and permission bits no greater than `0777`. An absent observation has empty bytes, mode zero and `regular:false`. Settings still require their explicit path when absent. Managed observations require an existing absolute `managed_hooks_path` reference in settings; with no such reference, use `{"path":"","present":false,"bytes":"","mode":0,"regular":false}`. Relative managed sources, missing referenced files, malformed settings and unresolved ownership are refused rather than repaired.

Ownership equalities are caller observations for planning. They do not grant capture consent. Resolve them with the existing ownership/canonical-location observer outside the pure planner, preserve other installations, and recheck the actual file and location bindings before launch. Missing or ambiguous equality observations are refused; differing path spellings are never assumed to represent a safe foreign installation.

The response freezes protocol version 1 and includes the normalized control, exact observed settings/managed bytes, owner presence, a hypothetical install plan, an owner-only plan and removal from the **actual observed file**. A missing owner has no removal plan. It is not reported as installed. The original byte observations remain distinct from hypothetical replacement bytes.

The Python helper returns the original observations, hypothetical settings, a `0700` direct runner, a `0600` managed document and per-run argv overrides. The runner executes the exact generated production hook command with the launch's bounded marker. Existing foreign handlers/order and managed environment entries remain present. Ordinary settings and global environment allowlists are not changed. Optional `materialize_private_files` creates only an absent directory in an owned private stable parent and exclusive regular files; it never replaces an existing launch directory.

The external runner owns the launch files until the native root and **every** owned hook, child, observer, workflow and server descendant has joined or been terminated and accounted for by its qualified supervisor. Root exit alone does not authorize cleanup. Retain the files and receipts if complete drain cannot be established. No automatic launch or cleanup follows preparation.
