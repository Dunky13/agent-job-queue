# Automatic installation and upgrade recovery

Opening `Agent Queue.app` from `/Applications` or `~/Applications` runs setup automatically, whether the app arrived from the DMG, the ZIP, an update download or the Homebrew cask (Homebrew itself runs nothing at install or upgrade). The app includes the queue, resource controller and HTTPS runtime; installed users need no development toolchain or separately installed Python, Pueue or Caddy executable.

Setup runs from inside the app bundle (`agent-heavy install --app-bundle`), writes the CLI wrappers to `~/.local/bin`, migrates state, registers only the app's login item with Service Management and copies no executables out of the bundle. It also unregisters any queue, resource or phone-access launchd registrations left by older versions, including crash-looping ones that hold no process and ones still awaiting approval. It no longer registers those services or waits for daemon readiness: the menu bar app starts them afterwards (see below). A launch that finds a completed, current installation only reads installation state and checks that no registration from an older version remains; it never repeats installation or recovery. Every normal launch presents the menu bar immediately with only "Starting…", then finishes incomplete installation in the background. Setup wording and **Retry** appear only after an actual failure. It never opens a setup wizard, progress window or installation confirmation, including during upgrades, waits, failures and approval. The same status item is retained throughout and becomes the normal queue menu when setup completes.

## How the app runs the queue

Once setup has finished, the menu bar app starts `agent-heavy daemon` and `agent-heavy scheduler` as its own child processes and, when phone access is enabled (from the app's settings, or already enabled at launch), `agent-remote serve`. Each runs in its own session with a minimal launchd-like environment (`PATH`, `HOME`, `USER`, `LOGNAME`, `TMPDIR`, `LANG`; the app's own `AGENT_HEAVY_*` and `AGENT_REMOTE_*` variables are never passed on) and logs to `~/.local/state/agent-heavy/daemon.log`, `resources.log` and `remote/service.log`, each bounded to 1 MiB when the process is spawned. The daemon and scheduler restart after any exit while the app runs; the phone-access service restarts after a failure or while its settings remain enabled; a restart waits until 10 seconds after the previous start. If a daemon is already serving, such as one left behind by a crashed app, the app adopts it instead of starting a second one, and the daemon's own lifetime locks still refuse duplicate instances.

Quitting the app (and logout) sends these processes SIGTERM and then SIGKILL to any that do not exit within a grace period; the app exits once they have stopped. Running job processes are independent and keep running. If jobs were active, the queue comes back paused on the next launch under the existing unclean-restart rule, and nothing is replayed.

Setup inspects ownership and readiness, performs supported repairs, waits for a running daemon's jobs to finish, and verifies installation before opening the queue. It restores verified missing service definitions, cleans completed jobs through the normal ownership checks, starts a verified stopped legacy Go service, and disables incompatible phone access when migration requires it. Credentials, trusted certificates, history and queue settings remain retained.

For an incompatible legacy cleanup callback, automatic setup backs up the exact original configuration privately before replacing the callback. Its SHA-256 revision is checked again under the installation locks. Incompatible options in verified legacy service definitions are backed up before those definitions are retired. Backups use content-derived names in the private configuration directory. An existing changed backup blocks the operation before services are stopped. Setup does not replace another installation's commands or environment paths. An existing managed Go runtime never executes a retained Pueue callback; automatic upgrades preserve that inactive custom configuration unchanged.

## Upgrading with active jobs

Setup never starts a live handover. If a queue daemon is still running, as one registered by an older version can be, setup waits for its running and queued jobs to finish before replacing it. Removal and relocation always require a drained queue. Uncertain attempts, ownership or changed custody still fail closed; setup never discards their evidence.

Quitting the app stops its daemon without stopping independent jobs, so a normal upgrade at the same location has no daemon to wait for. What happens next differs by who is watching:

- A running job keeps running. Its outcome is recorded, and it runs exactly once.
- Its attached `agent-heavy run` caller loses the daemon and exits with status 125 within about a second, so that exit status is not the job's outcome.
- A queued job's caller also exits with status 125. The job stays queued.
- If jobs were running, the next daemon comes up paused under the unclean-restart rule above, and queued jobs run after `agent-heavy resume` with no caller attached.

## Finishing a handover from an older release

Older releases could replace a running daemon without waiting, recording an installation journal and a daemon receipt. Setup keeps only what finishes those. The receipt binds the transfer to one exact executable, build T, so only T's bundle can complete it, though a newer app's controller can run that bundle. A released receipt cannot finish after a reboot; see below.

When T was interrupted after publishing, its unfinished marker and app remain at the recorded location. Opening a newer build from the other Applications folder finishes T automatically through the `finish-pending-installation` repair, then sets up the newer build at its own location. A newer build dragged over T at the same path has replaced T's app, so it has no such route.

Finishing T resumes from the prepared, committed, released or consumed phase. Existing workers finish their original attempts, callers retain their original outcomes and exit statuses, queued jobs remain queued, pause state and the parallel limit survive, and no attempt is replayed. New queue mutations can be briefly rejected before acceptance while the transfer is in progress.

