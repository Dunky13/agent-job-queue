# Queue job transitions

## Result

The queue store now owns job state transitions, in `internal/backend/queue_store_transitions.go`. There is no schema or handover protocol change. A healthy queue behaves as on main, and its snapshot is byte-identical to main's.

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

An illegal edge returns `errQueueIllegalTransition`, and a row whose phase and status disagree returns `errQueueStateDisagreement`. Both fail before any write. Each statement keeps its original `WHERE` guards. `importLegacy` is the one other writer, inserting drained history.

## Corrupted rows: settle it, and log it

**Decision history:** the first version refused such a row and let it stop the queue. The second held only that row. Having seen the cost of that machinery, the owner decided to settle the row as main does and log it. No writer in any release can produce such a row, and independent review found 0 of 2,772 live rows rejected.

A corrupted row can never stop the queue, refuse startup or stall other jobs. The daemon's own settling path is the only thing allowed to overwrite one, through three narrow store methods that the file header names as the deliberate exception:

| Method | Stands in for | Used by |
| --- | --- | --- |
| `overwriteOutcome` | `recordOutcome` | `finish`, for `recover`, `tick`, `cancel` and a finished child |
| `overwriteLeaseEnd` | `endLease` | `endLease` in the daemon: TTL, owner exit, cancel and release |
| `overwriteCancelled` | `markCancelled` | `cancel` and `kill`, so the settled result still reads as cancelled |

Each keeps the guards of the write it stands in for, and refuses a healthy row or a finished one. The review asked for one method; three were needed because each writes different columns.

**What the daemon does:**

- **`recover`:** a `starting` row in any state records 125 and startup proceeds. That includes a lease at `starting`, which no lease edge covers.
- **`tick`:**
  - A corrupted `running` row verifies its group identity as main does, and settles when no owned process remains. A verified live process is left alone.
  - A corrupted `outcome` row keeps its published exit status and retires.
  - A `queued` row with an unreadable status settles with 125.
- **`Cancel()`, the client path:** behaves as on main. It settles the row, or returns an error if it can't; a live process is never reported as cancelled.
- **Logging:** one line per row per daemon run, with the job ID, phase and status. `daemon.log` is bounded only at spawn, so this can't flood it, and each row has its own entry so one row can't hide another. The first report stands.

### Compared with main

A scratch scenario using raw SQL and shared APIs ran on both main (`7b4b27c`) and this branch, covering `recover`, `tick`, lease TTL, a healthy snapshot and `Cancel()`, including live processes. Only these outcomes differ. All three follow from the decision that a corrupted row can never stop the queue:

| Row | Main | This branch |
| --- | --- | --- |
| `starting` with an unreadable status | `recover` refuses startup | records 125 and starts |
| `outcome` with a bad status | reaches `done` with that bad status | status rewritten to agree; reaches `done` with its exit status, or 125 |
| `running`, disagreeing, worker cannot be verified | stops the whole tick | stays unsettled and is not signalled; the tick continues |

Beyond that scenario, a corrupted lease row with no lease object panics the daemon on main. Here it no longer does.

### Stricter than main, by design

The strict writes stay strict, so these differ on a corrupted row. None can stall other jobs:

- `claimWorker` refuses, so the command doesn't run. Main ran it. The worker exits 125 and the daemon settles the row.
- A worker can't publish its own final outcome. The daemon records the child's exit status, or 125 or 130 once no owned process remains.
- `priority`, `next`, `last` and `lease-renew` go through `saveTask` and return the disagreement error, where main wrote the field.
- `pause-job` and `resume-job` on a queued row reading `Running` or `Paused` return an illegal-transition error, where main changed the status.
- `hold` and `enqueue` on a corrupted row do nothing, where main could change the status.

`start` keeps main's result: `Job changed before launch` for a command, and `errQueueChanged` for a lease.

### Left as main

- **Undecodable task JSON:** still stops every read of unfinished jobs, and the tick error still prints every 100 ms.
- **A lease settled at `starting`:** stays at `outcome` afterwards, as on main. It holds no slot, but without a TTL it stays unfinished and blocks live handover.
- **A corrupted `starting` row during a run with no child process:** keeps its slot until a restart or `cancel`, as on main.
- **Adaptive scheduling:** a corrupted row that reads as waiting can still be selected, and the refused `start` ends that pass, as on main. Scheduler changes were removed on request.
- **Findings 12 and 17:** a cancel whose signal fails leaves the cancel flag set, and `SIGCONT` is sent only to a row reading `Paused`. Both match main, so both stay.

Finding 14 is fixed, because the settling path depends on it: an unreadable status now satisfies `errors.Is(errQueueStateDisagreement)`.

## Where the checks are not strict, and why

