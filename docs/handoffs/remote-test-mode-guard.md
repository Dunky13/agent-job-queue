# Installer test mode could control real phone access services

## Verdict

Confirmed on `origin/main` at `9f57b7c`, and still present at `ed2e75d` after
the installer controller port (#103), which this branch is rebased on. The route
existed in the product code. No test or CI job completed it with a real
`agent-remote`.

## Route

Line numbers refer to `ed2e75d`.

1. `InstallMain` sets test mode from `AGENT_QUEUE_INSTALL_TEST_MODE=1`
   (`internal/backend/install.go:1698`). Test mode does not require a private
   home.
2. The installer runs `<app>/Contents/MacOS/agent-remote disable` or `migrate`
   (`install_managed.go:1506` and `:1641`, `install_preflight.go:927`).
3. The controller port sends only Service Management and app replacement calls
   to the private fixture (`install_private_services.go`). `run` executes the
   helper as given (`install.go:252-256`).
4. `commandEnvironment` strips `AGENT_REMOTE_*` and sets the installer's `HOME`
   (`install.go:214-224`). The helper is therefore outside remote test mode.
5. `newPaths` loads `managedApp` from `installation.json` in that home
   (`internal/remote/control.go:93-99`).
6. `serviceAction` (`control.go:753`) then takes one of two real routes:
   - Managed metadata present: `managedServiceAction` runs
     `<managedApp>/Contents/MacOS/AgentQueue --service-management status remote`
     and, if registered, `unregister remote`
     (`internal/remote/managed_service.go:16-76`). A `notFound` status also runs
     `/bin/launchctl print` (`:88`).
   - Metadata absent (not in the original report):
     `/bin/launchctl bootout gui/<uid>/io.went.agent-remote`
     (`control.go:784-786`). Enabling could also reach `bootstrap` (`:789`) for
     a helper outside an app bundle. This is the likely branch for legacy phone
     access recovery, which runs before managed metadata is published. A test
     `launchctl` on `PATH` cannot intercept the absolute path.

## Bundle and identifier

`managedApp` is the `app` field of
`<installer home>/.local/share/agent-heavy/installation.json`. It must sit in
`/Applications` or `<home>/Applications` (`internal/appservices/appservices.go:188`),
and `ValidateBundle` accepts only the production identifier
`io.went.agent-queue` (`:242`). With a real home, that is the developer's
installed app. With a private home, metadata may still name a bundle under
`/Applications`. The launchd label on the second route is the production
`io.went.agent-remote`.

How macOS treats an unregister request from a second copy with the same
identifier was not tested, because doing so would touch real Service Management.

## Which tests reached it

- `TestInstallMainLegacyPhoneAccessRecoveryDiagnostic`
  (`internal/backend/install_test.go:1599`) goes through `InstallMain` and the
  real command runner, and executes `agent-remote disable` in test mode. Its
  `agent-remote` is a shell stub that exits 99. The stub was the only barrier.
  The fixture writes no `installation.json`, so a real binary there would have
  taken the `bootout` route. CI runs it in the `macos-tests` job's `core-1` or
  `core-2` shard (`scripts/test-go-runtime.sh`), on both macOS runners. Its name
  does not match the `^TestInstaller` pattern in `scripts/test-installer.sh`, and
  the `installer` shard runs only the live resume test.
- `TestInstallerPackagedAppBundle` and `TestInstallerPackagedLiveResume` use the
  real packaged app but replace the installer command runner, so the installer
  never executes `agent-remote disable` or `migrate` there.
- `TestRemotePackagedGateway` runs the packaged `agent-remote` with
  `AGENT_REMOTE_TEST_MODE=1`, so it stays in the remote fixture.

The packaged tests run in the app packaging job's `scripts/test-installer.sh`
step and in `distribution.yml`. No script or workflow sets installer test mode
itself.

## Fix

- `newPaths` records `AGENT_QUEUE_INSTALL_TEST_MODE=1`, and `serviceAction`
  refuses every action when it is set. This is the single entry point for both
  routes, so it fails closed for Service Management and `launchctl`.
- `commandEnvironment` sets that variable for the installer's `agent-remote`
  calls whenever the installer is in test mode. It already survived the
  `AGENT_REMOTE_*` strip, but the installer state is now the source of truth.
- With test mode off, behaviour is unchanged.

Effect: in installer test mode, a real `agent-remote disable` or `migrate` now
fails, and the installer reports the failure. `disable` still writes the phone
settings in the installer home and runs its own process cleanup there before
reporting. No current test relied on success. A future test that needs success
must supply a private fixture; the refusal should not be relaxed for it.

## Out of scope

- Process cleanup in `stopOwnedService` still acts on processes recorded in the
  installer home. It does not use Service Management or `launchctl`, but with a
  real home it would act on real phone access processes.
- The installer's own `--service-management` calls go through the controller
  port. This change does not touch them.

## Tests

- `internal/remote/installer_mode_test.go`:
  - `TestInstallerTestModeRefusesServiceControl`: managed and unmanaged routes,
    with every command replaced by a recorder.
  - `TestInstallerTestModeControllerNeverRunsAgentQueue`: the whole `disable`
    and `migrate` commands with real process execution, against an `AgentQueue`
    that only records its arguments.
- `internal/backend/install_remote_test_mode_test.go`: the installer's real
  command runner delivers the signal to a stub helper in test mode, and does not
  add it otherwise.

Each managed test checks the ownership metadata before running, so a missing
guard cannot fall through to `launchctl`.

Without the fix all three failed (run with `/bin/launchctl` execution denied by
`sandbox-exec`). The recorders showed `launchctl bootout` and `bootstrap` on the
unmanaged route and `--service-management status remote` on the managed route.
The stub helper received no signal. All pass with the fix.

### Real binary probe

The first version of the backend test built `./cmd/agent-remote` from this tree
and ran it through the installer's command runner with a recording `AgentQueue`.
Without the fix it recorded `--service-management status remote` for both
`disable` and `migrate`; with the fix both were refused and nothing was recorded.
It is not committed: a cold-cache build of that binary took 54 s on an Apple M2
Pro (`-p 2`, 943 packages) inside a package that otherwise does not compile
Caddy, which made its 2 minute bound unsafe on slower CI runners.

## Validation

- `gofmt -l cmd internal`: clean.
- `go vet -p 2 ./internal/remote/... ./internal/backend/...`: passed.
- `go run -p 2 ./cmd/queue-tool check-source`: passed.
- Through the shared queue (job 8220, on the `ed2e75d` base):
  `go test -p 2 -parallel 2 -timeout 20m -skip '^TestInstallerOwnershipParsesRealPrivateLaunchctlService$' ./internal/backend/... ./internal/remote/...`
  passed (`internal/backend` 401 s, `internal/remote` 25 s). This run did not use
  `-race`.

Not run: `-race`, the packaged app tests, the full lint script, native tests and
hosted CI. No independent review has run.

## Next steps

1. Run independent Standards and Spec review.
2. Push the branch and open a PR.
3. Wait for required CI before merging.
