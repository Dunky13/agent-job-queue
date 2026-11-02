# Queue job transitions

## Result

The queue store now owns job state transitions, in `internal/backend/queue_store_transitions.go`. There is no schema or handover protocol change. Behaviour on valid rows is unchanged, apart from the additive snapshot field described below.

Callers ask the store for one of these:

| Method | Change | Stale result |
| --- | --- | --- |
| `enqueue` | new row to queued | none; the insert and ID reservation are atomic with `publish` |
| `claimAttempt` | queued to starting | `Job changed before launch` |
| `claimWorker` | starting to running | `Execution attempt is no longer authorized` |
| `recordOutcome` | queued, starting or running to outcome | `errQueueChanged` |
| `retire` | outcome to done | `errQueueChanged` |
| `grantLease` | lease queued to running | `errQueueChanged` |
| `endLease` | lease queued or running to done | `errQueueChanged` |
| `setWaiting` | Queued to or from Stashed; never a lease | `errQueueChanged` |
| `setPaused` | Running to or from Paused; never a lease | `errQueueChanged` |
| `markCancelled` | records a cancel request; status unchanged | `errQueueChanged` |
| `saveTask` | fields other than status, such as priority | `errQueueChanged` |
| `settleHeld`, `settleHeldLease` | ends a held row only | `errQueueChanged` |

An edge outside the command or lease lifecycle returns `errQueueIllegalTransition`. A row whose phase and status disagree returns `errQueueStateDisagreement`. Both fail before any write. Each statement keeps its original `WHERE` guards. `importLegacy` is the one other writer: it inserts drained history. `validateQueueHandoverJobState` keeps its stricter attempt and worker checks and its error text, and takes phase and status agreement from the same rule.

## Disagreeing rows: hold only that row

No writer in any release can produce a disagreeing row, and independent review found 0 of 2,772 live rows rejected. The owner decided what happens if one appears anyway: **hold only that row**. There is no repair command, and the old self-healing is not restored.

- **Other jobs keep running.** `tick` moves past a held row, so higher job IDs, lease expiry and dispatch all continue. `checkLeaseOwners` and `recover` do the same, and the scheduler no longer sees a held row that reads as waiting.
- **Startup does not refuse.** `recover` holds a disagreeing `starting` row and the daemon serves its socket.
- **It keeps its slot.** A row in `starting`, `running` or `outcome`, or a held lease, still counts against its lane and the limit. A `queued` row had no slot and is not dispatched.
- **It is logged at a limited rate.** Each row logs at most once a minute. The tick's own error line has the same limit.
- **It shows in the status snapshot.** `problems` is an optional top-level array of `{id, kind, message}`, with kind `held` or `cleanup`. It is omitted when empty, so a healthy snapshot does not change. A held job also reports state `held`, a phase saying how to settle it, `canCancel` true, and `canStart` and `canReorder` false. `agent-heavy status` prints a line for each problem.
- **`cancel` and `kill` settle it.**
  - A process that cannot be verified is never signalled, and the request fails visibly with the row unchanged.
  - A live owned process is signalled and the request is recorded. The next tick after it exits settles the row with 130. This is the only way a tick ends a held row, and it needs the operator's request first.
  - With no process left, the row settles at once with 130. A row already at `outcome` keeps its published exit status. A lease ends as usual.
- **Other operations refuse clearly.** `start`, `hold`, `enqueue`, `priority`, `next`, `last`, `pause-job` and `resume-job` say only cancel or kill can settle the row. `lease-release` and `lease-renew` return the disagreement error.

A `Cleanup` that fails permanently used to stall in silence, with its message written to a field nothing read. Its message now appears in `problems`. This fixes that existing stall as well.

### Behaviour worth reviewing

- **A running row with no worker recorded and no live child settles without a signal.** There is no identity to verify and no process to find. Verification failing on a recorded worker still refuses.
- **A recorded worker that stays unverifiable blocks settling for as long as it does**, for example while a reused process group exists. This is the fail-closed choice, and there is no way past it other than the group going away.
- **A held row in `starting`, `running` or `outcome` at shutdown keeps `dirty=1`.** The next start then pauses the queue under the existing unclean restart rule, until an operator runs `resume`.
- **A held row stops live handover.** The upgrade falls back to waiting for a drain, and managed setup treats the row as unfinished, until it is settled.
- **A held lease does not expire or end when its owner exits.** It waits for cancel or kill.
- **A row with undecodable task JSON is unchanged.** Every read of unfinished jobs still fails, so it stops the queue as it did on main. Only a bad status inside readable JSON is held.
- **Held commands appear among recent jobs.** The dashboard and menu bar sort them with finished work, as they already do for a granted lease, because neither counts `held` as running or waiting. The dashboard shows the state, the phase and a Cancel button. The menu bar was not exercised.

## Where the checks are not strict, and why

