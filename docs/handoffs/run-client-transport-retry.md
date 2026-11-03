# Run client survives queue daemon transport failures

## Result

`agent-heavy run` no longer exits 125 when one queue call times out while it follows a submitted job. The job lives in the daemon and keeps running; only the client lost sight of it, and an agent reading 125 would conclude the job failed and may submit it again.

## Design: one place retries

Retrying happens in exactly one place, the follow loop (`followWithToken`), plus `agent-heavy lane wait`. Everything else reports a lost call once and truthfully, and the follow loop decides what to do.

- **One definition of recovery:** an iteration with no transport failure anywhere (the read, the held-interrupt cancellation, and any cleanup it triggers). A read that answers while a `cancel` or `kill` is lost is not recovery, so a daemon that answers snapshots but loses mutations reaches the bound. A busy handover answer to a held cancellation counts as a failed call too, so alternating lost replies and busy answers cannot restart the clock.
- **Budget:** per continuous outage, not per run. The window is `queueOutageWindow`, two minutes, counted from when the first timed-out call returns. Calls already in flight finish after it passes (the follow read, the cancellation's read and mutation, a cleanup probe, up to 15 s each), so a hung daemon ends the run in under three minutes (measured about 2 min 33 s to 2 min 54 s at one tenth scale). The window covers the app's 10 second restart throttle (`AgentSupervisor.swift`) several times over plus startup. Backoff is 0.4 s doubling to 3 s.
- **Second interrupt latency:** a second interrupt is not observed while `cancelJob` runs, so it can take about 27 s (its 12 s deadline plus one 15 s call), or about 54 s when cleanup is also being confirmed. No interruptible RPC was added.
- **`Cancel` does not retry** (`agent-heavy cancel`, the signal path, installer preflight). It returns at once with `Cancellation of job N could not be confirmed; the job may still be running; inspect it with: agent-heavy logs N`, wrapping the real cause. A waiting client retries it within its budget. Trade-off: a standalone `agent-heavy cancel` against a briefly slow daemon fails once; re-running is safe.
- **Exit status with a held interrupt.** After an ACKNOWLEDGED `cancel` the client reports the interrupt status (128 plus the signal), as before this change, so a network blip cannot decide the exit status; this includes a blip while cleanup is confirmed. With NO cancel acknowledged, a job seen finished reports its own exit status, including when `cancelJob` read `Done` and its cleanup probe was lost. The notes say only what is known: "no cancellation was confirmed", or "after its cancellation was acknowledged".
- **Cleanup.** `WaitCleanup` does not retry. If cleanup of a finished job cannot be confirmed, because the daemon does not answer the probe or answers yet still holds the receipt after the 12 s window, the follow loop reports the job's own exit status with a stderr note. The daemon keeps retrying cleanup and the job holds its slot, so a 125 added no safety. Other cleanup errors (unsafe files, custody mismatches, non-transport daemon errors) still fail.
- **Grace and kill.** The time of the first acknowledged cancel in a run is carried across `cancelJob` calls, so `kill` follows an acknowledged cancel plus the 3 s grace even if reads were lost in between. `kill` is never sent without an acknowledged cancel.
- **Identity.** The task token is checked on every read in the follow loop and in `Cancel` (standalone `Cancel` fixes it on its first read), before the local `exit_code: 130` marker is written and before any mutation.
- **Store at the bound.** The client reads the terminal outcome from the queue store read-only (`queueReadHandoverStore`: `mode=ro`, no migration, no write, no daemon lock), only for the exact token and only for `done` or `outcome` phases.
- **Interrupts.** The first interrupt during an outage is held and announced; a second ends the wait with the inspect message. The bound message is stated once.
- **Handover custody.** A handover receipt in any phase, including a committed one left by a crash, only covers jobs that existed when the handover began. A job with `id >= receipt.NextID` that is not in the dispatch inventory is outside custody, so a stale receipt no longer ends its follower on the first lost reply and the outage path runs.
- **Paused queue.** After a loss, a queued job checks the group once on recovery, again if that check was lost, and on the 15 s tick, and prints one notice per run: `the queue is paused; job N will not start until the queue is resumed`. No cause and no command.
- **Notices:** at most one loss and one recovery pair per 30 s; a handover is described as a handover.
- **Also:** a reply over 32 MiB fails once instead of being retried. `lane wait` says "lease" and reports whether the withdrawal on interrupt worked.

## Unchanged

Non-transport errors fail at once as before. `queueHandoverFollowTask` is consulted first. Submission still reconciles by token with two immediate retries. The command is never run directly. The daemon server code and its 100 ms tick are untouched. `QueueControl` is unchanged apart from the response size cap.

## Known gaps, out of scope

- The custody follow-wait stays unbounded by the outage budget: while an authenticated handover explains the loss, the client keeps waiting as before (`outage.reset()`), because the spec preserves that path.
- A worker waiting for the shared slot exits 125 on a transport failure outside handover custody (`execution.go`, worker wait; `queue_handover_test.go` `missing-custody` requires it). Resubmitting is safe.
- A follower of a job that WAS in custody still uses custody checks against a stale receipt (`queue_handover.go`): a dead consumer on a consumed one, and "crashed before releasing" or "custody expired" on a committed one, each on its first lost reply. No non-test code removes `run/handover.json`.
- Between `ListenUnix` and `Chmod` the socket has mode 0700 (`queue_daemon.go:244-250`), so a retrying client can land in that window and exit 125. The socket check must not be relaxed.
- `lane acquire` retries submission immediately, so a refused socket exhausts its two retries at once.
- Why the daemon becomes slow under memory pressure.

## Validation

Locally, directly, with `-race` on what changed; the full package is left to hosted CI because of host memory pressure. `TestInstallerPackagedLiveUpgrade` needs a built macOS app bundle (`AGENT_QUEUE_TEST_APP_BUNDLE`) and skips without one. See the pull request for the exact commands and results.
