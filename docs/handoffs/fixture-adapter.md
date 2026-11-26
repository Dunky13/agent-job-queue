# Fixture adapter handoff

Preview behaviour was a `fixtureMode` flag that production stores and views checked at about a hundred places. Fixture runs now use a stand-in backend at the same seam the live app uses, so production holds no preview state and the shipped binary holds no fixture code.

## Result

- **Backend:** `native/WindowCoordinator.swift` defines `QueueBackend`: the controller `command`, the job `logTail`, the `desktop` requests (open a URL, reveal a file, copy text) and the app's `runningVersion`. `QueueBackend.live` is the only implementation in production, and only `AppDelegate.finishLaunching` names it.
- **No live defaults:** `WindowCoordinator(backend:pollSleep:)` and every store require their command adapter. The coordinator's unused `arguments` parameter is gone.
- **Routes:** `QueueAppRoutes` also carries `logTail` and `desktop`. `JobDetail`, `RemoteAccessSettings` and `QueueUpdatesSettings` use those instead of calling `QueueClient`, `NSWorkspace` or `NSPasteboard`. The releases page link is routed through its `openURL` environment.
- **Fixture:** `native/tests/hooks/QueueFixture.swift`, compiled only into test builds. It answers reads from the fixture file and canned data, records changes, desktop requests and log reads, and fails any command it does not know. Its coordinator never polls on a timer. The canned learning, integration, remote and update data moved here from the stores.
- **Launch:** in a test build, `finishLaunching` gives any launch with a `--fixture` argument the fixture coordinator, including one whose file cannot be loaded (the queue then shows that error). Snapshot and verification hooks run only on that coordinator and load what they show through it.
- **Removed:** `fixtureMode` (105 occurrences in production sources before, 0 after), the controls it disabled, the fixture branches in stores and views, and the PREVIEW badges.

## Safety

A test build launched with `--fixture` cannot reach the installed queue, its launch agents or its helpers:

| Path out of the app | What stops it |
| --- | --- |
| Controller commands, log reads, desktop requests, running version | `QueueBackend`, replaced as a whole by the fixture |
| A command the fixture does not know | It throws; nothing is passed on |
| Helper processes | `makeSupervisor()` returns nil because `supervisesHelpers` requires `allowsInstallation`, which is false with `--fixture`. Only a test that injects a fake supervisor gets one |
| Setup and installation | `inspect()` answers `.legacy`, `serviceStatusRequests()` is empty and `allowsInstallation` is false |
| Snapshot or verification hooks against a live app | They require the coordinator that the fixture built |
| Test options on the shipped app | `validateDiagnostics()` refuses them |

Bootstrap guards:

| Guard in `AgentBootstrap.swift` | Outcome |
| --- | --- |
| `allowsInstallation` is false with `--fixture` | Kept |
| `isTestOption` and the release build's refusal | Kept |
| Test build validation of `--fixture` and `--snapshot` values | Kept, and tightened: any other argument starting with `--fixture`, or a repeated `--fixture`, is refused, so on the real launch path no spelling can select the fixture backend while the guards read the launch as live. The test-only injected environment can differ from the process arguments |
| `inspect()` returns `.legacy` for `--fixture` | Kept |
| `serviceStatusRequests()` is empty for `--fixture` | Kept |
| `makeSupervisor(fixtureMode:)` | Parameter removed. The flag came from the same command line that `--fixture` is read from, so `supervisesHelpers` already refuses those launches |

The shipped app still contains these `--fixture` checks, all of which only refuse or skip work. No store parses arguments.

Not behind the new seam, because fixture mode never suppressed them: the setup window's Finder and browser buttons and its injected setup executor (a fixture launch skips setup), and the certificate Share button (the fixture reports no certificate path, so it is not shown). A direct call added to a view later would bypass the fixture; new calls belong in `QueueBackend`.

## Evidence

Queued batch on 5f7cddd (arm64): `scripts/test-native.sh` 1147 PASS, `run-resume-ui-tests.sh` 10, `run-bootstrap-ui-tests.sh` 93 (including the `supervised-quit` scenario), the release build and `check-source` (455 files) passed. The resume runner with `--before-ref 2295abe1` failed to compile: its source rewrite did not cover the new inertness check. The rewrite now covers it, and both resume runs pass on the final commit (10 and 14 checks).

