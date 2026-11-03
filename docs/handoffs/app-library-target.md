# App library target

Branch: `arch/app-library-target`. Authorized endpoint: draft PR. Independent review, merge and release are separate steps.

## Result

The menu bar app's code now compiles as one static library, `AgentQueueCore`, that the app and the tests both link. Tests no longer rewrite production source, and the test hooks are out of the shipped binary.

- `native/build-library.sh DIR ARCH release|test` is the only place that lists the app's sources. It takes every `native/*.swift`, so a new file needs no script changes. #57 had to edit six.
- `native/app/AgentQueueMain.swift` is the thin `@main`. `AgentQueueApp.main()` stays in the library and is the only `public` declaration added. Tests reach everything else through `-enable-testing` and `@testable import AgentQueueCore`.
- `native/build.sh` still uses `swiftc` directly and builds each architecture with the same `-O`, Swift 6 and `-warnings-as-errors` flags, followed by `lipo`. `-force_load` keeps every object in the binary. It now also fails if hook symbols reach the binary, with a check that the symbols were readable at all.
- `native/tests/build-test.sh` links any `@main` source against the `test` flavour. `scripts/test-native.sh` compiles that library once for its six suites, where it compiled the app six times before.
- `native/tests/hooks/AppTestHooks.swift` holds the `--snapshot*` and `--verify*` launch path, its verification reports, `NativeSnapshotRenderer` and `FixtureDispatchRecorder`, moved verbatim. Only the `test` flavour compiles this directory.
- `AGENT_QUEUE_TEST_HOOKS`, set only by the `test` flavour, gates `--fixture` parsing, the one call into the snapshot path, and the sample data seeded by the stores.
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

Runtime class names change from `arm64.AppDelegate` and `x86_64.AppDelegate` to `AgentQueueCore.AppDelegate`. Nothing in `native/` looks up classes by name.

## Evidence

All of this ran on macOS 27 through the shared queue. The commands ran before the rebase onto #64, except the final bootstrap runner below. #64 changed only Go code and its docs: `native`, `scripts`, `.github` and `CONTRIBUTING.md` are byte-identical across the rebase.

- `bash scripts/test-native.sh`: exit 0, 1,008 PASS lines, including the supervisor and service management suites on both architectures.
- `scripts/build-app.sh`: exit 0. `queue-tool verify-app`: exit 0. `codesign --verify --strict` passes on the ad hoc signature. Developer ID signing was not exercised, because PRs receive no Apple secrets, and `macos_package_app` is unchanged.
- `bash scripts/lint.sh`: exit 0.
- `native/tests/run-resume-ui-tests.sh` with `--before-ref`: exit 0, both sides.
- `native/tests/run-menu-bar-ui-tests.sh`: exit 0, `passed: true`. Five system-click checks were skipped because this terminal has no event-posting permission.
- `scripts/capture-native-screenshots.sh`: exit 0, and the images show the fixture views. They differ from the committed ones because of macOS 27 rendering, so they were restored and not committed.
- Test Swift files: 13 lines added and none removed. No timeouts changed.

`native/tests/run-bootstrap-ui-tests.sh` exits 1 here, including on the final head, so this runner has no local pass. It stops at its first scenario, at the known `NSApp.windows` check in `checkNeutralStartup`, which is not changed. Run one at a time, 10 of its 12 scenarios passed, and the other two stopped at that check. The check is intermittent on this host on both branches. Over three rounds of the default, `--cancellable-wait` and `--cancellable-wait --direct-quit` scenarios, `origin/main` failed all three in round 1 and passed the rest, and this branch passed all nine runs. Hosted CI on macOS 15 remains the evidence for this runner.

## Remaining work

- Independent review, hosted CI and merge. Check both native matrix jobs.
- Follow-up: delete `fixtureMode` from production. It is always false in the shipped app, but about 80 references in the views and stores, including the `PREVIEW` labels, still use it. Removing it is a larger UI change than this PR should carry.
- Follow-up: `QueueStore.init(arguments:)` is unused in the release flavour. `allowsInstallation`, `inspect()` and `serviceStatusRequests()` keep their `--fixture` guards as a second safety layer, although the refusal now runs first. `AppDelegate(environment:…)` is still a test-only seam in production.

## Next steps

```sh
gh pr checks PR_NUMBER
bash scripts/test-native.sh
```
