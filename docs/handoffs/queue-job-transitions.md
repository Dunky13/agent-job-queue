# Queue job transitions

## Result

The queue store now owns job state transitions, in `internal/backend/queue_store_transitions.go`. There is no schema or handover protocol change. A healthy queue behaves as on main, and its snapshot is byte-identical to main's. Corrupted rows are held to "no worse than main", with the known limits listed below.

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

## Corrupted rows: no worse than main

**Decision history:** the first version refused such a row and let it stop the queue. The second held only that row. The third settled the row and logged it, and claimed a corrupted row could never stop the queue. Review showed that claim was false, and it is retracted. **This change's bar is no worse than main on corrupted rows, plus the listed fixes.** No writer in any release can produce such a row, and independent review found 0 of 2,772 live rows rejected.

The daemon's own settling path is the only thing allowed to overwrite a corrupted row, through three narrow store methods that the file header names as the deliberate exception:

| Method | Stands in for | Used by |
| --- | --- | --- |
| `overwriteOutcome` | `recordOutcome` | `finish`, for `recover`, `tick`, `cancel` and a finished child |
| `overwriteLeaseEnd` | `endLease` | `endLease` in the daemon: TTL, owner exit, cancel, release and an unreadable status |
| `overwriteCancelled` | `markCancelled` | `cancel` and `kill`, so the settled result still reads as cancelled |

Each keeps the guards of the write it stands in for, and refuses a healthy row or a finished one.

**What the daemon settles:**

- **`recover`:** a `starting` row in any state records 125 and startup proceeds, including a lease at `starting`, which no lease edge covers.
- **`tick`:**
  - A corrupted `running` row verifies its group identity as main does, and settles when no owned process remains. A verified live process is left alone.
  - A corrupted `outcome` row retires. It keeps its exit status from the outcome column, or from a readable result in its status. It gets 130 if cancelled, and 125 only if neither gives a code.
  - A `queued` row, or a `starting` row with no live child, settles with 125 if its status is unreadable.
  - A lease with an unreadable status ends with reason `corrupted`.
- **`LeaseCapacity`:** an unreadable row no longer fails every job with 125.
  - It adds nothing to capacity, which is `max(limit, readable admitted rows)`.
  - That errs on the safe side because counting a row *raises* this ceiling, so counting an unreadable one could let extra jobs in.
  - A row with a verified live worker still counts against the ceiling through its slot member, which `AcquireLease` compares with capacity. Its slot is also kept by `tick`, which keeps counting that row while the worker lives.
  - It runs in the worker, whose output goes to the job's own log, so it logs nothing.
- **`pause-job` and `resume-job`:** the store's check now runs before the signal, so a refusal sends nothing.

**Logging:** one line per row per daemon run, with the job ID, phase and status, each row with its own entry. It is written when the daemon settles a row or can't verify its worker. **Rows the daemon doesn't touch are never logged:**

- `done` rows.
- `queued` rows reading a readable wrong status.
- `starting` rows reading a readable wrong status, until a restart settles them.
- Leases reading a readable wrong status, until they end.
- A corrupted row with a verified live worker or a live child, until it settles.

### What a corrupted row can still do

Each of these matches main unless noted. They are listed under "Remaining work":

- **Stop the tick for every job:** undecodable task JSON, which stops every read of unfinished jobs and prints an error every 100 ms. An agreeing `running` row with an unverifiable worker also does this.
- **Stall adaptive mode:** `schedulingPermit` and `heldLeases` still fail on an unreadable row, so a job in adaptive mode gets 125. A corrupted row that reads as waiting can end a scheduling pass.
- **Hold a slot:**
  - A corrupted `running` row whose worker can't be verified stays until it can be, and survives restart. Main stopped the tick here.
  - A `starting` row reading a readable wrong status, with no child, stays until a restart or `cancel`.
  - A lease reading a readable wrong status stays until TTL, owner exit or release.
  - At limit 1 a held slot blocks every other job that shares the limit.
- **Break `agent-heavy status`:** `StatusData` fails while any row has an unreadable status. That is permanent for a `done` row, and lasts while a live worker's row exists.
- **Break reordering:** `next` and `last` fail while any row has an unreadable status.
- **Resist `Cancel()`:** see Defects 2 and 3 under "Remaining work".

### Differences from main

A scratch scenario using raw SQL and shared APIs ran on both main (`7b4b27c`) and an earlier commit of this branch, covering `recover`, `tick`, lease TTL, a healthy snapshot and `Cancel()` with live processes. It did not cover every row shape, and it was not re-run after these fixes. These are all the outcome differences known so far:

| Row | Main | This branch |
| --- | --- | --- |
| `starting` with an unreadable status, in `recover` | refuses startup | records 125 and starts |
| `starting` with an unreadable status and no child, in `tick` | keeps its slot | records 125 |
| `queued` with an unreadable status, in `tick` | stops the tick | records 125 |
| lease with an unreadable status | left alone unless expired, then stops the tick | ends with reason `corrupted` |
| `outcome` with a bad status | reaches `done` with that bad status | status rewritten to agree; reaches `done` with its exit status |
| `outcome`, no outcome column, status `Done` | reaches `done` with no column | column filled from the result, which is kept; its end time is rewritten |
| `running`, disagreeing, worker cannot be verified | stops the whole tick | stays unsettled and is not signalled; the tick continues |
| any unreadable row, in `LeaseCapacity` | fails every job with 125 | skipped |
| `cancel` on a `done` row reading `Queued` or `Running` | `Cancel()` times out after 12 s | fails at once with an illegal-transition error |
| lease with no lease object, granted or ended | panics the daemon | no panic |

