# Automatic installation and upgrade recovery

Opening `Agent Queue.app` from `/Applications` or `~/Applications` runs setup automatically, whether the app arrived from the DMG, the ZIP, an update download or the Homebrew cask (Homebrew itself runs nothing at install or upgrade). The app includes the queue, resource controller and HTTPS runtime; installed users need no development toolchain or separately installed Python, Pueue or Caddy executable.

Setup runs from inside the app bundle (`agent-heavy install --app-bundle`), writes the CLI wrappers to `~/.local/bin`, migrates state, registers only the app's login item with Service Management and copies no executables out of the bundle. It also unregisters any queue, resource or phone-access launchd registrations left by older versions, including crash-looping ones that hold no process and ones still awaiting approval. It no longer registers those services or waits for daemon readiness: the menu bar app starts them afterwards (see below). A launch that finds a completed, current installation only reads installation state and checks that no registration from an older version remains; it never repeats installation or recovery. Every normal launch presents the menu bar immediately with only "Starting…", then finishes incomplete installation in the background. Setup wording and **Retry** appear only after an actual failure. It never opens a setup wizard, progress window or installation confirmation, including during upgrades, waits, failures and approval. The same status item is retained throughout and becomes the normal queue menu when setup completes.

## How the app runs the queue

Once setup has finished, the menu bar app starts `agent-heavy daemon` and `agent-heavy scheduler` as its own child processes and, when phone access is enabled (from the app's settings, or already enabled at launch), `agent-remote serve`. Each runs in its own session with a minimal launchd-like environment (`PATH`, `HOME`, `USER`, `LOGNAME`, `TMPDIR`, `LANG`; the app's own `AGENT_HEAVY_*` and `AGENT_REMOTE_*` variables are never passed on) and logs to `~/.local/state/agent-heavy/daemon.log`, `resources.log` and `remote/service.log`, each bounded to 1 MiB when the process is spawned. The daemon and the phone-access service restart after a failure and stay stopped after a clean exit; the scheduler always restarts; a restart waits until 10 seconds after the previous start. If a daemon is already serving, such as one left behind by a crashed app, the app adopts it instead of starting a second one, and the daemon's own lifetime locks still refuse duplicate instances.

Quitting the app (and logout) sends these processes SIGTERM and then SIGKILL to any that do not exit within a grace period; the app exits once they have stopped. Running job processes are independent and keep running. If jobs were active, the queue comes back paused on the next launch under the existing unclean-restart rule, and nothing is replayed.

Setup inspects ownership and readiness, performs supported repairs, selects a compatible live handover or waits for jobs to finish, and verifies installation before opening the queue. It restores verified missing service definitions, cleans completed jobs through the normal ownership checks, starts a verified stopped legacy Go service, and disables incompatible phone access when migration requires it. Credentials, trusted certificates, history and queue settings remain retained.

For an incompatible legacy cleanup callback, automatic setup backs up the exact original configuration privately before replacing the callback. Its SHA-256 revision is checked again under the installation locks. Incompatible options in verified legacy service definitions are backed up before those definitions are retired. Backups use content-derived names in the private configuration directory. An existing changed backup blocks the operation before services are stopped. Setup does not replace another installation's commands or environment paths. An existing managed Go runtime never executes a retained Pueue callback; automatic upgrades preserve that inactive custom configuration unchanged.

## Upgrading while jobs continue

A managed upgrade can hand over without waiting for running or queued jobs when the installed daemon, target controller and every outstanding job caller support handover protocol 1 and queue schema 2. Setup authenticates the transfer against the exact target, database and attempt identities before replacing services. Existing workers finish their original attempts, callers retain their original outcomes and exit statuses, and queued jobs remain queued. Pause state and the parallel limit survive the transfer. No attempt is replayed. New queue mutations can be briefly rejected before acceptance while the handover is in progress.

Setup retains each complete previous signed app needed by outstanding callers or workers visibly under `~/Applications/Agent Queue Runtimes`. A private durable record binds its device, inode, build and digest to the original attempts; matching names or content alone do not authorize adoption. Multiple upgrades may retain multiple generations. Only a later setup operation removes a generation after matching terminal attempts and dead original callers and owned workers are established. Missing or uncertain evidence keeps the generation. The app and daemon never delete executable code.

Legacy releases or outstanding callers without this protocol require one drained upgrade before live handover becomes available. Unsupported capabilities or schemas fall back to drain. Uncertain attempts, ownership or changed custody still fail closed; setup never discards their evidence. Removal and preparation for external app replacement always require a drained queue.

## Upgrading from an installer-era install

Earlier releases installed `~/Applications/Agent Queue.app` through `Agent Job Queue Installer.app` and ran the services from that copy. Open the new app from either Applications folder to continue. When the recorded copy still exists, setup verifies both signed bundles, their build metadata, the recorded installation and every loaded service. It waits for the queue to drain, unregisters services through the recorded app's own verified helpers, and sets this app up at its new location, where it starts its own queue processes. The previous app stays in place. Relocation uses the drained lifecycle rather than live handover, so running work keeps its original executable and outcome.

