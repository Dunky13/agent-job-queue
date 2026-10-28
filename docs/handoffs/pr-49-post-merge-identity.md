# PR 49: post-merge identity verification and main recovery

[Pull request](https://github.com/Dunky13/agent-job-queue/pull/49)

## Confirmed failure

[Main CI run 40728953939](https://github.com/Dunky13/agent-job-queue/actions/runs/40728953939) rejected PR 48's squash commit `01910c61dd95760fb357ce6e02af9bf3be57c584` for `missing author-matching DCO sign-off`. Its author is `Marc Went <Dunky13@users.noreply.github.com>`; all three certifications use `Marc Went <marc@went.io>`. GitHub reports the signature as verified. Every other validation job passed; only commit policy and its aggregate failed.

The unchanged local checker reproduces the rejection. Green PR checks only certified PR 48's source commits. The guarded command supplied by that PR could not intercept other merge paths, and its successful exit did not independently verify the actual squash result. The recovery must use the explicit primary author email and one matching certification when creating a new main commit. Historical commits remain unchanged.

## Change and evidence

The guarded merge helper now queries the completed PR's actual merge commit, verifies its primary maintainer author and GitHub signature, and requires exactly one primary maintainer certification. A queued or missing result, lookup failure, unexpected author, missing or duplicate certification, or unverified signature returns failure. Verification failure after a merge requires recovery; the helper never rewrites main.

Seven new regression scenarios failed against the previous helper. All 21 merge scenarios now pass with race detection, together with existing pre-push and source-boundary tests. The test also checks that the API lookup targets the actual merged commit and requests the documented fields. ShellCheck, POSIX shell and zsh syntax, public-source portability, and whitespace checks passed. The actual metadata query was exercised against PR 48's failed merge commit.

```sh
go test -p 2 -parallel 2 -race ./internal/tooling -run '^(TestMaintainerSquashMerge|TestPrePushPreservesFullRangeAndFailsClosed|TestSourceBoundaryFindsPrivatePathsAndSymlinks)$' -count=1
shellcheck scripts/merge-pr.sh
sh -n scripts/merge-pr.sh
zsh -n scripts/merge-pr.sh
go run -p 2 ./cmd/queue-tool check-source
git diff --check
```

## Completion procedure and limits

Require hosted checks on the final PR head and obtain owner merge approval. Prepare a squash body with exactly one `Signed-off-by: Marc Went <marc@went.io>` line, preserving other contributors' certifications if any. From the reviewed checkout, execute `sh scripts/merge-pr.sh 49 REVIEWED_HEAD SQUASH_BODY_FILE` with the full approved head and prepared file. Require the helper's verified actual-merge result and successful new main CI before calling the main recovery complete.

The helper cannot enforce the identity of a separate web merge. The contributor guide and PR template now explicitly direct maintainer squash merges through the guarded CLI. No automatic account-alias acceptance, DCO exception, branch-protection change, history rewrite, application change, or distribution-pipeline change is included. Merge approval does not establish artifact publication or runtime acceptance.

Source commits retain the configured cryptographic signatures, primary maintainer identity, and single matching DCO certification. The installed pre-push hook validates the complete PR range. Temporary test fixtures are automatically removed. The ignored HTML counterpart and prepared squash body remain local review resources until the approved merge and handoff are complete.
