# Contributing

Use Go 1.26.10+, Node.js 26.12.1+, macOS 15+, and Xcode command-line tools. Building the native apps and the Darwin resource sampler requires the macOS SDK. The distributed installer includes universal Apple Silicon and Intel binaries, so installed users need neither Xcode nor a Go toolchain. Python and uv are not used.

```sh
brew install go actionlint shellcheck
npm ci
npx playwright install chromium
git config core.hooksPath .githooks
```

Run the complete finite validation batch through your shared heavy-job queue:

```sh
agent-heavy run --label "Agent Queue validation" -- bash scripts/validate.sh
```

The batch checks source portability, Go formatting and vet, known reachable Go vulnerabilities, shell and workflow lint, Go race tests, standalone trusted tap tests, Linux compilation of CI tools, browser schema and interaction tests, native regressions, and the universal installer. Hosted CI independently runs the runtime suite on Apple Silicon and Intel Macs.

For focused development, these commands use only this checkout and private fixtures:

```sh
go test -p 2 -parallel 2 ./internal/backend -run TestResults
go test -p 2 -parallel 2 ./internal/backend -run TestRealDaemon
go test -p 2 -parallel 2 ./internal/remote
go test -p 2 ./internal/tooling ./internal/delivery ./packaging/homebrew
bash scripts/build-go.sh
npm run test:web
npm run test:browser
bash scripts/test-native.sh
scripts/build-installer.sh
scripts/test-installer.sh "dist/Agent-Job-Queue-Installer-$(cat VERSION)-universal.zip"
bash scripts/test-dmg.sh "dist/Agent-Job-Queue-Installer-$(cat VERSION)-universal.dmg"
```

Apply the shared queue policy to broad tests, builds, analyzers, browser suites, and installers. Keep the Go package and test worker limits at two unless explicitly authorized otherwise. Do not nest queue wrappers. Persistent preview servers stay outside the queue.

Core and HTTPS acceptance tests launch temporary private daemons, directories, child processes, ports, and test credentials. They remove inherited queue overrides before creating their fixture. Browser tests use simulated jobs through the actual Go HTTP interface. Installer and agent integration tests use isolated user directories. They must never operate on the installed queue or real agent instructions. The installer artifact test installs the actual packaged Go binaries into a temporary home and exercises their private queue and scheduler.

The disk-image check requires the DMG explicitly, mounts it read-only at a private mountpoint, and verifies the enclosed installer signatures, executable permissions, universal architectures, and build identities. A failed detach fails validation and retains its temporary directory. Packaging flushes staged files and disables indexing on its HFS+ image; verbose image-creation diagnostics remain available if macOS reports resource contention.

New disk images contain the self-contained `Agent Queue.app` and an `Applications` shortcut. The app embeds only the source support payload, with no recursive app bundle. First launch from the shared or user Applications folder installs its private backend in place. The installer ZIP retains its existing custom-installer layout for updater and Homebrew compatibility. Disk-image verification also supports retained legacy installer images. Validate a standalone app with `go run -p 2 ./cmd/queue-tool verify-app --app APP_PATH`.

Local installer builds use ad hoc development signatures by default. Main CI validates and retains the universal installer. A separate main-only distribution workflow consumes that exact build, requires Developer ID signing, payload-first Apple notarization, stapled app and DMG tickets, and fresh Apple Silicon and Intel acceptance before publication and Homebrew delivery. Distribution fixes can process a retained build with newer trusted main automation. PRs receive no Apple secrets. See [Apple signing and notarization](docs/apple-signing.md) for credential custody, recovery commands and acceptance evidence. Never remove quarantine to make a notarized release pass.

The native regression tests are headless. For menu-bar gestures, a logged-in graphical macOS session is also needed:

```sh
mkdir -p build
bash native/tests/run-menu-bar-ui-tests.sh build/menu-bar-verification.json
```

The runner builds a private fixture app and sends AppKit events through the actual status button, menus, and window handlers. It records `skippedChecks` when macOS declines foreground activation. `--require-foreground` fails those unavailable checks. It cleans up its own fixture and preserves the installed app and clipboard.

First-launch setup decisions, retries, and guided upgrade recovery are included in the headless native suite. The read-only preflight lists blockers together and offers bounded waiting, reviewed job cancellation, verified cleanup and service startup, phone access disablement, and guided configuration repairs. See [guided upgrade recovery](docs/upgrade-recovery.md) for the action and controller contracts. In a logged-in graphical session, verify the production setup window, failed setup, Disable phone access and continue action, ordinary Retry button, and transition to the simulated queue:

```sh
agent-heavy run --label "First-launch UI checks" -- bash native/tests/run-bootstrap-ui-tests.sh build/bootstrap-ui-verification
```

This runner uses temporary app and home directories and an injected setup executor. It writes verification results, screenshots, and a self-contained HTML view into the output directory without installing or changing the real queue.

## Preview and screenshots

