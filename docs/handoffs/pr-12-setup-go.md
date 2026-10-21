# PR 12: setup-go v7 and concurrent cleanup

[Pull request](https://github.com/Dunky13/agent-job-queue/pull/12)

## Problem and repair

The original Dependabot commit, `800082c3413f18d4bad0848fc05b0234ee0621d8`, had a verified signature but failed the author-matching DCO check. Its author used `49699333+dependabot[bot]@users.noreply.github.com`, while its sign-off used `support@github.com`.

Recreate the dependency update as a maintainer-authored, cryptographically signed commit with the maintainer's matching DCO trailer. Replace only the PR branch using an explicit lease against the original head. Keep commit policy and branch protection intact.

The workflow update changes all eight setup-go references from v6.5.0 to v7.0.0, pinned to `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`. GitHub's upstream v7.0.0 tag resolves to that commit. Its action metadata uses Node 24 and retains the workflow's `go-version-file` and `cache` inputs.

## Cleanup race exposed by fresh CI

The signed replacement, `ae6267d`, passed commit policy. Its Intel macOS test job failed in `TestRealDaemonCrashRetainsOwnershipAndPausesRestart` with `Slot ownership changed during cleanup; shared slot retained`. The restarted daemon and the recovery caller can both clean the same job. One removes the receipt while the other waits for the process group to exit; the second caller then mistakes the completed cleanup for an ownership change.

Separate the final release phase so this interleaving can be tested deterministically. After verifying process-group quiescence and reacquiring the lease lock, return success without further mutation only when both marker and receipt are absent. A replacement receipt, changed marker, live group, or uncertain identity still retains its existing rejection behavior. Cleanup never removes another job's lease.

A concurrent-cleanup regression reproduced the exact error in five of ten runs before the fix. Ten repetitions of the repaired concurrent test, deterministic stale-cleanup cases, and existing ownership guards passed. Deterministic cases cover completion by another caller, a new pending receipt, a new lease for the same task, and a lease for another task. Full Go race validation and repeated real crash recovery are required for the expanded change.

## Local evidence

- The unchanged commit checker reproduces the original commit's exact DCO failure.
- `actionlint` passes for the updated workflow.
- `go test -p 2 -parallel 2 ./internal/tooling -run TestPrePush -v` verifies the existing fail-closed, full-range push checks.
- The installed pre-push hook and `check-commits --verify-local` must verify the replacement commit before push.

The original PR run passed lint, vulnerability scanning, both macOS architectures, web tests, and universal installer validation. Those results belong to the original head. Fresh hosted checks and independent review of the final head, including the cleanup repair, are required before merge; local checks do not establish that outcome.

## Completion procedure

Review the exact replacement head, require successful `ci-required`, and inspect unresolved review threads. Squash through the protected path with author `Marc Went <marc@went.io>` and exactly matching `Signed-off-by` trailer. Verify the resulting main commit's signature and DCO through GitHub. The CI workflow automatically publishes main installers after its checks; release publication and Homebrew delivery are separate results from this requested PR merge.

This repairs PR 12 only. Future Dependabot commits still need matching author and sign-off identities under the existing policy.

## Squash identity recovery

PR 12 merged after all required checks passed on `552417d67948a7e4a04540fdcb8afb3f5e9e17f1`. Standards and spec reviews had no findings; the Jev advisory returned 2.92/3, ready. Cross-provider review was skipped because Anthropic subscription access was unavailable.

The resulting commit, `3e0be5efcd6b00b053f716345f7c560123c7960f`, is GitHub-verified and has exactly the reviewed tree. However, GitHub retained author `dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>` despite the merge request's maintainer email. Its trailer remains `Signed-off-by: Marc Went <marc@went.io>`. The unchanged checker rejects that historical squash for missing an author-matching sign-off.

A maintainer-owned follow-up PR corrects the contributor instructions and PR checklist and records this recovery. Its new signed, author-matching commit becomes a valid main head through the normal protected merge path; it does not retroactively certify or rewrite the historical bot squash. The main workflow checks the new push range and retains publication gates. Verify the follow-up merge identity and the main commit-policy result before declaring recovery complete. Installer publication and Homebrew delivery remain separate outcomes.

PR 13 merged as `e7eaa7b96dec2366dc4a6fd660f7f3eed942cc96` with the reviewed tree and a verified signature, but its web squash selected `Marc Went <Dunky13@users.noreply.github.com>` while retaining only the primary-email trailer. The checker rejects that second historical mismatch too. The next corrective commit carries both established sign-offs for the same maintainer, following the remedy already recorded in `docs/handoffs/pr-1-signoff-recovery.md`. The contributor instructions and checklist now require preserving both in the final squash body. Neither historical commit is rewritten or retroactively certified; verify the new main head and its hosted commit-policy result.
