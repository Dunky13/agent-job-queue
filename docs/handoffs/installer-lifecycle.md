# Installer lifecycle

## Result

The installer now acts on one thing: the marked app bundle named by `--app-bundle`. `--payload`, `--prepare-replacement` and the unmarked install path are gone, along with everything that only served them. Every shipped flow keeps its behaviour, apart from the two fixes listed below.

Native code and the cask only ever passed `--app-bundle`. `--prepare-replacement` could only be parsed together with `--payload`, and nothing outside `internal/backend` passed it. The native `--prepare-app-replacement` route is a different command and stays: `managedStopMenu` calls it on the removal path.

## Shape

No lifecycle type was added, because deleting the other states left nothing for it to hold.

| Flag | Branch sites before | After | Why |
|---|---|---|---|
| `appBundle` | 39 | deleted | Always true. |
| `managed` | 11 | deleted | Always true. A bundle without the marker now fails validation. |
| `prepareReplacement` | 16 | deleted | Only reachable through `--payload`. |
| `payload` field | | deleted | Always derivable from `app`. |
| `unregisterServices` | 21 | 13 | The one remaining mode, set once in `InstallMain`. |
| `pendingIdentity`, `pendingBundle` | | kept | Mark the copy `pendingInstallation` points at a retained build. |
| `testing` | 10 | 4 | Test isolation, not a lifecycle. |
| `disablePhoneAccess` | 2 | 1 | A resolved option. |
| `automaticSetup` | 8 | 8 | See below. |

`validatePayload`, now `validateBundle`, returns the identity and `validateServices` returns the slot. Their callers store the result, so nothing is set as a side effect of validation, and `managedValidateExpected` no longer has to restore `identity` afterwards. `identity` and `serviceSlot` stay as fields because planning and `commandEnvironment` read them.

`automaticSetup` is still set by `repairAndExecute`, not at parse. Moving it would change what `finishPendingInstallation` and every repair caller see, and validation does not set it, so it stayed. A later PR can resolve it at parse.

`publishManaged` became `verifyManagedBundle`, because setup never replaces the bundle. `bundled`, the parameter on `installPayloadParent`, `installReadPayload` and `installValidatePayloadTree`, is gone because no production caller passed false.

## Deleted

- `execute`'s unmarked tail, with `support`, `copyFile`, `copyApp`, `installCopyTree`, `installCopyPayloadTree`, `writeConfig`, `writePlist`, `serviceDefinitions` and `publishApp`. `installCopyTree` and `writePlist` moved to test code, which still needs them.
- `resolvePayloadApp`, `validateAppPath`, `recoverManagedPublication`, `managedCheckPublicationParent`, the copy tail of `publishManaged` and almost all of `install_managed_copy.go`.
- The retained-build branch for an unmarked app in `pendingInstallation`. It was only reachable when N itself came from `--payload`. An app bundle resolves the retained app from `installation.json`, which only marked installs write.
- The `service-approval` report in `preflightManaged`, because planning stopped producing that error. Go and Swift still recognise the ID, which is now inert.

Still live, despite how they read: `serviceEnvironment`, `managedStopMenu`, `managedPublicationParent`, `managedBackupAccess`, `managedReadReceipt`, `managedCleanupBackup`, `retainManagedRuntime`, non-supervised activation, `pythonMigration`, `queueMigration`, `stopLegacy`, `removeLegacy` and `preflightLegacy`.

## Behaviour that changed

1. **Removal no longer refuses a helper service that was never registered.** `--unregister-services` failed with `The remote service is notFound` whenever phone access had never been registered, seen on a real Mac on build 166. For queue, resources and remote, a `notFound` lookup now passes during planning and in the final check, unless launchd still has the label loaded. `managedRemovalNotFound` holds that check. Three limits apply:
   - **The login item refuses on `notFound`.** It has no launchd label, so nothing shows that a miss means it is gone, and passing would retire wrappers and custody while it might still be registered. Setup requires `enabled` or `requiresApproval` to finish, so a completed setup does not leave this state, but a `brew upgrade` that was never opened or a reset of macOS background items can. Nothing shipped registers the item again by itself, so the refusal and the README give the route: quit the app and run `agent-heavy install --app-bundle PATH`. That route has run against mocked Service Management only.
   - **A miss straight after a real unregister fails closed,** because `managedAction` requires `notRegistered` at that moment. The next run finishes for a helper service, and for the login item once its lookup answers. `TestManagedRemovalRetryAfterNotFoundFollowsUnregister` verifies both.
   - Registered, enabled and awaiting-approval services are handled as before, and retained phone process records still refuse.
