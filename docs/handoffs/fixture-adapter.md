# Fixture adapter handoff

Preview behaviour was a `fixtureMode` flag that production stores and views checked at about a hundred places. Fixture runs now use a stand-in backend at the same seam the live app uses, so production holds no preview state and the shipped binary holds no fixture code.

## Result

- **Backend:** `native/WindowCoordinator.swift` defines `QueueBackend`: the controller `command`, the job `logTail`, the `desktop` requests (open a URL, reveal a file, copy text) and the app's `runningVersion`. `QueueBackend.live` is the only implementation in production, and only `AppDelegate.finishLaunching` names it.
- **No live defaults:** `WindowCoordinator(backend:pollSleep:)` and every store require their command adapter. The coordinator's unused `arguments` parameter is gone.
- **Routes:** `QueueAppRoutes` also carries `logTail` and `desktop`. `JobDetail`, `RemoteAccessSettings` and `QueueUpdatesSettings` use those instead of calling `QueueClient`, `NSWorkspace` or `NSPasteboard`. The release notes link is routed through its `openURL` environment.
- **Fixture:** `native/tests/hooks/QueueFixture.swift`, compiled only into test builds. It answers reads from the fixture file and canned data, records changes, desktop requests and log reads, and fails any command it does not know. Its coordinator never polls on a timer. The canned learning, integration, remote and update data moved here from the stores.
- **Launch:** in a test build, `finishLaunching` gives any launch with a `--fixture` argument the fixture coordinator, including one whose file cannot be loaded (the queue then shows that error). Snapshot and verification hooks run only on that coordinator and load what they show through it.
- **Removed:** `fixtureMode` (105 occurrences in production sources before, 0 after), the controls it disabled, the fixture branches in stores and views, and the PREVIEW badges.

## Safety

A test build launched with `--fixture` cannot reach the installed queue, its launch agents or its helpers:

| Path out of the app | What stops it |
| --- | --- |
| Controller commands, log reads, update checks, desktop requests, running version | `QueueBackend`, replaced as a whole by the fixture |
| A command the fixture does not know | It throws; nothing is passed on |
| Helper processes | A test build never builds the supervisor for a fixture launch. Independently, `makeSupervisor()` returns nil because `supervisesHelpers` requires `allowsInstallation`, which is false with `--fixture` |
| Setup and installation | `inspect()` answers `.legacy`, `serviceStatusRequests()` is empty and `allowsInstallation` is false |
| Snapshot or verification hooks against a live app | They require the coordinator that the fixture built |
| Test options on the shipped app | `validateDiagnostics()` refuses them |

Bootstrap guards:

| Guard in `AgentBootstrap.swift` | Outcome |
| --- | --- |
| `allowsInstallation` is false with `--fixture` | Kept |
| `isTestOption` and the release build's refusal | Kept |
| Test build validation of `--fixture` and `--snapshot` values | Kept, and tightened: any other argument starting with `--fixture`, or a repeated `--fixture`, is refused, so no spelling can select the fixture backend while the guards read the launch as live |
| `inspect()` returns `.legacy` for `--fixture` | Kept |
| `serviceStatusRequests()` is empty for `--fixture` | Kept |
| `makeSupervisor(fixtureMode:)` | Parameter removed. The flag came from the same command line that `--fixture` is read from, so `supervisesHelpers` already refuses those launches |

The shipped app still contains these `--fixture` checks, all of which only refuse or skip work. No store parses arguments.

Not behind the new seam, because fixture mode never suppressed them: the setup window's Finder and browser buttons and its injected setup executor (a fixture launch skips setup), and the certificate Share button (the fixture reports no certificate path, so it is not shown). A direct call added to a view later would bypass the fixture; new calls belong in `QueueBackend`.

## Evidence

SUITE_EVIDENCE

Implementation checks, run directly on the working tree before the commit (arm64):

