# PR 12: setup-go v7

[Pull request](https://github.com/Dunky13/agent-job-queue/pull/12)

## Problem and repair

The original Dependabot commit, `800082c3413f18d4bad0848fc05b0234ee0621d8`, had a verified signature but failed the author-matching DCO check. Its author used `49699333+dependabot[bot]@users.noreply.github.com`, while its sign-off used `support@github.com`.

Recreate the dependency update as a maintainer-authored, cryptographically signed commit with the maintainer's matching DCO trailer. Replace only the PR branch using an explicit lease against the original head. Keep commit policy and branch protection intact.

The workflow update changes all eight setup-go references from v6.5.0 to v7.0.0, pinned to `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`. GitHub's upstream v7.0.0 tag resolves to that commit. Its action metadata uses Node 24 and retains the workflow's `go-version-file` and `cache` inputs. No application or queue behavior changes.

## Local evidence

- The unchanged commit checker reproduces the original commit's exact DCO failure.
- `actionlint` passes for the updated workflow.
- `go test -p 2 -parallel 2 ./internal/tooling -run TestPrePush -v` verifies the existing fail-closed, full-range push checks.
- The installed pre-push hook and `check-commits --verify-local` must verify the replacement commit before push.

The original PR run passed lint, vulnerability scanning, both macOS architectures, web tests, and universal installer validation. Those results belong to the original head. Fresh hosted checks and independent review of the replacement head are required before merge; local checks do not establish that outcome.

## Completion procedure

Review the exact replacement head, require successful `ci-required`, and inspect unresolved review threads. Squash through the protected path with author `Marc Went <marc@went.io>` and exactly matching `Signed-off-by` trailer. Verify the resulting main commit's signature and DCO through GitHub. The CI workflow automatically publishes main installers after its checks; release publication and Homebrew delivery are separate results from this requested PR merge.

This repairs PR 12 only. Future Dependabot commits still need matching author and sign-off identities under the existing policy.
