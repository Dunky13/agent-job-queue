# Run client survives queue daemon transport failures

## Result

`agent-heavy run` no longer exits 125 when one queue call times out while it follows a submitted job. The job lives in the daemon and keeps running; only the client lost sight of it, and an agent reading 125 would conclude the job failed and may submit it again. The follow loop, finished-job cleanup wait, cancellation, and `agent-heavy lane wait` now keep trying through a transport failure (`errQueueTransportUnavailable`), print one stderr notice on loss and one on recovery, and keep draining the job log meanwhile.

## Bound

A continuous outage may last `queueOutageBound`, two minutes (the handover prepare lifetime). The supervisor restarts a stopped daemon after at most a 10 second throttle, so the bound covers a restart several times over. A daemon gone longer ends visibly with exit 125 and a message naming the job and `agent-heavy logs JOB_ID`, saying it may still be running. Backoff between attempts is 0.4 s doubling to 3 s. Both are package variables so tests use millisecond values.

## Unchanged

Non-transport errors fail at once as before. The handover path (`queueHandoverFollowTask`) is consulted first and still wins. Submission still reconciles by token (two immediate retries). The daemon and its 100 ms tick are untouched.

## Remaining

- Why the daemon becomes slow under memory pressure is not addressed.
- A Ctrl-C during an outage is held until the daemon answers or the bound passes, then cancels. A second Ctrl-C does not shorten the wait.
- `lane acquire` submission retries twice immediately and can still fail on a slow daemon, leaving a queued lease with no ID shown. Same class, different seam; not changed here.

## Validation

Locally, directly: `go test -p 2 -parallel 2 ./internal/backend -run 'Outage|Transient|StaysUnreachable|NonTransport|TestResults|TestRealDaemon|Handover|Lane|Follow|Cancel'` passed, and the new tests plus the two adjusted real-daemon tests passed under `-race`. The full package and other packages were left to hosted CI because of host memory pressure.

Two real-daemon tests asserted the old behavior (client exits 125 when the daemon dies). They now assert that the client keeps waiting, then finishes with the real result after the daemon returns.
