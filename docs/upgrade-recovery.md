# Automatic installation and upgrade recovery

Opening `Agent Queue.app` from `/Applications` or `~/Applications` runs setup automatically, whether the app arrived from the DMG, the ZIP, an update download or the Homebrew cask (Homebrew itself runs nothing at install or upgrade). The app includes the queue, resource controller and HTTPS runtime; installed users need no development toolchain or separately installed Python, Pueue or Caddy executable.

Setup runs from inside the app bundle (`agent-heavy install --app-bundle`), registers the bundle-owned services with Service Management, writes the CLI wrappers to `~/.local/bin` and copies no executables out of the bundle. A launch that finds a completed, current installation only reads readiness and authorization state; it never repeats installation or recovery. An incomplete installation is finished on the next launch, and the setup window appears only for a review, a failure or macOS approval.

Setup inspects ownership and readiness, performs supported repairs, selects a compatible live handover or waits for jobs to finish, and verifies installation before opening the queue. It restores verified missing service definitions, cleans completed jobs through the normal ownership checks, starts a verified stopped legacy Go service, and disables incompatible phone access when migration requires it. Credentials, trusted certificates, history and queue settings remain retained.

For an incompatible legacy cleanup callback, automatic setup backs up the exact original configuration privately before replacing the callback. Its SHA-256 revision is checked again under the installation locks. Incompatible options in verified legacy service definitions are backed up before those definitions are retired. Backups use content-derived names in the private configuration directory. An existing changed backup blocks the operation before services are stopped. Setup does not replace another installation's commands or environment paths. An existing managed Go runtime never executes a retained Pueue callback; automatic upgrades preserve that inactive custom configuration unchanged.

## Upgrading while jobs continue

A managed upgrade can hand over without waiting for running or queued jobs when the installed daemon, target controller and every outstanding job caller support handover protocol 1 and queue schema 2. Setup authenticates the transfer against the exact target, database and attempt identities before replacing services. Existing workers finish their original attempts, callers retain their original outcomes and exit statuses, and queued jobs remain queued. Pause state and the parallel limit survive the transfer. No attempt is replayed. New queue mutations can be briefly rejected before acceptance while the handover is in progress.

Setup retains each complete previous signed app needed by outstanding callers or workers visibly under `~/Applications/Agent Queue Runtimes`. A private durable record binds its device, inode, build and digest to the original attempts; matching names or content alone do not authorize adoption. Multiple upgrades may retain multiple generations. Only a later setup operation removes a generation after matching terminal attempts and dead original callers and owned workers are established. Missing or uncertain evidence keeps the generation. The app and daemon never delete executable code.

Legacy releases or outstanding callers without this protocol require one drained upgrade before live handover becomes available. Unsupported capabilities or schemas fall back to drain. Uncertain attempts, ownership or changed custody still fail closed; setup never discards their evidence. Removal and preparation for external app replacement always require a drained queue.

## Upgrading from an installer-era install

Earlier releases installed `~/Applications/Agent Queue.app` through `Agent Job Queue Installer.app` and ran the services from that copy. That location stays recorded as the managed one, and setup refuses a new copy placed elsewhere (for example `/Applications`) while the recorded copy exists. Either drag the new app over `~/Applications/Agent Queue.app` and choose **Replace**, or stop the services with `~/.local/bin/agent-heavy install --app-bundle "$HOME/Applications/Agent Queue.app" --unregister-services`, move the old app to the Trash, then open the new app. Homebrew users coming from the installer cask must use the second route. Legacy `~/Library/LaunchAgents/io.went.agent-*.plist` definitions can be retired and backed up during migration, and incompatible options are backed up first.

## Waiting and platform approval

When a transition requires drain, running and queued work continues while automatic setup waits. Each inspection, recovery and installation operation retains a finite five-minute budget; waiting for work to finish has no aggregate five-minute limit. Installation does not automatically cancel, resume or retry job attempts. Deliberately paused or stashed jobs can therefore need an operator decision on a drained transition. A compatible live handover preserves a paused queue without resuming it. Receipts, lease files and locks are never deleted to bypass a blocker.

macOS may require approval for background services. Setup displays the requirement, reads status without re-registering denied services, and resumes the exact pending candidate after approval. The app offers a shortcut to Login Items settings. Application code cannot approve this operating-system decision or bypass endpoint-protection policy. A completed setup opens the queue only after readback succeeds.

An interrupted publication retains its candidate, backup and receipt. Automatic recovery finishes that exact pending build before applying a newer candidate. Changed or missing custody evidence remains a failure. Do not delete the receipt or pending marker to force an upgrade. Removal still requires the ownership-checked service-removal command to succeed before deleting the runtime app.

## Diagnostic workflow

The optional `--setup` app argument retains guided diagnosis and individual recovery controls. It can show affected files and jobs, run a reviewed recovery, or perform a cancellable read-only wait. Destructive manual actions, including job cancellation and callback replacement, retain their confirmations. Fixtures and developer builds never install into the real queue.

The bundled controller supports read-only preflight and automatic installation:

```sh
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --preflight --json
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --repair-and-continue
```

Preflight returns a schema-versioned JSON report with an `issues` array. Each issue has a stable ID, title, explanation, optional affected path or jobs, and supported actions. An empty array permits an installation attempt; it does not establish completed installation. Automatic setup emits progress and returns success only after installation completes.

Individual recovery commands remain available for diagnosis:

```sh
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover cancel-job --job-id JOB_ID
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover cleanup-job --job-id JOB_ID
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover repair-service
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover restore-service-definitions
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover disable-phone-access
agent-heavy install --app-bundle ABSOLUTE_APP_PATH --recover reset-callback --expected-revision REVIEWED_SHA256
```

Each recovery rechecks current eligibility and ownership, then returns a fresh preflight report. Manual callback replacement requires the exact revision from that report. Unknown action IDs, invalid paths and malformed job IDs are rejected. Installation never relaxes filesystem ownership, signature checks, Gatekeeper, or shared directory permissions.

## Verification

Run the finite shared-queue validation batch documented in CONTRIBUTING.md. Backend regressions cover automatic legacy migration, original configuration and service backups, live queued/running jobs and original caller results, pause and limit preservation, legacy fallback, interrupted handover and publication, retained runtime custody and later setup reclamation, approval waiting without repeated registration, stale revisions, changed backup evidence and unrelated ownership. Native regressions exercise read-only packaged launch across fresh, incomplete and stale installations, missing registration, approval resumption, diagnostic isolation and installation readback. Graphical fixtures use disposable homes and injected executors; Homebrew fixtures exercise the plain app cask, quarantine retention and uninstall without touching runtime files. These local proofs do not establish native Service Management worker survival, Apple signing, notarization, release publication, SentinelOne acceptance or live registration on another account or architecture.
