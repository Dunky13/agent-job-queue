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

Push CI checks the range from the previous `main` commit (`github.event.before`), so the next commit on top of `55060e0` that passes the commit policy turns `main` green without rewriting history. This PR is that commit. It is merged with `scripts/merge-pr.sh`, which sets the author to `marc@went.io` and verifies the result. A green main run then starts Distribution for the current `main`, which includes every change since build 263.

The failed runs stay failed. Do not dispatch Distribution with a failed run's ID; that skips the commit policy.

## Recurrence

The owner chose to map the alias in the repository. `.mailmap` maps `Dunky13@users.noreply.github.com` to `Marc Went <marc@went.io>`, and `CheckCommits` reads the author with `%aN`/`%aE`, which apply it. A web squash merge whose body keeps the single `Signed-off-by: Marc Went <marc@went.io>` trailer now passes. The trailer is still required, the rule is still an exact match after mapping, and GitHub still verifies every signature. The alternatives were changing the account's email settings, which cannot be checked from the repository, and merging only through `scripts/merge-pr.sh`, which six merges in a row did not use.

`TestCheckCommitsMapsTheMaintainerNoreplyAlias` fails without the mapping and passes with it. With the mapping, `check-commits --base 58ec253 --head 55060e0` passes all six failed commits.

The mapping comes from the checked-out tree, so a PR could add another alias. Review any `.mailmap` change as a change to the commit policy.

## Next steps

1. Confirm main CI passes on the recovery commit and Distribution publishes a new build.
2. If Distribution fails on a transient Apple network error, let `Distribution recovery` continue it, then check its result.
3. Merge the next PR from the web interface and confirm its main run passes the commit policy.
