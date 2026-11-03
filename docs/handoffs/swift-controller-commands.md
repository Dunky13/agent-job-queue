# Swift controller commands

Base: `7b4b27c`. Branch: `arch/swift-controller-commands`. Authorized endpoint:
draft PR. Independent review, merge and release are separate steps.

## Result

`QueueClient.Command` now owns each controller command's argv, controller, input,
timeout, cancellation, reporting and failure decoder. Call sites name a factory
such as `.status` or `.installPreflight(target)`. `runBlocking` applies that
policy to `runProcess`, a byte-level runner that takes an executable, argv, input,
deadline and capture mode, and knows nothing about what a command means.

- The transport no longer reads argv. `isAutomaticSetup`, `isSetupPreflight`,
  `isNativeServiceStatus`, `timeout(for:)`, the inline duplicate, `Budget` and the
  `arguments.first == "profiles"` decoder choice are gone.
- `Transport` stays as the executable injection point, because setup uses it in
  production. Its timeout is now an optional override, so setup no longer computes
  a deadline from argv.
- `QueueBootstrapRequest` carries a typed command. The new `QueueBootstrapInstall`
  holds the selected installer and names the setup forms it can send.
- `RemoteAccessStore` takes a `command:` closure like the other stores. This was a
  small change.
- "Decoder" here means failure decoding, which is what the transport used to
  choose. Stores still decode their own success responses.

## Old-to-new table

Reporting sets the output cap, deadline wording and failure window. `queue` and
`updates` reject output over 2 MB and show the first 1,000 bytes of a failure.
`setup` keeps the last 32 KB and shows the last 8,000 bytes of a failure. All
commands use the `agent-heavy` controller unless the row says `agent-remote`.

| Call site | Argv | Factory | Timeout | Cancellation | Reporting | Failure decoder |
| --- | --- | --- | --- | --- | --- | --- |
| `QueueStore` status loader | `status --json` | `.status` | 8 s | discard | queue | text |
| `QueueStore.completeAction` | `next\|last\|cancel\|start ID` | `.jobAction(_:id:)` | 25 s | retain | queue | text |
| `QueueStore.dispatchResume` | `resume` | `.resume` | 25 s | retain | queue | text |
| `QueueStore.completeLimit` | `limit N` | `.limit(_:)` | 25 s | retain | queue | text |
| `QueueStore.dispatchScheduling` | `scheduling MODE` | `.scheduling(_:)` | 25 s | retain | queue | text |
| `RemoteAccessStore.refresh` (`agent-remote`) | `status --json` | `.remoteStatus` | 8 s | discard | queue | text |
| `RemoteAccessStore` toggle and enable (`agent-remote`) | `enable`, `disable` | `.remoteEnabled(_:)` | 125 s | retain | queue | text |
| `RemoteAccessStore.configure` (`agent-remote`, stdin) | `configure` | `.remoteConfigure(_:)` | 125 s | retain | queue | text |
| `AgentIntegrationStore` | `agents status --json` | `.agentsStatus` | 25 s | retain | queue | text |
| `AgentIntegrationStore.save` | `agents install\|remove --runner ID --json` | `.agents(_:runner:)` | 25 s | retain | queue | text |
| `LearningStore.readCatalog` | `profiles list --offset N --limit N --query Q --json` | `.profilesList` | 25 s | discard | queue | structured |
| `LearningStore.readDetail` | `profiles show ID --json` | `.profilesShow(_:)` | 25 s | discard | queue | structured |
| `LearningStore` member detail | `profiles show ID --family F --json` | `.profilesShow(_:family:)` | 25 s | discard | queue | structured |
| `LearningStore` member page | `profiles show ID --members-query Q --members-offset N --json` | `.profilesShow(_:membersQuery:membersOffset:)` | 25 s | discard | queue | structured |
| `LearningStore.mutate` (stdin) | `profiles edit\|merge\|split\|combine\|reset ID --json` | `.profilesChange` | 25 s | retain | queue | structured |
| `QueueUpdateStore.refreshInstalled` | `version --json` | `.version` | 25 s | retain | queue | text |
| `QueueUpdateStore.checkForUpdates` | `updates check --json` | `.updatesCheck` | 130 s | retain | updates | text |
| `QueueUpdateStore.downloadUpdate` | `updates download --build N --json` | `.updatesDownload(build:)` | 130 s | retain | updates | text |
| Bootstrap `--setup`, reviewed install and service setup | `install --app-bundle PATH` | `.install` | 330 s | retain | setup | text |
| Bootstrap preflight | `install --app-bundle PATH --preflight --json` | `.installPreflight` | 330 s | discard | setup | text |
| Bootstrap automatic install | `install --app-bundle PATH --repair-and-continue` | `.installRepairAndContinue` | none | stop while waiting | setup | text |
| Bootstrap phone-access recovery | `install --app-bundle PATH --disable-phone-access` | `.installDisablingPhoneAccess` | 330 s | retain | setup | text |
| Bootstrap repairs | `install --app-bundle PATH --recover ACTION [--job-id N] [--expected-revision R]` | `.installRecovery` | 330 s | retain | setup | text |
| Bootstrap leftover registrations | `--service-management status queue\|resources\|remote` | `.serviceStatus(_:)` | 5 s | discard | setup | text |

