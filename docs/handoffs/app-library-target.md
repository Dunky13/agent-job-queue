# App library target

Branch: `arch/app-library-target`. Authorized endpoint: draft PR. Independent review, merge and release are separate steps.

## Result

The menu bar app's code now compiles as one static library, `AgentQueueCore`, that the app and the tests both link. Tests no longer rewrite production source, and the test hooks are out of the shipped binary.

- `native/build-library.sh DIR ARCH release|test` is the only place that lists the app's sources. It takes every `native/*.swift`, so a new file needs no script changes. #57 had to edit six.
- `native/app/AgentQueueMain.swift` is the thin `@main`. `AgentQueueApp.main()` stays in the library and is the only `public` declaration added. Tests reach everything else through `-enable-testing` and `@testable import AgentQueueCore`.
- `native/build.sh` still uses `swiftc` directly and builds each architecture with the same `-O`, Swift 6 and `-warnings-as-errors` flags, followed by `lipo`. `-force_load` keeps every object in the binary. It now also fails if hook symbols reach the binary, with a check that the symbols were readable at all.
- `native/tests/build-test.sh` links any `@main` source against the `test` flavour. `scripts/test-native.sh` compiles that library once for its six suites, where it compiled the app six times before.
- `native/tests/hooks/AppTestHooks.swift` holds the `--snapshot*` and `--verify*` launch path, its verification reports, `NativeSnapshotRenderer` and `FixtureDispatchRecorder`, moved verbatim. Only the `test` flavour compiles this directory.
- `AGENT_QUEUE_TEST_HOOKS`, set only by the `test` flavour, gates these:
  - `--fixture` parsing and `fixtureRecorder` in `QueueStore`
  - the fixture branches of `actionsDisabled` and `completeAction`
  - the one call into the snapshot path, in `finishLaunching`
  - the sample data seeded by the stores, and `LearningStore.previewDetail`
  - the fixture and snapshot argument checks in `validateDiagnostics`, whose release branch is the refusal
- `scripts/test-installer.sh` launches the packaged app once with `--fixture`. It requires exit 1, the refusal text and an empty private home. CI's `app` job and the distribution workflow both run that script.
- `scripts/capture-native-screenshots.sh` links the same `AgentQueueMain.swift` against the `test` flavour, so the capture app differs from the shipped one only by the flavour's flags and hooks.

## Decisions

- **A real library, not only a shared source list.** A list would have removed the `sed` and repeated lists too, but it would still compile the app once per suite, and the brief asks for one unit that both link. A scratch spike showed all nine test executables link with one added import line each.
- **The shipped app refuses test options.** With the hooks gone, `--fixture` would otherwise fall through to a real launch and real setup. Instead, `validateDiagnostics` exits with an error on any option starting with `--fixture`, `--snapshot` or `--verify-`.
- **`AgentServices.swift` and `AgentSupervisor.swift` keep their standalone compiles,** on both architectures. Those runs prove the files compile with their limited dependencies, so they do not link the library. Those two runners are unchanged.
- **The historical `--before-ref` path keeps its `sed` and shims,** and also removes the new import line from its copy of `ResumeUITests.swift`.

## What the shipped binary contains

Measured on the universal app from `scripts/build-app.sh`, which does not strip it.

| | `origin/main` at `cab5248` | This branch |
|---|---|---|
| `AgentQueue` size from `native/build.sh` | 8,846,504 bytes | 8,672,632 bytes |
| Symbols matching the hook types and functions (`nm -arch all`) | 890 | 0 |
| Linked libraries (`otool -L`) | Same set | Same set |
| Minimum macOS | 15.0 | 15.0 |

The packaged binary measured 8,696,752 bytes, well under setup's 128 MB limit.

`strings` shows only `--fixture`, `--snapshot` and `--verify-`, used by the refusal and by the guards listed below. `--snapshot-menu`, `--verify-detail-navigation`, the fixture error text and the sample addresses are gone. Swift stores strings of 15 bytes or fewer inline in instructions, so `strings` cannot prove a short literal absent. `nm` is the stronger evidence.

Launching the packaged binary with `--fixture`, `--snapshot` or `--verify-detail-navigation` exits 1 with `Test options need the test build`, and left its private home empty.

Every native suite links the `test` flavour, so the release flavour's normal launch was checked separately, on two paths that cannot start helpers, each with a private home:

