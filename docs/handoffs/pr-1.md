# PR 1: public distribution and reliability

## Delivery contract

Publish Agent Job Queue as a public MIT repository, include the Mac app and queue tools, build a downloadable universal installer on each successful main push, and require passing checks before merges to main. The expanded scope also includes public README screenshots and Homebrew installation.

Repository: [Dunky13/agent-job-queue](https://github.com/Dunky13/agent-job-queue). Repair: [PR 1](https://github.com/Dunky13/agent-job-queue/pull/1).

## Current state

The initial signed import is on main. GitHub reports its signature verified. Main protection requires a pull request, up-to-date ci-required from GitHub Actions, verified commit signatures, and resolved conversations. Force pushes, deletion, and administrator bypasses are disabled.

The complete bootstrap CI passed. The first main run then exposed a real Darwin process-lifecycle race in the crash acceptance test. Its aggregate gate failed and correctly withheld publication. [Failing main run](https://github.com/Dunky13/agent-job-queue/actions/runs/39216528279).

PR 1 repairs that race and synchronizes the native refresh fixture explicitly. All hosted validation passed on b70ee5d, including both macOS architectures, web tests, and the universal installer. The later README screenshots and Homebrew additions need validation on their updated head. Owner merge approval remains before the first main installer release. This document records the repair's delivery state; query GitHub for the current result.

## Diagnosis and repair

Darwin can return EPERM for a group containing only exited, unreaped processes. A test-owned child held unreaped reproduced this behavior, and regression tests failed against the original implementation. Apple XNU filters zombie group members before returning its permission result.

The repair accepts kernel ESRCH as proof of departure. Otherwise, EPERM requires two identical, positive zombie-only process listings, including process identities, with a second kernel probe. A final zero-signal probe may confirm reaping between observations. Live, empty-but-inaccessible, changing, malformed, incomplete, or failed observations keep the lease.

Boot and process-birth checks still precede actual signals. The same departure race between ownership verification and signal delivery is handled without retrying nonzero signals against live inaccessible groups. No timeout was increased and no existing assertion was removed.

CI push triggers cover main and the bootstrap branch; pull request triggers still cover every PR targeting main. This avoids duplicate feature push and PR builds. Each main run has a unique concurrency group, and only successful validation can publish its installer.

The native refresh test previously used 10 ms and 50 ms sleeps to assume a poll was still in flight. On the Apple Silicon hosted runner, the poll could complete before the simulated mutation, invalidating that assumption. Checked continuations now hold the captured read until the test explicitly releases it, so both silent polling and stale-read rejection have deterministic ordering. A follow-up assertion verifies that refresh resumes after mutation. Production refresh behavior and CI timeouts are unchanged.

## Screenshots and Homebrew

README screenshots show the actual native queue, its start confirmation, and the actual phone dashboard with artificial jobs. Native capture uses fixture mode and an inert action recorder, including confirmation navigation verification. The dashboard preview injects the browser-test fake runner and cannot call the installed queue. Capture scripts and instructions are included; temporary preview services were stopped after inspection.

The public Homebrew tap is Dunky13/homebrew-agent-job-queue, with a signed initial commit on its default tap distribution branch. It requires verified signatures and blocks force pushes and deletion. It consumes checksum-pinned installers from successful main releases; app source remains on the protected main branch. Refresh regenerates Ruby from a strict manifest, checks the downloaded ZIP and the real Homebrew DSL, and publishes GitHub-signed commits with matching DCO sign-off. It runs every 15 minutes or by owner dispatch, without a new secret. The tap cannot offer an install until the first main release exists.

Review identified a stale-checkout race in tap refresh. The updater now requires the live branch to match the checked-out commit before comparing current build metadata, then uses that same commit for GitHub expected-head publication. A deterministic regression rejects a branch advanced since checkout. Source publication compares existing release targets and every expected asset; it refuses missing or different immutable assets instead of overwriting them.

## Validation evidence

- The full local repair run passed core tests, 22 real-daemon acceptance/environment tests, 18 real HTTPS/API tests, five schema tests, six browser tests, native regression checks, and 11 installer tests.
- After the final reaping-boundary change, all 59 core tests and the original crash acceptance test passed again. The installer payload was compared with the final backend source and matched.
- After replacing fixture sleeps, all native regression checks passed. The refresh tests passed 20 consecutive executions with warnings treated as errors. A negative control removing the production mutation guard failed the stale-read assertion as expected.
- Installer checks exercise generated configuration and installed wrappers against a real temporary Pueue daemon. Payload checks verify both executable architectures, ad hoc signatures, and exclusion of credentials and state.
- Scoped independent source reviews reported no remaining actionable blocker before the final positive-kernel-confirmation refinement. Root review and regression tests covered that refinement.
- All seven Homebrew tests passed, including the real installed Homebrew DSL and automatic installer artifact against owned temporary HOME, Caskroom, and tool fixtures. The private app, wrappers, and queue login entry were created without enabling remote credentials. Independent review also ran these tests and reported no further actionable issue.
- The final queued packaging batch passed complete lint, rebuilt the universal ZIP and DMG, passed all 11 installer tests, verified signatures and both architectures, and loaded a cask rendered from that actual ZIP through the real Homebrew DSL. Graphify structural data was refreshed locally.
- All three README screenshots were visually inspected. The native capture verified confirmation navigation with inert actions. The phone viewport had no console errors or horizontal overflow; its preview was stopped. Unsafe preview bind addresses were rejected before server startup.
- Public source-release publication, the first tap metadata update, and installation from the published cask remain unexecuted pending owner merge approval and the first successful main release.
- Physical Android certificate installation, an off-site VPN connection, and Apple notarization are outside this repair's validation. The installer is ad hoc signed.

All process tests use owned temporary groups and private state. The current user's active queue, credentials, and installed application were not changed by this repair.

## Completion steps

1. Verify PR 1's latest head has passing required checks, no conflicts, and no unresolved review conversations. Keep branch protection enabled.
2. Obtain the repository owner's merge approval. Preserve a DCO trailer matching the squash author and GitHub's verified-signature status.
3. Merge through the protected PR path. Verify the main workflow succeeds and publishes its prerelease ZIP, DMG, and basename-only SHA-256 files.
4. Download the published artifact, verify checksums and its universal payload, populate the public Homebrew tap from the validated release, and verify the cask installation path before reporting the repository and release links.

Useful current-state commands:

```sh
gh pr view 1 --repo Dunky13/agent-job-queue
gh pr checks 1 --repo Dunky13/agent-job-queue
gh run list --repo Dunky13/agent-job-queue --workflow CI --branch main
gh release list --repo Dunky13/agent-job-queue
```
