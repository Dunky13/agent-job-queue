# Release recovery after PR 1

## Outcome

PR 1 merged as 107199c1f5c9f5f0d37c1395f1a939c36d23f44e. The first merged-main run passed both macOS suites, web tests, lint, and universal installer/Homebrew verification. Publication was correctly withheld because commit policy failed.

## Cause

GitHub authored the squash commit as Marc Went <Dunky13@users.noreply.github.com>. Its retained sign-offs use Marc Went <marc@went.io>. The commit signature is Verified, but neither existing trailer matches the squash author email exactly. Compulsory web sign-offs were enabled and did not prevent this combination.

## Repair

This follow-up adds a maintainer squash checklist and instructions for setting the final author email and matching DCO trailer together. Its signed commits include sign-offs for both observed public identities of the same maintainer, so either of those squash author selections retains a matching trailer. The pre-push hook also now checks the documented full PR range from origin/main, including previously pushed PR commits. Its previous new-branch path checked all history and incorrectly included the already-remote merge commit. Fresh repositories still check their initial history, and unexpected Git lookup, signature, or DCO failures still block pushes. The exact commit checker and main protection remain enforced.

## Evidence

Both failures were observed directly: the main commit-policy job rejected the squash identity, and an actual new-branch push incorrectly included that existing main commit in its range. The exact author-matching checker still rejects the original squash commit and accepts the new PR range. Seven isolated hook regression tests passed, covering new and existing PR branches, bootstrap fallback, lookup and commit-resolution failures, checker failure, and deletion/nonbranch handling. Ruff, ShellCheck, workflow lint, source-boundary and diff checks passed. Contributor instructions use installed GitHub CLI flags verified through gh pr merge --help. Hosted required checks must pass before merge.

## Remaining work

After this PR passes checks, obtain owner approval and merge through the protected path with an exact author/trailer match. Verify the new main commit signature and DCO, wait for successful main publication, download and verify ZIP/DMG checksums and universal payload, then trigger the tap refresh and verify its cask and signed metadata commit. No public installer release or installable cask exists yet. The previous main commit is preserved without rewriting history.

[Failed main run](https://github.com/Dunky13/agent-job-queue/actions/runs/39223660205) · [Merged PR 1](https://github.com/Dunky13/agent-job-queue/pull/1)
