# Agent Job Queue

<img src="remote/static/app-icon.png" width="80" height="80" alt="Agent Queue stacked-layers icon">

[![CI](https://github.com/Dunky13/agent-job-queue/actions/workflows/ci.yml/badge.svg)](https://github.com/Dunky13/agent-job-queue/actions/workflows/ci.yml)
[![Distribution](https://github.com/Dunky13/agent-job-queue/actions/workflows/distribution.yml/badge.svg)](https://github.com/Dunky13/agent-job-queue/actions/workflows/distribution.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Run many coding agents while sharing a limited number of heavy local job slots. Agents can read, reason, and edit concurrently. Builds, test suites, type checks, and other expensive commands wait their turn.

The macOS menu-bar app shows what is running, what is waiting, and which project and branch each job belongs to. An optional private web dashboard works from a phone over home Wi-Fi or a VPN.

| Mac menu-bar queue | Phone dashboard |
| --- | --- |
| [![Mac queue showing running and waiting jobs, project labels, and the parallel job limit](docs/screenshots/mac-queue.png)](docs/screenshots/mac-queue.png) | [![Phone dashboard showing the queue and controls to start or reorder jobs](docs/screenshots/phone-queue.png)](docs/screenshots/phone-queue.png) |

Screenshots use sample jobs. The Mac image uses the app's safe preview mode.

<details>
<summary>Start a waiting job in parallel</summary>

![Native confirmation for starting a waiting job outside the parallel limit](docs/screenshots/mac-start-now.png)

</details>

## Download and install

### Drag and drop

1. Use macOS 15 or later on an Apple Silicon or Intel Mac.
2. Download `Agent-Queue-<version>-universal.dmg` (or the ZIP) from [Releases](https://github.com/Dunky13/agent-job-queue/releases).
3. Open the DMG and drag **Agent Queue** onto **Applications**. To install for your account only, drop it in `~/Applications` instead.
4. Open Agent Queue from Applications. It sets itself up silently and appears in the menu bar.

The app is self-contained: the menu app, the `agent-heavy` and `agent-remote` command-line tools, the queue daemon, the resource controller and the HTTPS runtime all live inside `Agent Queue.app`. There is no installer and nothing is copied out of the bundle. On first launch the app registers its own bundled background services with macOS Service Management, creates its private data directories and adds `agent-heavy` and `agent-remote` wrappers to `~/.local/bin`. Xcode, Go, Python, Pueue and Caddy are not required, and setup never uses `sudo`.

Opening the app immediately adds its menu-bar icon and prepares services automatically in the background, on both first launch and upgrades. There is no setup wizard or progress window. macOS may show a "Background Items Added" notification or require approval in **System Settings > General > Login Items**. The menu shows status, a Settings shortcut when approval is needed, and diagnostics with Retry if preparation fails. Setup resumes automatically after approval. Only the explicit `--setup` diagnostic mode opens a recovery window.

To upgrade, quit Agent Queue, drag the new version over the old one and choose **Replace**, then open it. Queue history, settings, credentials and certificates are kept. If jobs are queued or running, setup either hands them over to the new version or waits for them to finish; see [upgrade recovery](docs/upgrade-recovery.md).

If you move the app between `/Applications` and `~/Applications`, delete or replace the old copy first. Setup refuses to adopt a second copy while the recorded one still exists.

To remove Agent Queue, finish queued and running jobs, stop its services, then move the app to the Trash:

```sh
agent-heavy install --app-bundle "/Applications/Agent Queue.app" --unregister-services
```

Use the app's actual location if it differs. Private data under `~/.local/share/agent-heavy`, `~/.local/state/agent-heavy` and `~/.config/agent-heavy` is preserved.

### Homebrew

```sh
brew install --cask dunky13/agent-job-queue/agent-job-queue
```

The cask places `Agent Queue.app` in Applications and nothing else; open it to use the menu bar while services prepare automatically. `brew upgrade --cask dunky13/agent-job-queue/agent-job-queue` replaces the app, and the next launch immediately shows the menu bar and completes the upgrade in the background. Homebrew verifies the download's SHA-256 and keeps quarantine so macOS assesses the notarized app. Maintainer setup is documented in [Verified Homebrew delivery](docs/homebrew-delivery.md).

### Signing, Gatekeeper and endpoint security

Releases from the distribution workflow are Developer ID signed, notarized and stapled (app and DMG). Quarantine is never removed. Because setup only registers services that ship inside the signed app, endpoint security tools see a notarized app from Applications managing its own login items, not a separate process copying executables. See [Apple signing and notarization](docs/apple-signing.md) for verification details.

If macOS blocks a downloaded build you trust, open **System Settings > Privacy & Security**, choose **Open Anyway** next to **Agent Queue**, and confirm. Do not clear quarantine to work around a failed verification of a release advertised as notarized. See [Apple's opening instructions](https://support.apple.com/102445).

### Upgrading from the installer

Earlier releases shipped `Agent Job Queue Installer.app`, which installed `~/Applications/Agent Queue.app` and ran the services from that copy. That location stays recorded as the managed one, and setup refuses a new copy placed anywhere else (for example `/Applications` from the new DMG or cask) while the recorded copy exists. Either:

- Quit Agent Queue, drag the new app over `~/Applications/Agent Queue.app` and choose **Replace**, then open it; or
- Stop the old services, move the old app to the Trash, then open the new app wherever you installed it:

  ```sh
  ~/.local/bin/agent-heavy install --app-bundle "$HOME/Applications/Agent Queue.app" --unregister-services
  ```

Homebrew users upgrading from the installer cask must use the second route: the old cask left `~/Applications/Agent Queue.app` in place, so run the command above, move that app to the Trash, then `brew upgrade --cask dunky13/agent-job-queue/agent-job-queue` (or install it) and open the app from Applications. Queue history, settings, credentials and certificates are kept. If jobs are queued or running, finish them first or see [upgrade recovery](docs/upgrade-recovery.md).

## Version and updates

Choose **About and updates** from the queue's gear menu to see the running app and installed controller versions. **Check for updates** finds published builds. **Download update** downloads and verifies the selected build, places a verified copy of `Agent Queue.app` in your Downloads folder and reveals it in Finder. Quit Agent Queue, drag the new copy into Applications and choose **Replace**; opening it finishes the update. Checking never installs anything and tracked downgrades are refused. Details are in [Installed version and updates](docs/updates.md).

## Connect your coding agents

Open **Agent integration** from the queue's gear menu to add queue instructions for **Codex** and **Claude Code**. Select the runners you use, review the instructions, then choose **Save instructions**. Start new agent sessions afterward. This is optional and never blocks installation or opening the queue.

<details>
<summary>Preview agent integration setup</summary>

![Agent integration setup offering Codex and Claude Code with no runners selected](docs/screenshots/agent-integration.png)

</details>

Setup shows each instruction file and whether this app has configured it. It preserves existing text, adds its own marked section, and makes a private backup before changing an existing file. Deselecting a runner removes only that section. Existing custom rules and shared imports remain in place; the status describes this app's section only.

The command line supports the same actions:

```sh
agent-heavy agents status
agent-heavy agents install --runner codex
agent-heavy agents install --runner claude
agent-heavy agents remove --runner codex
```

Codex uses its global `AGENTS.md`, or its existing nonempty `AGENTS.override.md` when that takes precedence. Claude Code uses its user `CLAUDE.md`. The CLI honors `CODEX_HOME` and `CLAUDE_CONFIG_DIR`; run it from the runner's shell when using a custom profile. The app shows the paths visible in its own environment. See the official [Codex instruction discovery](https://learn.chatgpt.com/docs/agent-configuration/agents-md) and [Claude Code memory](https://code.claude.com/docs/en/memory) documentation.

These instructions guide agents; they do not intercept commands or enforce queue usage. Other runners can use the shared rule below. Installation and upgrades do not change agent instructions until you explicitly save your selections.

## Queue heavy commands

```sh
agent-heavy run --label "Build and unit tests" -- npm run build
agent-heavy -- npm run typecheck
agent-heavy -- go test -p 2 ./...
```

Commands run in their original working directory with their literal arguments and environment. The wrapper waits, streams output, and returns the command's exit status. Several repositories, worktrees, terminals, and agents share the same queue.

Queue resource-intensive parallel work. Small serial compilations, focused tests, source inspection, and editing can run directly. Keep persistent development servers and watch modes outside the queue and account for their memory separately. Do not nest queue wrappers. Jobs are noninteractive and receive EOF on stdin; pass scripts as files or arguments.

For other runners, add this rule to their shared instructions:

```text
Run resource-intensive builds, broad tests, type checks, browser suites, and
expensive generators through agent-heavy -- <command> [arguments...].
Use the same shared queue across repositories and worktrees. Keep each tool's
internal worker count conservative. Run small focused checks directly.
```

The Go queue daemon owns submitted jobs independently of the calling terminal and menu app. Closing the UI does not stop jobs. Abruptly losing a submitter leaves its accepted job visible until it finishes or is explicitly cancelled.

## View and manage jobs

The menu-bar app provides project and branch labels, queue positions, elapsed time, results, job details, and bounded log tails. Background checks leave unchanged content in place. Details and confirmations stay within the queue panel.

Click the menu-bar icon to open the queue popover. Right-click or Control-click it for **Open window**, learned workloads, About and updates, agent integration, remote access settings, and Quit. Double-click the icon to open or focus the main queue window. The same settings remain available in the popover's gear menu.

Use the app or CLI to change future scheduling:

```sh
agent-heavy status
agent-heavy status --json
agent-heavy limit 2
agent-heavy next 17
agent-heavy last 17
agent-heavy start 17
agent-heavy cancel 17
```

Replace `17` with a real job ID. The default parallel limit is one, adjustable from one through eight. Lowering the limit lets active jobs finish and holds future jobs. **Start now** starts one selected job alongside active work without changing the limit. Cancellation targets that job's positively identified owned processes.

The queue limits how many wrapped commands can run. A command may still create many workers or use substantial memory, so configure worker limits within your build and test tools too. There is no host memory quota, and persistent agent processes are not controlled by this queue.

## Plugins and lanes

Plugins add **lanes**: named queues with their own concurrency, priority and share of the parallel limit. A plugin is a declarative manifest in `~/.config/agent-heavy/plugins/<name>/plugin.json`; it runs no code inside the queue. Optional hooks are separate, time-limited processes started on queue events. Without plugins, the queue behaves exactly as before in its `default` lane.

```sh
agent-heavy plugins list
agent-heavy run --lane qa -- npm run e2e
agent-heavy lane acquire qa --owner-pid "$PPID" --label "QA session" --timeout 60
agent-heavy lane release 42 --owner-pid "$PPID"
```

A **lease** holds a lane slot without a command, for multi-step work such as driving an emulator across many agent tool calls. It ends when released, when its owner process exits, or when its optional TTL passes. See [plugins and lanes](docs/plugins.md) for the manifest, scheduling rules, lease commands, status fields and hooks.

## Learn job costs and schedule by available resources

The installer also starts an independent resource controller. It samples host CPU, memory pressure, available memory, swap activity and disk capacity, and measures the owned workers of running jobs. Job details show predicted memory and CPU demand, observed memory peaks, CPU-seconds, sample counts and confidence. Monitoring keeps working when the menu app or submitting terminal closes.

The default **Fixed** mode preserves the existing parallel limit while collecting history. **Learn only** makes shadow admission decisions without controlling starts. **Adaptive** holds waiting jobs until predicted CPU and memory demand fit the current headroom, within the same operator-set parallel limit. Choose the mode in the menu app or dashboard, or use:

```sh
agent-heavy scheduling shadow
agent-heavy status
agent-heavy limit 2
agent-heavy scheduling adaptive
agent-heavy scheduling fixed
```

Changing the limit is an explicit operator choice. Adaptive mode cannot increase it. It reserves room for later job peaks, waits during memory pressure or active swapping, and requires a recovery interval before starting more work. Unfamiliar workloads run alone until comparable successful measurements accumulate. If resource telemetry is unavailable or stale, adaptive jobs remain waiting with an explanation. Fixed and learning modes remain available. Returning to Fixed returns waiting jobs to ordinary queue scheduling without stopping active jobs. **Start now** is an explicit override of resource admission and the parallel limit.

Worktrees of the same repository share a logical workload family, using repository-relative task and target identity rather than checkout paths or branch names. Changed source revisions, scripts, dependencies, toolchains and worker settings create variants that cautiously borrow the family's history. New worktrees can have cold caches; their timing is not assumed to match a warm checkout. For opaque commands, an optional logical name can relate comparable jobs deliberately:

```sh
AGENT_HEAVY_PROFILE=project-validation agent-heavy -- bash scripts/validate.sh
```

Use the same explicit profile only for related workloads. Their meaningful arguments and settings still distinguish variants. Do not put credentials in profile names. History stores opaque identities and numeric observations, with bounded retention; raw environment and command contents are not copied into the history database.

Choose **Learned workloads** in the app's gear menu or phone navigation to inspect measured peaks, variants, confidence and recent runs. Rename profiles, share learning between similar job families, separate a family, set minimum RAM/CPU reservations, or reset obsolete measurements. Saved corrections affect future estimates without stopping active jobs. The CLI exposes the same controls through `agent-heavy profiles`; see [Inspect and correct workload learning](docs/learning.md) for details.

Measurements are sampled estimates, not enforced CPU or memory quotas. Workers and peaks entirely between samples can be missed. Failed, cancelled, interrupted and incompletely observed runs cannot establish that a workload is cheap, while their observed peaks still raise conservative estimates. Adaptive mode does not suspend or terminate jobs, control unrelated apps, or rewrite tool flags. See [the scheduling contract](docs/adaptive-scheduling.md) for learning, ownership and recovery details.

## Phone access

Remote access starts off. In the menu-bar gear menu, open **Remote access**, set a password of at least four characters, and choose **Save password and enable**. A short word or PIN works, with no character-mix requirement. The username is `agent`.

The settings window displays the HTTPS address and certificate sharing controls:

1. Transfer the public `root.crt` certificate to your phone.
2. On Android, install it as a **CA certificate** in the system's certificate settings. Settings names vary by device; search for "CA certificate". This certificate is for HTTPS.
3. Open the displayed address in your browser and sign in. Away from home, connect a VPN that routes to the Mac's home LAN. The Mac must be awake and logged in.

The dashboard shows the queue, details and logs, and supports reordering, concurrency changes, starting, and cancelling jobs. Connection failures preserve the last data and disable mutations until a successful refresh. No router forwarding or public hosting is configured.

Caddy's Go modules provide HTTPS and Basic authentication inside the remote server process. Only a bcrypt hash is stored. Gorilla CSRF protects mutations, and the Go HTTP server serves a private Unix-socket interface with a fixed queue-control allowlist. Existing passwords and certificate trust survive the embedded gateway upgrade. Disabling access stops only the remote service. Quitting the menu app leaves an enabled remote service running independently. A failed stop remains visible as **Needs attention**.

Remote authentication admission defaults to two concurrent checks, four requests per second per socket-peer IP, a burst of 64, and at most 1,024 peer entries expiring after 600 idle seconds. Cached dashboard polls count toward the rate. A bounded queue of at most `burst` requests accommodates initial asset requests; excess work receives HTTP 429 with `Retry-After`. Forwarded headers cannot choose the rate-limit identity. The existing bcrypt cost, Caddy cache, TLS, and CSRF protections remain in place.

Advanced automation can include this optional object in the password configuration JSON passed privately on stdin to `agent-remote configure`:

```json
"auth_admission": {
  "max_concurrent": 2,
  "requests_per_second": 4,
  "burst": 64,
  "max_peers": 1024,
  "peer_ttl_seconds": 600
}
```

The configuration input still requires `password` and accepts an optional `port`; never put the password in command-line arguments. All five admission fields are required when the object is supplied. Allowed ranges are `max_concurrent` 1-32, `requests_per_second` 0.1-100, `burst` 1-512, `max_peers` 1-65,536, and `peer_ttl_seconds` 1-86,400. The peer TTL must also be at least `burst / requests_per_second`, preventing expiry from restoring a burst before normal refill. Omitting the object preserves existing custom limits, including when the native form saves a password; an unconfigured installation uses the defaults. `agent-remote status --json` reports the effective limits as `authAdmission`. Configuration changes to enabled access use the existing ownership-checked service restart.

## Local data and service operation

The installation uses these intentional per-user locations:

| Purpose | Location |
| --- | --- |
| App (everything runs from inside it) | `/Applications/Agent Queue.app` or `~/Applications/Agent Queue.app` |
| CLI commands (wrappers that call the app's bundled tools) | `~/.local/bin/agent-heavy`, `~/.local/bin/agent-remote` |
| Installation markers (`installation.json`, `build-info.json`), no binaries | `~/.local/share/agent-heavy/` |
| Private configuration | `~/.config/agent-heavy/` |
| Private jobs, receipts, logs and daemon state | `~/.local/state/agent-heavy/` |
| Login services | Bundle-owned, registered with Service Management from `Agent Queue.app/Contents/Library/LaunchAgents/io.went.agent-*.plist` |

No executables are copied out of the app bundle, and new installs write nothing to `~/Library/LaunchAgents`. When migrating from an earlier installer-based install, setup can retire and back up legacy `io.went.agent-*.plist` definitions found there.

The dedicated Go daemon uses a private Unix socket and a durable SQLite database. The queue fails closed when its daemon, state, or process ownership cannot be verified. Do not clear a lock or kill a process to bypass an unknown owner.

After an unclean daemon restart, automatic scheduling remains paused while owned workers and their outcomes are reconciled. Inspect `agent-heavy status`, then use `agent-heavy resume` to resume the queue. `agent-heavy pause` holds future automatic starts without stopping active jobs. An uncertain command attempt is never automatically replayed.

`agent-remote status --json` reports the remote service. `agent-remote disable` stops it. Passwords are passed privately through the native settings form, not command-line arguments.

For an optional migration bridge, `AGENT_HEAVY_LEGACY_SLOT` selects an existing external lease directory; otherwise the queue uses its private execution lease. `AGENT_HEAVY_STATE` selects an alternative private queue state directory. `AGENT_HEAVY_CONFIG` retains the legacy configuration location for migration. Set overrides consistently for both submitters and the daemon environment.

## Development and CI

See [CONTRIBUTING.md](CONTRIBUTING.md) for local commands and the [CI workflow](.github/workflows/ci.yml) for exact hosted checks. The separate [distribution workflow](.github/workflows/distribution.yml) signs, notarizes, verifies and publishes retained main-build artifacts.

Pull requests run Go formatting, vet, vulnerability scanning and race tests, JavaScript lint, shell and workflow checks, isolated Go daemon acceptance tests, embedded Caddy HTTPS/API tests, browser regression tests, native Swift regression tests, and universal app and disk image verification. The workflow runs macOS behavior on Apple Silicon and Intel runners. All validation jobs feed one required `ci-required` check; a failure, cancellation, or skipped required job fails that check.

`main` requires a pull request, current passing checks, resolved review conversations, and verified commit signatures. Force pushes and deletion are blocked, including for administrators. Commits also require an author-matching DCO sign-off. The initial repository import bootstraps the branch; later changes use pull requests.

Distribution requires a successful, verified `main` CI build. A manual distribution run can use corrected main automation with that same build's retained artifacts and notarization checkpoints, preserving its original commit, version and build number. See [Apple signing and notarization](docs/apple-signing.md) for recovery. Pull request code receives read-only repository permissions. Actions are pinned to immutable commit SHAs, npm dependencies use a lockfile, and Go dependencies are pinned in `go.mod` and `go.sum`.

## License

[MIT](LICENSE). See [third-party notices](THIRD-PARTY-NOTICES.md) for bundled Go modules, Zod, and separately installed dependencies.
