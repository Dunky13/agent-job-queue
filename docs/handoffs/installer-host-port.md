# Installer host port

Step 2 of the installer refactor, after `installer-service-port.md`: setup asks the host about processes, lock holders and bundle signatures through typed operations instead of building `ps`, `lsof` and `codesign` argv at each call site. Behaviour, error messages and call order are intended to be unchanged.

## Result

- `internal/backend/install_host.go` defines `installHost` (`Processes()`, `LockHolders(path)`, `VerifySignature(path)`), `installProcess` and the `installMacOSHost` adapter. `installer.queries` is nil in production and `host()` supplies the adapter. The adapter runs through `installer.run` with the same argv, so executable resolution, environment, timeouts and output limits are the same.
- The adapter owns the parsing. `installServices` is unchanged and separate.
- `GLOSSARY.md` is unchanged: no new domain term.

## Call sites moved

| Function | Operation |
| --- | --- |
| `stopLegacy` | processes |
| `preflightLegacyProcesses` | processes |
| `managedHeldByApp` | lock holders |
| `managedVerify` | verify signature |

## Preserved parsing behaviour

- Process list: `Command` is the text after the process ID, with outer blanks trimmed and inner spacing kept. A line with fewer than two fields or a non-integer process ID is returned with `Unparsed` set and the whole line as `Command`. The check before migration still stops on such a line when it names old queue code. Preflight still skips it. Every searched text starts with `pueued` or an absolute path, so no match can depend on the process ID prefix.
- Lock holders, no holder: `lsof` exits 1 when nothing holds the path. Before this change that surfaced as `Installer command lsof failed: exit status 1`, and it still does. `no holder found` remains reachable only when `lsof` succeeds with empty output. No exit code is interpreted.
- Lock holders, invalid entry: the adapter fails with the same `holder %q is not a process` on the listed text, and also returns the holders before it. `managedHeldByApp` checks those first, so the first problem in listing order is still the one reported.
- Signature check: the command error is returned as is and `managedVerify` adds the same prefix.

## Tests

`internal/backend/install_host_test.go` holds the in-memory adapter, `TestInstallMacOSHost` (argv, parsing and failures over the command seam) and `TestInstallerLegacyProcessChecks`. The last one is new coverage: preflight refuses old queue processes before setup reaches `stopLegacy`, so removing the check there broke no existing test.

The base install fixture uses the in-memory adapter and records `{"host", operation, path}` in the same call log as services and commands. Its command fake fails the test if `ps`, `lsof` or `codesign` reach it. The in-memory adapter refuses a listed process without a positive ID and command, a holder ID of 1 or lower and verification of a missing path. A lock path with no stated holder fails, as `lsof` does. The earlier fixture returned empty success there; no assertion depended on it.

Migrated files, all `internal/backend/*_test.go`:

| File | Change |
| --- | --- |
| `install_test.go` | base fixture; the unowned Python process is stated as running |
| `install_preflight_test.go` | the external controller process is stated as running |
| `install_managed_test.go` | signature fault is `host verify-signature` |
| `install_managed_review_test.go` | the bundle changes during verification of the app |
| `install_relocation_test.go` | signatures under the old bundle are refused |
| `install_recovery_safety_test.go` | the invalid bundle assertion matches the host record |
| `install_managed_running_app_test.go` | holders come from the real adapter on a separate installer, so `lsof` stays real; the order assertion matches the host record |

The real `lsof` in the running app tests now runs through the production runner, as in setup: standard error is captured with the output, the command has the 30 second bound and a failure carries the `Installer command lsof failed:` prefix.

Still on commands, on purpose:

- `TestInstallMacOSHost`: the command fake is the adapter's own seam.
- `TestManagedHeldByAppRefusesWithoutEvidence`: states raw `lsof` output and covers the adapter's parsing together with the caller's messages. It matches no argv.
- `TestInstallerCommandDeadlinesBoundServiceProbes`: sample argv for the generic runner.
- The `ps` calls in the live runtime and live upgrade tests, and the `codesign --sign` step of the packaged bundle test: the tests run those tools themselves.

## Validation

Run locally on macOS, base `88d4071`, code commit `f4461cb`:

| Command | Result |
| --- | --- |
| `gofmt -l internal` | no output |
| `go vet ./internal/backend/...` | pass |
| `go test -p 2 -parallel 2 -count=1 -json -skip '^TestInstallerOwnershipParsesRealPrivateLaunchctlService$' ./internal/backend/...` | 1 package ok (377 s); 714 tests passed, 0 failed, 3 skipped; 1010 subtests passed |

- Deliberate breakages each failed a test: verifying through the runner, no signature check, a signature check during preflight of an invalid bundle, preflight or migration ignoring the process list, preflight reporting an unparsed line and no lock holder check.
- Skipped by their own conditions: `TestRealDaemonPreservesRawUnixDirectoryExecutableAndFilenames`, `TestInstallerPackagedLiveResume` and `TestInstallerPackagedAppBundle`.
- The test excluded with `-skip` bootstraps a temporary agent into the user's real `launchd`, which this task excluded from local runs. Hosted CI still selects it.
- Not run: race detection, the CI partition scripts, Intel hardware and an independent review.

## Remaining steps

1. Controller calls: `--service-management`, `--prepare-app-replacement` and `privateManagedCommand`.
2. Remove the test-mode argv rewrite from `installer.run` and the "User With Spaces" check in `install_private_services.go`.
