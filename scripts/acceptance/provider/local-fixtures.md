# Independent disposable provider fixtures

These opt-in tests exercise current main's portable listing, ordinary publication
journal 2 and staged parent/child publication journal 3 against a real loopback
MinIO service. They preserve modern selection proofs, stage ownership and storage
capabilities. They add no production capture route and enable no workflow.

Build the two exact sources using Go 1.27.2 with normal SumDB verification:

```sh
GOBIN=/absolute/private/tools CGO_ENABLED=0 go install github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c
GOBIN=/absolute/private/tools CGO_ENABLED=0 go install github.com/minio/mc@v0.0.0-20251106162529-77f82e18b540
python3 scripts/acceptance/provider/run-local.py --run-disposable-provider \
  --tools /absolute/private/tools --output /absolute/new/evidence
```

The runner requires a clean committed candidate, checks tool module identities,
records binary hashes and source commit, and starts only its own service process.
It creates an ephemeral directory, loopback listener, scratch bucket and two
separate users restricted to a randomly generated run prefix. It removes inherited
AWS/MinIO/provider settings, supplies explicit SDK credentials, and reaps only its
own process. Native fixtures, local state, service storage and administration
credentials remain temporary. Test constructors prove actual out-of-prefix PUT
and LIST denial before running lifecycle assertions and verify their own child
prefix is empty during cleanup. HTTP requests have fixed time and attempt bounds.
Credential detection redacts evidence and fails acceptance.

The three named tests cover portable summaries and canonical reader fallback,
actual successful metadata PUT followed by injected acknowledgement loss and
exact journal-2 recovery after native deletion/restart, and journal-3 staged
parent/child publication after both native files disappear. A separate staged
subtest loses the parent acknowledgement while the child publishes and creates
a genuinely newer parent request: it requires the frozen parent to refuse
cleanup and preserve both the exact journal and the newer request across restart.
That guarded refusal is a safety result, not completed newer-work publication. Independent credential
clients read the retained sources without access to local state. Ordinary CI
compiles these tests and skips them without explicit provider opt-in. A skip is
not provider acceptance. Retain successful command exit and independently verify
all three named tests and their package terminals using the evidence verifier.

These fixtures do not establish sourceSet5 history publication, changed-policy
history replay, live-WAL admission, complete deletion/retention/restoration,
cloned-owner concurrency, distributed CAS, AWS/R2 accounts, real native app
layouts, native macOS/arm64 execution or old-release compatibility. Those gates
remain separate and must be integrated against their immutable implementations.
