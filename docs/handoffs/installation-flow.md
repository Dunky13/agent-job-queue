# Drag-to-Applications recovery

## Result and scope

Opening the dragged app automatically finishes supported setup and recovery, then starts the menu bar client. A retained pending build is completed at its recorded location before a different build can take over. Moving between the two supported Applications folders uses the verified, drained service lifecycle and preserves the previous app.

The reported Mac was repaired using the changed controller against its existing signed apps. Build 150 completed at the old location, then build 162 became the managed installation in `/Applications`. A normal launch exposed the Agent Queue menu bar item with no setup window. The queue stayed paused with limit 3, and all 30 job outcomes matched the captured before-state exactly. Neither signed app bundle was replaced by a development build.

The endpoint is a reviewed PR and the authorized local repair. Merge, a new signed release, Apple submission, publication and deployment remain separate actions.

## Causes and changes

- Pending recovery used the new explicit app path instead of the recorded retained app. It now selects, verifies and pins the exact retained bundle, including publication custody and any owned backup.
- Setup refused a new Applications location while the recorded app still existed. It now verifies both bundles and the completed previous build, waits for jobs and retained callers, unregisters through the previous app's owned helpers, and registers the new location. The old app remains in place.
- This Mac's launchd prints `path = (submitted by smd.PID)`. A valid submission descriptor no longer contradicts a live process's verified executable path. A descriptor alone cannot establish stopped-service ownership; malformed, duplicate and unrelated paths still fail closed.
- Ordinary startup now uses supported backend recovery and repairs missing registration without a redundant review. Unrecoverable pending candidates offer the official release history.
- Job and approval waits display progress, Settings when needed, Stop waiting and Quit. A cooperative stop request ends inspections and waits while an accepted installation step retains its original context and response. Nested recovery shares the stop request. A stopping phase blocks retry and makes quit/logout await controller completion.
- The final throttled progress update flushes even if the controller emits no more output, so an approval or waiting state cannot remain hidden indefinitely.

No job is automatically cancelled, resumed or replayed. Uncertain process ownership, changed bundles, missing custody, invalid signatures and unknown requirements remain blockers. macOS approval and endpoint security policy are preserved.

## Evidence

- The exact original installed-state preflight returned only `reveal`; the changed controller offered `finish-pending-installation` and completed the recovery. The additional launchd refusal was reproduced before its parser fix, which retained process ownership checks.
- The complete finite validation batch passed on the initial corrected candidate: lint/vet, reachable-vulnerability scan, complete Go race scope, Linux tool compilation, schema tests, 25 browser tests, native tests, universal app, packaged installer and DMG checks. No reachable vulnerable calls were reported.
- Final focused Go race regressions passed after the cooperative-stop changes. They cover relocation, exact pending recovery, retained callers, negative custody/ownership cases, accepted mutation contexts and stopping a nested pending approval wait.
- Final native tests cover real private-process cancellation, an intentionally stale waiting marker during an accepted mutation, delivery of a quiet last progress hint, and guarded retry/termination while stopping.
- Seven graphical fixture scenarios cover failure/retry, preflight controls, approved repair, drag launch/approval, silent recovery and cancellable waiting. Screenshots use an injected executor, disposable home and simulated jobs. The original reveal-only report is replayed before the recoverable report in the same private environment.
- The final universal development package passed installed-payload, packaged live-upgrade, app-bundle, gateway and DMG verification. Development signatures do not establish Developer ID or Apple acceptance.
- Independent Sol 6.1 high Standards and Spec closure reviews reported no remaining source findings on core tree `07a51b918fff58366e14b41f15f8d5922e9158a5`, against base `d53dc7bca717974fa2acd1db36676660fae248e1`. The later screenshot replay and CI artifact wiring receive local fixture, workflow-contract and actionlint verification.

Hosted Apple Silicon and Intel results, including the uploaded `First-launch` screenshot artifacts, must be read from the exact PR-head checks. This handoff does not substitute local ARM evidence for hosted Intel execution. Cross-provider review was not requested. The advisory Jev probe is separate from independent review and required checks.

## Reproduction and next gates

From the repository root, use the toolchain documented in `CONTRIBUTING.md`:

```sh
go test -p 2 -parallel 2 -race ./internal/backend -run 'TestManaged(SetupStop|LoadedBundleProgram|StoppedBundleProgram|DraggedApp|RecordedPending|PendingSelection|DistinctApp|Relocation|AutomaticRelocation)|TestInstallerApprovedRepair'
bash scripts/test-native.sh
bash native/tests/run-bootstrap-ui-tests.sh build/bootstrap-ui
```

The tests use private fixtures. A temporary home alone does not isolate real Service Management, so do not substitute it for the injected lifecycle fixtures. Real installed-state acceptance is already recorded separately above.

Before merge, inspect exact-head hosted checks and the first-launch artifacts. Obtain the owner's merge authorization. A release still needs its normal Developer ID, notarization, stapling, Gatekeeper and publication gates; no new Apple submission or release was performed here.

Fixture processes, directories and mounted images are disposable and are removed by their runners. The real app, queue data and previous signed app are retained. Local evidence and development packages are intentionally retained for PR review; screenshots and the standalone HTML handoff stay outside Git.