- **Reads do not validate.** `job`, `jobs` and `snapshot` must still show a corrupt row to an operator.
- **Agreement covers phase, status and outcome, not attempt or worker.** A granted lease has neither. `validateQueueHandoverJobState` keeps its stricter checks and error text, and takes agreement from the same rule.
- **Legacy history is not validated.** `importLegacy` retains completed rows without an outcome column. `done` has no outgoing edge and reads never validate, so nothing refuses or settles such a row.
- **`starting` accepts `Paused`.** Pausing between launch and the worker's claim is a real sequence.
- **Two stale results stay silent at the caller.** `retire` in the cleanup goroutine and the worker's final `recordOutcome` had no affected-row check on main, so those two callers ignore `errQueueChanged`.
- **Raw settings queries stay raw.** `queue_settings` updates and `PRAGMA user_version` are not job state.

`RowsAffected` errors are handled one way in `swapRow`. The pinned `modernc.org/sqlite` driver never returns one.

## Tests

- `queue_store_transitions_test.go` covers the store's interface on temporary SQLite: both lifecycles, every kind, phase and method combination, stale views, disagreeing rows and atomic enqueue.
- `queue_corrupted_test.go` covers the daemon through `recover()`, `tick()` and the real client `Cancel()`, and runs a real daemon end to end. Its healthy-snapshot golden was captured from main.

These raw SQL fixtures stay on purpose:

- Deliberate corruption, in the corrupted-row, runtime generation and edge tests.
- `TestRealDaemonUnclaimedAttemptFailsWithoutReplay`. Its daemon is live, so a queued row could be dispatched before a real claim.
- Bulk history in the operational and benchmark fixtures.
- Fixtures with no store route, such as a changed attempt on a running row, and installer rows that only need to exist.

## Evidence

Results are for the commit that added the settling path. The host is under memory pressure and `scripts/validate.sh` is being fixed separately, so **no full batch ran**, and hosted CI is the only broad check.

- `bash scripts/lint.sh` and `go vet ./internal/backend/` passed.
- `go test -p 2 -parallel 2 -race ./internal/backend -run 'Corrupted|TestHealthySnapshot|TestQueueControlHold|TestQueueStoreStatus|TestQueueStoreOverwrite|TestLegacyHistory|TestQueueTransition|TestQueueEdge|TestQueueCleanup|TestLease' -count=1` passed in 30s.
- `go test -p 2 -parallel 2 ./internal/backend -run 'TestRealDaemon|Handover|TestQueue|TestLease|TestLane|TestRuntimeGeneration|Drained|Drain|TestLegacy|Corrupted|TestHealthySnapshot|Scheduling|TestScheduler|Supervised|DragDrop|TestManaged.*Stopped' -count=1` passed in 155s.
- `node --test remote/static/schema.test.mjs` passed all 8 tests.
- The main comparison above gave 36 rows on this branch. The 976-byte healthy snapshot matched main's exactly.

**Mutants:** 26 temporary mutants against the real paths each failed the tests, and were reverted.

- **Failing on `Cancel()` subtests, confirmed by running only those subtests (6):** not settling an outcome, dropping the cancel flag, refusing the flag on a corrupted row, not settling a lease end, settling while a process lives, and the missing lease object panic.
- **Failing through `tick()` and `recover()` only (11):** the worker verification rules, outcome retirement and its exit status, the unreadable queued row, the log rules, the lease at `starting`, and the raw agreement error. Settling an unverifiable row fails a test that mixes `tick()` and `Cancel()`, so it is counted here.
- **Failing at the store or control level only (9):** the overwrite guards, `markCancelled` strictness, the stashed lease, the added snapshot field, and the `start`, `hold` and lease grant results. None of these covers `Cancel()`.
- One mutant first failed only to compile. It was re-run so that it really executes.

**Known test risk:** `exitedWorker` could flake if its exited process ID is reused before the check runs, though the chance is low.

**Earlier commit `79060b5`, not re-run:** `validate.sh` (job 2743) did not pass locally, on timeouts and timing failures in unrelated tests under load. The CI-partition batch (job 2765) passed except `https` and `test-native.sh`, whose failures were in unchanged code and passed alone. All 20 hosted checks passed.

## Remaining work

Independent review of this commit comes next. These existing problems are out of scope and are not fixed here:

- A `Cleanup` that fails permanently stalls in silence, because `d.errors` is written and never read.
- An agreeing `running` row with an unverifiable worker stops the whole tick.
- A `done` row with a bad status fails `StatusData` and `LeaseCapacity`.
- The 0.5-second assertion in `native/tests/TransportTests.swift` can fail on a loaded host.
- A later refactor could move the `queue_settings` writes behind the store.

## Next steps

1. Review the draft pull request against this handoff.
2. Fix findings at their root and re-run the focused checks above.
3. Confirm hosted CI on the reviewed head.
4. Ask the owner to mark it ready and merge it. It is not merged or marked ready.
