# Main CI: squash author recurrence

## Failure

Main CI has failed on every merge from PR #77 to PR #83, so Distribution was skipped each time and no build followed build 263 (`b34ee05`). PR #76 (`58ec253`) passed CI, but its distribution failed on an Intel CloudKit network error during `stapler validate`.

| Commit | PR | Failing job |
|---|---|---|
| `bc582b1` | #77 | Commit policy |
| `678ec61` | #78 | Commit policy |
| `0f36470` | #79 | Commit policy |
| `7e748be` | #80 | Commit policy, Go vulnerability scan |
| `3d32712` | #81 | Commit policy |
| `55060e0` | #83 | Commit policy; the vulnerability scan was cancelled before it got a runner |

Each commit was a squash merge from the GitHub web interface. GitHub authored them as `Marc Went <Dunky13@users.noreply.github.com>`, while their certification is `Signed-off-by: Marc Went <marc@went.io>`. The commit policy requires a trailer that matches the author exactly, so each failed with `missing author-matching DCO sign-off`. PR checks cannot see this, because they validate the source commits, not the squash commit GitHub creates at merge time. This is the mismatch described for PRs 47 and 48 in `CONTRIBUTING.md`.

The scan failure on `7e748be` was GO-2026-6859 in `net/http` of Go 1.26.10, fixed when `go.mod` moved to Go 1.26.11 in PR #83. `govulncheck` reports no vulnerabilities on `55060e0`.

## Recovery

Push CI checks the range from the previous `main` commit (`github.event.before`), so a correctly authored commit on top of `55060e0` passes the commit policy without rewriting history. This handoff is that commit. It is merged with `scripts/merge-pr.sh`, which sets the author to `marc@went.io` and verifies the result. A green main run then starts Distribution for the current `main`, which includes every change since build 263.

The failed runs stay failed. Do not dispatch Distribution with a failed run's ID; that skips the commit policy.

## Recurrence

Any web-interface squash merge by the maintainer fails main CI again. Closing that gap needs an owner decision between:

- mapping the noreply alias to the primary identity in the commit policy,
- changing the account's email settings so web merges use `marc@went.io`, or
- keeping the rule and merging only through `scripts/merge-pr.sh`.

## Next steps

1. Confirm main CI passes on the recovery commit and Distribution publishes a new build.
2. If Distribution fails on a transient Apple network error, let `Distribution recovery` continue it, then check its result.
3. Apply the owner's recurrence decision.