- **Reads do not validate.** `job`, `jobs` and `snapshot` must still show a corrupt row to an operator. Only writes and the daemon's hold check validate.
- **Agreement covers phase, status and outcome, not attempt or worker.** A held lease has neither, and handover keeps that stricter check.
- **Legacy history is not validated.** `importLegacy` retains completed rows without an outcome column. Such a row would disagree, but `done` has no outgoing edge and the daemon's hold check skips `done` rows, so it is never refused or held.
- **`starting` accepts `Paused`.** Pausing between launch and the worker's claim is a real sequence, and it leaves that pair.
- **Two stale results stay silent at the caller.** `retire` in the cleanup goroutine and the worker's final `recordOutcome` had no affected-row check before. The store now reports the miss and those two callers ignore `errQueueChanged`, which keeps the behaviour unchanged.
- **Raw settings queries stay raw.** `queue_settings` updates and `PRAGMA user_version` are not job state.

`RowsAffected` errors are handled one way in `swapRow`. Some sites used to fold them into their stale error. The pinned `modernc.org/sqlite` driver never returns one, so that path cannot be reached.

A `starting` row with a bad task now fails before the worker runs its command, where the old blind `UPDATE` would have run it. The worker's final `recordOutcome` reports a disagreeing row in its returned error, where the old `UPDATE` wrote over it. That row is then held.

## Tests

- `queue_store_transitions_test.go` covers the store's interface on temporary SQLite: both lifecycles, every kind, phase and method combination, stale views with their exact errors, disagreeing rows and atomic enqueue.
- `queue_held_test.go` covers the daemon. It drives `tick`, `recover`, `control` and `checkLeaseOwners` directly, and `TestRealDaemonStartsWithHeldRowAndRunsOtherJobs` runs a real daemon end to end.
- `edgeInsertJob` is gone: those tests reach their phase through real transitions.

These raw SQL fixtures stay on purpose:

- Deliberate corruption, in the held, runtime generation and edge tests.
- `TestRealDaemonUnclaimedAttemptFailsWithoutReplay`. Its daemon is live, so a queued row could be dispatched before a real claim.
- Bulk history in the operational and benchmark fixtures. The operational fixture's `outcome` row has a `Queued` status, which `retire` correctly refuses.
- Fixtures with no store route, such as a changed attempt on a running row, and installer rows that only need to exist.

## Evidence

### Hold-only-that-row commit

These are focused checks only, as requested: the host is under heavy memory pressure and `scripts/validate.sh` is being fixed separately. **No full batch ran for this commit**, so hosted CI is its only broad check.

- `go test -p 2 -parallel 2 -race ./internal/backend -run 'Held|TestQueueControlHold|TestQueueStoreStatus|TestLegacyHistory|TestCleanupFailure|TestQueueTransition|TestQueueEdge|TestQueueCleanup|TestLease' -count=1` passed in 12s.
- `go test -p 2 -parallel 2 ./internal/backend -run 'TestRealDaemon|Handover|TestQueue|TestLease|TestLane|TestRuntimeGeneration|Drained|TestLegacy|Held|TestCleanupFailure|Scheduling|TestScheduler' -count=1` passed in 112s.
- The install tests that seed raw `{}` rows passed: `go test -p 2 -parallel 2 ./internal/backend -run 'DragDrop|TestManaged.*(Drain|Unfinished|Stopped|Recovery|Supervised)|Supervised' -count=1`, 26s.
- `node --test remote/static/schema.test.mjs` passed all 8 tests. A direct parse confirmed the web schema accepts `problems` and drops it. Swift's synthesized `Codable` ignores unknown keys. Neither type needed a change.
- 16 temporary mutants each failed the tests and were reverted. Five were re-run after a broken test baseline was fixed, so each result stands on its own test. They covered the hold in `tick`, `recover` and owner checks, the slot, the log limit, settling without verification, settling while a process lives, self-healing, both `problems` kinds, the scheduler filter, the status view, release settling a lease, a stashed lease, settling a healthy row and losing a published exit status.
- A mutant restoring the removed legacy exception passed every test. That confirms it was dead code.
- `exitedWorker` in `queue_held_test.go` could flake if its exited process ID is reused before the check runs. The chance is low, and the failure would read "could not be verified".

### Earlier commit `79060b5`

The refactor before this change had this validation. None of it was re-run on the current code:

- `bash scripts/validate.sh` did not pass locally. Queue job 2743 exited 1 at load around 13, on timeouts and timing failures in unrelated tests.
- The same scope in CI partitions (job 2765) passed, except `https` and `test-native.sh`. Those failures were in unchanged code and passed when run alone.
- All 20 hosted checks passed.

## Remaining work

Independent review of this commit comes next. These are outside this change:

- `arch/status-snapshot-contract` has no pushed branch yet. That work should include the optional `problems` field and the command `state` value `held`.
- `problems` messages are not rendered by the dashboard or the menu bar. Both show a held job through its state, phase and cancel action. A later change could display the message.
- The 0.5-second assertion in `native/tests/TransportTests.swift` can fail on a loaded host, and did once.
- A later refactor could move the `queue_settings` writes behind the store.

## Next steps

1. Review the draft pull request against this handoff, starting with "Behaviour worth reviewing".
2. Fix findings at their root and re-run the focused checks above.
3. Confirm hosted CI on the reviewed head.
4. Ask the owner to mark it ready and merge it. It is not merged or marked ready.
