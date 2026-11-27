# A paused queue passes commands through

## Result

A paused queue no longer blocks `agent-heavy run`. Before, a run on a paused queue exited 125 with `Dedicated default queue must be Running`, and a caller whose job was already queued waited until someone resumed.

## Behaviour

- **New run, paused queue:** a default-lane command runs outside the queue through `runDirect`, the same path as `--on-drain direct`. It runs in its own session with the queued environment, and the exit status passes through. This applies at precheck, after a drain wait that ends while paused, and when the daemon refuses the submit with the coded `queue-paused` error (`errQueuePaused`). The daemon looks up the token before it refuses, so no job exists.
- **Queued caller, queue paused:** the follow loop reads the default group with the task (`taskWithQueue`).
  - When the job is Queued or Stashed and the queue is Paused, the caller sends the daemon `withdraw` op with its token. The op works only on a paused queue, for a phase-`queued` default-lane non-lease job with the same token, and it runs under the daemon lock, so nothing can claim the job in between.
  - It marks the job `withdrawn` and `cancelled` and finishes it with 130.
  - The caller runs its command when it reads `Done` plus `withdrawn`. It also does so through the queue store fallback (`queueStoredOutcome` reports `withdrawn`) when the reply was lost and the daemon stays away.
  - An interrupt that came first, including one not read yet, returns the signal status instead.
  - A failed withdraw is reported once per run, and the caller keeps waiting.
- **Unclean restart pause:** this passes through too, as the owner decided. Orphaned jobs from before the restart are not waited for.
- **Lanes and leases:** unchanged. A lane run on a paused queue still exits 125, and queued lane jobs and leases wait, because lanes protect exclusive resources.

## Evidence

- Focused tests:
  - `TestRealDaemonPausedQueuePassesCommandsThrough`: real daemon. A queued caller withdraws and runs, a new caller runs direct, the running job is untouched, and withdraw is refused for a running queue or a started job.
  - `TestWithdrawEndsOnlyItsCallersWaitingDefaultJobOfAPausedQueue`: refusals for wrong token, lane, lease, started, unknown or twice; withdraws a queued and a held job.
  - `TestFollowRunsTheCommandWhenTheWithdrawReplyIsLost` and `TestFollowRunsAWithdrawnCommandReadFromTheStore`: lost reply, and the interrupted variant.
  - `TestRunInLaneOnDrainRaceAppliesThePolicy`: subtests for the paused precheck, the paused submit refusal and the lane refusal.
  - The updated `TestRealDaemonCrashRetainsOwnershipAndPausesRestart` and `TestInstallerManagedLiveResumeRetainsRunningAttempts`.
- The installed queue was paused during development. Tests ran directly with `-p 2` and a private `GOCACHE`, because the shared cache was being trimmed by another process.

## Test changes

- `TestRealDaemonCrashRetainsOwnershipAndPausesRestart` now expects the waiter to withdraw and run its command once after the restart pause.
- The queued variant of `TestInstallerManagedLiveResumeRetainsRunningAttempts` keeps its job queued across the handover with the blocker instead of a pause. Its current-release caller would withdraw from a paused queue. Production handovers only finish journals that older releases left behind, and those callers cannot withdraw. Pause preservation across a handover stays covered by `TestRealDaemonHandoverFreezesBeforeAcceptanceAndPreservesSettings`.

## Known gaps

- A newer client talking to an older daemon has no `queue-paused` code and no `withdraw` op. It keeps the old behaviour (exit 125, or waiting) until the daemon is upgraded.
- If a withdrawn job is pruned from history before its caller reads it again (only possible during a long outage), the caller exits 125 with "disappeared" and does not run the command.
- The app banner still says "Queue paused. Waiting jobs will not start automatically." That remains true for queue dispatch, so it was not changed.
- History shows a withdrawn job as finished with 130 and `withdrawn: true`. Status output does not label it differently.
