# PR 51 and 52 review follow-up

## Scope and source identities

The initial scope was to review the drag-and-drop app and plugin lanes changes,
reproduce actionable findings, and fix them locally. The later authorization to
commit, push and merge on green is recorded below. Release, Apple submission,
deployment and operations on the installed queue remain outside this task.

- [PR 51](https://github.com/Dunky13/agent-job-queue/pull/51) is merged as
  `7d79088ec28d024922d1a2b7674d965bde324139`. Its reviewed base is
  `e26e7357eb851c4ea3d393c704763ebed04453cc`.
- [PR 52](https://github.com/Dunky13/agent-job-queue/pull/52) was still open when
  checked on 2026-10-29, at `839c7882bd8e738194aa523dc7c5ff88793efae3`.
- The task branch, `dunky13/address-review-findings`, was fast-forwarded to that
  PR 52 head before applying this follow-up. It includes unmerged plugin work;
  do not treat the branch as an isolated patch against main.
- The supplied review references identify the PRs. Neither PR had posted
  review findings or a stored review guide, so independent Standards and Spec
  reviewers inspected the pinned changes.

## Standards findings

All three confirmed findings are addressed.

| Finding | Original behavior | Correction and regression |
| --- | --- | --- |
| PR 51, P2: stopped-service custody | Bundle-relative launchd output could override contradictory absolute program evidence. A stopped service could pass using bundle identifiers alone, without proving which installed app owned it. | Reject conflicting or duplicate program/definition fields. A stopped service needs the canonical absolute executable or bundled LaunchAgent path. Running helpers retain the kernel executable check. Tests cover conflicting programs, missing location, wrong definition paths and duplicate definitions. |
| PR 52, P2: plugin cap bypass | The first overflow entry increased the registry length beyond the equality check; later entries loaded. A 35-entry fixture loaded 34 lanes. | Use a monotonic greater-than-or-equal check. The regression checks every accepted and refused entry across the boundary. |
| PR 52, P2: hook process-group reuse | Reaping an exited hook before its background descendants finished released the leader PID. A later signal used only the numerical group ID. | Keep a supervisor unreaped until the final group signal. Close supervisor descriptors in the hook, preserve its exit result, and retain bounded output and the original timeout. Regressions verify the leader remains reserved and background work is terminated. |

An independent recheck found that synchronous process inspection in the first
hook fix could delay termination. Inspection now races the original deadline;
termination happens before joining a slow inspection. A regression deliberately
blocks inspection while checking that the background child still dies at the
deadline.

Stopped-service output without canonical location evidence now fails closed.
These parser regressions use fixtures; they do not establish compatibility with
every live macOS launchd output variant.

## Spec findings

Both confirmed findings are addressed.

| Finding | Original behavior | Correction and regression |
| --- | --- | --- |
| PR 52, P2: concurrency reduction | Reload could publish concurrency 1 while two leases remained held, violating the lane's hard limit. | Validate the complete proposed registry before mutation. Refuse a concurrency below occupancy; preserve both leases and the previous definition. After one release, the reduction succeeds. |
| PR 52, P2: occupied lane reuse | Reintroducing a removed lane, or changing its plugin owner directly, replaced the rules for unfinished work. | Refuse reuse until the lane drains, preserving reservations, shared-limit accounting and ownership. Tests cover direct owner changes, retired lanes under another plugin, and readdition under the original plugin. Reuse succeeds after held and queued work ends. |

The plugin contract documents refused reloads and the operator's retry action.
A candidate status-position finding was retracted: valid adaptive stashed jobs
are normalized to queued before the position calculation, so the suspected
disagreement did not have a valid triggering flow.

## Verification

The new plugin-limit, reload, installer-custody and hook-leader regressions were
observed failing before their respective fixes. Focused tests then passed.

- `go test -p 2 -parallel 2 -race ./internal/backend ./internal/appservices -timeout 12m`
  passed: backend 521.292 seconds, appservices 1.216 seconds. This broad run
  started before the final hook inspection refinement and expanded same-plugin
  reuse test; the following focused runs verified those final bytes.
- `go test -p 2 -parallel 2 -race ./internal/backend -run '^TestHook' -count=1 -timeout 2m`
  passed in 44.121 seconds, including slow-inspection deadline enforcement.
- `go test -p 2 -parallel 2 -race ./internal/backend -run '^(TestPluginRegistryRejectsEveryEntryBeyondLimit|TestPluginReload.*|TestManaged.*BundleProgram.*)$' -count=1`
  passed in 1.804 seconds on the final backend patch.
- `go vet -p 2 ./internal/backend ./internal/appservices` passed.
- `go run -p 2 ./cmd/queue-tool check-source` passed, checking 362 files.
- Go formatting and `git diff --check` passed.

The local environment was Go 1.26.10 on macOS arm64. Only Go source and Markdown
changed. The broader frontend, native-app build and release validation batch was
not run for this scope.

Independent final Standards and Spec reviews found no remaining actionable
findings in the follow-up backend patch. Both were source reviews without
additional test runs. The original fixes were committed as
`088ce3b2947a252b328a45adf273d658b7aa83c3`. Their reviewed backend patch SHA-256 is
`503316f5ec8f31b1263431a15f78f143c4d8a761599c14b04076eec388ebe2b0`, computed with
`git diff --binary 839c788 088ce3b -- internal/backend | shasum -a 256`.
This statement covers the fixes; it is not an exhaustive clean certification of
both original PRs. Optional cross-provider review was not requested.

Jev merge readiness and hosted checks are separate from the source reviews.

## Packaged live-upgrade CI repair

When merge-on-green was requested, PR 52's Universal app check was failing in
[run 40899979250](https://github.com/Dunky13/agent-job-queue/actions/runs/40899979250/job/123539286250).
`TestInstallerPackagedLiveUpgrade/packaged-` lost its original caller with exit
125 and `Queue daemon crashed before releasing handover custody`.

A deterministic regression reproduced that exact error when a caller read a
`committed` receipt immediately before the old daemon published `released`
custody and exited. The subsequent process probe saw a dead daemon, while the
caller still used its stale receipt. The same race can span successor adoption.

After a confirmed daemon exit, the follower now re-reads the private receipt.
It accepts only the same immutable custody advanced to `released` or `consumed`,
revalidates database identity, and checks a consumed successor's liveness.
Unreleased crashes, changed tokens, targets, issuers, attempts or databases,
missing receipts, dead successors and process-probe errors remain failures.
It does not resubmit work or change timeouts.

The new eleven-case regression failed for the two valid transitions before the
fix and passed with race detection afterward in 2.663 seconds. Independent
Standards and Spec reviews found no actionable issue in the repair. The
reviewed two-file repair patch SHA-256 against `088ce3b` is
`e30b79ebaaab6fb39bdf3c557e64cd663a703bd966ed7121c358d14ceb9caf03`.

The broader repair validation passed in 171.846 seconds:

```sh
go test -p 2 -parallel 2 -race ./internal/backend \
  -run 'Test(Queue.*Handover|RealDaemon.*Handover|InstallerManagedLiveUpgradeRetainsRunningAttempts)' \
  -timeout 12m -count=1
```

This covers the real-daemon handover and six managed live-upgrade scenarios in
addition to the deterministic transition and refusal cases.

The production CLI was also built with `go build -p 2 -trimpath
-ldflags='-s -w'` into a private helper fixture. Running
`TestInstallerPackagedLiveUpgrade` with that helper and `-count=3` passed all
nine scenario executions in 59.281 seconds. This uses the same test as the
failed Universal app job, with a freshly built production helper; it is not a
full universal app, native Service Management, signing or Apple acceptance
claim. The disposable helper fixture was removed after verification.

Task-owned test processes completed. Go build scratch and private fixture
resources were reclaimed by their command and test cleanup paths. The retained
deliverables are this Markdown handoff and an ignored, self-contained HTML
version under the local build reports directory. No preview server or external
runtime resource was created.

## Delivery and remaining gates

The user authorized commit and push, then extended that authorization to a PR
and merge when all required checks are green. The follow-up targets main and
includes the plugin feature from the still-open PR 52 as well as the review
fixes and packaged live-upgrade repair. After this combined change merges, PR
52 can be closed as superseded. Every hosted check must cover the final head;
the inherited failing run is diagnostic evidence, not a passing gate.

No rendered UI or release packaging code changed. Native UI screenshots, signed
artifact builds, live Service Management acceptance, Intel runtime checks,
Apple notarization, Gatekeeper distribution checks and publication were not run
for this local backend follow-up. Existing release evidence is not evidence for
these changed bytes.

Any commit must retain configured signing and use the author-matching DCO
sign-off. Use the guarded maintainer squash path, verify the actual merge
identity, signature and tree, and check the exact post-merge main run. Release,
Apple submission, publication and deployment remain outside this authorization.