2. **`--recover disable-phone-access` now works on a packaged app.** It ran `Contents/Resources/payload/source/bin/agent-remote`, a path `appservices.ValidateBundle` forbids, so it could only fail. It now runs `Contents/MacOS/agent-remote`, as `executeManaged` does.
3. **Failure-path differences no shipped flow should notice.** A bundle without the marker is now refused instead of installing through the tail. When the bundle is invalid, preflight no longer also reports a spurious custom callback for an `~/.local/bin/agent-heavy` callback. A `validatePayload` failure now leaves `identity` empty, so a relocation in that state also reports that the app changed.

## Removal on a supervised app

Owner decision: removal checks the drain over RPC, then quits the app itself. `--unregister-services` on a running app now works like this:

1. **Recognise the lock holders.** For each lifetime lock that is busy with nothing loaded in launchd, `managedHeldByApp` finds the holders with `lsof`, then requires each one's exec path to be this bundle's helper and its parent's to be this bundle's `AgentQueue`. An exec path is the string `kern.procargs2` reports, not the file's identity, so it survives the bundle being moved, replaced or deleted. It fails closed, and it does not widen the threat model, since these processes already run as this user. Anything else, including no evidence, is still "unowned" and refuses. That includes the app's own helper running without the app.
2. **Ask the running daemon.** `managedRunningAppDrained` reads the snapshot over RPC. With active or queued jobs it refuses with a message that says so, and it never quits the app. If the daemon does not answer it also refuses.
3. **Quit, then unregister.** `managedStopMenu` now runs before `quiesceManaged`, because only quitting stops the helpers the app supervises and `quiesceManaged` needs their locks free. The native step waits for the app to exit, and the app exits only after its helpers stop.

The check runs twice, the second time under `scheduling.lock`. `agent-heavy run` and `lane acquire` both wait for that lock, so neither adds work between the check and the quit; `lane acquire` did not until the final review found a lease taken at that moment. A process calling the daemon's RPC directly could still do it, but no shipped client does. Removal now waits up to 10 seconds for the lock, the bound `LockPrivate` gives everyone else, because a running app's helpers hold it briefly during normal work.

`install-in-progress.json` is written only once the quit has succeeded. A refused quit, such as a running `--setup` change cancelling it after 30 seconds, therefore leaves a running app that still looks complete to the updater and to `agent-remote enable`, and a retry works.

**The app can be quit and removal still refuse.** All of these fail closed:

- A job still in `outcome`, or otherwise unfinished, in the stopped store.
- A phone access record staying active (`Phone access process cleanup is incomplete`).
- A Service Management failure after unregistering.
- A helper still holding its lifetime lock.

What clears each one differs, so `managedRemovalGuidance` says so:

- **An unfinished job or a stuck phone record:** retrying cannot clear these, because nothing is left running. The refusal, and an unfinished stopped store found while planning, say "Open Agent Queue, then run removal again". Opening the app clears an unfinished job: preflight is empty, setup succeeds and removes the marker. `TestManagedRemovalRefusalAfterTheQuitSaysToReopen` holds that. A phone record differs. With phone access on, supervisor startup reconciles it (`internal/remote/control.go`), which is traced but not run. With phone access off, `serve` returns before any cleanup, so reopening does not clear it: a probe got `disable phone access` on the next removal, and disabling it cleans up.
- **Anything else after the quit,** including `notFound` straight after an unregister and a failed status read: the refusal says to run removal again, and to open the app first only if it refuses the same way. A plain retry finishes `notFound` on a helper service, and a failed status read once the read answers. A login item still `notFound` then refuses with its own route, and a held lifetime lock refuses until the helper exits.
- **A login item it cannot verify:** the refusal names its own route, described above.

**Known limits:**

