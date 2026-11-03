# Cancel ownership race

## Result

`agent-heavy cancel` could refuse a job that was finishing on its own: "Process group ownership cannot be verified; shared slot retained without signalling". It was seen in CI as an intermittent failure of `TestRealDaemonCancellationRemovesDescendants` (PR #74, run 41672401460, `macOS tests (macos-15-intel, core-1)`). `docs/handoffs/process-census.md` already recorded the same failure on main and the state behind it.

## Cause

The old error text named no members and the job log is gone, so the CI run cannot separate these two paths. Both end the same way: a worker record read earlier holds only the worker and its own `ps`, taken when the attempt started. A descendant that ignores SIGINT, here `sleep 60 &` in a non-interactive shell, keeps the group alive after the worker has gone. The group then has a member, but none the record names, and `VerifyGroupIdentity` refuses. `Cancel` resends every 200ms until the job is Done, so it keeps looking during this time.

1. **Stale read.** The worker publishes and exits, and a repeat cancel read the row as running before that, then lists processes after it.
2. **The repeat cancel kills the worker.** `executeWithAttempt` stopped handling SIGINT on return, before the outcome was published, so a resend in that gap killed the worker with nothing written. The row stays running until the daemon's wait settles it, and a cancel in that interval refuses. The job also reported the wrong status. An independent review forced this with injected delays and got the CI error text in 5 of 5 runs, and 0 of 5 with the handler held. A natural refusal also needs the daemon's goroutine to stall about 200 ms, and its natural rate was not measured.

What the evidence supports, for either path:

- A capture of the failure in `docs/handoffs/process-census.md` showed the group holding only the `sleep` descendant while the recorded members had exited.
- The refusal cannot come from cleanup. `WaitCleanup` never returns the daemon's cleanup error and waits 12s, while the failure came 2.9s after the test began. So it came from the daemon's `cancel` reply.
- The focused test below builds the state and fails on unfixed code with `compared group N, recorded members [N@...], current members [N+1@...]`. It does not show which path produced the state in CI.

Ruled out:

- Birth instability. 400 full-table reads of a live group, four processes at once under `taskpolicy -c background`, never changed a birth. `lstart` single-pid reads (300 processes) did not either. Both ran on arm64 only.
- #62 changing the comparison. `NormalizeBirth` is applied to both sides, and the recorded `members` are written as before.
- The ordering "marker `members` written after the check": cleanup reads the marker, which the worker updates before it publishes the outcome, so cleanup is not where this fails.

## What a user sees

`agent-heavy cancel N` on a job that is ending, with a background child still running, can print the refusal and exit nonzero although the job then settles by itself. Repeating the command succeeds. Nothing is signalled on the refusal. After a daemon restart the same state in `tick` returned an error for that tick instead of waiting for the next one.

## Fix

- `VerifyGroupIdentity` ends in a named error, `errGroupOwnershipUnverified`, and its message now states the group, the recorded members and the current members it compared (`pid@birth`). A recurrence names its cause.
- `queueStore.settledAttempt` re-reads the row. If the same attempt and worker has published its outcome, the unverified group is no longer a reason to refuse: there is no worker to signal, and the settled job's cleanup, which uses the marker, owns what is left.
- `executeWithAttempt` no longer stops its SIGINT handling on return, so a resend cannot kill the worker before it publishes. This closes the second path. A resend after the command has ended is discarded, which costs nothing because the command's status is already known. An earlier version of this fix registered a separate channel in `ExecuteAttempt`, before anything read it, and that lost an acknowledged cancel arriving before the command started. A review forced that with an injected 800ms delay: the command ran and exited 5, where it should have exited 130. The window measured 43-72ms over 8 runs, and it needs the client to be gone, because a live client resends every 200ms.
- Used by `queueDaemon.signal`, the daemon `tick` for an adopted running row, and the three handover readers in `queue_handover.go` that already re-read the row when the group was gone. They now do so for the unverified error too.
- Not changed: a still-running row with no recorded member left keeps refusing, and nothing is ever signalled on unverified ownership. `Cleanup`, which uses the marker with all members, and `install_runtime_generations.go`, which retains on any error, are unchanged.

## Evidence

| Check | Result |
| --- | --- |
| `TestSignalFromAStaleRowAfterTheWorkerExited` with only the `queue_daemon.go` change reverted (the test needs the named error from `ownership.go` to compile) | fails: the settled row's refusal, with the new comparison text |
| Same test with the fix | passes |
| Backend tests matching `Queue\|Handover\|Cancel\|Ownership\|Group\|Lease\|Signal\|Census\|Corrupt\|RealDaemon`, queue job | pass, 138s |
| `go build ./... && go vet ./...` | pass |
| Race-built `TestRealDaemonCancellationRemovesDescendants` plus the new test, 120 runs under `taskpolicy -c background`, at `2a5f930`, before the second path was closed | pass |
| `TestRealDaemonWorkerSurvivesInterruptsWhilePublishing` with the handling stopped on return | fails: the job reports 125 instead of the command's 7 |
| `TestSettledAttemptKeysOnTheSameAttempt` with each part of the key, or either settled phase, removed | fails |

Not reproduced: the intermittent failure of the real test. It was 1 in 390 on main under throttling with `-race`, so 120 passes do not show the fix removes it. The focused test shows the refusal state and that the fix handles it.

## Not checked

- A worker that dies before publishing for any other reason, such as SIGKILL or a failed write, leaves the row running until the daemon's wait settles it, and a signal landing before that still refuses. A worker that died early, before its post-command census, also leaves a marker with only its initial members, so cleanup may retain the slot.
- After a restart, an unsettled row in this state makes `tick` return at that row every time, stopping later rows and admission. This predates the change and is read from code only. It needs a worker to die without publishing while the daemon is down.
- The full backend suite and hosted CI on this branch.
- Intel hardware. The reproduction and probes ran on arm64.
- `managedHeldByApp` reading a healthy lock holder as unowned on Intel. No link was found: the CI failure with "An unowned queue process holds its lifetime lock" comes from `managedDrain` in `install_managed.go`, which inspects locks and does not use process group ownership.
- A regression test for the lost cancel before the command starts. The window cannot be forced without a production hook, so nothing pins the registration staying in `executeWithAttempt`.
- The `tick` change and the three handover readers have no focused test for this path, because a stale snapshot cannot be forced without a hook. Removing the check at any of them passes every test.

## Remaining work

1. Hosted CI on this PR, especially both core shards on `macos-15-intel`.
2. If the real test fails again, the refusal text now names what was compared. A descendant as the only current member fits a worker that died before publishing or a stale read, so check which. A current member that is not a descendant of the worker would be a different cause.
