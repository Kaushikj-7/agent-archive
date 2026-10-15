# Repository instructions

Follow [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow.

## Merge policy

This repository runs many PRs in parallel. Branches do not need to be up to
date with `main` before merging. Do not require rebasing, merging the latest
`main`, or rerunning CI solely because `main` advanced.

Before merging, required CI checks must pass for the PR's current head.
Obtain appropriate review and resolve actual merge conflicts or identified
integration defects.

Do not enable GitHub's "Require branches to be up to date before merging",
change bypass settings, or change any repository protection settings.
