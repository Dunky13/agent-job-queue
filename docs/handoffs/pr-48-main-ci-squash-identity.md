# PR 48: main CI squash identity recovery

[Pull request](https://github.com/Dunky13/agent-job-queue/pull/48)

## Failure and scope

[Main job 122854660738](https://github.com/Dunky13/agent-job-queue/actions/runs/40699362686/job/122854660738) rejected commit `0be8659760225ecd445bb0456b27d133599d2814` with `missing author-matching DCO sign-off`. GitHub reports its cryptographic signature as verified. The squash author is `Marc Went <Dunky13@users.noreply.github.com>`; all three retained certifications use `Marc Went <marc@went.io>`. The unchanged local checker reproduces this failure.

This PR adds merge tooling, tests, and contributor guidance. Application behavior, the exact DCO checker, signature requirements, existing history, and distribution pipelines are unchanged. Authorization ends at a signed, pushed recovery PR. Owner approval remains required before merging; publication and runtime acceptance are separate evidence.

## Repair

After owner approval, use `sh scripts/merge-pr.sh PR_NUMBER REVIEWED_HEAD SQUASH_BODY_FILE` from the repository root. Supply the full reviewed head and a prepared squash body with exactly one `Signed-off-by: Marc Went <marc@went.io>` line. Preserve other contributors' certifications.

The helper copies the body into private temporary storage, rejects duplicate or alternate maintainer certifications, checks the maintainer login and name, requires the `main` base and reviewed head, and invokes `gh pr merge --squash` with the primary author email, validated body, and matching head. Lookup and merge failures propagate; temporary storage is removed on success and failure. It does not request administrator bypass or automatic merge.

GitHub's active `main` ruleset requires signed commits and PR merges; `main-ci` requires `ci-required`. These protections remain enabled. The existing tooling shards discover and run the new regression test automatically.

## Local evidence

- The unchanged `check-commits --base HEAD^ --head HEAD` rejected the original main commit with the same error as GitHub.
- The new test failed before the helper existed. Fourteen guarded-merge scenarios now pass under race detection, alongside existing pre-push and source-boundary tests.
- Cases cover the primary identity, preservation of another contributor's certification, missing or duplicate certification, noreply and case-variant aliases, bot ownership, changed author name, base or head, malformed head, metadata lookup failure, merge refusal, exact CLI arguments and body preservation, and temporary-storage cleanup.
- ShellCheck, POSIX shell and zsh syntax, public-source portability, and diff whitespace checks pass. The installed CLI supports `--squash`, `--author-email`, `--match-head-commit`, and `--body-file`; the metadata query was checked against the actual originating PR.
- Source commits use the primary maintainer author and committer identity, a single DCO trailer, and locally verified cryptographic signatures. The installed pre-push hook checks the complete PR range.

Focused validation command:

```sh
go test -p 2 -parallel 2 -race ./internal/tooling -run '^(TestMaintainerSquashMerge|TestPrePushPreservesFullRangeAndFailsClosed|TestSourceBoundaryFindsPrivatePathsAndSymlinks)$' -count=1
shellcheck scripts/merge-pr.sh
sh -n scripts/merge-pr.sh
zsh -n scripts/merge-pr.sh
go run -p 2 ./cmd/queue-tool check-source
git diff --check
```

## Remaining completion gates

Require hosted checks on the final PR head, resolve review findings, and obtain owner merge approval. Use the guarded command with one primary maintainer certification. After merging, verify the resulting author, single maintainer trailer, GitHub signature, and reviewed source tree, then require successful main CI. The historical failed run stays failed; recovery is established by the new correctly authored main commit and its successful checks. No history rewrite or policy exception is required.

The full native, installer, browser, and cross-architecture suites are delegated to hosted CI for this nonvisual tooling change. No native installation, Apple submission, artifact publication, or runtime acceptance was performed. Test fixtures clean up their private directories; the local HTML counterpart is an ignored review deliverable, retained until the review is finished.
