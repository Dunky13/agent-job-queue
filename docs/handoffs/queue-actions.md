# Queue actions module handoff

Queue actions had four copies of the mutation pipeline in the store, three store copies of the allowed rule, and a test hook whose Start now rule disagreed with the store. This change gives them one module and one pipeline without changing what the operator sees.

## Result

- **Module:** `native/QueueActions.swift` defines `QueueAction`: Run next, Move to end, Start now, Cancel, Resume, parallel limit and scheduling mode. Each action answers whether it is allowed (`isAllowed(in:)`), which controller command it sends, what notice it shows, and which confirmation it needs (`confirmation`).
- **Confirmation:** `QueueConfirmationKind` (`start`, `cancel`) is the one list of confirming actions. It builds the matching action, carries its unavailable message, and its raw values are the command-line strings of the snapshot hooks. `QueueNavigation.request` takes a kind and a job, so a non-confirming action cannot be requested. Row buttons and menus call `QueueNavigation.perform(_:in:)`, which asks first when the action needs confirmation and sends it otherwise.
- **Rules live in the module:** Start now, Run next, Move to end, Cancel, Resume, the limit range and "limit differs", and "mode differs" are all decided in `isAllowed`. `QueueJob.permitsStart` is removed; the decoded `canReorder`, `canCancel` and `canStart` flags stay on the job as data. `QueueStore.isPaused` uses the Resume rule.
- **Pipeline:** `QueueStore.dispatch(_:)` reserves the store, sends the command, sets the notice or error, and refreshes. It replaces `dispatch`, `dispatchResume`, `dispatchLimit` and `dispatchScheduling`. In fixture mode it never sends a command: job actions are recorded, everything else is refused or released.
- **Behaviour difference:** the confirmation test hook chose its Start now job with `isWaiting && canReorder`. It now uses the shared start rule, so a waiting job with `canStart` false is no longer picked. Row buttons for Run next and Move to end also now require a waiting job, which matches what the store already enforced. Both backends report `canReorder` only for queued jobs, so this is not visible today.
- **Older handoffs:** `docs/handoffs/swift-controller-commands.md` and `docs/handoffs/app-library-target.md` still name the removed store helpers as historical records.
- **Untouched:** remote access, learning, integration and updates stores keep their own pipelines, and `fixtureMode` behaves as before.

## Evidence

- `bash scripts/test-native.sh`: exit 0, no FAIL (final counts on the PR head are in the PR).
- `bash native/tests/run-menu-bar-ui-tests.sh <scratch dir>`: exit 0, 43 PASS, 0 FAIL.
- `bash native/tests/run-resume-ui-tests.sh <scratch dir>`: exit 0, 10 PASS, 0 FAIL.
- `scripts/capture-native-screenshots.sh` was not run because it overwrites the committed screenshots.
- New `BehaviorTests` checks pin each action's allowed rule (including `canStart` true on a job that cannot be reordered, and `canStart` false on one that can), controller arguments, notice text, confirmation need, that a disallowed action is rejected by the store without a command while an allowed one is sent, and that `perform` asks for Start now and Cancel but sends the others directly.
- The hook's new start pick is covered by the module-level start tests; no shared fixture has a job with `canReorder` true and `canStart` false.

## Next steps

1. Remove `fixtureMode` in the planned follow-up; the fixture branch in `dispatch` and `complete` goes with it.
2. Confirm in a live session that the row buttons and menu look unchanged for waiting, running and finished jobs.
