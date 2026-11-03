# Live installer job-start flake handoff

Stacked on PR #67 (`arch/retire-handover-begin`). Fixes the intermittent
`isolated queue condition did not become true before deadline` failure of the
managed live-resume installer test on the hosted Intel runner.

## Diagnosis

The test waited 8 seconds for the `started` file of a job submitted by a real
`agent-heavy run` caller. That deadline covered two different things:

1. The caller's own preparation, before the daemon sees the job: a snapshot RPC,
   project identification (`git` subprocesses), workload identification, and
   fsynced private writes (description, workload identity, job payload).
2. The daemon's 100 ms dispatch tick and one worker process start.

Evidence, from three temporary diagnostic runs on the hosted Intel runner (GitHub
Actions runs 41568662224, 41571805471 and 41577937033, from scratch branches since
deleted). The original hosted failures were never instrumented. The failing wait
was seen in PR #61 (three failures, then a pass on the unchanged head), in run
41548654243 (PR #64: `complete`, `unregister_login` and `register_resources`) and
in run 41448050366 (a Dependabot branch: `complete`). `complete` failed in both
runs whose logs were read, and package times do not separate failing runs from
passing ones, which fits a transient window better than a uniformly slow runner.

- Run 41568662224, no load: 16 of 16 starts took 0.78 to 2.7 s.
- Run 41571805471, three CPU hogs and a disk `sync` loop, iteration 1: 3 of 4
  scenarios failed. Timestamped probes put the delay in the caller's preparation:
  in one case 5.4 s before the description step finished and 2.2 s more before
  submit, and in the other two the caller had not finished its description step
  after 8 s. After submit the daemon launched the worker within 0.3 s and the
  worker took about 0.5 s more. In every failure the queue was Running, no other
  job or slot was held, the snapshot answered and the caller was alive: late, not
  stuck.
- Same run, iterations 2 and 3, load still on: 8 of 8 starts took 1.68 to 2.0 s.
  So load alone does not explain the failures. Something warms up or a transient
  clears, and that is unexplained.
- Run 41577937033, same load, all ten scenarios once, against `0eba4ed`, this
  PR's code with a 30 s stage-one cap instead of 20 s: all passed. Waiting for the daemon to list the job took 4.97 s in the first
  scenario (`complete`, cold) and 0.64 to 1.12 s in the other nine; the command
  then started in 0.21 to 0.50 s. The whole test took 448 s. This run did not
  reproduce the slow window. Its slowest caller start, 5.34 s, would also have
  passed the old 8 s wait, so it does not prove the fix, and it does not show how
  the later flat waits behave inside a slow window (see below). The evidence that
  the fix matters is local: a 12 s pre-submit delay now passes, which the flat 8 s
  wait could not allow.

Limits of the evidence:

- "The signature matched" rests on one error line (`isolated queue condition did
  not become true before deadline`, at the first wait) and the duration only.
- How much of the delay is `git` and how much is fsync is not separated. Each
  `git` call is capped at 2 s, so 5.4 s and over 8 s include at least 1.4 s and 4 s
  of something else, assuming the kills are prompt.
- `queued`, `unregister resources` and finish-pending were never in a slow window.
  The flat 8 s waits that remain (`validateManagedLive` after the `released`
  commit, the receipt reaching `consumed`, and the receipt phase in
  finish-pending) do not involve a caller. Nothing observed says they are slow, but
  no run exposed them to the slow window either.

## Fix

Test only, in `internal/backend/install_live_runtime_test.go`:

- `liveCallerWait` replaces the flat 8 s wait for every `agent-heavy run` caller
  start (blocker, active job, and the finish-pending caller) with the two
  conditions that precede the file: the daemon lists exactly the expected number
  of jobs (the listing includes retained history and the fixture starts empty),
  then the command creates `started`.
- Stage one, the caller's preparation, ends when the daemon lists the job or the
  caller exits. A caller that exits first fails at once with its exit error and
  output; one ended by the scenario deadline is reported as that. Only a caller
  that never submits reaches `liveAcceptBudget` (20 s). The effective bound on this
  stage therefore rose from 8 s to 20 s. It bounds only caller preparation, which
  no test asserts, and 20 s is 4 times the slowest completed preparation measured
  and keeps ten hung scenarios inside `go test -timeout 12m`: 448 s measured plus
  10 x 20 s is 648 s (30 s would reach 748 s). A timeout panic discards every
  buffered report and orphans fixtures.
- Stage two, dispatch plus worker start, keeps a short named deadline
  (`liveStartBudget`, 8 s): 0.2 to 0.5 s on the stressed Intel runner and at most
  0.6 s locally.
- Both stage durations are logged with `t.Logf` (visible on failure or with
  `-v`).
- On failure the helper prints the daemon's view of each job, the queue group,
  the last 4 KiB of each job log and daemon log, whether the caller is alive, its
  output, the caller's descendant processes (its `git` calls run in the test
  directory and do not mention the fixture home) and the fixture's queue
  processes. `ps` is time bounded.
- The caller output buffer is mutex guarded so the report can read it while the
  caller runs without a data race.

No assertion or coverage was removed. Sibling waits: the queued variant's "two
tasks listed" wait is now stage one of the same helper; the post-resume `started`
wait only needs the daemon and worker, so it uses stage two.

## Orphans on test timeouts

A `go test -timeout` panic skips `t.Cleanup`. Reproduced locally with a 14 s
timeout: the `agent-heavy daemon`, `scheduler`, the `run` caller, its worker and
the `sh` loop (it waits for a `release` file) plus the `/tmp/aq-live-*` directory
stay behind. A pass leaves nothing. A failing scenario normally leaves nothing,
but with a hung worker `execute-attempt` outlived cleanup in its own process group
and was reparented to PID 1.

## Follow-ups (not in this PR)

1. A watchdog from `t.Deadline()` that stops the fixture's processes, including
   workers in their own process groups, and removes the directory shortly before
   the timeout.
2. Product finding: `agent-heavy run` makes up to four `git` subprocesses before
   it submits (`ProjectDescription`, `profileDirectoryScope`, and two in
   `IdentifyWorkload`), each capped at 2 s, plus three fsynced writes, and the same
   `rev-parse` runs twice. On timeout `profileGit` silently returns an empty
   string, so workload identity and learning scope fall back from git to the
   directory, and load could split learned profiles. Sharing one `rev-parse`
   result would remove the repetition and the exposure.
3. `scripts/test-go-runtime.sh` runs the installer scope without `-v`, so a
   timeout panic loses subtest reports. Streaming them would keep the new reports.

## Validation

- `go test -p 2 -parallel 2 -race ./internal/backend -run 'TestInstallerManagedLive'`
  passes (exit 0, all ten scenarios).
- Failure reporting was exercised by pointing the wait at a file that never
  appears: the report names the job as Running, the live caller and its worker.
- #67 has merged, so this PR now targets `main` and runs hosted CI. A rare slow
  window cannot be proven absent from passing runs.
