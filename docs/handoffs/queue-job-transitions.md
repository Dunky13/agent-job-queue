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
- The worker's final `recordOutcome` now reports a disagreeing row in its returned error, where the old blind `UPDATE` wrote over it.

`RowsAffected` errors are handled one way in `swapJob`. Some sites used to fold them into their stale error. The pinned `modernc.org/sqlite` driver never returns one, so that path cannot be reached.

## Tests

`queue_store_transitions_test.go` covers the store's interface on temporary SQLite: both lifecycles, every kind, phase and method combination, stale views with their exact errors, disagreeing rows and atomic enqueue.

Raw SQL that only simulated a transition now uses store calls in `queue_store_edge_test.go`, `queue_daemon_test.go` and `queue_handover_test.go`. These raw fixtures stay on purpose:

- Deliberate corruption, in the runtime generation and edge tests.
- `TestRealDaemonUnclaimedAttemptFailsWithoutReplay`. Its daemon is live, so a queued row could be dispatched before a real claim.
- Bulk history in the operational and benchmark fixtures. The operational fixture's `outcome` row has a `Queued` status, which `retire` correctly refuses.
- Fixtures with no store route, such as a changed attempt on a running row, and installer rows that only need to exist.

## Evidence

All code results are for code commit `79060b5`. Later commits on this branch change only this file.

**`bash scripts/validate.sh` did not pass locally.** Queue job 2743 exited 1 with load around 13, so it does not count as a pass:

- `internal/backend` and `internal/tooling` each hit the 12-minute test timeout. The backend alarm fired 13 seconds into an unrelated installer test.
- Three `internal/remote` tests failed and `TestAppleSigningCleansUpAfterSignals` failed.
- No test from this change failed, and the log has no `DATA RACE`.
- `set -e` stopped the batch before its web, browser, native, app, installer and DMG steps.

**The same scope, split into the repository's CI partitions (queue job 2765), found one more failure and the `https` failure again:**

| Step | Result |
| --- | --- |
| `test-go-runtime.sh core-1` | pass, 728s |
| `test-go-runtime.sh core-2` | pass, 733s |
| `test-go-runtime.sh installer` | pass, 489s |
| `test-go-runtime.sh https` | **fail**, the same three `internal/remote` tests |
| `test-go-tooling.sh 1` and `2` | pass, 706s and 658s |
| `packaging/homebrew`, Go build and both Linux builds | pass |
| `npm run test:web` and `test:browser` | pass |
| `test-native.sh` | **fail**, exit 133 in `TransportTests` |
| `build-app.sh`, `test-installer.sh` and `test-dmg.sh` | pass |

The batch's scope matches `validate.sh` after its lint and vulnerability steps, which passed in job 2743.

**Why the two local failures do not come from this change:**

- `internal/remote` does not import `internal/backend`, and neither it nor `go.mod` changed. Its three tests pass when run alone with `-race`, in 114s at load around 7. As part of the whole package, `remote configure` is killed at its 35-second limit.
- `native/` and `scripts/` did not change. The failing `TransportTests` assertion requires a read to finish within 0.5 seconds of wall-clock time. It is the last line of `test-native.sh`, and a direct run of that binary passed 5 of 5 times with all 58 checks, at load around 10.

A baseline run of `validate.sh` on main was not taken. A retest of the full native script (job 2784) was cancelled while still queued.

**Hosted CI:** all 20 checks pass on `79060b5`, including `ci-required`, the Apple Silicon and Intel partitions, and native tests. `Lint` and the Apple Silicon `https` job first failed with `The job was not acquired by Runner of type hosted` and passed when rerun.

**Other checks:**

- `go test -p 2 -parallel 2 ./internal/backend -run 'TestRealDaemon|Handover|TestQueue|TestLease|TestLane|TestRuntimeGeneration|Drained|TestLegacy' -count=1` passed.
- Eight temporary mutants that removed a guard or check, or moved a side effect, each failed the tests and were reverted. They covered the kind check, both agreement checks, the launch error, the affected-row check, the cleanup tolerance, enqueue ordering and the illegal-edge check.
- A ninth mutant that wrote before rejecting an edge also failed the tests, and was reverted.
- The new tests add about 14 seconds to the backend suite, measured with `-race` on the loaded host.

## Remaining work

Independent Standards and Spec review of the draft pull request comes next. These are findings outside this change, and neither is fixed here:

- `validate.sh` runs the whole backend under one 12-minute limit, but its three CI partitions took 1,713 seconds of test time here, and about 15 minutes on hosted Apple Silicon. That limit may not hold on main either, and no baseline was run to confirm it.
- The 0.5-second assertion in `native/tests/TransportTests.swift` can fail on a loaded host, and it did once here. Swift is out of scope for this change.

A later refactor could move the `queue_settings` writes behind the store.

## Next steps

1. Review the draft pull request against this handoff.
2. Fix findings at their root and re-run the validation above.
3. Decide whether the `validate.sh` limit and the transport assertion need their own fixes.
4. Ask the owner to mark it ready and merge it. It is not merged or marked ready.