The app, installer, and dashboard share the stacked-layers identity defined in
`native/QueueBrand.swift`. Native surfaces use its template symbol so macOS can
adapt its color. The app and installer bundles use `native/Resources/AppIcon.icns`;
the dashboard header, browser favicon, and phone home-screen icon use the PNGs in
`remote/static`. These generated files are committed, so ordinary builds require
no asset generation. After changing the identity, regenerate them on macOS with
`bash scripts/generate-icons.sh`. The generator uses AppKit and `iconutil`, with
no external image dependencies. See [icon consistency](docs/icon-consistency.md).

```sh
bash scripts/build-go.sh
bash scripts/preview-screenshots.sh --bind YOUR_PRIVATE_LAN_IPV4 --port 8080
```

The Go preview serves the actual dashboard with public simulated jobs, including mobile and desktop layouts. It cannot read installed queue state, change instructions, or load real credentials. Share the printed LAN URL and keep the process running for the review window. Stop it with Ctrl+C afterward.

```sh
scripts/capture-native-screenshots.sh
```

Native screenshot capture uses artificial jobs and an inert action recorder. It needs a graphical session. Never capture private queue logs, passwords, certificates, or real instruction files.

## Migration contracts

The CLI command names, JSON fields, SQLite schema and workload identities remain compatible with the Python implementation. Preserve lifecycle, cancellation, process ownership, crash recovery, authentication, scheduling, history and UI regression coverage. macOS resource observations use the same native Mach and libproc counters. Missing or stale telemetry cannot authorize adaptive work.

The first Pueue-to-embedded installation requires a fully drained private queue; migration from Python also requires disabled old remote access. When old phone access blocks first-launch setup, choose **Disable phone access and continue** to stop it through the bundled controller's ownership checks and resume installation. Credentials, configuration, and trusted certificates remain available; phone access can be enabled again in settings after setup. The CLI equivalent is the trailing `--disable-phone-access` option on `agent-heavy install --payload DIRECTORY` or `agent-heavy install --app-bundle ABSOLUTE_APP_PATH`. This recovery only disables phone access when it blocks Python migration. After ownership-checked shutdown, it imports completed tasks and the retained ID sequence into the new SQLite queue, then starts the independent Go daemon and resource controller. Later embedded queue upgrades retain active daemon jobs. An enabled Go remote service is restarted through its ownership-checked lifecycle to adopt the embedded gateway. Its existing bcrypt hash, CSRF secret, and Caddy storage remain in place, including the CA trusted by phones. Private state, history, lease settings, credentials, and the incomplete installation marker remain authoritative across failures and retries. Pueue and a separate Caddy executable are not required for development, installation, or acceptance tests.

The command queue uses the existing pure Go SQLite driver. A durable attempt is claimed before process creation, and detached workers publish their own outcomes. Never automatically retry an attempt with an uncertain outcome. An unclean daemon restart pauses automatic dispatch; verified process ownership and explicit operator recovery remain required. Queue RPC handlers must not acquire backend filesystem locks already held by the scheduler or submitting process.

Go dependencies are pinned in `go.mod` and `go.sum`; run `go mod tidy` after deliberate dependency changes. Installer builds omit Go debug sections to keep universal binaries and downloads within the existing size limits; developer builds keep them. Installer builds collect the runtime and dependency notices into `source/licenses`. The standalone tap helper has no external dependencies. Trusted tap changes require a companion reviewed tap update before the source release, as described in [Homebrew delivery](docs/homebrew-delivery.md).

## Commit policy

Keep configured cryptographic signing enabled and include an author-matching DCO sign-off:

```sh
git commit -s -m "feat(queue): describe the change"
```

The pre-push hook checks the complete PR range from `origin/main`, including previously pushed commits, through the Go commit policy tool. Fetch `origin/main` before pushing. A bootstrap repository without that reference checks initial history or commits since the remote branch. GitHub verifies DCO and signatures and protects `main` with the aggregate required check. Do not bypass checks or branch protection.

A squash merge creates a new commit. Its author identity must exactly match a `Signed-off-by` trailer, including when GitHub selects a noreply address. Select the matching verified email or add the matching trailer. Keep the final identity and signature checks intact.

For this repository's maintainer, use `Marc Went <marc@went.io>` as the author and committer identity for future commits. Keep exactly one maintainer certification in each commit and final squash body: `Signed-off-by: Marc Went <marc@went.io>`. Let `git commit -s` add the trailer once. When preparing an amended or squash message, consolidate duplicate maintainer trailers and the maintainer's noreply alias into that single primary-email trailer; preserve other contributors' certifications.

For maintainer-owned PRs, select `marc@went.io` as the final squash author email in GitHub, or pass `--author-email marc@went.io` to the otherwise approved `gh pr merge --squash` command. Verify the resulting author, single maintainer trailer, and signature after merging. This replaces the earlier dual-address workaround; historical commits retain their original metadata.

For a Dependabot-owned PR, recreating its commits under a maintainer identity and supplying `gh pr merge --author-email` does not guarantee that GitHub changes the squash author. PR 12 retained Dependabot's noreply author while carrying the maintainer's DCO trailer. When taking over a bot update with a maintainer certification, open a maintainer-owned replacement PR from the validated changes and close the bot PR. Preserve the actual certifier's sign-off; do not synthesize a bot certification. Verify the resulting merge author, trailer, and GitHub signature after merging.