A receipt binds nothing when it is missing, cancelled or expired, when it carries another handover's token, which proves this journal never took custody, or when the daemon recorded in a prepared or committed receipt or the consumer recorded in a consumed one has exited: a restarted daemon runs ordinary crash recovery and adopts nothing unreleased. A recorded process whose start time no longer matches counts as exited only if the queue daemon's lifetime lock is free, because that time changes with the system's time zone. Setup then removes the journal and continues normally, waiting for a running daemon's jobs as above.

Another build refuses while a receipt still binds custody: a prepared or committed receipt whose daemon is alive (a prepared lease expires after two minutes), or a released receipt, which every daemon but T's refuses to start on. The refusal names build T and offers the official release history, because finishing needs that exact build. That does not help after a reboot: a released receipt belongs to the earlier boot, so T's daemon exits with status 125 and "Queue handover belongs to another boot", T's own setup fails the same way, and no other daemon starts on it either. No build can finish it today; this is a known limitation. A consumed receipt with a live consumer refuses only until T has completed; after that it is just the finished build's own daemon, and its journal is removed once that daemon exits. A journal whose target is older than the completed build cannot be finished, so it is treated as finished, and removed unless a live process still depends on it. An unknown phase or changed target is refused.

A resumed handover retains each complete previous signed app needed by outstanding callers or workers visibly under `~/Applications/Agent Queue Runtimes`. A private durable record binds its device, inode, build and digest to the original attempts; matching names or content alone do not authorize adoption. Multiple generations may be retained. Only a later setup operation removes a generation after matching terminal attempts and dead original callers and owned workers are established. Missing or uncertain evidence keeps the generation. The app and daemon never delete executable code.

## Upgrading from an installer-era install

Earlier releases installed `~/Applications/Agent Queue.app` through `Agent Job Queue Installer.app` and ran the services from that copy. Open the new app from either Applications folder to continue. When the recorded copy still exists, setup verifies both signed bundles, their build metadata, the recorded installation and every loaded service. It waits for the queue to drain, unregisters services through the recorded app's own verified helpers, and sets this app up at its new location, where it starts its own queue processes. The previous app stays in place. Relocation uses the drained lifecycle, so running work keeps its original executable and outcome.

If setup for a different build is still pending, it first finishes that exact retained app at the recorded location, including any owned publication receipt and last-good backup. Only then can it set up the newer dragged app. A missing or changed pending app, invalid signature, unrelated loaded service or changed custody stops setup without discarding the evidence. The startup menu offers the official release history when the exact retained candidate cannot be recovered. It never asks users to delete installation records or run terminal commands. Legacy `~/Library/LaunchAgents/io.went.agent-*.plist` definitions can be retired and backed up during migration, and incompatible options are backed up first.

## Waiting and platform approval

When a transition requires drain, running and queued work continues while automatic setup waits. Each inspection, recovery and installation operation retains a finite five-minute budget; waiting for work to finish has no aggregate five-minute limit. Installation does not automatically cancel, resume or retry job attempts. Deliberately paused or stashed jobs can therefore need an operator decision on a drained transition. Receipts, lease files and locks are never deleted to bypass a blocker.

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

Run the finite shared-queue validation batch documented in CONTRIBUTING.md. Backend regressions cover automatic legacy migration, original configuration and service backups, waiting for active jobs without starting a handover, resuming an older release's journal from each phase against real private daemons with original caller results and pause and limit preservation, finishing a retained build's handover from a newer app in the other Applications folder, a named refusal while a receipt binds live custody, removal of journals that bind nothing, interrupted publication, retained runtime custody and later setup reclamation, approval waiting without repeated registration when resuming a handover, app-bundle setup that registers only the login item and never blocks on its approval, migration of old registrations (including approval-pending and crash-looping ones without a process) and refusal of one whose process is not this app's helper, stale revisions, changed backup evidence and unrelated ownership. Relocation fixtures keep two distinct apps in private allowed Applications paths and verify exact pending completion, owned backup retirement, drain, approval resumption and original-caller retention before switching locations. Native regressions exercise read-only setup-disabled launches, silent ordinary setup and recovery, leftover-registration migration, diagnostic isolation and installation readback. The supervisor suite (`native/tests/run-supervisor-tests.sh`) spawns only private fake helper scripts and covers the environment allowlist, bounded private logs, own-session spawning, the restart policy and throttle, adoption of a running daemon and SIGTERM then SIGKILL shutdown. It compiles for arm64 and x86_64 and runs the host architecture's build. Graphical fixtures verify immediate menu visibility, retained status-item identity, no ordinary setup windows, failure/retry and safe quit, including the `--supervised-quit` scenario (quit waits for the supervised children and replies once, without SIGKILL or restarts), using disposable homes and injected executors; Homebrew fixtures exercise the plain app cask, quarantine retention and uninstall without touching runtime files. These local proofs do not establish native Service Management worker survival, Apple signing, notarization, release publication, SentinelOne acceptance or live login-item registration on another account or architecture.