**Stricter than main by design.** Each of these affects only that row's own request. None changes how other rows are dispatched in fixed or shadow mode:

- `claimWorker` refuses, so the command doesn't run. Main ran it. The worker exits 125 and the daemon settles the row.
- A worker can't publish its own final outcome. The daemon records the child's exit status, or 125 or 130 once no owned process remains.
- `priority`, `next`, `last` and `lease-renew` go through `saveTask` and return the disagreement error, where main wrote the field.
- `pause-job` and `resume-job` return an illegal-transition error and send no signal on a row outside `starting` or `running`. Main signalled and changed the status.
- `hold` and `enqueue` on a corrupted row do nothing, where main could change the status.

`start` keeps main's result: `Job changed before launch` for a command, and `errQueueChanged` for a lease. A cancel whose signal fails leaves the cancel flag set, and `SIGCONT` goes only to a row reading `Paused`, both as on main.

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
- `queue_corrupted_test.go` covers the daemon through `recover()`, `tick()` and the real client `Cancel()`. `TestRealDaemonJobRunsPastUnreadableRowAtLimitOne` submits a real job at limit 1 after an unreadable row in each phase, in both lease phases, and with a verified live worker. Its healthy-snapshot golden was captured from main.

These raw SQL fixtures stay on purpose:

- Deliberate corruption, in the corrupted-row, runtime generation and edge tests.
- `TestRealDaemonUnclaimedAttemptFailsWithoutReplay`. Its daemon is live, so a queued row could be dispatched before a real claim.
- Bulk history in the operational and benchmark fixtures.
- Fixtures with no store route, such as a changed attempt on a running row, and installer rows that only need to exist.

## Evidence

The branch is rebased on `origin/main`, which added the #61 `go.mod` and `go.sum` bump. All results below are on that base. The host is under memory pressure and `scripts/validate.sh` is being fixed separately, so **no full batch ran**, and hosted CI is the only broad check.

- `bash scripts/lint.sh`, `go vet ./internal/backend/` and `node --test remote/static/schema.test.mjs` each exited 0.
- `go test -p 2 -parallel 2 -race ./internal/backend -run 'Corrupted|TestLeaseCapacity|TestPauseRefusal|TestRealDaemonJobRunsPast|TestHealthySnapshot|TestQueueControlHold|TestQueueStoreStatus|TestQueueStoreOverwrite|TestLegacyHistory|TestQueueTransition|TestQueueEdge|TestQueueCleanup|TestLease' -count=1` exited 0 in 62s.
- `go test -p 2 -parallel 2 ./internal/backend -run 'TestRealDaemon|Handover|TestQueue|TestLease|TestLane|TestRuntimeGeneration|Drained|Drain|TestLegacy|Corrupted|TestHealthySnapshot|Scheduling|TestScheduler|Supervised|DragDrop|TestManaged.*Stopped|Ownership|Slot|Shared' -count=1` exited 0 in 150s.

**Mutants for these fixes:** 10 temporary mutants each failed the tests, and were reverted. They ignored an unreadable lease, ignored an unreadable `starting` row, settled one with a live child, failed the worker in `LeaseCapacity`, counted an unreadable row as capacity, ignored a readable result, ignored the outcome column, ignored the cancel flag, signalled before the pause check, and removed the store's pause check. Two first failed only to compile and were rewritten so that they really ran.

**Earlier mutants, not re-run:** 26 mutants at the previous commit failed the tests. 6 of those fail on `Cancel()` subtests. None of the 10 above exercises `Cancel()`.

**Known test risk:** `exitedWorker` could flake if its exited process ID is reused before the check runs, though the chance is low.

**Earlier commit `79060b5`, not re-run:** `validate.sh` (job 2743) did not pass locally, on timeouts and timing failures in unrelated tests under load. The CI-partition batch (job 2765) passed except `https` and `test-native.sh`, whose failures were in unchanged code and passed alone. All 20 hosted checks passed.

## Remaining work

Independent review of this commit comes next. These are follow-ups, not fixed here. Each matches main:

- **The most realistic of these:** an agreeing `running` row with an unverifiable worker stops the whole tick. A reboot with a reused process group could reach this without any corrupted write.
- **Defect 2:** `Cancel()` returns nil without settling a lease at `running` reading `Done`. Only `lease-release` clears it.
- **Defect 3:** `Cancel()` can't clear a `starting` or `queued` row reading `Done` or an unknown name.
- **Defect 4:** adaptive mode stalls on a corrupted row, and can exceed the limit with a held lease reading `Stashed` or `Done`, because `heldLeases` counts only `Running`.
- `StatusData` fails on an unreadable row.
- `next` and `last` fail when any row is unreadable.
- A `Cleanup` that fails permanently stalls in silence, because `d.errors` is written and never read.
- A `done` row with a bad status fails `StatusData`. `LeaseCapacity` no longer fails on an unreadable one, but `LeaseCapacity` and `StatusData` still read a readable wrong status at face value.
- The 0.5-second assertion in `native/tests/TransportTests.swift` can fail on a loaded host.
- A later refactor could move the `queue_settings` writes behind the store.

**Accepted, not a follow-up:** a disagreeing `running` row with an unverifiable worker takes a slot indefinitely and survives restart, by fail-closed ownership.

## Next steps

1. Review the draft pull request against this handoff.
2. Fix findings at their root and re-run the focused checks above.
3. Confirm hosted CI on the reviewed head.
4. Ask the owner to mark it ready and merge it. It is not merged or marked ready.
