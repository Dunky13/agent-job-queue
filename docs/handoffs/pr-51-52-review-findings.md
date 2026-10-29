# PR 51 and 52 review follow-up

## Scope and source identities

Review the drag-and-drop app and plugin lanes changes, reproduce actionable
findings, and fix them locally. No merge, release, Apple submission, deployment,
or operation on the installed queue is included.

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
additional test runs. The reviewed backend patch SHA-256 is
`503316f5ec8f31b1263431a15f78f143c4d8a761599c14b04076eec388ebe2b0`, computed with
`git diff --binary -- internal/backend | shasum -a 256` against the PR 52 head.
This statement covers the fixes; it is not an exhaustive clean certification of
both original PRs. Optional cross-provider review was not requested.

Jev merge readiness is not asserted for this follow-up: no follow-up PR exists.

Task-owned test processes completed. Go build scratch and private fixture
resources were reclaimed by their command and test cleanup paths. The retained
deliverables are this Markdown handoff and an ignored, self-contained HTML
version under the local build reports directory. No preview server or external
runtime resource was created.

## Delivery and remaining gates

After local validation, the user authorized a signed, DCO-certified commit and
push of the task branch. A follow-up PR and merge are outside that delivery
request. This handoff makes no hosted CI result or merge-readiness claim for
these fixes. The original PR 52 remained independently open when rechecked
before committing.

No rendered UI or release packaging code changed. Native UI screenshots, signed
artifact builds, live Service Management acceptance, Intel runtime checks,
Apple notarization, Gatekeeper distribution checks and publication were not run
for this local backend follow-up. Existing release evidence is not evidence for
these changed bytes.

When preparing delivery, retain the dependency on PR 52: either land the fixes on
top of that feature, or separate the PR 51 custody correction from the
plugin-dependent fixes. Any commit must retain configured signing and use the
author-matching DCO sign-off. Merge and release remain separate authorization
boundaries.