The later commits change only `MenuBarUITests.swift`, `ResumeUITests.swift` and `run-resume-ui-tests.sh`. `run-menu-bar-ui-tests.sh` ran directly from the session (the queue's process lacks event-posting permission) 13 times in a row on the final test source, all 88 checks passing each time.

Implementation checks, run directly on the working tree before the commit (arm64):

- Release and test library flavours compile with warnings as errors. Hook symbols: 0 in the release library.
- `native/tests/run-refresh-tests.sh` 50 PASS, `run-bootstrap-tests.sh` 259, `run-service-management-tests.sh` 239, `run-supervisor-tests.sh` 119, `BehaviorTests` 126, `UpdateTests` 63 (after the merge with #90), `LearningTests` 86, `TransportTests` 234; no failures.
- - `go run -p 2 ./cmd/queue-tool check-source`, ShellCheck on the changed Bash scripts, `git diff --check`.

What the new checks cover:

- `RefreshTests` drives every coordinator store against a fixture with a temporary home that holds tripwire controllers. Negative controls show that a live call trips the tripwire, and that the check reports all 11 expected commands as missing when another fixture receives them.
- Checks that asserted a disabled or inert preview now assert that the live pipeline ran and ended at the recorder, with the exact commands.
- `BootstrapTests` replaces the `fixture-mode` supervisor scenario with a bare `--fixture` argument, and checks that both fixture launches get the stand-in backend and request no service status.
- The UI suites check that the launched coordinator is the fixture's and received no change. `MenuBarUITests` waits for each settings window's read to reach the fixture.

## Merged with Sparkle updates (#90)

Main replaced the download flow with Sparkle and added `fixtureMode` branches to `QueueUpdateStore`. The merge removes them the same way as in the other stores:

- `QueueUpdatePolicy.unavailableReason` no longer takes `fixtureMode`. A fixture launch is always a test-hook build, which already never creates the updater.
- The store requires `running` and `command`; the coordinator passes `backend.runningVersion()` and `backend.command`. The drain methods lose their guards: they run only on the helper path, which a fixture launch never takes (only the bootstrap UI test injects a fake supervisor, under a temporary home).
- The fixture no longer answers the removed `updates check` and `updates download` commands. `UpdateTests` replaces its preview cases with a fixture coordinator check; `RefreshTests` expects 11 commands.
- `docs/updates.md`, `docs/updates.html` and the README now describe fixture launches as test-hook builds. The Sparkle ADR and spec still mention preview mode as part of their historical record.

## Review dispositions

Independent Standards and Spec reviews (Opus) found no live regression and no new route from a fixture launch to the real controllers. Fixed:

- A `--fixture` spelling the guards did not recognise could select the fixture backend while the launch read as live. Test builds now refuse such spellings. A second layer that skipped the supervisor for any fixture launch was dropped after re-review: it was redundant on the real launch path and removed the fake supervisor the bootstrap UI `supervised-quit` scenario injects.
- The update store read the running version when the coordinator was built. It is now a `QueueBackend` member read on first use, as before, and the store requires it.
- The menu-bar status check could race a refresh already in flight. It now fails the queue through a real fixture read and waits for the error.
- Vacuous count conditions in `UpdateTests` and `LearningTests` were removed; `LearningTests` now checks that a reload reads the selected profile from the fixture. `ResumeUITests` refreshes through the fixture before checking that it recorded no change.
- `firstSettingsClickOpensMenuInInactivePopover` failed intermittently, here and on the window coordinator branch, on about one run in five. Diagnostics showed the posted click reaching the app below: `NSPopover.isShown` was already true, but the popover's window was not yet on screen (absent from the on-screen window list), and it becomes visible 0.3 to 0.5 s after the show. The check now waits until the popover window is visible before it posts the click, and the precondition check asserts that; on failure it reports where the click landed. Its sibling `externalClickDismissesPopoverWithoutApplicationDeactivation`, which runs next in the same process, then failed two runs in ten. The first check no longer slept 0.3 s, so it moved on while its footer menu was still tracking, and the queue ignores outside clicks during menu tracking. The first check now waits for that menu to end tracking (`firstSettingsMenuEndsTracking`), and the outside-click check waits for its outcome instead of sleeping and requires the popover on screen first. Two quirks of the earlier diagnosis are worth knowing: a status item's own window always reports `isOnActiveSpace` false and a placeholder window number, so neither indicates a missing menu bar item.

## Behaviour differences

Live app: none intended. The releases page link now opens through the injected `openURL` action, which calls `NSWorkspace.open`.

Fixture runs, in test builds only:

- No PREVIEW badge, and controls are enabled.
- Changes, including resume, limit and scheduling, run the live pipeline and show its notices. They are recorded, not applied: later reads return the same canned data.
- Stores load when their view or a snapshot hook asks, as in the live app. Learned Workloads opens at its projects.
- Stores never poll on a timer, so a window that stays open does not reload by itself.
- The job log shows a sample line instead of the previous preview-mode message.
- Open log, Copy address, Open, the releases page and the remote certificate's Show in Finder are recorded as desktop requests, not performed.
- About shows the fixture's installed version and the test-build reason; a test-hook build never creates Sparkle's updater.
- Password verification reports `rejectionsSentNothing` and `acceptedPasswordReachedTheFixture`; confirmation verification reports `limitReachedOnlyTheFixture`. These replace the three fields that asserted preview mode dispatched nothing.

The screenshots in `docs/screenshots/` that showed the PREVIEW badge were regenerated with `scripts/capture-native-screenshots.sh`.

## Next steps

1. Review and merge the PR after hosted CI passes.
2. PR 5: the installer platform port in Go.
