# Guided installation and upgrade recovery

First-launch setup checks the installation before changing it. When requirements need attention, the setup window lists them together with the affected jobs, service, or file. A clean preflight permits an installation attempt; the installer repeats its authoritative checks before publication, and the app verifies the installed state before opening the queue.

## Available choices

- **Wait and continue** rechecks transient blockers for a bounded period. **Stop waiting** cancels the read-only check without stopping queue jobs or an accepted repair. Paused and stashed legacy jobs still need attention in the existing queue.
- **Review jobs** shows the unfinished legacy jobs. Cancelling a selected job requires confirmation because running work may be stopped. Normal Go upgrades retain active jobs and do not require this legacy drain.
- **Run verified cleanup** uses the established ownership-checked cleanup path for a selected completed job. Setup does not remove receipts, leases, or lock files merely to bypass a blocker.
- **Start verified service** starts a confirmed stopped, canonical installed Go service. An already loaded service or uncertain job attempt is not restarted by recovery; its diagnostics remain available for inspection.
- **Disable phone access and continue** preserves credentials, configuration, and trusted certificates. It covers the Python migration blocker and a failed enabled remote migration during a Go upgrade. Access can be enabled again in settings after setup.
- **Review configuration** opens the affected file and explains the proposed callback replacement. **Back up and replace callback** requires a separate confirmation, preserves the complete original privately, and checks that the configuration is still the version reviewed. The custom callback's behavior will no longer run after replacement. Arbitrary custom service definitions and missing configuration remain guided repairs rather than guessed defaults.
- **Show affected file**, **Open Applications**, and **Download current release** provide a path forward for unsafe private paths, misplaced apps, damaged packages, or a downgrade attempt. Use Finder to copy and reopen a correctly signed app; setup does not silently replace another app, relax signature checks, or take ownership of unknown files.

Recovery failures retain the original diagnostic and refresh the preflight choices. A successful recovery is followed by a new preflight before installation continues. None of these choices authorizes an unidentified process to be killed, a shared lock to be removed, or a custom configuration to be silently overwritten.

## Controller contract

The bundled `agent-heavy` controller supports the existing installation targets with a read-only preflight mode:

```sh
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --preflight --json
agent-heavy install --payload DIRECTORY --preflight --json
```

A successful invocation returns a schema-versioned JSON report with an `issues` array. Each issue has a stable ID, title, explanation, optional affected path or jobs, and a list of supported actions. An empty array allows an installation attempt; it does not establish completed installation.

Recovery operations use the same installation target and a fixed action name:

```sh
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover cancel-job --job-id JOB_ID
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover cleanup-job --job-id JOB_ID
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover repair-service
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover disable-phone-access
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover reset-callback --expected-revision REVIEWED_SHA256
```

Each operation rechecks current eligibility and ownership. Callback replacement requires the revision supplied by the reviewed report. Recovery returns a fresh report and does not itself finish installation. Navigation and waiting are native UI operations, not arbitrary controller command execution.

The native transport treats only the exact read-only preflight command as cancellable. Installation and recovery retain the existing accepted-mutation behavior. Unsupported schemas, unknown action IDs, malformed job IDs, and invalid paths are rejected before execution.

## Verification

Use the shared heavy-job queue for the finite validation batch documented in CONTRIBUTING.md. Headless bootstrap regressions cover report parsing, multiple blockers, explicit consent, recovery rechecks, waiting cancellation, stale responses, and installation readback. Transport tests exercise real private children to distinguish cancelled preflight reads from accepted mutations. The graphical bootstrap fixture uses disposable homes, injected setup executors, and simulated queue jobs. Backend tests use private installation and launch-service fixtures rather than the installed queue.
