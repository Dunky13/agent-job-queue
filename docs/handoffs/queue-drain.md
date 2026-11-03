# Queue drain, phase 1

## Result

`agent-heavy drain` closes intake without stopping the queue, so a steady stream of submissions can no longer keep it from emptying. Callers choose what happens to them during a drain with `agent-heavy run --on-drain wait|fail|direct`, and `wait` is the default. Attached callers survive a daemon gap while a drain is active, and return the job's real exit status with one execution.

The in-place upgrade flow becomes `agent-heavy drain`, Quit, drag, open, `agent-heavy undrain`. The commands and their exact behaviour are in the README under "Drain the queue".

## How it fits the run client

The run client's outage handling (`queue_outage.go`, `followWithToken`) is the only place that rides out a daemon outage. Drain adds one rule to it: while the drain holds, `queueOutage.hold` replaces `lose`, so the outage neither spends the budget nor ends the run, and the budget starts when the drain ends. A first interrupt during such an outage detaches at once with 128 plus the signal and never cancels; a finished job whose cleanup waits ends with the job's own exit status and a note. `Cancel` and `WaitCleanup` still do not retry. `cancel-queued` moves jobs through the store's own transitions (`markCancelled`, `recordOutcome`, `endLease`) in one pass under the daemon lock.

## Decisions

- Drain state is a private `drain.json` with an expiry, not a third queue status. Callers must be able to read it while no daemon runs, and a status would need a schema migration. Pause and drain stay independent.
- The daemon only reads the file. The dirty-restart pause is unchanged, and `undrain` provides the explicit resume.
- The default expiry is 4 h. Without `--for`, an active drain keeps its expiry, so an escalation such as `drain --cancel-queued` cannot shorten it.
- `--cancel-queued` runs as one daemon operation, so a job cannot start and then be killed mid-pass.
- `--on-drain direct` runs the command in its own session and is refused for a non-default lane, because lanes protect exclusive resources.

## Deliberately not in this change

These were reproduced in review. They predate drain and live in older safety code. Fixes attempted here broke `TestCleanupAfterConcurrentCompletion` and fail-closed ownership rules, so they were reverted and belong in focused PRs. The first two are also listed as known limitations in the README and `docs/upgrade-recovery.md`.

1. **Half-removed slot after SIGKILL.** A SIGKILL during slot teardown (`ownership.go`) can leave only `agent-heavy-lease.json` in the slot. After a restart, callers fail with `lstat .../agent-heavy-jobs` and later jobs do not run. Seen once in 44 SIGKILLs. Teardown needs to become crash-safe. An empty, complete slot left by a crash also blocks the setup gate with no receipt to drive recovery, so a repair must cover that window too, and must only ever touch a slot that is provably ours.
2. **Adaptive grant expiry.** The controller (`scheduler_controller.go:636`) deletes an expired grant even for a started worker still waiting for the shared slot, and the permit (`scheduler.go:447`) then stays false. Reproduce with adaptive mode, Start now, an external slot owner, a gap longer than the 30 s manual grant, then free the slot. The job waits until cancelled, with or without a drain. Likely fix: expire grants only for Queued or Stashed tasks and refresh them for started workers with no pgid.
3. **Socket mode window.** On daemon start the socket exists at a non-0600 mode for tens of microseconds, and a caller polling then exits 125 with "Unsafe queue socket". Bind to a short temporary name, chmod, then rename, keeping the path within the 104-byte limit.

## Remaining work

1. Phase 2: setup closes intake when it enters a drain wait, and **Stop waiting** reopens it.
2. Phase 3: app banner, a **Prepare for update** action, and dashboard display of `queue.drainUntil`. Neither the app nor the dashboard shows a drain today, so only `agent-heavy status` and the callers' notices reveal one.
3. A real app quit, drag and reopen with active jobs, on a build containing this change. Only private daemons have been exercised so far.
4. Older daemons ignore the drain file, so this cannot help the first transition from a build without it. Callers started before the upgrade ride out the outage for the two-minute budget of the run client and then exit 125, unless the queue store shows their job finished.