- A helper not parented by the new app, or a wrong loaded launchd label, also refuses setup after reopening, so opening the app does not clear those.
- An app opened in the very short window between `managedStopMenu` and the marker write takes the `.ready` path. The chance is very low.
- A helper still holding its lifetime lock refuses removal and, after reopening, setup. The first refusal names the lock (`Old service has not stopped`), and the retry says `An unowned queue process holds its lifetime lock`, which is the same cause in different words. Removal finishes once the lock is released.
- Returns after the final status check (wrappers, custody, backup cleanup, metadata) keep their own text and add no guidance. They report a change made during removal, and nothing here shows what clears each one. A wrapper edited during removal also refuses a retry and setup until it is restored.
- Planning's refusal under the lock (`managedRemovalGuidance` in `removeManaged`) has no test of its guidance, because reaching it needs a race.

**Preflight had the same check.** Run from outside against a healthy running app, it reported "An unowned queue process holds its lifetime lock", from the same `managedDrain` branch. It and setup now report `app-running` and `errInstallAppRunning`, and change nothing. Setup still cannot run beside the app's helpers.

**What the app does beside a running copy.** It stops and shows the message, with Retry in the startup menu and Check again in the `--setup` window. It does not wait, because only the user can clear this, and a silent wait would look like a hang. `app-running` therefore offers no `wait` action, and neither `install_approved_repair.go` nor `approvedRepairStep` recognises it, so both stop for review as they do for any unknown requirement. No Swift logic changed; `BootstrapTests` and `TestInstallerAutomaticSetupStopsForARunningApp` hold this. A newer copy opened beside the running recorded app gets the same report, because recognition checks the recorded app during a relocation. Either button then continues once the other copy has quit. The menu's wording for this has not been looked at on a real Mac.

**What ran against the real thing.** `install_managed_running_app_test.go` uses a real process tree: copies of the test binary at the bundle's `AgentQueue` and `agent-heavy` paths, with the real daemon and scheduler as children, real `flock`, real `lsof`, real exec paths and parents from the kernel, and real daemon RPC. Mutations removing recognition, the parent check or the drain check, or moving the quit after unregistering, each fail these tests.

**What has not run against the real thing:**

- Real Service Management, `codesign` and the native quit request are mocked, so nothing here shows `NSRunningApplication` quitting a real menu bar app. The fixture "app" stops its children on SIGTERM.
- The phone service in the test is a stand-in holding its lock and record, not `agent-remote serve`.
- `lsof` on a real signed app has not been seen.
- Kernel paths are compared exactly. A real app launched through LaunchServices should record its plain `/Applications/...` path, as `managedLoadedBundleProgram` already assumes, but only a run on a real Mac shows it.

## Tap companion PR

