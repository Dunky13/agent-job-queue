# Live installer job-start flake handoff

Stacked on PR #67 (`arch/retire-handover-begin`). Fixes the intermittent
`isolated queue condition did not become true before deadline` failure of the
managed live-resume installer test on the hosted Intel runner.

## Diagnosis

The test waited 8 seconds for the `started` file of a job submitted by a real
`agent-heavy run` caller. That deadline covered two different things:

1. The caller's own preparation, before the daemon sees the job: a snapshot RPC,
   project identification (several `git` subprocesses), workload identification,
   and fsynced private writes (description, workload identity, job payload).
2. The daemon's 100 ms dispatch tick and one worker process start.

Evidence, from two temporary diagnostic runs on the hosted Intel runner (GitHub Actions
runs 41568662224 unstressed and 41571805471 stressed, from a scratch branch since deleted).
The original hosted failures (for example run 41548654243) were never instrumented, so
the mechanism below comes from the stressed reproduction, whose signature matched them:

- Without load, 16 of 16 starts took 0.78 to 2.7 s. The failure does not occur
  on an idle runner.
- With three CPU hogs and a disk `sync` loop, 3 of 4 first-iteration subtests
  failed. Timestamped probes put the delay in the caller's preparation: in one
  case 5.4 s before the description step finished and 2.2 s more before submit,
  and in the other two cases the caller had not finished its description step
  after 8 s. After submit, the daemon launched the worker within 0.3 s and the
  worker took about 0.5 s more.
- In every failure the queue was Running, no other job or slot was held, the
  daemon snapshot answered, and the caller was alive. The job was late, not
  stuck. More time helped, so the product has no stuck-state bug here.

What the data does not separate: how much of the caller's delay was `git`
subprocess start and how much was fsync latency. Both are host-load dependent
and are done by the shipped CLI, which is the thing under test.

## Fix

Test only, in `internal/backend/install_live_runtime_test.go`:

- `liveCallerWait` replaces the flat 8 s wait for every `agent-heavy run` caller
  start (blocker, active job, and the finish-pending caller) with the two
  conditions that precede the file: the daemon lists the job, then the command
  creates `started`.
- The first stage ends on the daemon's acceptance, not on a new clock. If the caller
  exits first the test fails at once with its exit error and output instead of
  eight seconds later. The scenario's existing 60 s (3 min for finish-pending)
  context still bounds a wedged caller.
- The second stage keeps a short, named deadline (`liveStartBudget`, 8 s) for
  bounded local work: one dispatch tick plus one process start, measured at no
  more than 0.6 s locally and under 2 s on the stressed Intel runner.
- On failure the helper prints the daemon's view of each job, the queue group,
  every job log, whether the caller is alive, its output, the daemon log and the
  fixture's queue processes.
- The caller output buffer is now mutex guarded so the failure report can read
  it while the caller runs without a data race.

No assertion or coverage was removed. Sibling waits: the queued-variant
"two tasks listed" wait is now stage one of the same helper; the post-resume
`started` wait only needs the daemon and worker, so it uses stage two; the
receipt-phase and managed-live-validation waits do not depend on a caller and are
unchanged.

## Orphans on test timeouts

A `go test -timeout` panic skips `t.Cleanup`. Reproduced locally with a 14 s
timeout: the `agent-heavy daemon`, `scheduler`, the `run` caller, its worker and
the `sh` loop (it waits for a `release` file) plus the `/tmp/aq-live-*` directory
stay behind. A normal pass or `t.Fatal` leaves nothing. A small follow-up could
use `t.Deadline()` to start a watchdog that stops the fixture's processes and
removes the directory shortly before the timeout. It is not part of this change.

## Validation

- `go test -p 2 -parallel 2 -race ./internal/backend -run 'TestInstallerManagedLive'`
  passes (exit 0, all ten scenarios).
- Failure reporting was exercised by pointing the wait at a file that never
  appears: the report names the job as Running, the live caller and its worker.
- Hosted Intel CI on this PR is the remaining check. A rare slow runner cannot be
  proven absent from a single pass.