"Stop while waiting" sends SIGUSR1, keeps reading, and throws `CancellationError`
after exit only if the last reported state was a wait. The reader still parses the
output before trimming it, as before. The sentinel parser is now a closure supplied
by the command layer.

### Fail-closed forms

- A relative path gets the conservative row for both `--preflight --json` and
  `--repair-and-continue`: 330 s and retained, as before.
- `.setup(_:)` gives any other setup argv the same conservative policy, so extra
  options and malformed forms stay finite and retained.
- The `--payload` form is not sent by the app. `InstallTarget` keeps it because the
  existing cancellation assertion covers it and the Go CLI still accepts it.

### One intended difference

An absolute path containing NUL used to get an infinite deadline with retained
cancellation, because the inline duplicate skipped the NUL check that
`isAutomaticSetup` had. It now gets 330 s and retained, which is what a single
validation gives. This cannot change runtime behaviour: the path comes from a file
URL, and `Process` raises an uncaught `NSException` on such an argument, so no
child ever launches under either policy.

## Test changes

- `TransportTests.policyChecks` runs every row above through the real process
  runner, and asserts all policy fields, the cancellation outcome, the deadline
  wording and child reaping, and the failure decoder.
- It also covers a transport without a timeout and `runInstaller`, so the command's
  own deadline is exercised.
- `BehaviorTests` now covers `RemoteAccessStore` through its injected closure,
  including password handling and redaction.
- The stub's `mutation` and `profile-failure` scenarios now drain stdin. Otherwise
  a child could exit before a row's input was written and fail with EPIPE.

Five assertions were restated because the type no longer accepts the old inputs:

| Old assertion | New assertion |
| --- | --- |
| `timeout(for: ["status"], budget: .updates) == 8` | `.status.timeout == 8` |
| `timeout(for: ["updates", "download"]) == 25` | `.version.timeout == 25`, the app's only non-`updates` command in that store |
| Malformed `--service-management` argv yields 330 s | `.setup(argv)` yields 330 s and retained; `.serviceStatus` accepts only the `Service` enum |
| Ambiguous setup argv (`--repair-and-continue` plus `--preflight --json`) yields 330 s, `TransportTests.swift:33` | `.setup(argv)` yields 330 s. **This no longer tests classification:** `.setup` is finite and retained by construction, and no factory can build this form, so it now only pins the `.setup` default. |
| Preflight argv plus an extra option keeps its accepted operation, `TransportTests.swift:136` | `.installRecovery` and `.installDisablingPhoneAccess`, the real forms for two of the three options, still run through the real runner with no discard. `--unexpected` has no factory, so it uses `.setup(argv)`, which is retained by construction and pins only that default. |

Two existing assertions were changed because they were flaky on the base commit too:

"Read finishes after its direct child exits while an independent daemon holds
inherited output" required the read to finish within 0.5 s. That limit includes
launching two large test binaries, so it measured host load. With the queue's
other jobs running, it failed on unmodified `7b4b27c` in 4 of 6 runs and on this
branch in 6 of 6. The fixture daemon now holds the inherited output until the test
releases it, or for 5 s if never released. The read's own deadline is 2 s, so a
read that waited for EOF still fails, without any wall-clock limit in the test. No
timeout was increased. Measured read times matched on both sides, 0.46 to 0.53 s on
base and 0.47 to 0.52 s on the branch, so the refactor did not slow this path.

