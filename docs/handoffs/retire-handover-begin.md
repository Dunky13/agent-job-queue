# Retire starting a live handover

## Result

Setup no longer begins a live handover. Before replacing a running daemon it waits for that daemon's active and queued jobs, as the owner decided. Resume stays, and it is reachable on newer builds through the route below. No `--payload`, unmarked-install or other installer mode was removed.

## How a journal finishes

The receipt binds a handover to one exact executable, build T, so only T's bundle can complete it. A newer build N can still drive that through its own controller:

1. T is interrupted after publishing, leaving its unfinished marker and app at the recorded location.
2. The user opens N from the other Applications folder. `managedPendingSuperseded` returns false because that app is retained, so preflight offers `finish-pending-installation`, and automatic setup selects it.
3. `pendingInstallation` points the installer at T's app and payload, so `validatePayload` makes the identity T and N's code passes the resume identity check as a non-supervised app bundle.
4. T completes and its journal is removed. N then relocates once T's adopted jobs finish.

N dragged over T at the same path has replaced T's app, so it has no such route. A released receipt cannot finish after a reboot on any build; see follow-up 1.

## Which receipts bind custody

| Receipt | Binds? | Why |
|---|---|---|
| Missing, cancelled, expired | No | No transfer is in progress. |
| Another handover's token | No | Verified across the only shipped `beginManagedLive` (`2c1bac6` through `7b4b27c`): it writes the journal before `prepare-handover`, and replaces an earlier journal only after removing one that was finished or void. So this journal never took custody. Its receipt is left in place. |
| Prepared or committed, daemon alive | Yes | That daemon holds the freeze. A prepared lease expires after two minutes. |
| Prepared or committed, daemon gone | No | Traced: a restarted daemon sees a non-released phase, adopts nothing and runs ordinary crash recovery. Followers get nothing from these phases once the daemon is gone. A changed process birth counts as gone only if `queue-daemon.lock` is free, because `lstart` follows the system time zone. |
| Released | Yes | Every daemon but T's refuses to start, and workers may be following it. Spent only if a newer build completed. After a reboot, T cannot adopt it either. |
| Consumed, consumer alive | Yes | The queue runs under that daemon. Another build refuses only until T has completed; after that it is the finished build's own daemon. |
| Consumed, consumer gone | No | The next daemon runs ordinary crash recovery. |

`retireSpentLive` removes a journal only when it binds nothing, under the install locks, using `managedRemoveFile`'s byte comparison. It never removes a receipt.

## Follow-ups, not fixed here

1. **A released receipt from an earlier boot stops every daemon.** The exact target's daemon exits 125 with "Queue handover belongs to another boot", T's resume fails the same way, and another build is refused and told to fetch T, which cannot finish either. Fix: check the boot before the digest in `queueAdoptHandover`, and treat a released receipt from an earlier boot as not adoptable. That also lets a reboot clear follow-up 2.
2. **T released, then the app removed.** The drained removal path lets N install, and the leftover receipt then leaves a daemon that exits 125 and restarts every 10 s with "The new daemon executable does not match queue handover custody".
   - **Likelihood is narrow.** Removal requires every job `done`, and a job finishing after release stays at `outcome`. So it needs a handover whose jobs were all `done`, a caller still reading, an interruption at released, then removal, and only older payload installers can start that.
   - **This PR makes one thing slightly worse.** The second setup used to refuse visibly. It now succeeds and removes the journal, so the main sign left is in `daemon.log`.
   - **A caller on a job in that receipt never returns,** from reading `execution.go:65`, not from a run.
3. **Busy lock during installation.** `execute` with `scheduling.lock` held returns "Another installer or queue change is running", which is a failure with Retry in the app. A test hit this once and I restructured the test. The holder is unverified. Fix later by treating it as a wait in the repair loop.
4. **Misleading caller message.** A caller whose job is still running can print "Queue daemon unavailable; command was not run directly" (`queue_daemon.go:67,79`), which reads as if the job never ran. This PR does not change that code, and #64 touches nearby behaviour.
5. **A finished journal can now refuse where `7b4b27c` did not.** That build skipped reading the receipt there. An unreadable receipt, a same-token digest change or a `ps` failure now refuses. A daemon cannot start on an unreadable receipt in either version, so setup shows that problem earlier.
6. **An unassessable journal kept by a run that skips resume** does not block that run, but the next upgrade and preflight refuse with a bare `unexpected EOF`. `7b4b27c` also refused that upgrade, with a different message.
7. **Reused PID with the lock held.** A dead daemon's receipt whose PID now belongs to another process, while a restarted daemon holds the lock, keeps binding and says "Finish that exact build", which T cannot do. It clears when the lock is free, the other process exits, or after a reboot. No message or doc says so. Failing closed is the right call here.
6. **Payload path behaviour change.** It previously took a handover when a job was done but its caller was still reading output. It now treats that queue as drained, so that caller can see a transport error during the outage. App bundles never had this protection.

## Fixed along the way

- **Finished journals.** Beginning a handover was the only code that removed journals that no longer bound anything. Left in place, they refused later upgrades.
- **Stranding.** A journal's target being older than the completed build, a receipt that binds nothing, a moved app and a dead daemon's prepared receipt no longer block setup. One exception remains: a finished journal with a live consumer, opened from a moved app of the same build, can still get "Invalid retained live-upgrade custody". That clears when the consumer exits or the app moves back.
- **Retirement errors.** Where resume is skipped, preflight never assessed the journal, so a journal setup cannot assess is kept with a message on stderr rather than failing a run preflight reported as clear.
- **Refusal.** It names the build through `installPendingError`, and preflight reports `pending-installation` with the release history and reveal actions.
- **Cleanup repair race.** `recoverManagedCleanup` failed setup if the job had been cleaned since inspection. Already clean now counts as success, and the repair loop inspects again.

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

Limits of this coverage:

- The in-tree daemons are built from this tree. The independent review separately drove a real `7b4b27c` binary through the phase scenarios.
- The live fixture uses one binary for every build, so it cannot show a digest refusal.
- `TestManagedLiveJournalOlderThanCompletedBuildCannotStrandUpgrades/phase-released` uses a fake daemon, so it passes in the exact state where a real daemon will not start (follow-up 2). It proves setup proceeds, not that the queue comes up.

## Evidence

Validation results are recorded in the PR description, pinned to its head.

## Next steps

1. Independent re-review and hosted CI on the new head.
2. Merge per the owner's authorization once both pass.
3. Follow-ups above, starting with 1, which also clears 2 after a reboot.
4. The stacked follow-up removing `--payload` and the unmarked install path.
