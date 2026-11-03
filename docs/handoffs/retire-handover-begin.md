# Retire starting a live handover

## Result

Setup no longer begins a live handover. Before replacing a running daemon it waits for that daemon's active and queued jobs, as the owner decided. Resume stays, and it is reachable on newer builds through the route below. No `--payload`, unmarked-install or other installer mode was removed.

## How a journal finishes

The receipt binds a handover to one exact executable, build T, so only T's bundle can complete it. A newer build N can still drive that through its own controller:

1. T is interrupted after publishing, leaving its unfinished marker and app at the recorded location.
2. The user opens N from the other Applications folder. `managedPendingSuperseded` returns false because that app is retained, so preflight offers `finish-pending-installation`, and automatic setup selects it.
3. `pendingInstallation` points the installer at T's app and payload, so `validatePayload` makes the identity T and N's code passes the resume identity check as a non-supervised app bundle.
4. T completes and its journal is removed. N then relocates once T's adopted jobs finish.

N dragged over T at the same path has replaced T's app, so it has no such route.

## Which receipts bind custody

| Receipt | Binds? | Why |
|---|---|---|
| Missing, cancelled, expired | No | No transfer is in progress. |
| Prepared or committed, daemon alive | Yes | That daemon holds the freeze. A prepared lease expires after two minutes. |
| Prepared or committed, daemon gone | No | Traced: a restarted daemon sees a non-released phase, adopts nothing and runs ordinary crash recovery. Followers get nothing from these phases once the daemon is gone. |
| Released | Yes | Every daemon but T's refuses to start, and workers may be following it. Spent only if a newer build completed. |
| Consumed, consumer alive | Yes | The queue runs under that daemon. Another build refuses only until T has completed; after that it is the finished build's own daemon. |
| Consumed, consumer gone | No | The next daemon runs ordinary crash recovery. |

`retireSpentLive` removes a journal only when it binds nothing, under the install locks, using `managedRemoveFile`'s byte comparison. It never removes a receipt.

## Findings still open

1. **A stale released receipt stops a later daemon.** If T is released and the user removes the app, the drained removal path lets N install. The journal is now spent and removed, but the receipt stays, and `queueAdoptHandover` refuses N's daemon because its digest differs. Found by reading the code, not reproduced end to end. Fixing it means changing what a daemon does with an unadoptable receipt, which is larger and riskier than the fixes here, so I stopped on it.
2. **A journal with another handover's receipt still refuses.** An older release wrote the journal before preparing, so an interruption there can pair it with an earlier handover's receipt. Setup fails closed with "Daemon handover differs". The journal's own token bound nothing, so this could arguably be removed, but it is outside the cases decided so far.
3. **Busy-lock race.** A job's caller cleaning up can hold a lock between setup's inspection and installation, which returns a busy error. A test using a full wait loop hit this once. I restructured the test instead, and did not investigate how the app surfaces it.
4. **Payload path behaviour change.** It previously took a handover when a job was done but its caller was still reading output. It now treats that queue as drained, so that caller can see a transport error during the outage. App bundles never had this protection.

## Fixed along the way

- **Finished journals.** Beginning a handover was the only code that removed journals that no longer bound anything. Left in place, they refused later upgrades.
- **Stranding.** A journal's target being older than the completed build, a receipt that binds nothing, a moved app and a dead daemon's prepared receipt no longer block setup.
- **Refusal.** It names the build through `installPendingError`, and preflight reports `pending-installation` with the release history and reveal actions.
- **Cleanup repair race.** `recoverManagedCleanup` failed setup if a job's caller finished cleaning first. Already clean now counts as success, and the repair loop inspects again.

## Deleted

- `planManagedLive` and its two call sites in `managedDrain`.
- The capability negotiation in `liveTarget`, now `liveTargetDigest`. The fail-closed digest check stays.
- The begin-only parts of `beginManagedLive`, now `confirmManagedLive`: replacing a previous journal, writing a new one, and the `prepared` field.
- The `handover-capabilities` CLI command. An older installer treats its absence as unsupported and drains. Without it, an older installer cannot begin a handover toward this build.
- The daemon's `handover-status` operation, which now answers "Unknown queue operation". An older installer reads any error as "use the drained path".
- Tests of beginning a handover: `liveManagedFixture`, the preflight and eligibility-gate tests, and `TestQueueHandoverCapabilitiesDoesNotTouchPrivateState`.

## Retained, and why

| Code | Reason |
|---|---|
| `resumeManagedLive`, `confirmManagedLive`, `commitManagedLive`, `cancelManagedLive`, `validateManagedLive`, `finishManagedLive` | Resume itself. |
| `managedActivate`'s non-supervised branch and `retainManagedRuntime` | Reached by finishing a retained build, as well as by `--payload`. |
| `prepare-handover`, `commit-handover`, `cancel-handover`, including the fresh-token prepare branch | Resume drives these on the journal's daemon. Fresh prepare shares its code with the idempotent path, and real-daemon phase tests need it to reach each phase. |
| `queueAdoptHandover`, `consumeQueueHandover`, `validateReleasedQueueHandover`, `validateConsumedQueueHandover` | A daemon receiving a handover. |
| `d.handover` freeze, `handoverStop`, `releaseHandover`, `errQueueHandoverBusy` | The daemon's side of a resumed prepared or committed handover. Not shown unused. |
| `queueHandoverFollowTask`, `queueHandoverWorkerWait`, `client_handover_*`, task `handoverProtocol` and `handoverCaller` | Outstanding callers and workers across a handover. Job tagging also keeps queue content compatible with older releases. |
| `install_runtime_generations.go` | Resume retains generations, and later setup reclaims those older releases left. |
| `decodeQueueHandoverStatus` | Already used only by tests, which now decode the prepare response. |

## Newly unreachable, for the follow-up

Only the in-tree daemon acting as a handover's source: fresh prepare from a new token, and its freeze and release. A shipped installer no longer asks a daemon of this build to begin one. Tests still use it to reach each phase.

## Test coverage

`TestInstallerManagedLiveResumeRetainsRunningAttempts`, the installer partition test, seeds an older release's journal against real private daemons in ten scenarios:

| Phase | How it is exercised |
|---|---|
| prepared | Seeded, then a full resume, with running and queued-and-paused variants |
| committed | A held connection pins the phase, so resume observes it without a second RPC |
| released | Seeded by a real commit, and again by retrying after an `unregister login` failure |
| consumed | Retrying after a `register resources` failure, once the real replacement daemon adopts |
| cancelled | An `unregister resources` failure cancels; setup then waits for the job and upgrades drained |
| finish pending, released and consumed | T is interrupted for real, then N in the other Applications folder finishes it as a non-supervised app bundle with the caller's exit 23 and one execution, then relocates |

Also: `TestRealDaemonPreparedReceiptBindsNothingOnceItsDaemonRestarts` shows the receipt state against a real daemon restart. Expired, missing-receipt, unknown-phase, different-token and dead-process states use hand-written receipts in the `TestManagedLive*` unit tests.

The in-tree daemons are built from this tree. The independent review separately drove a real `7b4b27c` binary through the phase scenarios.

## Evidence

Validation results are recorded in the PR description, pinned to its head.

## Next steps

1. Independent re-review and hosted CI on the new head.
2. Merge per the owner's authorization once both pass.
3. Owner decision on open findings 1 and 2.
4. The stacked follow-up removing `--payload` and the unmarked install path.