This landed as [tap #7](https://github.com/Dunky13/homebrew-agent-job-queue/pull/7) before this PR merged, reset to build 242. Its dispatched refresh passed and published nothing, and build 255 was the first build since 162 to pass `Verify Homebrew delivery` through dispatch. The steps stay here for any later change to `packaging/homebrew/`. Builds and tags below are as they stood when written.

The caveat in `internal/tooling/tooling.go` and `packaging/homebrew/refresh.go` now reads "Before removing the app, finish its jobs, then run this. It quits the app and removes its login item:".

**Changing the template alone would stop Homebrew publishing.** The tap's installed cask, at build 222, already differs from that build's release asset, a drift left by #56: its caveat says "finish setup" and the asset says "use its menu bar". Today the tap's old template renders its own cask exactly, so nothing notices. With the new template the refresh falls through to the historical asset check, and every run fails with `Current tap cask differs from its historical published release`. The independent review confirmed this by running it: exit 1 with the cask unchanged, exit 0 with the cask reset to the asset. `go test` passes either way, so the failure would only appear after merging.

Files, from this PR's final head:

| From | To, in the tap |
|---|---|
| `packaging/homebrew/refresh.go` | `refresh.go` |
| `packaging/homebrew/refresh_test.go` | `refresh_test.go` |
| `packaging/homebrew/test-cask.rb` | `test-cask.rb` |
| `packaging/homebrew/.github/workflows/refresh.yml` | `.github/workflows/refresh.yml` |
| `LICENSE` | `LICENSE` |
| Release asset `agent-job-queue.rb` of the tap's current build | `Casks/agent-job-queue.rb` |

Do not copy `SEED.txt`. Only `refresh.go`, `refresh_test.go` and the cask differ from what the tap has now. Setting the cask is the one exception to the rule against editing `Casks/`, because it restores the exact bytes of a published asset. Leave `Casks/agent-job-queue.json` alone.

Steps, once this PR's head is final:

1. In a checkout of the tap's `tap` branch, read the current build from the `version` and `url` lines of `Casks/agent-job-queue.rb`. It is `build-222-03fcbd2` as I write this, but **the target moves**: the tap's scheduled refresh can publish a newer build at any time, rendered by its old template and so drifting the same way, and builds 229 and 232 are already out.
2. `gh release download TAG --repo Dunky13/agent-job-queue --pattern agent-job-queue.rb --clobber --dir Casks`, using that build's tag, and copy the other files above.
3. Run `go test -p 2 refresh.go refresh_test.go`, `go vet refresh.go` and `HOMEBREW_DEVELOPER=1 go run -p 2 refresh.go` with Go 1.26 or later, `gh` and `brew`. Only the last one reads the cask, and it publishes nothing. It must exit 0 with `Validated ...; publication disabled`, or with `Published build verified; template change awaits a newer app release` if the tap is already on the latest release. With the cask unchanged it exits 1 with `Current tap cask differs from its historical published release`. The variable is required, as the tap's `refresh.yml` already sets it: without it the command exits 1 with `Required brew operation failed` even with the right cask, hiding brew's `Homebrew requires casks to be in a tap, rejecting`.
4. Open a PR against `tap` with signed, DCO-signed-off commits.
5. Just before merging, re-read the tap's head. If its build moved, repeat from step 1 against the new build. Then merge.
6. Dispatch `refresh.yml` on the `tap` branch and confirm it succeeds. If the schedule moved the tap in that gap, it fails with the error above; repeat from step 1.
7. Only then merge this PR, and do it promptly. The dispatch in step 6 publishes the latest release with the new caveat, "It quits the app and removes its login item", before any build has that behaviour. A Homebrew user in that gap is told something untrue until this PR's build ships. If the tap was already on the latest release, the dispatch publishes nothing and the caveat arrives with the next release.

**Dispatch has been dead since build 162.** Per the final review, logs for builds 166, 212, 220, 222, 229 and 232 all show `Tap tooling differs: refresh.go` and `exit status 1`, and all report success. The tap's `*/15` cron is active but fires every 2.9 to 10.2 hours, which looks like GitHub schedule throttling, so Homebrew gets new builds only at that pace.

**What the delivery check blocks, and the order.** `Verify Homebrew delivery` (`homebrew-sync`) is the last job in `distribution.yml`, and nothing needs it. The release is already published by then, and `ci-required` belongs to a different workflow. A tooling mismatch stops it before it dispatches a refresh, so that run is not dispatched or verified, and the tap's scheduled refresh keeps publishing with the tap's own tooling.

That mismatch exists today and was hidden: run 41625944175 logged `Tap tooling differs: refresh.go` and `exit status 1`, yet reported success, because the step had no `shell:` and so no pipefail through `tee -a`. This PR sets `shell: bash`, and `TestWorkflowPipelinesFailWhenAnyCommandFails` holds that for any step piping into `tee`.

**The companion should land first.**

- **Companion first, as above:** the check goes straight from hidden mismatch to passing when this PR merges. Between the two merges, `main` still lacks the fix, so its runs stay green and mismatched, as they do today.
- **This PR first:** publishing is not broken, but every distribution run shows red and dispatches nothing until the companion lands, and automatic retries add cost:
  - `scripts/distribution-recover.sh` classes a red `Verify Homebrew delivery` with signing and acceptance green as `delivery` and reruns it: first immediately, then at least 15 minutes apart, up to attempt 48 or 7 days for each run.
  - A mismatch stops before dispatch, in about 20 seconds per retry. A failure after dispatch would start a macOS tap run on each retry.
  - The observer requests at most five reruns per pass and reaches older builds first, so five or more red runs could delay an Apple signing continuation on a newer build.
  - A rerun after the companion lands should turn green within that budget.
- **The companion without the cask reset:** this is what does break publishing, so step 2 is not optional.

Anything else in the batch that changes `packaging/homebrew/` needs the same sync.

## Test porting

The test fixtures now build a marked bundle at `install.app`. Three helpers stand in for state only an older release can produce:

- `managedOlderRegistered`: a completed install with helpers registered under launchd.
- `managedOlderPublication`: a build published over the app, with its backup, receipt and marker.
- `installOlderServiceDefinitions`, `installOlderEmbedded`, `installOlderEmbeddedBlocked`, `installOlderUnmarkedState`: older launchd layouts.

**`testFinishPendingLiveResume` could not stay literally unchanged.** It reached "T interrupted after publishing" by running a real payload upgrade, and it names `i.appBundle` and `i.payload`. The diff in that function is three places: T's publication is seeded with `managedOlderPublication`, N is copied before being given build 125, and `i.app = moved` replaces the three-field assignment. Every assertion after the seeding is unchanged, including exit 23, one execution, the retained runtime and the relocation.

Limits of the seeding:

- The receipt comes from a helper using the current `managedPublicationReceipt` struct, not from a real older installer. The independent review of #67 drove a real `7b4b27c` binary, and repeating that against this tree would close the gap.
- The live scenarios now publish before seeding the journal. An older installer journalled first.
- `TestInstallerPackagedLiveResume` and `TestInstallerPackagedAppBundle` mock Service Management and `codesign`.

Seeding the marker first caused a flake while porting, in about one run in 12 on a busy host. The fixture's real daemon and scheduler take `scheduling.lock` during normal work, so setup can get the transient refusal in follow-up 3 of `retire-handover-begin.md`. `runPackagedSetup` retries that only when no pending marker predates the call, and on #67 none did. Now `managedOlderPublication` sets `seededPending`, which excuses that one marker for one call. `TestPackagedSetupRetriesBusyOnlyOverASeededMarkerOnce` holds the rule, and the strict case in `TestPackagedSetupNeverRetriesMutationOrUncertainHistory` is unchanged. The finish-pending test now logs the setup error, last phase and daemon log when that wait fails.

Backend top-level tests went from 581 to 578. Tests not listed below keep their names. Many had their setup ported and lost assertions on the launchd definitions that only the tail wrote. Each kept its subject: callback spellings, drain, Pueue import, slot, phone recovery, ownership refusal and so on.

| Before | After | Reason |
|---|---|---|
| `TestManagedBackupSubstitutionRefusedOnRetryRecoveryAndCleanup` | `TestManagedBackupSubstitutionRefusedOnRetryAndCleanup` | The recovery operation was payload only. |
| `TestManagedNotFoundCannotPrepareReplacement` | `TestManagedNotFoundStillLoadedCannotBeRemoved` | Now the fail-closed side of the removal fix. |
| `TestManagedApprovalFailureRetainsPendingAndNeverReregisters`, `TestInstallerAutomaticManagedApprovalWaitsWithoutReregistering` | `TestManagedResumedActivationWaitsForApprovalWithoutReregistering` | Registration only happens on resume now. |
| `TestManagedReadinessRejectsWrongLoadedHelper` | `TestManagedResumedActivationRejectsWrongLoadedHelper` | Same. |
| `TestManagedApprovedRepairFinishesRetainedPendingBuild` | `TestManagedSamePathReplacementCannotTakeOverRetainedPublication` | At the same path T's app is gone, so the property is refusal. Finishing T is covered by the relocation tests and the finish-pending test. |
| `TestManagedPreparationFreezesBeforeAppMoveAndPostflightRetries` | `TestManagedRemovalHoldsLocksThroughShutdown` | Kept the lock and custody property on removal. |
| `TestManagedPreparationPreservesPueueImportAndSlotForPostflight` | `TestManagedRemovalPreservesPueueImportAndSlotForReinstall` | Same. |
| `TestInstallerRemoteMigrationRespectsConcurrentDisableAndControlLock` | `TestInstallerRemoteControlLockSerializesSetup` | The concurrent disable half duplicated `TestManagedConcurrentRemoteDisableBeforeAndDuringMigration`. |
| `TestManagedAutomaticRetainedCandidateWaitsForApprovalThenUpgrades` | deleted | Setup no longer waits on a helper's approval. `TestManagedRecordedPendingDrainsBeforeRelocationWithoutWaitingForLoginApproval` holds the current rule. |
| `TestManagedApprovedRepairCompletesOwnedBackupBeforeUpgrade` | deleted | Duplicate of `TestManagedDraggedAppRecoversRecordedPublicationBackup`. |
| `TestManagedApprovedRepairFinishesRetainedLegacyBuild` | deleted | An unmarked retained build; see Deleted. |
| `TestInstallerRecoversInterruptedAppRename`, `TestManagedInterruptedPublicationRestoresKnownGoodThenRetries`, `TestManagedOwnedBackupRecoveryRestoresThenRetriesExactCandidate`, `TestManagedRecoveryRechecksBackupAfterSignatureAndTargetBeforeRename` | deleted | `recoverManagedPublication` never ran for an app bundle. |
| `TestInstallerClassicPayloadCanonicalBoundary`, `TestInstallerPreservesAppQuarantineMetadata`, `TestInstallerRemoteCopyFailurePreservesOldSupportAndRetriesMigration`, `TestManagedInstallerOpensAppOnlyAfterDurableCompletion`, `TestManagedPayloadRefusesUnwritableAppBeforeStoppingServices`, `TestManagedPayloadResolvesBothRecordedApplicationsLocations`, `TestManagedPayloadUpgradeAndPreflightUseRecordedTarget`, `TestManagedPrivateCandidateFailureCleanupAndSubstitution`, `TestManagedPublicationRechecksAccessAfterStaging`, `TestManagedPublicationRefusesReadonlyCodeBeforeStopping`, `TestManagedReplacementRetainsPreviousUntilSuccessfulRetry` | deleted | Payload copy and publication only. |
| `TestManagedSharedAppStagesThenMovesThroughFD`, `TestManagedSharedCopyBoundsAndOwnership`, `TestManagedSharedCopyDetectsReboundChild`, `TestManagedSharedCopyPinsParentDescriptor`, `TestManagedSharedCopyPreservesPrivateBoundary`, `TestManagedSharedCopyRefusesUnsafeEntries`, `TestManagedSharedNativeCopyRetainsMetadataAndVerifiesBeforeMove` | deleted | Tested the deleted copier. |
| | `TestManagedRemovalAfterUnregisterAcceptsNotFoundOnlyWhenUnloaded`, `TestManagedRemovalOfSupervisedApp` | New, for the removal fix. Both fail with the old checks restored. |
| | `TestPackagedSetupRetriesBusyOnlyOverASeededMarkerOnce` | New, for the seeded marker above. |
| | `TestLaneAcquireWaitsForSchedulingLock`, `TestHeldLeaseFailsTheRemovalDrainCheck` | New: a lease cannot start after removal's last check, and a held one fails it. |
| | `TestManagedRemovalRefusalAfterTheQuitSaysToReopen`, `TestManagedRemovalRefusalGuidanceAfterTheQuit`, `TestManagedRemovalRefusedQuitLeavesARunningAppComplete` | New, for what follows a refusal. |
| | `TestManagedResumedActivationNotFoundRemoteCannotHideOwnedWork` | Restores the resume-route gate's coverage, which the port lost. |
| | `TestManagedRemovalRefusesLoginItemItCannotVerify`, `TestManagedRemovalRetryAfterNotFoundFollowsUnregister` | New, for the login item and for retrying. |
| | `TestInstallerPreflightReviewsStateBesideAnInvalidBundle`, `TestInstallerPreflightLegacyDiagnostics` | New, for the invalid-bundle branch and the diagnostics that lost coverage. |
| | `TestInstallerAutomaticSetupStopsForARunningApp`, `TestInstallerNewerCopyBesideRunningRecordedAppStops` | New, for `app-running`. |
| | `TestManagedRemovalOfRunningAppQuitsItOnceDrained`, `TestManagedRemovalOfRunningAppStopsItsPhoneService`, `TestManagedRemovalOfRunningAppRefusesActiveJobs`, `TestManagedRemovalRefusesWhenDrainCannotBeVerified`, `TestManagedRemovalRefusesUnownedLockHolder`, `TestManagedSetupAndPreflightNameARunningApp`, `TestManagedPreflightStillReportsUnownedLockHolder`, `TestManagedRemovalWaitsBrieflyForSchedulingLock` | New, for removal from a running app. |

Subtests that went away:

- `TestManagedApprovedRepairPreservesUnrecoverablePending` kept `failed-retry` and `busy-queue`. The missing-app, replaced-app and stray-backup cases are the `missing`, `changed-build` and `unowned-backup` cases of `TestManagedRecordedPendingRefusesChangedOrUnsafeApp`.
- `TestManagedRefusedMenuShutdownPreservesPreviousBundle` kept only removal, the one route that asks the menu app to quit.

Some legacy preflight tests now seed phone access or a blocked older install, because that is how a marked bundle reaches `preflightLegacy`. `TestInstallerPreflightFindsExternalProcessesAndStoppingController` and `TestInstallerPreflightExplainsLegacyStateWithoutConfiguration` call `preflightLegacy` directly, because managed planning refuses those states with a general `managed-setup-blocked`. Their issue IDs are reachable only by another route into `preflightLegacy`, such as an invalid bundle or blocking phone access.

## What later PRs can rely on

- `install.app` is always the bundle in question, and the payload is always `Contents/Resources/payload` inside it.
- Any validated bundle is marked and self-contained. No code path copies an app or writes a launchd definition.
- `validateBundle` and `validateServices` return their results and set nothing.
- Setup registers only the login item, except when a live handover is being resumed.
- A lifetime lock held by this app's own child helper is the running app, not an unowned process. Setup refuses it as `errInstallAppRunning`, and removal quits it once drained.
- Publication receipts and backups are only ever read and retired. Nothing writes a new one.
- `install.run`'s argv sniffing and the Swift setup logic are unchanged, apart from removing the payload alternatives from `commandEnvironment`.

## One unexplained CI failure

`TestManagedRemovalRefusalAfterTheQuitSaysToReopen` failed once on the Intel shard, on `08a98ce` (run 41646896569): `An unowned queue process holds its lifetime lock`, before any quit, against a healthy fixture app. That check fails closed and nothing had changed.

It did not reproduce on Apple silicon: 360 holder checks over two 90-second probes, one throttled to background priority, and 16 throttled repeats of the test all stayed clean. The cause is unknown. One unproven idea is that `lsof` caught a helper's child between fork and exec, since the lock's descriptor closes only at exec, and that child's parent is a helper, not the app. The refusal now names the missing evidence, so a recurrence shows the cause. No retry was added.

## Line counts

| | Added | Removed | Net |
|---|---|---|---|
| Production | 548 | 1728 | -1180 |
| Tests | 2278 | 2142 | +136 |

This is one PR rather than two: with `--payload` gone, the small shape is what remains.

## Follow-ups, not fixed here

1. **The tap companion PR** for the changed cask template, which must merge first. See that section.
2. **An untracked build cannot install after removal.** Removal keeps `build-info.json` so it cannot reset version ordering, and setup refuses an untracked build over it. The check stays. `CONTRIBUTING.md` now gives the route: build with a higher `BUILD_NUMBER` and `SOURCE_COMMIT`. `TestManagedRemovalOfSupervisedApp` holds both sides.
3. **`native/AgentQueue.swift` and `TransportTests.swift` still classify `--payload` argv.** This is harmless, and belongs in a Swift PR.
4. **The `service-approval` ID** is still recognised in `install_approved_repair.go` and `AgentBootstrap.swift`, and nothing emits it now.
5. **`automaticSetup`** can move to parse. See Shape.
6. **Naming.** `validatePayload`, `prepareManaged` and `install_managed_copy.go` are now `validateBundle`, `removeManaged` and `install_managed_parent.go`. Left to rename: `readPayload`, `installReadPayload`, `installPayloadParent`, `installValidatePayloadTree`, and the `payload-invalid` ID, which the wire contract uses.
7. **The follow-ups in `retire-handover-begin.md` stand.** Its item 6, the payload path's different drain rule, no longer applies.
8. **`buildManagedLiveFixtureCLI` has a one-minute build limit,** and a race build was killed at that limit on a busy host, failing the iteration. This predates this PR. Hosted runners have not shown it.
9. **`lane acquire` can wait for `scheduling.lock` past a short `--timeout` or `--no-wait`,** up to 10 seconds and then exit 1, and up to three 15-second RPC attempts more before the `--timeout` clock starts. No shipped caller is known, so `docs/plugins.md` documents it rather than this PR changing lock handling during real-Mac testing.
10. **Removal could unregister a login item that reads `notFound`.** A Swift `mainApp.unregister` attempt might remove the need for the manual route. Not done here, by decision.
11. **Gaps the independent review listed, all present before this PR:** `validate-skips-pendingBundle`, `pending-skips-retained-backup-access`, `setup-skips-runtime-cleanup`, `plan-ignores-unsafe-runtime-bin` and same-path runtime reclamation.

## Evidence

Validation results are recorded in the PR description, pinned to its head.

## Next steps

1. A real older build through the seeded phases, if wanted.
2. A run of removal against a running app on a real Mac, on a released build. The last one was on a local build of `87599b9`.
3. The stacked PR for `install.run` argv sniffing and typed commands.
