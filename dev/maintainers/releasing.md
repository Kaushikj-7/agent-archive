# Releasing

Pushing a `vX.Y.Z` tag starts `.github/workflows/release.yml`: a read-only
preflight followed by the build and signing jobs below.

The **preflight** resolves the remote tag and refuses a mismatched main
commit. If a release already exists, it downloads and verifies the complete
candidate, API digests and sizes, manifest, checksums, provenance, Darwin
signatures and online notarization assessment. A verified existing candidate
(or already promoted release) skips all rebuild/sign/publish jobs. A partial,
draft or mismatched release fails closed before signing.

1. **build** (macOS runner, read-only token, no secrets). Refuses a tag that
   isn't exactly `vX.Y.Z` or whose commit is not on `main`
   (`git merge-base --is-ancestor`), runs `go vet`, the existing source-bound
   full macOS tier, and the script tests, cross-builds `darwin/amd64` and `darwin/arm64`
   with `scripts/build-release.sh darwin` (the same script contributors
   run), and checks the embedded version.
2. **build-linux** (`ubuntu-24.04`, read-only token, no secrets). The same
   tag and `main` guard and the same tests, then cross-builds `linux/amd64`
   and `linux/arm64` with `scripts/build-release.sh linux` (pure Go,
   `CGO_ENABLED=0`, statically linked). `scripts/verify-linux-release.sh`
   checks that both are static, runs the amd64 binary natively and requires
   its `--version` to equal the tag, and checks that the arm64 binary (which
   cannot run on the runner) embeds the tag.