- Release and test library flavours compile with warnings as errors. Hook symbols: 0 in the release library.
- `native/tests/run-refresh-tests.sh` 50 PASS, `run-bootstrap-tests.sh` 259, `run-service-management-tests.sh` 239, `run-supervisor-tests.sh` 119, `BehaviorTests` 126, `UpdateTests` 34, `LearningTests` 86, `TransportTests` 234; no failures.
- `MenuBarUITests`, `BootstrapUITests` and `ResumeUITests` compile but were not run. The resume runner's historical build at `2295abe1` compiles but was not run.
- `go run -p 2 ./cmd/queue-tool check-source`, ShellCheck on the changed Bash scripts, `git diff --check`.

What the new checks cover:

- `RefreshTests` drives every coordinator store against a fixture with a temporary home that holds tripwire controllers. Negative controls show that a live call trips the tripwire, and that the check reports all 12 expected commands as missing when another fixture receives them.
- Checks that asserted a disabled or inert preview now assert that the live pipeline ran and ended at the recorder, with the exact commands.
- `BootstrapTests` replaces the `fixture-mode` supervisor scenario with a bare `--fixture` argument, and checks that both fixture launches get the stand-in backend and request no service status.
- The UI suites check that the launched coordinator is the fixture's and received no change. `MenuBarUITests` waits for each settings window's read to reach the fixture.

## Review dispositions

Independent Standards and Spec reviews (Opus) found no live regression and no new route from a fixture launch to the real controllers. Fixed:

- A `--fixture` spelling the guards did not recognise could select the fixture backend while the launch read as live. Test builds now refuse such spellings, and `finishLaunching` skips the helper supervisor for any fixture launch.
- The update store read the running version when the coordinator was built. It is now a `QueueBackend` member read on first use, as before, and the store requires it.
- The menu-bar status check could race a refresh already in flight. It now fails the queue through a real fixture read and waits for the error.
- Vacuous count conditions in `UpdateTests` and `LearningTests` were removed; `LearningTests` now checks that a reload reads the selected profile from the fixture. `ResumeUITests` refreshes through the fixture before checking that it recorded no change.
- `firstSettingsClickOpensMenuInInactivePopover` failed on the first run after a build on this branch and on main's predecessor PR. The posted click arrives asynchronously, and the check waited a fixed 0.3 s; it now waits for the menu to open, up to 5 s.
- Docs said the fixture only records what it would open or reveal; an update download is also recorded, as a change.

## Behaviour differences

Live app: none intended. The release notes link now opens through the injected `openURL` action, which calls `NSWorkspace.open`.

Fixture runs, in test builds only:

- No PREVIEW badge, and controls are enabled.
- Changes, including resume, limit and scheduling, run the live pipeline and show its notices. They are recorded, not applied: later reads return the same canned data.
- Stores load when their view or a snapshot hook asks, as in the live app. Learned Workloads opens at its projects, and About shows no update check until one is requested.
- Stores never poll on a timer, so a window that stays open does not reload by itself.
- The job log shows a sample line instead of the previous preview-mode message.
- Open log, Copy address, Open, release notes and the remote certificate's Show in Finder are recorded as desktop requests, not performed. An update download is recorded as a change and saves nothing.
- Password verification reports `rejectionsSentNothing` and `acceptedPasswordReachedTheFixture`; confirmation verification reports `limitReachedOnlyTheFixture`. These replace the three fields that asserted preview mode dispatched nothing.

The committed screenshots in `docs/screenshots/` will differ once regenerated. They were not regenerated here.

## Next steps

1. Run `bash scripts/test-native.sh`, the menu-bar, resume (also with `--before-ref`) and bootstrap UI suites, and the release build, then fill in `SUITE_EVIDENCE`. The popover now refreshes through the fixture, so if the menu-bar suite flakes, look first at `statusTitleHoldsWidthWhilePopoverShown`, `heldStatusTitleAppliesOnClose` and `closedStatusTitleUpdatesImmediately`: a refresh finishing during their wait could change the simulated error.
2. Regenerate the screenshots with `scripts/capture-native-screenshots.sh` and review them.
3. PR 5: the installer platform port in Go.
