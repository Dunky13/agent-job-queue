# Queue job transitions

## Result

The queue store now owns job state transitions, in `internal/backend/queue_store_transitions.go`. This is a behaviour-preserving refactor with no schema or handover protocol change.

Callers ask the store for one of these:

| Method | Edge | Stale result |
| --- | --- | --- |
| `enqueue` | new row to queued | none; the insert and ID reservation are atomic with `publish` |
| `claimAttempt` | queued to starting | `Job changed before launch` |
| `claimWorker` | starting to running | `Execution attempt is no longer authorized` |
| `recordOutcome` | queued, starting or running to outcome | `errQueueChanged` |
| `retire` | outcome to done | `errQueueChanged` |
| `grantLease` | lease queued to running | `errQueueChanged` |
| `endLease` | lease queued or running to done | `errQueueChanged` |
| `saveTask` | no phase change | `errQueueChanged` |

An edge outside the command or lease lifecycle returns `errQueueIllegalTransition`. A row whose phase and status disagree returns `errQueueStateDisagreement`. Both fail before any write. `validateQueueHandoverJobState` keeps its stricter attempt and worker checks and its error text, and now takes phase and status agreement from the same rule. Each statement keeps its original `WHERE` guards.

## Where the checks are not strict, and why

- **Reads do not validate.** `job`, `jobs` and `snapshot` must still show a corrupt row to an operator. Only writes check.
- **Agreement covers phase, status and outcome, not attempt or worker.** A held lease has neither, and existing fixtures hold running rows without a worker. Handover keeps that stricter check.
- **A done row may have no outcome.** `importLegacy` retains completed history without that column. An outcome row must have it.
- **`starting` accepts `Paused`.** Pausing between launch and the worker's claim is a real sequence, and it leaves that pair.
- **Two stale results stay silent at the caller.** `retire` in the cleanup goroutine and the worker's final `recordOutcome` had no affected-row check before. The store now reports the miss and those two callers ignore `errQueueChanged`, which keeps the behaviour unchanged.
- **Raw settings queries stay raw.** `queue_settings` updates and `PRAGMA user_version` are not job state.

## Stricter only on rows no writer can produce

These changes affect corrupted rows only. Every writer is one atomic statement, so normal running, crashes and unclean restarts cannot produce them.

- `recover` now refuses daemon startup on a `starting` row whose status disagrees, where it previously recorded 125. An unparseable status already refused startup.
- `tick` now returns an error on a `running` row whose status disagrees, which holds dispatch until the row is repaired. An unparseable status already did this.
- An `outcome` row that disagrees repeats cleanup on each tick instead of retiring, as a failed `Cleanup` already does.
- A `starting` row with an undecodable or disagreeing task now fails before the worker runs its command. The old blind `UPDATE` would have run it.

`RowsAffected` errors are handled one way in `swapJob`. Some sites used to fold them into their stale error. The pinned `modernc.org/sqlite` driver never returns one, so that path cannot be reached.

## Tests

`queue_store_transitions_test.go` covers the store's interface on temporary SQLite: both lifecycles, every kind, phase and method combination, stale views with their exact errors, disagreeing rows and atomic enqueue.

Raw SQL that only simulated a transition now uses store calls in `queue_store_edge_test.go`, `queue_daemon_test.go` and `queue_handover_test.go`. These raw fixtures stay on purpose:

- Deliberate corruption, in the runtime generation and edge tests.
- `TestRealDaemonUnclaimedAttemptFailsWithoutReplay`. Its daemon is live, so a queued row could be dispatched before a real claim.
- Bulk history in the operational and benchmark fixtures. The operational fixture's `outcome` row has a `Queued` status, which `retire` correctly refuses.
- Fixtures with no store route, such as a changed attempt on a running row, and installer rows that only need to exist.

## Evidence

Validation evidence is listed in the pull request description for the reviewed head. The temporary mutants that removed each guard and agreement check all failed the tests, and were reverted.

## Remaining work

Independent Standards and Spec review of the draft pull request comes next. There is no follow-up in this change. A later refactor could move the `queue_settings` writes behind the store.

## Next steps

1. Review the draft pull request against this handoff.
2. Fix findings at their root and re-run the validation listed in the pull request.
3. Ask the owner to mark it ready and merge it. It is not merged or marked ready.
