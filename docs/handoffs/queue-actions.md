# Queue actions module handoff

Queue actions had four copies of the mutation pipeline and three separate copies of the allowed rule. This change gives them one module and one pipeline without changing what the operator sees.

## Result

- **Module:** `native/QueueActions.swift` defines `QueueAction`: Run next, Move to end, Start now, Cancel, Resume, parallel limit and scheduling mode. Each action answers whether it is allowed (`isAllowed(in:)`), which controller command it sends, what notice it shows, and whether it needs confirmation.
- **Pipeline:** `QueueStore.dispatch(_:)` reserves the store, sends the command, sets the notice or error, and refreshes. It replaces `dispatch`, `dispatchResume`, `dispatchLimit` and `dispatchScheduling`.
- **Callers:** the store, `QueueNavigation`, the row buttons and menu, and the test hooks all ask the action. `ConfirmationAction` is gone; `needsConfirmation` replaces it. The `start` and `cancel` command-line strings of the snapshot hooks are unchanged.
- **Data:** the decoded `canReorder`, `canCancel` and `canStart` flags stay on the job. The action owns the rules over them.
- **One behaviour difference:** the confirmation test hook chose its Start now job with `isWaiting && canReorder`. It now uses the shared start rule, so a waiting job with `canStart` false is no longer picked. Row buttons also now require a waiting job for Run next and Move to end, which matches what the store already enforced.
- **Untouched:** remote access, learning, integration and updates stores keep their own pipelines, and `fixtureMode` behaves as before.

## Evidence

- `scripts/test-native.sh`, `native/tests/run-menu-bar-ui-tests.sh` and `native/tests/run-resume-ui-tests.sh` results are recorded in the PR description.
- New checks in `BehaviorTests` pin each action's allowed rule, command arguments and confirmation need, including that a waiting job with `canStart` false and `canReorder` true is not startable.

## Next steps

1. Remove `fixtureMode` in the planned follow-up; the store's job-only fixture guard in `dispatch` goes with it.
2. Confirm in a live session that the row buttons and menu look unchanged for waiting, running and finished jobs.
