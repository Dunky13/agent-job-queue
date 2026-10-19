# PR 1: Darwin crash recovery

## Delivery contract

Publish Agent Job Queue as a public MIT repository, include the Mac app and queue tools, build a downloadable universal installer on each successful main push, and require passing checks before merges to main.

Repository: [Dunky13/agent-job-queue](https://github.com/Dunky13/agent-job-queue). Repair: [PR 1](https://github.com/Dunky13/agent-job-queue/pull/1).

## Current state

The initial signed import is on main. GitHub reports its signature verified. Main protection requires a pull request, up-to-date ci-required from GitHub Actions, verified commit signatures, and resolved conversations. Force pushes, deletion, and administrator bypasses are disabled.

The complete bootstrap CI passed. The first main run then exposed a real Darwin process-lifecycle race in the crash acceptance test. Its aggregate gate failed and correctly withheld publication. [Failing main run](https://github.com/Dunky13/agent-job-queue/actions/runs/39216528279).

PR 1 repairs that race. Its first complete hosted PR run passed crash recovery on both macOS architectures but exposed a separate scheduling assumption in the native refresh test. That fixture is now synchronized explicitly. Hosted validation of the updated head and owner merge approval remain before the first main installer release. This document records the repair's delivery state; query GitHub for the current result.

## Diagnosis and repair

Darwin can return EPERM for a group containing only exited, unreaped processes. A test-owned child held unreaped reproduced this behavior, and regression tests failed against the original implementation. Apple XNU filters zombie group members before returning its permission result.

The repair accepts kernel ESRCH as proof of departure. Otherwise, EPERM requires two identical, positive zombie-only process listings, including process identities, with a second kernel probe. A final zero-signal probe may confirm reaping between observations. Live, empty-but-inaccessible, changing, malformed, incomplete, or failed observations keep the lease.

Boot and process-birth checks still precede actual signals. The same departure race between ownership verification and signal delivery is handled without retrying nonzero signals against live inaccessible groups. No timeout was increased and no existing assertion was removed.

CI push triggers cover main and the bootstrap branch; pull request triggers still cover every PR targeting main. This avoids duplicate feature push and PR builds. Each main run has a unique concurrency group, and only successful validation can publish its installer.

The native refresh test previously used 10 ms and 50 ms sleeps to assume a poll was still in flight. On the Apple Silicon hosted runner, the poll could complete before the simulated mutation, invalidating that assumption. Checked continuations now hold the captured read until the test explicitly releases it, so both silent polling and stale-read rejection have deterministic ordering. A follow-up assertion verifies that refresh resumes after mutation. Production refresh behavior and CI timeouts are unchanged.

## Validation evidence

- The full local repair run passed core tests, 22 real-daemon acceptance/environment tests, 18 real HTTPS/API tests, five schema tests, six browser tests, native regression checks, and 11 installer tests.
- After the final reaping-boundary change, all 59 core tests and the original crash acceptance test passed again. The installer payload was compared with the final backend source and matched.
- After replacing fixture sleeps, all native regression checks passed. The refresh tests passed 20 consecutive executions with warnings treated as errors. A negative control removing the production mutation guard failed the stale-read assertion as expected.
- Installer checks exercise generated configuration and installed wrappers against a real temporary Pueue daemon. Payload checks verify both executable architectures, ad hoc signatures, and exclusion of credentials and state.
- Scoped independent source reviews reported no remaining actionable blocker before the final positive-kernel-confirmation refinement. Root review and regression tests covered that refinement.
- Physical Android certificate installation, an off-site VPN connection, and Apple notarization are outside this repair's validation. The installer is ad hoc signed.

All process tests use owned temporary groups and private state. The current user's active queue, credentials, and installed application were not changed by this repair.

## Completion steps

1. Verify PR 1's latest head has passing required checks, no conflicts, and no unresolved review conversations. Keep branch protection enabled.
2. Obtain the repository owner's merge approval. Preserve a DCO trailer matching the squash author and GitHub's verified-signature status.
3. Merge through the protected PR path. Verify the main workflow succeeds and publishes its prerelease ZIP, DMG, and basename-only SHA-256 files.
4. Download the published artifact, verify checksums and its universal payload, and report the repository and release links.

Useful current-state commands:

```sh
gh pr view 1 --repo Dunky13/agent-job-queue
gh pr checks 1 --repo Dunky13/agent-job-queue
gh run list --repo Dunky13/agent-job-queue --workflow CI --branch main
gh release list --repo Dunky13/agent-job-queue
```
