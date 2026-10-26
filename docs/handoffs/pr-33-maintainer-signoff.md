# PR 33: maintainer sign-off identity

[Pull request](https://github.com/Dunky13/agent-job-queue/pull/33)

## Decision and scope

Future maintainer commits use `Marc Went <marc@went.io>` as author and committer, with exactly one `Signed-off-by: Marc Went <marc@went.io>` trailer. The contribution guide and PR checklist replace the previous dual-address workaround. Consolidate duplicate maintainer trailers and the maintainer's noreply alias when preparing amended or squash messages. Preserve other contributors' certifications and historical commits.

No runtime code, commit checker, signature requirement, or branch protection changes are included. The repository's local Git email is also set to the primary address; that configuration is not committed.

## Evidence

The public-source boundary check and `git diff --check` passed. The first source commit has the primary author and committer email, one matching trailer, and a locally verified cryptographic signature. The installed pre-push hook passed its complete PR-range DCO and signature check. The installed GitHub CLI supports `--author-email`, `--match-head-commit`, and `--body-file` for merge preparation.

## Completion procedure

Require successful hosted CI on the final reviewed head and resolve review threads. Squash with author email `marc@went.io` and an explicit message containing one matching maintainer trailer. After merging, verify the resulting author, trailer count, GitHub signature, and tree against the reviewed head. Installer publication and Homebrew delivery are separate from this documentation change.
