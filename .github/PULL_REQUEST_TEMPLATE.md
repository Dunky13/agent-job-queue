## Summary

Describe the problem and resulting behavior.

## Evidence

List relevant validation and any remaining verification work.

## Merge Danger

**Door:** Two-way / One-way

**Blast Radius:**

Before confirming a squash merge:

- Required checks pass on the reviewed head.
- The final squash author name and email have an exactly matching `Signed-off-by` trailer. Check the selected email, including any GitHub noreply default.
- For a maintainer-owned PR, select squash author `Marc Went <marc@went.io>` and retain exactly one maintainer trailer: `Signed-off-by: Marc Went <marc@went.io>`. Consolidate duplicate maintainer trailers and the maintainer's noreply alias; preserve other contributors' certifications. Follow the commit policy below for GitHub CLI author selection and post-merge verification.
- A maintainer takeover of a bot update uses a maintainer-owned PR so the final author and certification remain aligned.
- Keep Verified signatures and branch protection enabled.

See [the commit policy and squash author instructions](https://github.com/Dunky13/agent-job-queue/blob/main/CONTRIBUTING.md#commit-policy).
