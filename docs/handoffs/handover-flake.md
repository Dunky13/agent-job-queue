# Handover flake: TestRealDaemonLiveHandoverRetainsWorkerCallerAndQueuedJobs

## Symptoms

Both on the hosted job "macOS tests (macos-15-intel, core-2)":

- Run 45838597510 (main @ 0861e70): the queued job's caller exited 125 with
  `Queue handover custody changed while observing daemon exit`.
- Run 45900842336, job 140185931504 (commit fe1e6e4): `outage-one` never
  reached the running job's caller output within 8 s. Daemon output was empty.

A log search of the 25 most recent failed runs found this test failing only in
the first of these two.

## Root cause

### 1. Test race (the observed failures)

The test backdated `expiresAt` in the released receipt right after the daemon
exited, while both callers were still following it.

1. A caller reads the `committed` receipt (`queueHandoverRetainedTaskWithProbe`
   in `internal/backend/queue_handover.go`).
2. Its process census is slow. The daemon writes `released` and exits.
3. The test rewrites `expiresAt`.
4. The census reports the daemon gone. The caller re-reads the receipt and
   requires that only the phase and consumer differ from its first view.
5. `expiresAt` differs, so the caller exits 125.

No product code edits a released receipt: release changes only the phase, and
adoption adds only the phase and consumer. The check is correct and unchanged.

The first symptom is the queued caller losing this race. The second symptom's
cause is not established: that run stopped before printing the caller's error. A
running job's caller that exits 125 stops copying the job log, so `outage-one`
never appears. The induced reproduction below produces the same test message
through this race. Race 2 would produce it too, since the test's own rewrite
replaced the receipt under the callers. Both are fixed.

Likely trigger for it appearing now: 429bac4 stopped race-built children
sleeping one second at exit, so the test's rewrite follows the release much
sooner. This is inferred from timing, not measured.

### 2. Product race (found while investigating)

The daemon replaces `handover.json` by rename. A caller that had opened the
previous file then saw a link count of 0 in `readQueueHandoverReceipt` and
rejected it with `Unsafe queue handover receipt`, which ends the caller with
exit 125. The daemon does this on every release and adoption, so real callers
can hit it during a live upgrade. No captured CI error shows it.

Sibling link-count checks were searched. The others inspect a name with `lstat`
or `fstatat`, open lock files, logs or executables, or read installer and
integration files that the daemon does not replace.

## Fix

- `internal/backend/queue_handover.go`: the receipt reader rejects only more
  than one link. A file with no links was replaced after it was opened; it is
  complete, and is what a slightly earlier read would have returned. Owner, mode,
  type, symlink and shape checks are unchanged, and the existing `hardlink` case
  of `TestQueueReleasedHandoverRefusesUnsafeReceiptNamespace` still passes.
- `internal/backend/queue_handover_test.go`: the live test backdates
  `expiresAt` only after both callers have printed job output written after the
  daemon exited. A caller copies output and reads the receipt in one loop, so
  this proves every earlier read has finished. A second barrier per caller then
  proves both keep following the expired released receipt (the queued caller was
  not asserted before). No deadline, sleep or retry changed.
- New `TestQueueHandoverReceiptReadSurvivesAtomicReplacement` covers race 2.

## Reproduction

Local Apple Silicon host shared with other jobs, Go 1.26.11, race build, runs
of the live test. "Induced" adds two temporary, uncommitted sleeps: 700 ms in
`releaseHandover` before it writes `released`, and 1 s in the caller before its
liveness check on a `committed` receipt.

| Condition | Before (219ff73) | After |
|---|---|---|
| Induced, default GOMAXPROCS | 5 of 5 failed | 20 of 20 passed |
| Induced, GOMAXPROCS=1 | 5 of 5 failed | 20 of 20 passed |
| Unmodified, 6 CPU burners, default GOMAXPROCS | 0 of 50 failed | 0 of 50 failed |
| Unmodified, 6 CPU burners, GOMAXPROCS=1 | 0 of 50 failed | 0 of 50 failed |

- The failure did not reproduce without the induced sleeps.
- Every induced failure before the fix showed the second symptom's message, and
  both callers had printed `Queue handover custody changed while observing daemon
  exit`. The test stopped there, so the first symptom's line was not reached.
- `TestQueueHandoverReceiptReadSurvivesAtomicReplacement` without the product
  fix: 10 of 10 failed with `Unsafe queue handover receipt`.

## Validation

All passed:

- `gofmt -l internal` (no output)
- `go vet ./internal/backend/...`
- `go test -p 2 -parallel 2 -skip '^TestInstallerOwnershipParsesRealPrivateLaunchctlService$' ./internal/backend/...`
- Race build, 5 runs each:
  `TestQueueHandoverReceiptReadSurvivesAtomicReplacement`,
  `TestQueueHandoverFollower*`, `TestQueueReleasedHandover*`

The backend suite ran on base 219ff73 without the race detector.

Not checked: hosted CI, and an Intel host.
