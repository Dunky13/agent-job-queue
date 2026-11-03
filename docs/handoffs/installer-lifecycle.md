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

`validatePayload` now returns the identity and `validateServices` returns the slot. Their callers store the result, so nothing is set as a side effect of validation, and `managedValidateExpected` no longer has to restore `identity` afterwards. `identity` and `serviceSlot` stay as fields because planning and `commandEnvironment` read them.

`automaticSetup` is still set by `repairAndExecute`, not at parse. Moving it would change what `finishPendingInstallation` and every repair caller see, and validation does not set it, so it stayed. A later PR can resolve it at parse.

`publishManaged` became `verifyManagedBundle`, because setup never replaces the bundle. `bundled`, the parameter on `installPayloadParent`, `installReadPayload` and `installValidatePayloadTree`, is gone because no production caller passed false.

## Deleted

- `execute`'s unmarked tail, with `support`, `copyFile`, `copyApp`, `installCopyTree`, `installCopyPayloadTree`, `writeConfig`, `writePlist`, `serviceDefinitions` and `publishApp`. `installCopyTree` and `writePlist` moved to test code, which still needs them.
- `resolvePayloadApp`, `validateAppPath`, `recoverManagedPublication`, `managedCheckPublicationParent`, the copy tail of `publishManaged` and almost all of `install_managed_copy.go`.
- The retained-build branch for an unmarked app in `pendingInstallation`. It was only reachable when N itself came from `--payload`. An app bundle resolves the retained app from `installation.json`, which only marked installs write.
- The `service-approval` report in `preflightManaged`, because planning stopped producing that error. Go and Swift still recognise the ID, which is now inert.

Still live, despite how they read: `serviceEnvironment`, `managedStopMenu`, `managedPublicationParent`, `managedBackupAccess`, `managedReadReceipt`, `managedCleanupBackup`, `retainManagedRuntime`, non-supervised activation, `pythonMigration`, `queueMigration`, `stopLegacy`, `removeLegacy` and `preflightLegacy`.

## Behaviour that changed

1. **Removal no longer refuses a service that was never registered.** `--unregister-services` failed with `The remote service is notFound` whenever phone access had never been registered, seen on a real Mac on build 166. A `notFound` service now passes both before and after unregistering, unless launchd still has its label loaded. `managedRemovalNotFound` holds that check. Registered, enabled and awaiting-approval services are handled as before, and retained phone process records still refuse.
2. **`--recover disable-phone-access` now works on a packaged app.** It ran `Contents/Resources/payload/source/bin/agent-remote`, a path `appservices.ValidateBundle` forbids, so it could only fail. It now runs `Contents/MacOS/agent-remote`, as `executeManaged` does.
3. **Failure-path differences no shipped flow should notice.** A bundle without the marker is now refused instead of installing through the tail. When the bundle is invalid, preflight no longer also reports a spurious custom callback for an `~/.local/bin/agent-heavy` callback. A `validatePayload` failure now leaves `identity` empty, so a relocation in that state also reports that the app changed.

## Removal on a supervised app

Traced through the code and exercised by `TestManagedRemovalOfSupervisedApp` and the packaged test. It has not been run against real Service Management on a real Mac.

With the app quit, it works: planning accepts `notFound` for queue, resources and remote, the offline drain gate runs against the stopped store, only the login item is unregistered, the menu app is asked to quit, and the wrappers, `installation.json` and the marker are removed. The app, queue store and private data stay.

**With the app running, it refuses.** Its daemon holds `queue-daemon.lock` and launchd knows nothing of it, so `managedDrain` stops with "An unowned queue process holds its lifetime lock". That is before the quit step runs, and nothing changes. The README said the command quits the app, which was wrong for any running app, so the README now says to quit first. See follow-up 1.

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

Backend top-level tests went from 500 to 477. Tests not listed below keep their names. Many had their setup ported and lost assertions on the launchd definitions that only the tail wrote. Each kept its subject: callback spellings, drain, Pueue import, slot, phone recovery, ownership refusal and so on.

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

Subtests that went away:

- `TestManagedApprovedRepairPreservesUnrecoverablePending` kept `failed-retry` and `busy-queue`. The missing-app, replaced-app and stray-backup cases are the `missing`, `changed-build` and `unowned-backup` cases of `TestManagedRecordedPendingRefusesChangedOrUnsafeApp`.
- `TestManagedRefusedMenuShutdownPreservesPreviousBundle` kept only removal, the one route that asks the menu app to quit.

Some legacy preflight tests now seed phone access or a blocked older install, because that is how a marked bundle reaches `preflightLegacy`. `TestInstallerPreflightFindsExternalProcessesAndStoppingController` and `TestInstallerPreflightExplainsLegacyStateWithoutConfiguration` call `preflightLegacy` directly, because managed planning refuses those states with a general `managed-setup-blocked`. Their issue IDs are reachable only by another route into `preflightLegacy`, such as an invalid bundle or blocking phone access.

## What later PRs can rely on

- `install.app` is always the bundle in question, and the payload is always `Contents/Resources/payload` inside it.
- Any validated bundle is marked and self-contained. No code path copies an app or writes a launchd definition.
- `validatePayload` and `validateServices` return their results and set nothing.
- Setup registers only the login item, except when a live handover is being resumed.
- Publication receipts and backups are only ever read and retired. Nothing writes a new one.
- `install.run`'s argv sniffing and the Swift setup logic are unchanged, apart from removing the payload alternatives from `commandEnvironment`.

## Line counts

| | Added | Removed | Net |
|---|---|---|---|
| Production | 291 | 1627 | -1336 |
| Tests | 1124 | 2158 | -1034 |

This is one PR rather than two: with `--payload` gone, the small shape is what remains.

## Follow-ups, not fixed here

1. **Removal refuses a running app, with a misleading message.** The cask caveat, "Before removing the app, stop its services with", does not say to quit either, and the tap tooling pins that text. The options are to keep quit-first and improve the message, or to verify the drain over RPC when the lifetime lock is busy and then quit the app. The second makes the old README promise true, and is what I would do.
2. **An untracked build cannot install after removal.** Removal keeps `build-info.json` so it cannot reset version ordering, and setup refuses an untracked build over it. The check stays. `CONTRIBUTING.md` now gives the route: build with a higher `BUILD_NUMBER` and `SOURCE_COMMIT`. `TestManagedRemovalOfSupervisedApp` holds both sides.
3. **`native/AgentQueue.swift` and `TransportTests.swift` still classify `--payload` argv.** This is harmless, and belongs in a Swift PR.
4. **The `service-approval` ID** is still recognised in `install_approved_repair.go` and `AgentBootstrap.swift`, and nothing emits it now.
5. **`automaticSetup`** can move to parse. See Shape.
6. **Naming.** `install_managed_copy.go` now holds only `managedPublicationParent`, and its name should change.
7. **The follow-ups in `retire-handover-begin.md` stand.** Its item 6, the payload path's different drain rule, no longer applies.
8. **`buildManagedLiveFixtureCLI` has a one-minute build limit,** and a race build was killed at that limit on a busy host, failing the iteration. This predates this PR. Hosted runners have not shown it.

## Evidence

Validation results are recorded in the PR description, pinned to its head.

## Next steps

1. Independent review of the head, including a real older build through the seeded phases if wanted.
2. Hosted CI on this PR, which is the first run of the ported packaged tests on Intel and Apple Silicon.
3. The owner's decision on follow-up 1.
4. The stacked PR for `install.run` argv sniffing and typed commands.