If setup for a different build is still pending, it first finishes that exact retained app at the recorded location, including any owned publication receipt and last-good backup. Only then can it set up the newer dragged app. A missing or changed pending app, invalid signature, unrelated loaded service or changed custody stops setup without discarding the evidence. The startup menu offers the official release history when the exact retained candidate cannot be recovered. It never asks users to delete installation records or run terminal commands. Legacy `~/Library/LaunchAgents/io.went.agent-*.plist` definitions can be retired and backed up during migration, and incompatible options are backed up first.

## Waiting and platform approval

When a transition requires drain, running and queued work continues while automatic setup waits. Each inspection, recovery and installation operation retains a finite five-minute budget; waiting for work to finish has no aggregate five-minute limit. Installation does not automatically cancel, resume or retry job attempts. Deliberately paused or stashed jobs can therefore need an operator decision on a drained transition. A compatible live handover preserves a paused queue without resuming it. Receipts, lease files and locks are never deleted to bypass a blocker.

macOS may ask you to approve the app's login item, which only decides whether Agent Queue opens at login. That approval never blocks setup or the queue: the helpers are the app's own children, not registered services, so setup does not wait for it and the startup menu shows no approval state. Setup also unregisters a queue, resource or phone-access registration from an older version while it is still awaiting approval, so a stale pending approval cannot hold the upgrade. Application code cannot approve this operating-system decision or bypass endpoint-protection policy. Ordinary launch stays menu-bar-only; an explicit `--open-window` requests the queue window.

Automatic waits keep status and **Quit** in the menu bar. Quit and logout send the private controller a cooperative stop request and wait for it to finish, then stop the app's queue processes as described above; independent jobs continue and pending installation evidence remains available for the next launch. Accepted installation steps retain their finite mutation context and original response before stopping. Explicit diagnostic waits also offer **Stop waiting**, and block retry until the controller has finished.

An interrupted publication retains its candidate, backup and receipt. Automatic recovery finishes that exact pending build before applying a newer candidate. Changed or missing custody evidence remains a failure. Do not delete the receipt or pending marker to force an upgrade. Removal still requires the ownership-checked service-removal command to succeed before deleting the runtime app.

## Diagnostic workflow

Ordinary launches silently continue supported repairs through the bundled controller's `--repair-and-continue` flow. A current installation that still has a queue, resource or phone-access registration from an older version also completes setup, retiring it, without a second launch or **Check again**. Unknown requirements and failures remain visible in the menu with diagnostics, Retry and the official download link; a healthy launch shows only "Starting…". Reopen stays in the menu bar during preparation.

Only the optional `--setup` app argument opens guided diagnosis and individual recovery controls. Ordinary launch, reopen and quit never open it automatically. It can show affected files and jobs, run a reviewed recovery, or perform a cancellable read-only wait. Destructive manual actions, including job cancellation and callback replacement, retain their confirmations. Fixtures and developer builds never install into the real queue.

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

Run the finite shared-queue validation batch documented in CONTRIBUTING.md. Backend regressions cover automatic legacy migration, original configuration and service backups, live queued/running jobs and original caller results, pause and limit preservation, legacy fallback, interrupted handover and publication, retained runtime custody and later setup reclamation, approval waiting without repeated registration on the payload path, app-bundle setup that registers only the login item and never blocks on its approval, migration of old registrations (including approval-pending and crash-looping ones without a process) and refusal of one whose process is not this app's helper, stale revisions, changed backup evidence and unrelated ownership. Relocation fixtures keep two distinct apps in private allowed Applications paths and verify exact pending completion, owned backup retirement, drain, approval resumption and original-caller retention before switching locations. Native regressions exercise read-only setup-disabled launches, silent ordinary setup and recovery, leftover-registration migration, diagnostic isolation and installation readback. The supervisor suite (`native/tests/run-supervisor-tests.sh`) spawns only private fake helper scripts and covers the environment allowlist, bounded private logs, own-session spawning, the restart policy and throttle, adoption of a running daemon and SIGTERM then SIGKILL shutdown. It compiles for arm64 and x86_64 and runs the host architecture's build. Graphical fixtures verify immediate menu visibility, retained status-item identity, no ordinary setup windows, failure/retry and safe quit, including the `--supervised-quit` scenario (quit waits for the supervised children and replies once, without SIGKILL or restarts), using disposable homes and injected executors; Homebrew fixtures exercise the plain app cask, quarantine retention and uninstall without touching runtime files. These local proofs do not establish native Service Management worker survival, Apple signing, notarization, release publication, SentinelOne acceptance or live login-item registration on another account or architecture.