3. **publish** (macOS runner, needs both builds, runs in the `release`
   environment, the only place the Apple secrets exist). Checks the signing
   configuration, codesigns and notarizes the two macOS binaries, checks
   them against the same Developer ID requirement `install.sh` uses,
   computes `SHA256SUMS` over all four binaries
   (`scripts/write-checksums.sh`), records a
   [build provenance attestation](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
   for all four and the candidate manifest, then stages a **prerelease with
   latest=false**. It retains the signed workflow artifact for 30 days before
   creating the release. The current public latest release stays unchanged.

Both build jobs explicitly select the full tier for the release tag, retaining
all discovered Test/Fuzz/Example parents and their children, race, `-count=1`,
and each invocation's 20-minute package timeout. The existing standalone
`[[0], [1, 2]]` schedule gives the qualification parent its own package lifetime;
discovery, race invocations and full-size plain performance checks share the
unchanged 2400-second tier deadline. macOS also retains its protected-configuration
writer check. The jobs verify exact-source/raw-body evidence before Build and
upload separate `release-validation-macos` and `release-validation-linux`
artifacts even on failure. These validation artifacts are separate from the
release assets; they do not authorize signing or promotion.

The candidate and promoted release have six assets: `agent-archive-darwin-amd64`,
`agent-archive-darwin-arm64`, `agent-archive-linux-amd64`,
`agent-archive-linux-arm64`, `SHA256SUMS`, and `release-candidate.json`.
The manifest binds the tag, full commit SHA, repository, Go toolchain, signing workflow run ID, Accepted
notarization submission IDs, and SHA256 digests of the four binaries and
`SHA256SUMS`. It has its own build provenance attestation; its digest is
recorded in the later acceptance sheet, avoiding a self-digest cycle. The
checksum file still lists exactly four binaries, as the installer expects.
The Linux binaries are
**unsigned**: Apple signing and notarization apply to the macOS binaries
only, and Linux has no equivalent step. Trust in a Linux binary rests on the
checksum in `SHA256SUMS` (integrity only, since both files come from the same
release) and on the GitHub build attestation, which ties the exact bytes to
this workflow run and commit.

The attestation subjects are the four binaries, `SHA256SUMS`, and the
manifest; the retained workflow artifact and prerelease upload carry the same
files;
`scripts/test_release_assets.py` fails when they disagree, or when a Linux
binary reaches the codesign or notarize steps. When you add or rename a
release binary, change `scripts/write-checksums.sh`, those inventories, and
that test together. `scripts/test_release_candidate.py` checks the six-asset
inventory, API digests, retry reconciliation and promotion gate.

The staging workflow runs only on a tag and has no manual trigger, so it is not
exercised by pull requests. The [Test workflow](../../.github/workflows/test.yml)'s `cross-build` job runs the
Linux half of it (`scripts/build-release.sh linux` and
`scripts/verify-linux-release.sh` on a placeholder version) on every pull
request. To rehearse the rest, push a `vX.Y.Z` tag on a commit in `main` to
a fork: `build` and `build-linux` run in full, and `publish` stops at its
signing-configuration check because the fork's `release` environment has no
Apple configuration. Never work around that stop to publish from a fork.

The tag name reaches scripts only through the `VERSION` environment
variable, never expanded into a script's text.

Update [CHANGELOG.md](../../CHANGELOG.md) before tagging; the release notes
point at it and at the [install guide](../../docs/getting-started/install.md).

## Repository settings and release evidence

Before tagging, an owner checks the live GitHub settings and records **checker,
UTC date, tag, commit SHA, result, and evidence link or screenshot** for each
item in the release record. Mark inaccessible checks **unverified**, not
absent. Workflow files and issue templates describe intent; they do not prove
the corresponding settings or labels are active.

| Check | Live evidence to record |
| --- | --- |
| `release` environment | Settings → Environments shows required reviewers, deployment tag restriction `v*.*.*`, `APPLE_SIGNING_ENABLED=true`, and the six Apple secrets scoped to this environment (names and scope only; never record values). |
| `main` protection | Settings → Rules → Rulesets or Branches shows protection enabled and the exact required pull-request CI checks for `main`; compare their names with [Test](../../.github/workflows/test.yml) and [Levenshtein](../../.github/workflows/levenshtein.yml). Require `linux-race`, `macos-smoke`, `cross-build`, `lint`, and `verify` after confirming their exact displayed check names. Coordinate the switch with the CI workflow merge: retire `test (ubuntu-latest)`, `test (macos-14)`, `fuzz`, and `real-systemd`, then refresh other open PR branches so they emit the new checks. |
| Exact-commit validation | Push a run-owned `release-candidate/**` ref at the final main SHA. Record passing Test, Levenshtein verify and Extended macOS full/fuzz/real-systemd runs on that exact SHA. A PR merge-ref run or earlier nightly is insufficient. No signing runs on candidate branch pushes. |
| Private vulnerability reporting | Settings → Security → Code security and analysis shows private vulnerability reporting enabled; test the private reporting route described in [SECURITY.md](../../SECURITY.md) without submitting a real report. |
| Issue labels | Confirm `bug`, `capture-gap`, and `enhancement` exist in live repository labels and match the [issue templates](../../.github/ISSUE_TEMPLATE/). |
| Signed candidate tag | Confirm the release tag points to the tested `main` commit, the publish job completed signing and accepted notarization for both macOS architectures, and the release has `agent-archive-darwin-arm64`, `agent-archive-darwin-amd64`, `agent-archive-linux-arm64`, `agent-archive-linux-amd64`, `SHA256SUMS`, and `release-candidate.json`. Download all six; verify the macOS signatures and every checksum (`shasum -a 256 -c SHA256SUMS` on macOS, `sha256sum -c SHA256SUMS` on Linux). The Linux binaries have no signature to verify. |
| Provenance | Verify all four downloaded binaries, `SHA256SUMS`, and the manifest, constrained to the staging workflow, exact tag ref and full commit; record the verified subject digests and workflow run. Promotion runs this automatically using the [gh verification flags](https://cli.github.com/manual/gh_attestation_verify). |
| `release-promotion` environment | Owner verifies required reviewers, prevent self-review, main-only deployment restriction, no signing secrets and `PROMOTION_ENABLED=true` only after protection is verified. Keep the variable false/unset until then. A workflow environment name alone creates no approval protection. |
| Release freeze | Confirm no other owner/process will edit tags, release state, assets or latest during acceptance/promotion. Prefer immutable releases where supported and verified; do not assume the setting is enabled. REST release updates have no compare-and-swap, so the workflow's repeated checks and shared concurrency cannot serialize external owner edits. |

On 2026-09-28, the GitHub connector showed the repository public and a
published `v0.1.1` release with both architecture assets and `SHA256SUMS`.
Its branch-protection request returned HTTP 403 and the rulesets collection
was empty. Those responses leave live `main` protection **unverified**; they
do not establish that protection is absent. The other settings and the
signing, notarization, and attestation checks above still need their own live
evidence.

## Candidate acceptance and same-asset promotion

1. Merge the release-bound changes and record their final main SHA. Create a
   fresh run-owned `release-candidate/**` branch pointing at that exact main
   commit, with no additional source commit, and push it to run the
   [candidate campaign](../contributing/testing.md). Tag that **same SHA** only
   after Test, Levenshtein verify and Extended pass. A premerge integration
   campaign cannot substitute for this postmerge commit's evidence. Update
   CHANGELOG before the final campaign/tag. Historical CI and local unsigned
   builds are useful context but do not satisfy this gate.
2. After owner authorization to release, push the `vX.Y.Z` tag at that SHA.
   Approve only the protected `release` signing job. Confirm the prerelease is
   complete and public latest still points to the previous stable release.
3. Download the six assets from that tag, verify digests and provenance, then
   exercise the pinned installer using the **candidate tag**, never the
   latest URL, in disposable accounts. Test both Darwin architectures and both
   Linux architectures natively. Check the online notarization ticket after
   strict Developer ID signature validation, and perform quarantined clean-user
   installation, clean setup and upgrade, all three supported apps (Claude Code, Codex, and Cursor),
   real S3 and R2, listing cost/correctness, recovery, and cleanup. Follow the
   [published acceptance sheet](open-source-acceptance.md) and
   [Linux acceptance recipe](../contributing/testing.md#the-linux-live-acceptance-run).
4. Copy [the pending JSON template](release-evidence/example.json) to
   `dev/maintainers/release-evidence/vX.Y.Z.json`. Commit it to main in a reviewed
   documentation-only PR. Fill the tag, exact tagged commit, all six downloaded
   SHA256 digests, and every check with `result: passed`, actual tester,
   ISO-8601 UTC timestamp and a durable HTTPS evidence URL. Each evidence record
   must identify the candidate and describe the actual procedure/result;
   generic links or an earlier release's evidence do not establish acceptance.
   `repository-settings` includes both approval environments, main protection,
   vulnerability reporting, labels and release freeze. `documentation` includes
   command/link/generated reference audits and actual current provider/app
   versions. Pending, skipped, unavailable or failed checks stay pending and
   **block promotion**; the example intentionally fails this gate.
5. Under `ci`, record exact successful push run IDs for `Test`, `Levenshtein`,
   and `Extended`, with tester, UTC timestamp and evidence URL. Promotion queries
   GitHub and requires the exact repository/SHA/workflow plus successful jobs:
   Test `linux-race`, `macos-smoke`, `cross-build`, `lint`; Levenshtein `verify`;
   Extended `macos-full`, `fuzz`, `real-systemd`. Only main or candidate branch
   push runs count, since a PR's checkout can be a synthetic merge commit.
6. Dispatch [Promote release](../../.github/workflows/promote-release.yml)
   **from main**, supplying tag, full tagged SHA and the committed JSON path.
   The acceptance documentation commit may be newer than the tagged code;
   the tag still must resolve to that exact main ancestor. Inputs enter scripts
   only via environment variables. The file must be tracked, repository-relative,
   inside the release-evidence directory, free of symlink escape and at most
   256 KiB. Approve the separate `release-promotion` environment after reviewing
   that evidence and the exact digests. It has contents-write and read access to
   attestations/actions, with no signing secrets or OIDC signing permission.

Promotion downloads all six assets again. It verifies the remote tag,
checksums, manifest/API digests, all five provenance subjects constrained to
`.github/workflows/release.yml`, source SHA and tag ref, strict pinned-team
Darwin signatures and a separate online `codesign --verify --strict
-R=notarized --check-notarization` ticket check. Apple recommends this for
[other code](https://developer.apple.com/forums/thread/130366), including raw
executables: app-oriented `spctl` assessment can reject a valid CLI tool.
The installed `codesign(1)` manual describes `--check-notarization` as forcing
an online ticket check. A ticket check does not replace quarantined clean-user
installation acceptance. Commands have a five-minute limit and REST calls a
two-minute limit; CI job enumeration allows at most ten pages of 100 jobs
per run and fails closed if the final page is unavailable within that bound.
Unavailable services/timeouts block the gate. It checks the acceptance and
exact successful CI jobs, then rechecks tag/release/assets/latest before a
single REST PATCH setting `prerelease=false` and `make_latest=true` on the
**same release ID**. There is no rebuild, upload, deletion or asset replacement.
It confirms the resulting state and asset inventory afterward. A prerelease
older than public latest is refused; retrying an already promoted tag that
has since been superseded is a verified no-op. Both workflows share a
non-cancelling publication concurrency group.

The human acceptance gate is deliberate: the script checks structure, exact
identity/digests and CI results, but cannot judge whether a provider/app test
was honestly performed. Protected reviewers must inspect linked evidence.
No live signed-candidate, architecture, provider, application or approval
setting acceptance is established by this implementation or its fake API tests.

## Retry and partial-upload recovery

An unknown create/PATCH outcome triggers one read reconciliation, never an
immediate blind mutation retry. A full matching candidate is reusable on a
workflow rerun; the preflight verifies it before any build or signing starts.
Codesign timestamps and notarization are nondeterministic, so freshly signed
rerun output must never replace existing assets. Existing candidate bytes
must match their original manifest, attestation and API digests.

If an upload is partial, the tag moved, the release is draft, or any approved
bytes differ, the run fails closed. Preserve the original run ID, signed
`agent-archive-vX.Y.Z` workflow artifact, its checksum/manifest, notary receipts,
attestations, release ID and asset IDs/digests. The artifact is retained for
30 days; archive it securely before expiry if recovery needs longer. An owner
must explicitly authorize and review recovery of **missing assets only from
that original signed artifact**, reconciling remote bytes before and after.
There is intentionally no automatic repair command, deletion, clobber or
re-sign path. If the original signed evidence is unavailable, stop and choose
a new tag under a separately reviewed release decision. Never move a tested
release tag to make a retry pass.

Apple service outages, unavailable notarization assessment, provenance or API
digest support, inaccessible CI evidence, and unavailable approval/settings
inspection are failures or pending evidence, not passes. An unconfirmed PATCH
requires owner reconciliation of actual release/latest state before retry.

## Before the first release

These are repository settings only the owner can make:

- Create the `release` environment (Settings → Environments) **before the
  first tag**: a job that names an environment that doesn't exist gets one
  created for it, with no protection. Add required reviewers, limit its
  deployment branches and tags to the tag pattern `v*.*.*`, and move the
  Apple secrets below into it, deleting the repository-level copies.
  Without reviewers, anyone who can push a tag to a commit on `main` can
  stage signed assets. Also create `release-promotion` with the separate
  reviewers and restrictions above. Set `PROMOTION_ENABLED=true` there only
  after verifying its live protection. No settings are configured by these
  workflow files.
- `team_id` in `install.sh` is the Apple Developer Team ID that signs
  releases, `568CGRV32C`. The `APPLE_TEAM_ID` secret must be the same value
  (shown under Membership details at developer.apple.com):
  `scripts/check-release-signing.sh` refuses to publish while they differ.
- Protect `main` so the tag's commit has passed CI.

## Signing and notarization

Release binaries are codesigned with a Developer ID Application certificate
and submitted to Apple's notary service, so Gatekeeper can verify them
without a manual approval step. This needs an Apple Developer Program
membership and these secrets in the `release` environment:

`APPLE_CERTIFICATE_P12_BASE64`, `APPLE_CERTIFICATE_PASSWORD`,
`APPLE_SIGNING_IDENTITY`, `APPLE_ID`, `APPLE_TEAM_ID`, and
`APPLE_APP_SPECIFIC_PASSWORD`.

Also set the variable `APPLE_SIGNING_ENABLED` to exactly the lowercase
string `true`; any other value (`True`, `TRUE`, `1`, `yes`) counts as
disabled. A tagged release fails before signing if the flag or any secret is
missing, or if `install.sh` names another team. Signing, signature
verification, and accepted notarization are required before either
architecture is published. Never put these values in repository files.

`scripts/check-release-signing.sh` and its test
(`scripts/test_release_signing.py`) check that the workflow fails closed.
Local builds from `scripts/build-release.sh` are unsigned and for
development only; they report `dev-<commit>` from `--version`. The Linux
binaries are never signed, in a release or locally; the secrets above do not
apply to them.

## What a user can verify

- `install.sh` checks the download against the release's `SHA256SUMS`
  (integrity only: both files come from the same release) and then requires
  a strict code signature from a Developer ID certificate issued to the
  pinned team (`codesign --verify --strict -R=…`).
- Anyone can check provenance by hand, for any of the four binaries:
  `gh attestation verify agent-archive-darwin-arm64 --repo wangjohn/agent-archive`
  (or `agent-archive-linux-amd64`, and so on).
- A Linux binary has no code signature, so its trust is the checksum plus the
  attestation. Check the download against the release's checksums with
  `sha256sum -c --ignore-missing SHA256SUMS` (Linux) or
  `shasum -a 256 -c --ignore-missing SHA256SUMS` (macOS, whose `shasum`
  accepts the same flag), then run the `gh attestation verify` command above.
  Do this before running a downloaded Linux binary. A downloaded release
  asset is not executable, so `chmod +x` it after the checks pass.

## Versions

A release that changes what is filtered or how metadata is derived must
already carry the matching version bumps; see [versions](versions.md).

## Release-time documentation audit

For every tag, check the [README](../../README.md) commands and synthetic
output against that tag's CLI, confirm pinned install examples name a
published tag and its installer script, and review the dated evidence in
[tested app versions](../../docs/reference/capture-capabilities.md) before
changing any version claim. Recheck the Cloudflare and AWS dashboard steps in
the [bucket guide](../../docs/getting-started/bucket.md), follow its current
provider pricing link, and open external links in the release-facing docs.
Run the repository's relative-link and generated CLI-reference checks too;
passing those checks does not establish that external URLs work. Record the
documentation commit and any pending live checks beside the release evidence.
