# Release recovery after PR 1

## Outcome

PR 1 merged as 107199c1f5c9f5f0d37c1395f1a939c36d23f44e. The first merged-main run passed both macOS suites, web tests, lint, and universal installer/Homebrew verification. Publication was correctly withheld because commit policy failed.

## Cause

GitHub authored the squash commit as Marc Went <Dunky13@users.noreply.github.com>. Its retained sign-offs use Marc Went <marc@went.io>. The commit signature is Verified, but neither existing trailer matches the squash author email exactly. Compulsory web sign-offs were enabled and did not prevent this combination.

## Repair

This follow-up adds a maintainer squash checklist and instructions for setting the final author email and matching DCO trailer together. Its signed commit includes sign-offs for both observed public identities of the same maintainer, so either of those squash author selections retains a matching trailer. No DCO, signature, validation, publication, or branch-protection check changes.

## Evidence

The failure was checked in the actual main commit metadata and commit-policy job log. The exact author-matching checker still rejects the original squash commit. Contributor instructions use installed GitHub CLI flags verified through gh pr merge --help. This is a documentation-only change; source-boundary and diff checks are sufficient locally, and hosted required checks must pass before merge.

## Remaining work

After this PR passes checks, obtain owner approval and merge through the protected path with an exact author/trailer match. Verify the new main commit signature and DCO, wait for successful main publication, download and verify ZIP/DMG checksums and universal payload, then trigger the tap refresh and verify its cask and signed metadata commit. No public installer release or installable cask exists yet. The previous main commit is preserved without rewriting history.

[Failed main run](https://github.com/Dunky13/agent-job-queue/actions/runs/39223660205) · [Merged PR 1](https://github.com/Dunky13/agent-job-queue/pull/1)
