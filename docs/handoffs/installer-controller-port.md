# Installer controller port

Step 3 of the installer refactor, after `installer-host-port.md`: setup asks the menu bar app's own executable for service management and replacement preparation through typed operations, and the private lifecycle fixture is an adapter chosen when the installer is built. `installer.run` no longer rewrites argv in test mode. Error messages and call order are intended to be unchanged.

## Result

- `internal/backend/install_controller.go` defines `installController` (`ServiceManagement(app, action, service)`, `PrepareAppReplacement(app)`), the accessor `appController()` for the `installer.controller` field, the test-mode choice `selectTestLifecycle()`, the production adapter `installAppExecutable` and `installRefusingController`.
- `internal/backend/install_private_services.go` is now `installPrivateController`, the private lifecycle adapter. `privateManagedCommand` is gone.
- When `AGENT_QUEUE_INSTALL_TEST_MODE=1`, `InstallMain` records the choice as it builds the installer: private lifecycle if `AGENT_QUEUE_INSTALL_TEST_MANAGED=private-native-lifecycle`. `appController()` then supplies the private adapter for that choice, the refusing one for any other test-mode installer (including one that never made the choice) and the production one otherwise.
- The adapter is built from the installer that asks, not stored. Pending recovery (`pendingInstallation`) copies the installer and points the copy at the retained app, so an adapter holding the original would refuse the copy's service commands and prepare the wrong app. The `controller` field is only set by tests.
- Both operations take the app path. Status and unregistration also address a previous bundle during relocation, and replacement preparation names the app the caller means.
- The production adapter runs through `installer.run` with the same argv, so environment, timeouts and output limits are the same. It owns the `appservices.Arguments` validation that the two callers did before.
- The private adapter runs its tool through `installer.runAs`, the part of `run` below the test seam, naming the app command the tool stands for. As before, that command selects the isolated environment and output limit, the tool argv selects the generic 30 second bound, and failures name `service-management`.
- `GLOSSARY.md` is unchanged: no new domain term.

## Call sites moved

| Function in `install_managed.go` | Operation |
| --- | --- |
| `managedStatus` | service management, status |
| `managedAction` | service management, register or unregister |
| `managedStopMenu` | replacement preparation |

## Guard mapping

Every refusal returns the same message as before, now held in `errInstallNativeRefused`. A service command that `appservices.Arguments` rejects keeps its own `invalid service management command`, from every adapter, as when the callers validated first.

| Guard in `privateManagedCommand` | Now | Test case |
| --- | --- | --- |
| installer is in test mode | `tool()`, each use | `not testing` |
| `AGENT_QUEUE_INSTALL_TEST_MANAGED=private-native-lifecycle` | selection in `selectTestLifecycle`, rechecked in `tool()` each use | `managed unset`, refusing adapter test |
| at least three arguments, exact replacement shape | held by the typed operations | not expressible |
| replacement target is `install.app` | `app != install.app` in replacement preparation | foreign app case |
| executable is the app's `AgentQueue` and its app is `install.app` | the adapter builds the executable path; `app != install.app` for service management; an unclean app refused for replacement, as the old path comparison did | foreign app cases, `unclean app` |
| exact `appservices.Arguments` service command | `appservices.Arguments` must succeed and its error is returned | unclean app, unknown action and unknown service cases |
| home is named `User With Spaces` | `tool()` | `foreign home name` |
| root has prefix `agent-queue-install-test-` | `tool()` | `foreign root name` |
| `AGENT_QUEUE_INSTALL_TEST_TOOLS` is `root/tools` | `tool()` | `foreign tools` |
| root is canonical | `tool()` | `root through a symlink` |
| root, home and tools are owner-only 0700 directories | `tool()` | `shared` and `narrowed` cases, `symlink tools` |
| tool is an owner-only, executable, single-link regular file | `tool()` | `shared tool`, `inert tool`, `linked tool`, `symlink tool`, `missing tool` |
| `root/native.sock` is an owner-only socket | `tool()` | `shared socket`, `file as socket`, `missing socket` |

Not covered by a test: the three owner checks, since an unprivileged test cannot give files away. The directory and regular file type checks cannot be isolated from the mode checks with a symlink, which has its own mode.

## Open decision: the home name check

The `User With Spaces` home basename check is kept, unchanged, in the private adapter. Production setup no longer runs it.

No replacement was found that is at least as strong without a test-chosen name. No other guard pins which child of the fixture root is the home: any 0700 directory directly under an accepted root satisfies them all. Dropping the check, or swapping it for a different condition (such as "home is not the account's home directory"), accepts some fixtures that are refused today. The root prefix is also a name the tests choose and is likewise unchanged.

Options for the owner: keep it (current), rename it to a purpose-built fixture name, or accept a weaker guard set explicitly.

## Argv matching left

`run` no longer matches argv and nothing chooses a different program. Three helpers it calls still pick policy from argv, unchanged:

