# Retire starting a live handover

## Result

Setup no longer begins a live handover. It waits for a running daemon's active and queued jobs, as the owner decided. Resume stays so a journal an older release left behind can still finish. No `--payload`, unmarked-install or other installer mode was removed.

## Findings to decide on

1. **Resume only runs on the build that began the handover.** `resumeManagedLive` requires the journal's `Target` to equal the running installer's identity, and older releases wrote their own identity there. A journal from build T therefore cannot finish on a later build N: the receipt binds the daemon to T's exact executable. On N, a journal whose T completed is ignored and removed, and an unfinished one is refused with "Another live candidate is pending". Only T's own binary, which still contains the old code, finishes it. So in shipped flows everything past that identity check on N is unreachable, including app-bundle resume with `managedSupervised` false. That behaviour predates this change and is kept as instructed. It means "older journals still finish" holds for the older binary, not for new builds. Removing the rest is a separate owner decision.
2. **An unfinished journal for another build blocks setup even when it binds nothing.** If T's receipt is cancelled, expired or missing, N still refuses until build T is finished. This predates this change and is not fixed here. The recovery is the startup menu's release history link, then opening exact build T.
3. **Setup waits only when a daemon is still running.** After a normal quit at the same location no daemon remains, so setup has nothing to wait for. The next daemon reconciles unfinished jobs and starts paused. The docs state this rather than a blanket "setup waits".
4. **Payload path behaviour change.** It previously took a handover when a job was done but its caller was still reading output. It now treats that queue as drained, so that caller can see a transport error during the outage. App bundles never had this protection, and no shipped caller uses `--payload`.

## Fixed along the way

Starting a handover was the only code that removed a journal left by a finished installation. Such a journal reads as pending once a later build completes, which refuses every further upgrade. An older release can leave one by cancelling a handover and then finishing drained. `retireFinishedLive` now removes it under the install locks, only after resume has declined it and only when live handling is allowed. `TestManagedFinishedLiveJournalCannotBlockLaterUpgrades` fails without the fix.

## Deleted

- `planManagedLive` and its two call sites in `managedDrain`.
- The capability negotiation in `liveTarget`, now `liveTargetDigest`. The fail-closed digest check stays.
- The begin-only parts of `beginManagedLive`, now `confirmManagedLive`: replacing a previous journal, writing a new one, and the `prepared` field.
- The `handover-capabilities` CLI command. An older installer treats its absence as unsupported and drains. Without it, an older installer cannot begin a handover toward this build.
- The daemon's `handover-status` operation. An older installer treats the error as "use the drained path".
- Tests of beginning a handover: `liveManagedFixture`, the preflight and eligibility-gate tests, and `TestQueueHandoverCapabilitiesDoesNotTouchPrivateState`.

## Retained, and why

| Code | Reason |
|---|---|
| `resumeManagedLive`, `confirmManagedLive`, `commitManagedLive`, `cancelManagedLive`, `validateManagedLive`, `finishManagedLive` | Resume itself. |
| `prepare-handover`, `commit-handover`, `cancel-handover`, including the fresh-token prepare branch | Resume drives these on the journal's daemon. Fresh prepare shares its code with the idempotent path, and real-daemon phase tests need it to reach each phase. |
| `queueAdoptHandover`, `consumeQueueHandover`, `validateReleasedQueueHandover`, `validateConsumedQueueHandover` | A daemon receiving a handover. |
| `d.handover` freeze, `handoverStop`, `releaseHandover`, `errQueueHandoverBusy` | The daemon's side of a resumed prepared or committed handover. Not shown unused. |
| `queueHandoverFollowTask`, `queueHandoverWorkerWait`, `client_handover_*`, task `handoverProtocol` and `handoverCaller` | Outstanding callers and workers across a handover. Job tagging also keeps queue content compatible with older releases. |
| `install_runtime_generations.go` | Resume retains generations, and later setup reclaims those older releases left. |
| `decodeQueueHandoverStatus` | Already used only by tests, which now decode the prepare response. |

## Newly unreachable, for the follow-up

No shipped flow now reaches these, though tests and an unsupported manual older-installer run still can: the resume phases and `managedActivate`'s non-supervised branch for an app bundle; `retainManagedRuntime`; and the daemon as handover source (fresh prepare, freeze, `releaseHandover`). `--payload` remains reachable by hand, and it is now the only caller of the non-supervised activation path.

## Test coverage

`TestInstallerManagedLiveResumeRetainsRunningAttempts` replaces the old installer partition test and seeds an older release's journal against real private daemons:

| Phase | How it is exercised |
|---|---|
| prepared | Seeded, then a full resume, with running and queued-and-paused variants |
| committed | A held connection pins the phase, so resume observes it without a second RPC |
| released | Seeded by a real commit, and again by retrying after an `unregister login` failure |
| consumed | Retrying after a `register resources` failure, once the real replacement daemon adopts |
| cancelled | An `unregister resources` failure cancels; setup then waits for the job and upgrades drained |
| expired, no receipt, unknown phase, different token | Hand-written receipts in `TestManagedLiveResumeSelectsOnlyActivePhases` |

The daemons are built from this tree, not from an older release binary, so this does not prove compatibility with an actual older binary. Daemon-side lease expiry remains covered separately by `TestQueuePreparedHandoverLeaseExpiresWithoutChangingQueueState`, which uses an in-process daemon.

## Evidence

Validation results are recorded in the PR description, pinned to its head.

## Next steps

1. Independent Standards and Spec review of this draft PR.
2. Owner decision on findings 1 and 2.
3. The stacked follow-up removing `--payload` and the unmarked install path.