"Deadline and cleanup survive ... controller I/O" also failed on the base commit,
in 2 of 8 runs. At the 0.15 s test deadline a child can be killed before it writes
its marker, and the check treated the missing marker as a failure. With a marker,
it still requires that exact child to be gone. With or without one, it now requires
`proc_listchildpids` to list no direct child, including zombies. Under a scratch
mutation with no marker and no teardown, this check fails, while the earlier
marker-only version passed the whole run.

The stub now writes its marker with `.atomic`, so a child killed mid-write cannot
leave an empty marker. A killed atomic write can leave a temporary file beside the
marker, so the selection checks now ask whether any file names a live child.

`run-resume-ui-tests.sh` builds `ResumeUITests.swift` against the historical sources
at `--before-ref`, where the store injects `[String]`. Its `before` build now adapts
that one closure, the way it already shims the historical `QueueStore`. The
reference itself is unchanged.

## Evidence

All jobs ran through the shared queue while other agents' work kept host load
around 10 to 13.

| Check | Job | Result |
| --- | --- | --- |
| `scripts/test-native.sh` | 2773 | PASS, exit 0, 1,007 PASS lines, 40 policy rows through the real runner |
| `scripts/validate.sh` | 2801 | FAIL, exit 1, at `go test`, so later steps did not run |
| The `validate.sh` lines after `go test`: Go build, web and browser tests, native tests, universal app build, installer and DMG | 2810 | PASS, exit 0 |
| `run-menu-bar-ui-tests.sh`, base and branch | 2802 | PASS on both: 32 passed, 0 failed, 8 skipped |
| `run-menu-bar-ui-tests.sh`, branch, earlier run | 2788 | FAIL on `singleClickShowsRealPopoverImmediately`, which allows 0.03 s after the click |
| `run-resume-ui-tests.sh --before-ref 2295abe`, both builds | 2818 | PASS, exit 0: `before` 4 of 4 capture checks, `after` 10 of 10 |
| `run-bootstrap-ui-tests.sh`, base and branch | 2780 | FAIL on both, at the same check in the first scenario |
| Bootstrap UI with one condition relaxed in scratch, branch | 2794 | PASS, all 12 scenarios, 92 PASS lines |

### `validate.sh`

This diff changes no Go files. `internal/backend` and `internal/tooling` hit the
12-minute limit with no assertion failure. In `internal/remote`, two tests failed
when `remote configure` was killed, and one exceeded its 30 s admission wait at
46.6 s. Validation jobs on at least four neighbouring branches also ended with
exit 1 in the same period. Hosted CI runs the Go partitions on separate machines
and is the evidence to check for them. Job 2810 ran the script's later lines
unchanged, so they have local evidence even though the script as a whole failed.

### Bootstrap UI runner

The committed runner cannot pass on this OS, and that is not caused by this PR. A
bare accessory app with one status item shows the same extra visible
`NSStatusBarWindow` at `(0, -33)` on macOS 27.0.1. It comes from AppKit on this OS,
and `NSApp.windows` is per-process, so no other app causes it.
`BootstrapUITests.swift:172` allows only the item's own window, which is wrong for
this OS on base and branch alike. It needs its own fix and is not part of this PR.
To exercise the later scenarios, a scratch copy allowed any `NSStatusBarWindow`;
that change is not in this PR.

These fixtures inject the setup executor. They verify setup and quit flows and the
argv the model sends, but do not launch a real installer. Real process behaviour
is covered by `TransportTests`. No check here ran against the installed queue.

### Not exercised

- The menu bar runner's 8 skipped checks. Five need event-posting permission that
  this terminal lacks.
- Local Go partitions, which are left to hosted CI.

## Remaining work

1. Independent Standards and Spec review of the draft PR.
2. Check hosted CI, especially the Go partitions that did not finish locally.
3. Separately, fix `BootstrapUITests.swift:172` for the extra AppKit status bar
   window on macOS 27.
4. Separately, consider a causal check for the 0.03 s popover settle, which is
   load-sensitive.
5. Separately, the `agent-heavy run` client exits with 125 on a transient daemon
   socket timeout while its job stays queued.
6. The owner has authorized merge once review and CI pass. Release needs separate
   authorization.
