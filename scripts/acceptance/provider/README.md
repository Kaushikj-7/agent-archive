# Provider acceptance evidence foundation

`verify-results.py` checks a Go `test -json` stream against explicit
`package::test` identities, including any required descendants. It requires each
expected package to start and pass, every observed test to run and pass, and every
required identity to appear. Missing terminals, duplicate executions, unexpected
packages, failures and skips refuse acceptance. Output and parallel scheduling
events are permitted. Use one stream per invocation with `-count=1`.

For example, after running the corresponding package with `go test -json`:

```sh
python3 scripts/acceptance/provider/verify-results.py events.jsonl \
  github.com/wangjohn/agent-archive/internal/listingindex::TestRevisionDestinationKeyLimitsBeforePut
```

This verifier checks execution completeness. It does not authenticate a log or
bind it to source, dependencies, credentials, a service or a runtime. A future
provider runner must record those independently, retain the command's successful
exit status, and pass every required descendant explicitly. Synthetic verifier
tests run in ordinary PR CI; they establish no provider acceptance.

The historical combined provider/native-runtime candidate remains incomplete.
Current main already implements portable listing summaries with 255-byte
components, legacy read compatibility and full destination key write limits.
The historical candidate's distinct 240-byte `c/` encoding must not replace it.
Its old lifecycle selectors and Go version cannot be copied into an active
workflow as evidence for the current implementation.

Before activating a provider workflow, integrate fixtures with the current
publication, privacy, live-WAL admission and deletion implementations. Run them
against an actual disposable service using run-prefix credentials and prove
out-of-prefix refusal, independent clients, uncertain acknowledgements, retained
reading after native/local-state loss, and owned cleanup. Record exact candidate
and dependency identities. Native Linux/Darwin amd64/arm64 execution and pinned
published-writer compatibility remain separate gates; cross-compilation and
synthetic storage do not satisfy them. AWS/R2 account behavior and live app
layouts require their own evidence.
