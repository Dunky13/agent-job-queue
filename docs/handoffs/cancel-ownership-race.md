# Cancel ownership race

## Result

`agent-heavy cancel` could refuse a job that was finishing on its own: "Process group ownership cannot be verified; shared slot retained without signalling". It was seen in CI as an intermittent failure of `TestRealDaemonCancellationRemovesDescendants` (PR #74, run 41672401460, `macOS tests (macos-15-intel, core-1)`). `docs/handoffs/process-census.md` already recorded the same failure on main and the state behind it.

## Cause

The daemon's `signal` checks a worker record read earlier. That record names only the worker, taken when the attempt started. The worker publishes its outcome and then exits. A cancel repeated while the worker finishes (`Cancel` resends every 200ms until the job is Done) can read the row as running, then take its process listing after the worker has exited. A descendant that ignores SIGINT, here `sleep 60 &` in a non-interactive shell, keeps the group alive. The group then has a member, but none the record names, and `VerifyGroupIdentity` refuses.

How it is known:

- A capture of the failure in `docs/handoffs/process-census.md` showed the group holding only the `sleep` descendant while the recorded members had exited.
- The refusal cannot come from cleanup. `WaitCleanup` never returns the daemon's cleanup error and waits 12s, while the failure came 2.9s after the test began. So it came from the daemon's `cancel` reply.
- The focused test below reproduces the state and fails on unfixed code with `compared group N, recorded members [N@...], current members [N+1@...]`.

Ruled out:

- Birth instability. 400 full-table reads of a live group, four processes at once under `taskpolicy -c background`, never changed a birth. `lstart` single-pid reads (300 processes) did not either. Both ran on arm64 only.
- #62 changing the comparison. `NormalizeBirth` is applied to both sides, and the recorded `members` are written as before.
- The ordering "marker `members` written after the check": cleanup reads the marker, which the worker updates before it publishes the outcome, so cleanup is not where this fails.

## What a user sees

`agent-heavy cancel N` on a job that is ending, with a background child still running, can print the refusal and exit nonzero although the job then settles by itself. Repeating the command succeeds. Nothing is signalled on the refusal. After a daemon restart the same state in `tick` returned an error for that tick instead of waiting for the next one.

## Fix

- `VerifyGroupIdentity` ends in a named error, `errGroupOwnershipUnverified`, and its message now states the group, the recorded members and the current members it compared (`pid@birth`). A recurrence names its cause.
- `queueStore.settledAttempt` re-reads the row. If the same attempt and worker has published its outcome, the unverified group is no longer a reason to refuse: there is no worker to signal, and the settled job's cleanup, which uses the full marker, owns what is left.
- Used by `queueDaemon.signal`, the daemon `tick` for an adopted running row, and the three handover readers in `queue_handover.go` that already re-read the row when the group was gone. They now do so for the unverified error too.
- Not changed: a still-running row with no recorded member left keeps refusing, and nothing is ever signalled on unverified ownership. `Cleanup`, which uses the marker with all members, and `install_runtime_generations.go`, which retains on any error, are unchanged.

## Evidence

| Check | Result |
| --- | --- |
| `TestSignalFromAStaleRowAfterTheWorkerExited` without the fix | fails: the settled row's refusal, with the new comparison text |
| Same test with the fix | passes |
| Backend tests matching `Queue\|Handover\|Cancel\|Ownership\|Group\|Lease\|Signal\|Census\|Corrupt\|RealDaemon`, queue job | pass, 138s |
| Race-built `TestRealDaemonCancellationRemovesDescendants` plus the new test, 120 runs under `taskpolicy -c background` | pass |

Not reproduced: the intermittent failure of the real test. It was 1 in 390 on main under throttling with `-race`, so 120 passes do not show the fix removes it. The focused test shows the refusal state and that the fix handles it.

## Not checked

- The full backend suite and hosted CI on this branch.
- Intel hardware. The reproduction and probes ran on arm64.
- `managedHeldByApp` reading a healthy lock holder as unowned on Intel. No link was found: the CI failure with "An unowned queue process holds its lifetime lock" comes from `managedDrain` in `install_managed.go`, which inspects locks and does not use process group ownership.
- The `tick` change has no focused test, because `tick` reads its own rows and a stale snapshot cannot be forced without a hook.

## Remaining work

1. Hosted CI on this PR, especially both core shards on `macos-15-intel`.
2. If the real test fails again, the refusal text now names what was compared. A failure with a current member that is not a descendant of the worker would be a different cause.
