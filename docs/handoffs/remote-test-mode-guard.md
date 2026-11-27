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
  service routes, so it fails closed for Service Management and `launchctl`.
- `commandEnvironment` sets that variable for the installer's `agent-remote`
  calls whenever the installer is in test mode. It already survived the
  `AGENT_REMOTE_*` strip, but the installer state is now the source of truth.
- With test mode off, behaviour is unchanged apart from the added refusal reason
  in installer errors.

## Review follow-up

An independent review of `8a5bfc3` found that the command as a whole did not fail
closed: with a real home, `disable` still rewrote `remote.json`, deactivated
`processes.json` and signalled recorded processes before or after the refusal.

| Severity | Finding | Disposition |
| --- | --- | --- |
| Medium | Test mode accepted a real home | Fixed. `installPrivateTestLayout` (`install_private_services.go`) is shared by `InstallMain` and the private adapter's `tool()`. `InstallMain` refuses to start outside the layout. `tool()` keeps its managed value, tool file and socket checks. Both now also refuse a root containing the path list separator, which would split the tools folder out of the test `PATH` |
| Medium | Refusal came after side effects | Fixed as a backstop. In installer test mode `runMain` refuses every action except `status` (help returns earlier) before `prepare`, the lock or any write: `configure`, `enable`, `disable`, `migrate`, `serve`, `child`. The `serviceAction` check stays |
| Medium (older) | A bare tool missing from the test folder resolved to the real one, such as `/bin/launchctl` | Fixed. In test mode `executable` searches only the first `PATH` entry, the test tools folder. This covers `launchctl`, `ps`, `pueue` and any later bare name |
| Low | The installer dropped the refusal reason | Fixed. `runRemote` appends the decoded `error` field, on one line and at most 500 characters, at all three `agent-remote` call sites |
| Low | Loop variable shadowed `testing`; end-of-line comment | Fixed |

The owner decided to keep the `User With Spaces` check.

The failure state file is no longer written for a refusal in installer test mode,
because that write is itself a side effect in the installer home.
`TestInstallerTestModeControllerNeverRunsAgentQueue` read that file; it now reads
the command output and asserts that the file is absent.

## Limits

- The tool lookup rule covers commands the installer resolves itself. `PATH` in
  the child environment still ends with the real tool folders, so a script in the
  test folder that calls a bare name can still reach them.
- Absolute tool paths (`/usr/sbin/lsof`, `/usr/bin/codesign`) are unaffected.
  Both only read.
- In installer test mode every step that runs agent-remote `disable`, `migrate`
  or `enable` now fails, so a test-mode install or removal that reaches one of
  them cannot complete. That is intended: no test-mode run may change phone
  access.
- A private acceptance harness outside this repository would now need every bare
  tool it exercises in its tools folder, and must not expect test-mode runs to
  pass an agent-remote lifecycle step. No script or workflow in this repository
  sets installer test mode; only Go tests do.
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

Each managed test checks the ownership metadata before running, and its
`AgentQueue` reports no registration, so a missing guard stays on the managed
route and cannot fall through to `launchctl`. The unmanaged route appears only in
`TestInstallerTestModeRefusesServiceControl`, where every command is a recorder.
`enterInstallerTestMode` also fails unless the controller sees installer test
mode, so a renamed variable cannot silently disarm both guards.

Without the fix all three failed (run with `/bin/launchctl` execution denied by
`sandbox-exec`). The recorders showed `launchctl bootout` and `bootstrap` on the
unmanaged route and `--service-management status remote` on the managed route.
The stub helper received no signal. All pass with the fix.

### Review follow-up tests

Each failed with the five changed production files restored to `8a5bfc3` in a
scratch copy (with a stand-in `runRemote` that drops output, so the tests
compile), and passes with the fix.

- `TestInstallMainTestModeRequiresPrivateTestHome`: five layouts outside the
  rules, each refused with the home left empty.
- `TestInstallerTestModeResolvesToolsOnlyInTestFolder`: `launchctl`, `ps` and
  `pueue` stand-ins later on the path are not resolved or run in test mode.
- `TestInstallerReportsRemoteHelperReason`: the reason is appended, also after
  other output, and cannot start a new line.
- `TestInstallerTestModeLeavesSettingsAndProcesses`: every changing action leaves
  the home byte for byte, two test-owned processes running, and the managed app's
  recording `AgentQueue` unrun. `runMain` builds its own controller, so the test
  uses real processes instead of the `killFn` and `killGroupFn` seams, and the
  managed fixture instead of a command recorder.
- `TestInstallPrivateTestLayoutRequiresCleanHome`: the shared layout check
  refuses an unclean or relative home by itself, without relying on the caller.
- `TestInstallerReportsRemoteHelperReason` also refuses a failure object with
  fields other than `error`, matching the house decoding rule.

`TestInstallerAppBundleCLIValidatesBundleBeforeSetup` moved to the private layout
and now asserts that it reaches bundle validation (the error names the bundle).

### Real binary probe

The first version of the backend test built `./cmd/agent-remote` from this tree
and ran it through the installer's command runner with a recording `AgentQueue`.
Without the fix it recorded `--service-management status remote` for both
`disable` and `migrate`; with the fix both were refused and nothing was recorded.
It is not committed: a cold-cache build of that binary took 54 s on an Apple M2
Pro (`-p 2`, 943 packages) inside a package that otherwise does not compile
Caddy, which made its 2 minute bound unsafe on slower CI runners.

## Validation

For the review follow-up:

- `gofmt -l cmd internal`: clean.
- `go vet -p 2 ./internal/backend/... ./internal/remote/...`: passed.
- `go run -p 2 ./cmd/queue-tool check-source`: passed.
- Through the shared queue (job 8228):
  `go test -p 2 -parallel 2 -count=1 -skip '^TestInstallerOwnershipParsesRealPrivateLaunchctlService$' ./internal/backend/... ./internal/remote/...`
  passed (`internal/backend` 536 s of the default 600 s package timeout,
  `internal/remote` 31 s). This run did not use `-race`.

Not run: `-race`, the packaged app tests, the full lint script (its web lint
needs `node_modules`, absent in this worktree), native tests and hosted CI.

## Next steps

1. Run independent Standards and Spec review of the follow-up.
2. Push the branch and open a PR.
3. Wait for required CI before merging.
