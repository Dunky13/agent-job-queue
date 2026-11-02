# Run client survives queue daemon transport failures

## Result

`agent-heavy run` no longer exits 125 when one queue call times out while it follows a submitted job. The job lives in the daemon and keeps running; only the client lost sight of it, and an agent reading 125 would conclude the job failed and may submit it again.

## Design: one place retries

Retrying happens in exactly one place, the follow loop (`followWithToken`), plus `agent-heavy lane wait`. Everything else reports a lost call once and truthfully, and the follow loop decides what to do.

- **One definition of recovery:** an iteration with no transport failure anywhere (the read, the held-interrupt cancellation, and any cleanup it triggers). A read that answers while a `cancel` or `kill` is lost is not recovery, so a daemon that answers snapshots but loses mutations reaches the bound.
- **One budget:** a continuous outage may last `queueOutageWindow`, two minutes, plus the one call in flight when it ends (a call times out after 15 s). The window covers the app's 10 second restart throttle (`AgentSupervisor.swift`) several times over plus startup on a slow host. Backoff is 0.4 s doubling to 3 s. A second interrupt is observed between calls, so it can take up to one call (15 s) to act.
- **`Cancel` does not retry** (used by `agent-heavy cancel`, the signal path and installer preflight). It returns at once with `Cancellation of job N could not be confirmed; the job may still be running; inspect it with: agent-heavy logs N`, wrapping the real cause. It never claims "not delivered" or "unreachable", because a lost reply can hide a delivered signal and snapshots may be answering. A waiting client retries it within its own budget. Trade-off: a standalone `agent-heavy cancel` on a briefly slow daemon fails once instead of retrying; the operator re-runs it, which is safe because cancellation is idempotent. This removed the nested outage state, the deadline refund, the interruptible sleeps and the three-phase composition (measured earlier at about 3 times the bound).
- **`WaitCleanup` does not retry** either. If the only problem is that the daemon does not answer the cleanup probe, it returns a transport error and the follow loop returns the job's real exit status with a stderr note. A daemon that answers but leaves the receipt behind is an anomaly, not an outage, and stays 125.
- **Grace:** `kill` follows only a `cancel` the daemon acknowledged in the same call, after the 3 s grace. A lost reply never starts the clock.
- **Identity:** the daemon's task token is checked on every read in the follow loop and in `Cancel` (standalone `Cancel` fixes the token on its first read), before any mutation.
- **Real result wins:** a job observed `Done` is reported with its own exit status, with a note when an interrupt was held ("had already finished, so the interrupt did nothing", or "finished ... while its cancellation was being confirmed" if a cancel had been sent). At the bound, the client reads the terminal outcome from the queue store read-only (`queueReadHandoverStore`: `mode=ro`, no migration, no write, no daemon lock), only for the exact submitted token and only for `done` or `outcome` phases.
- **Interrupts:** the first interrupt during an outage is held and announced; a second ends the wait with `... the cancellation could not be confirmed ... inspect it with: agent-heavy logs N`. At the bound the message says the cancellation requested by the interrupt could not be confirmed.
- **Paused queue:** after a loss, a client whose job is queued checks the group once on recovery, again if that check was lost, and on the 15 s tick. It prints one notice per run, `the queue is paused; job N will not start until the queue is resumed`, with no cause and no command.
- **Notices:** loss and recovery are printed at most once per 30 s, so a flapping daemon stays quiet. During a handover the notice says so instead of claiming the daemon is not responding.
- **Also:** `QueueControl` honours `ControlDeadline` before looking for the socket (a missing socket no longer defeats the installer's 20 s deadline). A reply over 32 MiB fails once instead of being retried. `lane wait` says "lease" and reports whether the withdrawal on interrupt worked.

## Unchanged

Non-transport errors fail at once as before. The handover path (`queueHandoverFollowTask`) is consulted first. Submission still reconciles by token with two immediate retries. The command is never run directly. The daemon server code and its 100 ms tick are untouched.

## Known gaps, out of scope

- A worker waiting for the shared slot exits 125 on a transport failure outside handover custody (`execution.go:709`; `queue_handover_test.go` `missing-custody` requires it). Resubmitting is safe.
- A retained `consumed` handover receipt ends a client immediately (`queue_handover.go:910-916`). No non-test code removes `run/handover.json`; PR #63 or later work may affect it.
- Between `ListenUnix` and `Chmod` the socket has mode 0700 (`queue_daemon.go:244-250`), so a retrying client can land in that window and exit 125. The socket check must not be relaxed.
- `lane acquire` retries submission immediately, so a refused socket exhausts its two retries at once.
- Why the daemon becomes slow under memory pressure.

## Validation

Locally, directly, with `-race` on the new tests; the full package is left to hosted CI because of host memory pressure. See the pull request for the exact commands and results.
