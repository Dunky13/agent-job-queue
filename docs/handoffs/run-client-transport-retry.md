# Run client survives queue daemon transport failures

## Result

`agent-heavy run` no longer exits 125 when one queue call times out while it follows a submitted job. The job lives in the daemon and keeps running; only the client lost sight of it, and an agent reading 125 would conclude the job failed and may submit it again. The follow loop, the finished-job cleanup wait, cancellation and `agent-heavy lane wait` now keep trying through a transport failure (`errQueueTransportUnavailable`), print one stderr notice on loss and one on recovery, and keep draining the job log meanwhile.

## Bound

A continuous outage may last `queueOutageWindow`, two minutes. A stopped daemon returns the same transport error. The menu bar app restarts it after at most a 10 second throttle (`AgentSupervisor.swift`), and startup adds the database open, so two minutes covers several restart attempts on a slow, swapping host. Backoff between attempts is 0.4 s doubling to 3 s. The bound, the backoff, the cleanup window and the cancel grace period are variables only so tests can shorten them.

When the bound passes, the client first reads the job's terminal outcome from the queue store, read-only (`queueReadHandoverStore`: no migration, no write, no daemon-held lock) and only for the exact submitted token. A finished job reports its real exit status. Otherwise the client exits 125 naming the job, saying it may still be running and pointing to `agent-heavy logs JOB_ID`. If the user interrupted, it also says the cancellation was not delivered.

## Behavior in detail

- **Cancel.** A read alone no longer ends an outage; only a delivered mutation or a terminal state does, so a daemon that answers snapshots but loses `cancel` and `kill` reaches the bound with one loss notice. `kill` is sent only after a `cancel` was acknowledged and the 3 second grace period passed, never because a reply was lost. The retry wait is interruptible.
- **Interrupts.** The first interrupt during an outage is held and announced; a second ends the wait with the inspect message. The held interrupt cancels the job when the daemon answers.
- **Paused queue.** An unclean daemon restart pauses the queue. When a client recovers and its job is still queued behind a paused queue, it prints one notice naming `agent-heavy resume`.
- **Finished job, cleanup blocked.** If only the daemon's absence blocks cleanup, the job's real exit status is returned with the notice on stderr instead of 125. Other cleanup failures still return 125.
- **Identity.** If the daemon reports a different token under the job ID, the client refuses to use or cancel it.
- **Oversized reply.** A reply over 32 MiB fails once with a definite error instead of being retried as a lost connection.
- **Lane wait.** Notices say "lease". An interrupt reports whether the withdrawal worked and how to inspect the lease.

## Unchanged

Non-transport errors fail at once as before. The handover path (`queueHandoverFollowTask`) is consulted first and still wins. Submission still reconciles by token with two immediate retries. The command is never run directly. The daemon and its 100 ms tick are untouched.

## Known gaps, out of scope here

- A worker waiting for the shared slot exits 125 on a transport failure outside handover custody, so the command never runs and the job records 125 (`execution.go`, worker wait; `queue_handover_test.go` `missing-custody` requires it). Resubmitting is safe.
- A retained `consumed` handover receipt ends a client immediately (`queue_handover.go`). No non-test code removes `run/handover.json`; PR #63 or later handover work may affect it.
- Between `ListenUnix` and `Chmod` in the daemon the socket has mode 0700, so a retrying client can land in that window and exit 125. The socket check must not be relaxed.
- `lane acquire` retries submission immediately, so a refused socket exhausts its two retries at once.
- Why the daemon becomes slow under memory pressure.

## Validation

Locally, directly: focused `go test -p 2 -parallel 2 ./internal/backend` runs over the new tests, `TestResults`, `TestRealDaemon`, handover, lane, follow, cancel and queue tests, and `-race` over the new tests and the two adjusted real-daemon tests. The full package and other packages are left to hosted CI because of host memory pressure.

Two real-daemon tests asserted the old behavior (client exits 125 when the daemon dies). They now assert that the client keeps waiting, announces the outage and the paused queue, and finishes with the real result.
