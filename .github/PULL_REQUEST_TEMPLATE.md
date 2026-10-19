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
- Keep Verified signatures and branch protection enabled.

See [the squash author instructions](https://github.com/Dunky13/agent-job-queue/blob/main/CONTRIBUTING.md#squash-author-and-sign-off).
