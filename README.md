# Agent Job Queue

[![CI](https://github.com/Dunky13/agent-job-queue/actions/workflows/ci.yml/badge.svg)](https://github.com/Dunky13/agent-job-queue/actions/workflows/ci.yml)
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

### Homebrew

On macOS 15 or later, install the app and CLI through the project's [Homebrew tap](https://github.com/Dunky13/homebrew-agent-job-queue):

```sh
brew install --cask dunky13/agent-job-queue/agent-job-queue
```

Homebrew installs the dependencies, sets up your private queue, and opens the menu-bar app. Both `agent-heavy` and `agent-remote` are linked into Homebrew's bin directory. No Xcode or manual installer window is needed.

The build is ad hoc signed and not notarized. The cask removes only `com.apple.quarantine` from its own staged download before running the installer, so installation does not require an Apple Developer Program membership or a Gatekeeper approval prompt. The download's SHA-256 is verified first; signatures and other extended attributes remain intact. This is a deliberate trust decision for this project's tap and does not change system-wide security settings.

The tap becomes installable after the first validated `main` installer release is published. It tracks published builds, so updates can use:

```sh
brew upgrade --cask dunky13/agent-job-queue/agent-job-queue
```

Uninstalling the cask removes the menu app and Homebrew command links. It preserves the queue daemon, running jobs, private backend, data, credentials, and any enabled remote service. Disable remote access in settings or with `agent-remote disable` first if you want it stopped.

### Installer app

1. Use macOS 15 or later on an Apple Silicon or Intel Mac. Install [Homebrew](https://brew.sh/) first.
2. Download the installer ZIP or DMG from [Releases](https://github.com/Dunky13/agent-job-queue/releases). Every successful push to `main` publishes a new prerelease build with SHA-256 checksums. CI runs also retain the installer as an artifact for 30 days.
3. Open **Agent Job Queue Installer.app**, then choose **Install**. The installer displays progress and dependency errors in its window.
4. The app appears in your menu bar. Add your local bin directory to your shell's `PATH` to use `agent-heavy` by name.

The installer includes prebuilt universal executables, so Xcode is not required. Homebrew supplies Pueue, Caddy, uv, and Python 3.12; the installer needs an internet connection to install missing dependencies and the locked Python environment. It installs into your user account without `sudo`.

The installer and app are ad hoc signed. Apple Developer ID signing and notarization are not configured, so macOS may require approval in Privacy & Security before opening a manually downloaded build. Installer checksums identify the downloaded bytes; they do not replace Apple notarization.

Upgrades preserve queue configuration, job data, the running daemon, concurrency settings, and remote credentials. Only the menu-bar UI is replaced. Existing dependencies are not upgraded by the installer.

### If macOS blocks opening the app

Current releases are **not notarized**. Passing the package's code-signature integrity check does not mean Gatekeeper will accept a quarantined download. The Homebrew cask clears quarantine on its own staged download as described above. A manually downloaded ZIP or DMG can still require approval for the installer or installed menu app.

For a downloaded Agent Job Queue app that you trust, use Apple's per-app approval flow:

1. Dismiss the blocked-app dialog, then open **System Settings → Privacy & Security**.
2. Find the entry naming **Agent Job Queue Installer** or **Agent Queue**, then choose **Open Anyway** and confirm **Open**. This option appears after an attempt to open the blocked app, when allowed by your Mac's policy.
3. Reopen the installer and choose **Install**. If the menu app is separately blocked afterward, approve its own named entry the same way.

Use the app name in Privacy & Security to identify a generic verification popup before approving anything. If no matching entry appears, the popup's source is still unknown. Managed Macs can restrict these choices. See [Apple's opening instructions](https://support.apple.com/102445).

Developer ID signing and notarization require an Apple Developer Program membership and are not configured for this project. The manual installer does not remove quarantine. The cask's exception applies only to its own staged files; Gatekeeper remains enabled system-wide.

## Connect your coding agents

On first launch, **Agent integration** offers to add queue instructions for **Codex** and **Claude Code**. Select the runners you use, review the instructions, then choose **Save instructions**. Start new agent sessions afterward. You can skip setup and reopen it from the queue's gear menu at any time, including after a Homebrew installation.

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

Pueue owns submitted jobs independently of the calling terminal and menu app. Closing the UI does not stop jobs. Abruptly losing a submitter leaves its accepted job visible until it finishes or is explicitly cancelled.

## View and manage jobs

The menu-bar app provides project and branch labels, queue positions, elapsed time, results, job details, and bounded log tails. Background checks leave unchanged content in place. Details and confirmations stay within the queue panel.

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

## Phone access

Remote access starts off. In the menu-bar gear menu, open **Remote access**, set a password of at least four characters, and choose **Save password and enable**. A short word or PIN works, with no character-mix requirement. The username is `agent`.

The settings window displays the HTTPS address and certificate sharing controls:

1. Transfer the public `root.crt` certificate to your phone.
2. On Android, install it as a **CA certificate** in the system's certificate settings. Settings names vary by device; search for "CA certificate". This certificate is for HTTPS.
3. Open the displayed address in your browser and sign in. Away from home, connect a VPN that routes to the Mac's home LAN. The Mac must be awake and logged in.

The dashboard shows the queue, details and logs, and supports reordering, concurrency changes, starting, and cancelling jobs. Connection failures preserve the last data and disable mutations until a successful refresh. No router forwarding or public hosting is configured.

Caddy provides HTTPS and Basic authentication. Only a bcrypt hash is stored. Flask-WTF handles CSRF checks, and Waitress serves a private Unix-socket API with a fixed queue-control allowlist. Disabling access stops only the remote service. Quitting the menu app leaves an enabled remote service running independently. A failed stop remains visible as **Needs attention**.

## Local data and service operation

The installation uses these intentional per-user locations:

| Purpose | Location |
| --- | --- |
| App | `~/Applications/Agent Queue.app` |
| CLI commands | `~/.local/bin/agent-heavy`, `~/.local/bin/agent-remote` |
| Runtime source and Python environment | `~/.local/share/agent-heavy/` |
| Private configuration | `~/.config/agent-heavy/` |
| Private jobs, receipts, logs and daemon state | `~/.local/state/agent-heavy/` |
| Login services | `~/Library/LaunchAgents/io.went.agent-*.plist` |

The dedicated Pueue daemon uses a private Unix socket. Existing default Pueue installations are not reused. The queue fails closed when its daemon, state, or process ownership cannot be verified. Do not clear a lock or kill a process to bypass an unknown owner.

`agent-remote status --json` reports the remote service. `agent-remote disable` stops it. Passwords are passed privately through the native settings form, not command-line arguments.

For an optional migration bridge, `AGENT_HEAVY_LEGACY_SLOT` selects an existing external lease directory; otherwise the queue uses its private execution lease. `AGENT_HEAVY_CONFIG` and `AGENT_HEAVY_STATE` select alternative queue configuration and state. `AGENT_HEAVY_PUEUE` and `AGENT_REMOTE_CADDY` can select absolute executable paths. Set overrides consistently for both submitters and the daemon environment.

## Development and CI

See [CONTRIBUTING.md](CONTRIBUTING.md) for local commands and the [workflow](.github/workflows/ci.yml) for exact hosted checks.

Pull requests run Python and JavaScript lint, shell and workflow checks, core unit tests, isolated Pueue acceptance tests, real Caddy HTTPS/API tests, browser regression tests, native Swift regression tests, and universal installer verification. macOS behavior is tested on Apple Silicon and Intel runners. All validation jobs feed one required `ci-required` check; a failure, cancellation, or skipped required job fails that check.

`main` requires a pull request, current passing checks, resolved review conversations, and verified commit signatures. Force pushes and deletion are blocked, including for administrators. Commits also require an author-matching DCO sign-off. The initial repository import bootstraps the branch; later changes use pull requests.

Only a successful `main` workflow can publish an installer. Pull request code receives read-only repository permissions. Actions are pinned to immutable commit SHAs, npm dependencies use a lockfile, and runtime Python dependencies are hash locked.

## License

[MIT](LICENSE). See [third-party notices](THIRD-PARTY-NOTICES.md) for bundled Zod and separately installed dependencies.
