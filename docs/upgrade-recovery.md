# Guided installation and upgrade recovery

Normal app launch checks installation and background-service authorization without installing or repairing files. Use the installer, or explicitly invoke the app with `--setup`, to enter the installation workflow. Explicit setup checks the installation before changing it. When requirements need attention, the setup window lists them together with the affected jobs, service, or file. A clean preflight permits an installation attempt; the installer repeats its authoritative checks before publication, and the app verifies the installed state before opening the queue.

## Approve repair once

Choose **Repair and continue** to approve the verified repairs needed for startup in one step. Setup restores missing standard service files, runs verified cleanup for completed jobs, starts a verified stopped service, and disables phone access when migration requires it. Credentials and trusted certificates are preserved. Phone access remains off until enabled again in settings. Lost custom startup settings cannot be recovered from missing files.

After approval, setup rechecks ownership before each operation, waits for active work, and continues through installation and readback automatically. **Stop waiting** cancels waiting and revokes that approval. An accepted repair is allowed to finish. A failed repair, repeated operation without progress, unknown ownership, custom configuration, or deliberately paused/stashed work stops the automatic flow for review. Approval never authorizes cancelling jobs, resuming held work, or replacing a custom callback.

The ZIP installer's **Install** button approves the same bounded repair policy. Command-line callers can explicitly opt in with the trailing `--repair-and-continue` flag. Controller commands keep their existing finite deadline; app waiting remains cancellable while active jobs finish.

## Available choices

- **Wait and continue** rechecks transient blockers for a bounded period. **Stop waiting** cancels the read-only check without stopping queue jobs or an accepted repair. Retained receipts and lease markers can belong to active work as well as completed cleanup. The report shows the actual blocking path; external lease paths require manual inspection because setup only opens files within the account or app folder. Paused and stashed legacy jobs still need attention in the existing queue.
- **Review jobs** shows the unfinished legacy jobs. Cancelling a selected job requires confirmation because running work may be stopped. Managed Go upgrades also require a drained queue before changing service registration. Active work blocks those changes.
- **Run verified cleanup** uses the established ownership-checked cleanup path for a selected completed job. Setup does not remove receipts, leases, or lock files merely to bypass a blocker.
- **Start verified service** starts a confirmed stopped, canonical installed Go service. An already loaded service or uncertain job attempt is not restarted by recovery; its diagnostics remain available for inspection.
- **Restore verified service files** is offered when a background service is still loaded but its startup file is missing, and its loaded command and installation paths can be verified. Confirmation restores standard startup files using the verified commands. Lost custom startup settings cannot be recovered. Existing files are preserved, and recovery does not stop, restart, or reload services. Setup checks all migration requirements again afterward, including unfinished jobs and phone access. A loaded service that cannot be verified still requires operator review.
- **Disable phone access and continue** preserves credentials, configuration, and trusted certificates. It covers the Python migration blocker and a failed enabled remote migration during a Go upgrade. Access can be enabled again in settings after setup.
- **Review configuration** opens the affected file and explains the proposed callback replacement. **Back up and replace callback** requires a separate confirmation, preserves the complete original privately, and checks that the configuration is still the version reviewed. The custom callback's behavior will no longer run after replacement. Arbitrary custom service definitions and missing configuration remain guided repairs rather than guessed defaults.
- **Show affected file**, **Open Applications**, and **Download current release** provide a path forward for unsafe private paths, misplaced apps, damaged packages, or a downgrade attempt. Use a complete signed installer to finish setup; a Finder copy alone does not register its services. Setup does not relax signature checks or take ownership of unknown files.

Recovery failures retain the original diagnostic and refresh the preflight choices. A successful recovery is followed by a new preflight before installation continues. None of these choices authorizes an unidentified process to be killed, a shared lock to be removed, or a custom configuration to be silently overwritten.

If an interrupted publication retains an owned backup and receipt, finish the exact pending setup before preparing an external replacement or removing the runtime. Those operations refuse before service changes while the backup is needed for recovery. Do not delete the receipt or pending marker to bypass the refusal. A partial first installation without a backup can still unregister denied services; it retires candidate-only custody while preserving the app and queue data.

## Controller contract

The bundled `agent-heavy` controller supports the existing installation targets with a read-only preflight mode:

```sh
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --preflight --json
agent-heavy install --payload DIRECTORY --preflight --json
agent-heavy install --payload DIRECTORY --repair-and-continue
```

A successful invocation returns a schema-versioned JSON report with an `issues` array. Each issue has a stable ID, title, explanation, optional affected path or jobs, and a list of supported actions. An empty array allows an installation attempt; it does not establish completed installation.

Recovery operations use the same installation target and a fixed action name:

```sh
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover cancel-job --job-id JOB_ID
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover cleanup-job --job-id JOB_ID
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover repair-service
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover restore-service-definitions
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover disable-phone-access
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover reset-callback --expected-revision REVIEWED_SHA256
```

Each operation rechecks current eligibility and ownership. Callback replacement requires the revision supplied by the reviewed report. Recovery returns a fresh report and does not itself finish installation. Navigation and waiting are native UI operations, not arbitrary controller command execution.

The native transport treats exact read-only preflight and native service-status commands as cancellable. Installation and recovery retain the existing accepted-mutation behavior. Unsupported schemas, unknown action IDs, malformed job IDs, and invalid paths are rejected before execution. Executable locations with group- or world-writable ancestors are refused; setup does not change shared Applications permissions to bypass that refusal.

## Verification

Use the shared heavy-job queue for the finite validation batch documented in CONTRIBUTING.md. Headless bootstrap regressions cover report parsing, multiple blockers, explicit consent, recovery rechecks, waiting cancellation, stale responses, and installation readback. Transport tests exercise real private children to distinguish cancelled preflight reads from accepted mutations. The graphical bootstrap fixture uses disposable homes, injected setup executors, and simulated queue jobs. Backend tests use private installation and launch-service fixtures rather than the installed queue.
