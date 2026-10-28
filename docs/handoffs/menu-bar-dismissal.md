# Menu bar dismissal and Settings clicks

## Result

The queue popover now observes outside mouse clicks even when the menu bar app never became active. The Settings menu explicitly accepts the click that activates its window. Focused graphical validation passed before delivery review; hosted validation and merge are recorded on the pull request.

The authorized endpoint is a signed commit, push, pull request and protected merge after required checks pass. Release, installation and deployment are separate from this task.

## Diagnosis and change

The popover uses application-defined dismissal to preserve right-click and Control-click on the status item. Its existing local event monitor sees only events sent to this app. When an accessory app shows the popover without becoming active, clicking another app produces neither a local event nor an application-deactivation notification.

A separate private nonactivating helper panel reproduced the defect: its button received the click while the queue popover stayed open. After adding a mouse-only global event monitor, the same check dismisses the popover and delivers the click to the helper. Both event monitors are removed when the popover closes. Existing child-menu tracking protection remains in place.

The Settings menu uses SwiftUI's `allowsWindowActivationEvents(true)` modifier, available on the app's macOS 15 minimum and the pinned Xcode 16.4 SDK. The reported two-click symptom did not reproduce in the local macOS 27.0.1 fixture before this change. Explicit activation-click support is now present, and actual single system clicks open both the gear and its arrow from an inactive popover in separate fresh processes. Verification on the affected installed app remains outstanding.

Apple documents the [distinct local and global event streams](https://developer.apple.com/library/archive/documentation/Cocoa/Conceptual/EventOverview/MonitoringEvents/MonitoringEvents.html) and [activation-event handling](https://developer.apple.com/documentation/swiftui/view/allowswindowactivationevents()).

## Regression coverage

The graphical runner launches its private executable directly so it retains the invoking terminal's existing event permissions. It never requests additional input permissions. Missing system-event permission is recorded as skipped unless `--require-system-events` is specified. The helper uses simulated UI only and exits if its parent disappears. Its scratch files live beneath the runner's trapped temporary directory so cancellation and the fixture watchdog cannot bypass file cleanup. The runner restores the pointer position after normal or thrown completion.

The fixture also now finds Undo by its responder selector because AppKit can change the title to name the current operation. This allowed previously skipped foreground editing checks to execute after real system mouse input activated the fixture.

## Validation

- Original fixture: 32 checks passed; three foreground keyboard checks skipped.
- New regression before the fix: the helper received the outside click, but dismissal failed.
- Final complete graphical fixture: 40 checks passed, zero skipped, with both system-event and foreground checks required.
- Separate fresh-process arrow check: six checks passed, zero skipped.
- Swift 6 compilation passed with warnings as errors and the macOS 15 deployment target on Apple Silicon.
- Public-source boundary check, ShellCheck, Bash syntax and whitespace checks passed.

The broad headless native suite, universal optimized build, Intel runtime, hosted CI and packaged distribution were not run for this local fix. The installed queue and its jobs were preserved. macOS emitted sandbox-extension diagnostics during graphical fixture startup, but all required input and foreground checks passed.

## Reproduce validation

Use a logged-in graphical macOS session and a terminal that already has permission to post events. These are focused tests; apply the operator's shared queue policy to broad validation and builds.

```sh
mkdir -p build
bash native/tests/run-menu-bar-ui-tests.sh build/menu-bar-verification.json --require-system-events --require-foreground
bash native/tests/run-menu-bar-ui-tests.sh build/menu-bar-chevron-verification.json --require-system-events --interaction-regressions --settings-chevron
shellcheck native/tests/run-menu-bar-ui-tests.sh
bash -n native/tests/run-menu-bar-ui-tests.sh
go run -p 2 ./cmd/queue-tool check-source
git diff --check
```

The JSON receipts record individual checks and skipped checks. A complete standalone HTML copy of this handoff is supplied as a local chat deliverable, outside version control.
