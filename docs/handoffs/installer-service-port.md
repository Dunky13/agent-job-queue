# Installer service port

Step 1 of the installer refactor: setup reaches the login session's services through a typed port instead of building `launchctl` argv at each call site. Behaviour, error messages and call order are intended to be unchanged.

## Result

- `internal/backend/install_launchctl.go` defines `installServices` (`Print(label)`, `Bootout(label)`, `Bootstrap(definition)`) and the `launchctl` adapter. `installer.services` is nil in production and `loginServices()` supplies the adapter. The adapter runs through `installer.run`, so executable resolution, environment, timeouts and output limits are the same.
- `errInstallServiceNotLoaded` is the port's answer for an absent service. The mapping moved from `installer.run` into the adapter's `Print` and still uses `installServiceAbsent`.
- One change to the runner: after a failed command that did not time out, `run` now returns the captured output together with the error, so an adapter can classify the failure. Every other caller returns or discards on error without reading it.
- `installer.domain()` moved beside the adapter. Service definition recovery still calls it to read the header of the print output.
- `GLOSSARY.md` gained **Service**.

## Call sites moved

| Function | Operation |
| --- | --- |
| `loaded` | print |
| `validatePhoneAccessRecovery` | print |
| `stopLegacy` | print, bootout |
| `stopManagedLegacy` | bootout (phone access, legacy queue and sampler, menu) |
| `inspectService` | print |
| `recover` (`repair-service`) | bootstrap |

## Tests

`internal/backend/install_services_test.go` holds the in-memory adapter and `TestInstallLaunchctlServices`, which checks the adapter's argv and not-loaded mapping. The base install fixture uses the in-memory adapter and records `{"service", operation, label or definition}` in the same call log as other commands, so ordering assertions still compare one sequence. Per-test overrides now wrap `services.print` or `services.bootout`. The fixture's command fake fails the test if `launchctl` reaches it, which keeps the earlier `kickstart` refusals meaningful.

Migrated files, all `internal/backend/*_test.go`:

- Fixtures: `install_test.go` (base fixture and its overrides), `install_managed_test.go` (managed fixture and its overrides).
- Print or bootout overrides: `install_live_runtime`, `install_managed_lifecycle`, `install_managed_review`, `install_managed_supervised`, `install_phone_compatibility`, `install_preflight`, `install_recovery_safety`, `install_relocation`, `install_service_notfound`, `install_service_recovery`.
- Call log assertions only: `install_app_bundle`, `install_approved_repair`, `managed_runtime_fixture`, `packaged_setup_retry`.

Still on `launchctl` argv, on purpose:

- `TestInstallLaunchctlServices`: the command fake is the adapter's own seam.
- `TestInstallMainLegacyPhoneAccessRecoveryDiagnostic`: runs `InstallMain` against a fake `launchctl` executable, so it covers the real adapter end to end.
- The command environment and timeout bounds test in `install_test.go`: sample argv for the generic runner.
- `TestInstallerOwnershipParsesRealPrivateLaunchctlService`: unchanged integration test of the print parser against real `launchd`. It does not use the installer.

## Validation

Run locally on macOS, base `5ff1b82`:

| Command | Result |
| --- | --- |
| `gofmt -l internal` | no output |
| `go vet ./internal/backend/...` | pass |
| `go test -p 2 -parallel 2 ./internal/backend/... -skip '^TestInstallerOwnershipParsesRealPrivateLaunchctlService$'`, production change only, no test edits | 1 package ok, 0 failed (416 s) |
| The same command on the migrated tree (`13a320c`) | 1 package ok, 0 failed (389 s) |
| The same command with `-json`, on `1105f16` | 712 tests passed, 0 failed, 3 skipped; 1006 subtests passed |

- Skipped by their own conditions: `TestRealDaemonPreservesRawUnixDirectoryExecutableAndFilenames`, `TestInstallerPackagedLiveResume` and `TestInstallerPackagedAppBundle`.
- `TestInstallMainLegacyPhoneAccessRecoveryDiagnostic` fails when `run` drops the output of a failed command again, so the moved not-loaded mapping is covered through a real process.
- The skipped test bootstraps a temporary agent into the user's real `launchd`, which this task excluded from local runs. Hosted CI still selects it.
- Injected forbidden calls failed the migrated checks: an extra bootout (`TestManagedFreshInstallUsesOnlyBundleHelpers`, `TestManagedLegacyOwnershipIsRecheckedImmediatelyBeforeShutdown`, `TestInstallerPhoneAccessRecoveryRejectsUnownedServices`), an extra bootstrap (`TestInstallerRecoveryRejectsSymlinkedExecutableParent`) and a direct `launchctl kickstart` through the runner.
- Not run: race detection, the CI partition scripts and Intel hardware.

## Review dispositions

An independent Opus review (Standards and Spec) found no high findings. Each finding was fixed on this branch:

| Severity | Finding | Fix |
| --- | --- | --- |
| Medium | The **Service** glossary entry named services current setup no longer registers, and used launchd vocabulary | The entry now says older versions registered the queue, resource controller and phone access, and current setup retires them and registers only the login item |
| Low | The in-memory adapter accepted a bootout of an unloaded service and a bootstrap of a loaded one, which `launchctl` refuses | Both now fail the operation |
| Low | No fixture test checked which definition `repair-service` bootstraps | `TestInstallerPreflightModernUpgradeRetainsActiveJobsAndOffersStoppedServiceStart` asserts the bootstrap of `io.went.agent-heavy.plist` under the launch directory |

## Remaining steps

1. Process list (`ps`) behind a typed port.
2. Lock holders (`lsof`).
3. Signature check (`codesign`).
4. Controller calls: `--service-management`, `--prepare-app-replacement` and `privateManagedCommand`.
5. Remove the test-mode argv rewrite from `installer.run` and the "User With Spaces" check in `install_private_services.go`.