- The packaged app outside Applications refuses setup and keeps running: alive after 8 seconds, no child processes, nothing written to its home, and exit 143 on SIGTERM.
- The same binary in a bundle with no payload or service marker, launched with `--open-window`, takes the legacy path through `finishLaunching` and the real `QueueMenuBarController`: alive after 8 seconds, no child processes, one 700x792 window on screen, nothing written to its home, and exit 143 on SIGTERM.

Neither run read the window's contents, because `screencapture` could not capture it here, or clicked the menu. A complete managed launch of the release binary that starts the helpers was not run on this host, because it would touch the live queue. Quitting through the menu was not run either.

Those two launches were a one-off and are not repeated. The first shared the bundle identifier `io.went.agent-queue` with the installed app, a risk not considered when it ran. The review asked for no further live launches beyond the single `--fixture` launch in `scripts/test-installer.sh`.

Runtime class names change from `arm64.AppDelegate` and `x86_64.AppDelegate` to `AgentQueueCore.AppDelegate`. Nothing in `native/` looks up classes by name.

## Evidence

All of this ran on macOS 27 through the shared queue. The commands ran before the rebase onto #64, except the final bootstrap runner below. #64 changed only Go code and its docs: `native`, `scripts`, `.github` and `CONTRIBUTING.md` are byte-identical across the rebase.

- `bash scripts/test-native.sh`: exit 0, 1,008 PASS lines, including the supervisor and service management suites on both architectures.
- `scripts/build-app.sh`: exit 0. `queue-tool verify-app`: exit 0. `codesign --verify --strict` passes on the ad hoc signature. Developer ID signing was not exercised, because PRs receive no Apple secrets, and `macos_package_app` is unchanged.
- `bash scripts/lint.sh`: exit 0.
- `native/tests/run-resume-ui-tests.sh` with `--before-ref`: exit 0, both sides.
- `native/tests/run-menu-bar-ui-tests.sh`: exit 0, `passed: true`. Five system-click checks were skipped because this terminal has no event-posting permission.
- `scripts/capture-native-screenshots.sh`: exit 0, and the images show the fixture views. They differ from the committed ones because of macOS 27 rendering, so they were restored and not committed.
- `scripts/test-installer.sh` with the new packaged `--fixture` launch: exit 0, on `arch/app-library-target` after 439bdf0. The check's failure branches (wrong exit, wrong text, a write to the home, a 30-second hang) were exercised on stub executables only. The installed app, daemon and scheduler kept the same process IDs and start times across the run.
- Test Swift files: 13 lines added and none removed. No timeouts changed.

`native/tests/run-bootstrap-ui-tests.sh` exits 1 here, including on the final head, so this runner has no local pass. It stops at its first scenario, at the known `NSApp.windows` check in `checkNeutralStartup`, which is not changed. Run one at a time, 10 of its 12 scenarios passed, and the other two stopped at that check. The check is intermittent on this host on both branches. Over three rounds of the default, `--cancellable-wait` and `--cancellable-wait --direct-quit` scenarios, `origin/main` failed all three in round 1 and passed the rest, and this branch passed all nine runs. Hosted CI on macOS 15 remains the evidence for this runner.

## Accepted review items

- F2: the dead `fixtureMode` guards ship as literal false. Follow-up, listed below.
- F3: the shipped app keeps refusing `--fixture`, `--snapshot*` and `--verify-*`.
- F5: a stray file in `native/` compiles into a local build. This is the documented design of using `native/*.swift` as the source list.

## Remaining work

- Independent review, hosted CI and merge. Check both native matrix jobs.
- Follow-up: delete `fixtureMode` from production. It is always false in the shipped app, but it still has 103 occurrences on 91 lines in `native/*.swift`, including the `PREVIEW` labels. It ships as dead guards that are always false, which the parent accepted as a follow-up. Removing it is a larger UI change than this PR should carry.
- Follow-up: `QueueStore.init(arguments:)` is unused in the release flavour. `allowsInstallation`, `inspect()` and `serviceStatusRequests()` keep their `--fixture` guards as a second safety layer, although the refusal now runs first. `AppDelegate(environment:…)` is still a test-only seam in production.

## Next steps

```sh
gh pr checks PR_NUMBER
bash scripts/test-native.sh
```