- `installCommandContext`: the native bounds for `--service-management` and `--prepare-app-replacement`, and the controller bound for `agent-remote migrate` and `disable`.
- `commandEnvironment`: the isolated target environment for replacement preparation, service management and `agent-heavy status --json`, and the recovery environment for `agent-remote` and `pueue`.
- `commandOutputLimit`: the legacy `pueue status` history budget.

## Behaviour notes

- Observed, not changed: service management on a bundle other than `install.app` (relocation) gets the native bound but not the isolated environment, so ambient `AGENT_HEAVY_*` and `PUEUE_*` values reach it.

## Tests

- `install_controller_test.go`: the in-memory adapter; production argv, validation and failures over the command seam; the production environment through the real runner against a private script; selection, the refusing adapter, its handling of an invalid command and that test mode never runs the app executable.
- `install_private_services_test.go`: the end to end private tool test, ported to the typed operations and extended with both operations on a copied installer and the failure label; one case per guard, each starting from an accepted fixture.
- Removing or breaking each of these made a test fail: each guard testable above, the selection in `InstallMain` and in `appController()`, binding the private adapter to the asking installer, the validation error in the private and refusing adapters, the test-mode refusal in `appController()`, the mutation predicate used by the packaged retry gate, and the isolated environment in `run` and in the private adapter.

The base install fixture uses the in-memory adapter, which records `{"controller", action, service, app}` and `{"controller", "prepare-app-replacement", app}` in the shared call log, holds service states and refuses commands `appservices.Arguments` rejects. Its command fake fails the test if either argv reaches it. A fixture failure string now also applies to controller records.

Migrated, all `internal/backend/*_test.go`: `install_test.go`, `install_managed_test.go`, `install_managed_lifecycle_test.go`, `install_managed_review_test.go`, `install_managed_running_app_test.go`, `install_managed_shared_test.go`, `install_managed_supervised_test.go`, `install_approved_repair_test.go`, `install_live_runtime_test.go`, `install_pending_test.go`, `install_recovery_safety_test.go`, `install_relocation_test.go`, `install_service_notfound_test.go`, `install_stop_test.go`, `managed_runtime_fixture_test.go` and `packaged_setup_retry_test.go`. The packaged retry gate tests now record their mutations through the adapter, so the gate is checked against the real record shape.

Still on argv, on purpose:

- `TestInstallAppController` and `TestInstallAppControllerIsolatesEnvironment`: the production adapter's own seams.
- `TestManagedInstallerComposesRemoteAndNativeDeadlines` and the `commandEnvironment` tests: sample argv for the generic runner.
- `TestInstallMainLegacyPhoneAccessRecoveryDiagnostic`: unchanged; it drives the private fixture end to end through `InstallMain`.

## Coverage in CI

No script or workflow sets `AGENT_QUEUE_INSTALL_TEST_MANAGED`. The private fixture is exercised only by the two Go tests above that run it (`TestManagedPrivateNativeCommandsCannotReachRealServices` and `TestInstallMainLegacyPhoneAccessRecoveryDiagnostic`) plus the guard cases. `TestInstallerPackagedAppBundle` and `TestInstallerPackagedLiveResume` use the in-memory controller with real packaged helpers, and run in `scripts/test-installer.sh` (CI and distribution workflows).

## Validation

Run locally on macOS 27.2.1 (Apple Silicon), base `0bf247c` (open PR #102), code commits `718b601` and `0b85674`, run on `0b85674`:

| Command | Result |
| --- | --- |
| `gofmt -l internal` | no output |
| `go vet ./internal/backend/...`, also with `GOOS=linux` | pass |
| `go test -p 2 -parallel 2 -count=1 -json -skip '^TestInstallerOwnershipParsesRealPrivateLaunchctlService$' ./internal/backend/...` | 1 package ok (425 s); 719 tests passed, 0 failed, 3 skipped; 1033 subtests passed |
| `scripts/build-app.sh` (ad hoc), then `go test -p 2 -parallel 2 -count=1 -v -run '^TestInstallerPackaged(AppBundle\|LiveResume)$' ./internal/backend` with `AGENT_QUEUE_TEST_APP_BUNDLE` | both pass, 3 subtests passed |

- The full suite, the app build and the packaged run went through the shared heavy-job queue, on the code commit's tree.
- Skipped in the full suite by their own conditions: `TestRealDaemonPreservesRawUnixDirectoryExecutableAndFilenames` and the two packaged tests run separately above.
- The test excluded with `-skip` bootstraps a temporary agent into the user's real `launchd`, which this task excluded from local runs. Hosted CI still selects it.
- Not run locally: `scripts/test-installer.sh` as a whole (it selects that `launchd` test), the packaged gateway test, race detection, the CI partition scripts and Intel hardware.

## Remaining steps

None in the accepted refactor. The home name check above is open for the owner. Review, push and a pull request stacked on #102 are not done.
