# Window coordinator handoff

`AppRuntime` was a global singleton that owned five stores, five window variables and the menu bar controller, with five near-identical `show*Window` methods. Stores read its windows to decide whether to poll, and views called it directly. This change replaces it with one injected coordinator without changing what the operator sees.

## Result

- **Coordinator:** `native/WindowCoordinator.swift` defines `WindowCoordinator`, the composition root once setup is verified. It owns the queue, remote access, integration, update and learning stores, the menu bar controller and the app windows. It builds every store with its command adapter, and `AppDelegate` creates it only in `finishLaunching`. The settings stores are still built on first use, because the update store reads the bundle's build metadata when it is created.
- **Windows:** `QueueWindowKind` gives each window a `QueueWindowSpec` (title, size, minimum size, style), and `show(_:profileID:)` is the one factory. It creates a window once, reuses it afterwards, and runs the per-kind reload or polling step after the window is on screen. The merge-target picker store now comes from the learning store (`targetPickerStore()`), so it uses the same adapter.
- **Surfaces:** `QueueSurface` is the one visibility interface (`isVisible`). `NSWindow` conforms, and so does `QueueMenuBarController`, whose flag is set from `popoverWillShow` until the popover is closed. `QueueStore` and `RemoteAccessStore` hold a weak `QueueSurfaces` list and ask it on every poll. The coordinator registers the queue and remote windows, and the controller registers itself.
- **Appear:** `QueueStore.appear()` refreshes at once and returns its task. The appear counter and `disappear` are removed.
- **Clock:** both polling stores take a `PollSleep` closure that defaults to `Task.sleep` on `ContinuousClock`. `RemoteAccessStore.startPolling()` returns the new loop, or nil when it started none.
- **Injected routes:** views and the menu bar controller receive `QueueAppRoutes` (`perform`, `openLearning`) through their initializers. `QueueMenuBarController.init` has no default performer.
- **Setup window:** `AppDelegate` keeps its guard and reuse rule, because the window exists before the coordinator and an open one is not brought forward again. It is built from the same `QueueWindowSpec`, with its own style (no resize or close button).
- **Untouched:** `fixtureMode` and the PREVIEW badge behave as before. Store initializers keep their existing parameters and add only a defaulted one.

## Behaviour differences

None intended or known for the operator. Cadences are unchanged: the queue refreshes at start, then ticks every 2 s and loads while any surface is visible, otherwise every fifth tick; remote access loads every 5 s while its window is visible.

## Pre-existing behaviour left unchanged

SwiftUI hosting overrides each window's configured `minSize` with one derived from the content. A probe of all five windows on `bcce14e` and on this change gave identical output, on first open and after reopening: style, no identifier, kept after closing, content size equal to the configured size, and these live minimum sizes:

| Window | Configured `minSize` | Live `minSize` on both |
| --- | --- | --- |
| Agent Queue | 490x480 | 470x472 |
| Learned Workloads | 800x550 | 66x81 |
| About Agent Queue | 590x500 | 137x169 |
| Agent Integration | 520x540 | 106x150 |
| Remote Access | 500x570 | 480x572 |

So most windows report a live minimum far below the configured one. Resizing them that far was not tried. This change does not alter the behaviour.

## Evidence

Counts are from one run through the shared queue. Only `MenuBarUITests.swift` and this file changed afterwards.

- `bash scripts/test-native.sh`: exit 0, 1125 PASS.
- `bash native/tests/run-resume-ui-tests.sh <scratch dir>`: exit 0, 10 PASS.
- `bash native/tests/run-bootstrap-ui-tests.sh <scratch dir>`: exit 0, 93 PASS.
- `ARCHS=arm64 bash native/build.sh <scratch file>`: exit 0, so no test hook symbol reached the release binary.
- `go run -p 2 ./cmd/queue-tool check-source`: exit 0.
- `bash native/tests/run-menu-bar-ui-tests.sh <scratch>/menu-bar.json`: run by parent. The queued run stopped after 35 PASS on a window check that expected the configured `minSize`; the test now pins it on the window spec, and the updated target type-checks.

- New headless `RefreshTests` checks drive both poll loops with a manual clock and count status loads: 2 s visible, 10 s hidden counted from the start, immediate load on appear, a window that is visible without announcing itself, popover and window together, remote 5 s, stop when hidden and restart when shown. A further check records that every coordinator store, including the merge-target picker, reaches only the injected adapter.
- `MenuBarUITests` now also pins each window's title, content size, configured minimum size, style, identifier and reuse after closing, and queue and remote surface visibility when open, closed, miniaturized and restored. `BootstrapUITests` also pins the setup window's title and style.
- `native/tests/run-resume-ui-tests.sh` rewrites the new calls back to the old ones for `--before-ref` builds. The rewritten text was compared by diff; a `--before-ref` build was not run.
- `scripts/capture-native-screenshots.sh` was not run because it overwrites the committed screenshots.

## Next steps

1. PR 4: inject a fixture adapter through `WindowCoordinator` and remove `fixtureMode` and the PREVIEW badge. `JobDetail` still calls `QueueClient.logTail` directly, so the log read needs the same seam.
2. In a live session, open and close each window and the popover, and confirm that live data keeps updating while one is shown.
3. Possible follow-up: make the configured minimum sizes take effect, either through the root views' minimum frames or the hosting view's sizing options.
