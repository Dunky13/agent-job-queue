# Spec: Sparkle updates, GitHub Pages feed and homepage

Decisions: [ADR 0001](../adr/0001-sparkle-updates-in-place.md) and [ADR 0003](../adr/0003-drop-homebrew-feed-on-github-pages.md), which supersedes ADR 0002. The pinned Sparkle 2.10.1 download is `packaging/sparkle/fetch.sh`, which verifies its SHA-256 and prints the extracted directory. All packages use it, so don't add another download path.

This work ships as stacked pull requests, merged in this order:

1. **B (drop Homebrew, Pages delivery)**.
2. **C (homepage)**, the `site/` content B deploys.
3. **A (Sparkle in the app)**. Merging A before B would ship builds whose feed returns 404.

## Rules for both packages

- Follow `AGENTS.md` and `CONTRIBUTING.md`: Conventional Commits, signed commits with DCO sign-off, no em dashes, and no machine-local paths.
- Run heavy validation through `agent-heavy run --label '...' -- <cmd>`, with at most 2 Go workers (`-p 2`).
- Keep `agent-job-queue.json` and the ZIP and DMG asset names unchanged; only `agent-job-queue.rb` is dropped from the publish script's `names=(...)` list. Installed builds use them for their last manual hop to the first Sparkle build.
- Keep the signing, notarization, checkpoint and resume flow fail-closed. Don't weaken any existing test or check. Update tests that pin shapes you deliberately change, such as `internal/tooling/workflow_test.go`.
- Where docs or tests contradict this spec, update them to match. That includes every statement that the updater never writes into Applications, that it quarantines its copy, that it delivers to Downloads, or that `agent-heavy updates` exists.

## A: Sparkle in the app

### Build

- `native/build.sh` links Sparkle:
  - `-F <sparkle dir> -framework Sparkle`.
  - `-Xlinker -rpath -Xlinker @executable_path/../Frameworks`.
- `native/build-library.sh` gets `-F` so `import Sparkle` compiles.
- Tests link the framework too, including `scripts/test-native.sh` and anything else that builds or runs the library.
- Under Swift 6 with `-warnings-as-errors`, use `@preconcurrency import Sparkle` if needed.

### Bundle assembly

- `scripts/build-app.sh` copies `Sparkle.framework` into `Contents/Frameworks` with `ditto`, preserving the symlinks. It then deletes `XPCServices` (the app is not sandboxed) and `PrivateHeaders`, `Headers` and `Modules` (not needed at runtime).
- It adds these `Info.plist` keys:
  - `SUFeedURL` = `https://dunky13.github.io/agent-job-queue/appcast.xml`.
  - `SUPublicEDKey` from the committed file `packaging/sparkle/public-ed-key.txt`, one base64 line.
  - `SURequireSignedFeed` = true.
  - `SUVerifyUpdateBeforeExtraction` = true.
  - `SUEnableAutomaticChecks` is left unset, so Sparkle asks for consent once.
- If the key file is missing or malformed for a tracked build (`BUILD_NUMBER` set), the build fails. Untracked builds may omit it.
- `THIRD-PARTY-NOTICES.md` and the license output include Sparkle's `LICENSE` from the tarball.

### Signing (`scripts/macos-signing.sh` and resume/checkpoint state)

- Sign inside-out, with hardened runtime and a timestamp and without `--deep`. The order is:
  1. `Sparkle.framework/Versions/B/Autoupdate`.
  2. `Sparkle.framework/Versions/B/Updater.app`.
  3. `Sparkle.framework`.
  4. The existing helpers.
  5. The app.
- Ad hoc local signing follows the same order.
- If the checkpoint or resume state lists signed executables, add these entries.

### Verification (`queue-tool verify-app` and its tests)

- Allow exactly one nested app, `Contents/Frameworks/Sparkle.framework/Versions/B/Updater.app`, and the expected framework layout. Reject any other nested app or framework, and any `XPCServices`.
- Sparkle's Mach-O files must be universal and strictly signed by the same team.

### Runtime (Swift)

- Replace the download UI in `native/AgentUpdates.swift` with an `SPUStandardUpdaterController`, created only when all of these hold:
  - The running build is tracked (`RunningQueueVersion.publishedBuild != nil`, with no metadata error).
  - It is not a test-hook build.
  - It is not running in preview mode.
- The **About and updates** window keeps the version display and gets a **Check for Updates…** button bound to `canCheckForUpdates`. Untracked builds show why updates are unavailable and link to the releases page.
- An `SPUUpdaterDelegate` refuses checks and installs (`updater(_:mayPerform:)` throws) while the incomplete-installation marker exists. Reuse the existing marker check.
- Drain before installing. Run the drain in `updater(_:willInstallUpdateOnQuit:immediateInstallationBlock:)` and in `updater(_:shouldPostponeRelaunchForUpdate:untilInvokingBlock:)`:
  - The app runs the bundled `agent-heavy drain` with the default bounded expiry, then calls the install block.
  - If the drain fails, it logs the failure and still proceeds, because the drain only protects callers.
  - After relaunch, once setup reports the queue up, setup runs `agent-heavy undrain` only when a marker file written by this flow exists. It never removes a drain that an operator set.
- `applicationShouldTerminate` already stops setup and helpers and then replies. Verify that Sparkle's install-on-quit and relaunch complete through it, and that nothing returns `.terminateCancel` during an update.

### Deletions

- `internal/backend/updates.go`, `updates_test.go` and the `updates` CLI command and its usage text.
- The Swift download, staging and Downloads-copy code and its tests in `native/tests/UpdateTests.swift`.
- `agent-heavy version` stays. Keep the code that `version` still uses.

### Docs

- Rewrite `docs/updates.md`, and regenerate `docs/updates.html` the way the other docs are generated.
- Update `docs/upgrade-recovery.md`.
- Add a handoff in `docs/handoffs/sparkle-updates.md`.

### Tests

- Info.plist keys present for tracked builds.
- Key missing for a tracked build fails.
- verify-app accepts the Sparkle layout and rejects an extra nested app, XPCServices or a wrong signature.
- Updater not created for untracked or preview builds.
- Delegate refuses while the marker exists.
- Undrain runs only with the flow's marker.

## B: Drop Homebrew and deliver through GitHub Pages

### Removals

- `Casks/`, cask rendering (`tooling.RenderCask` and its callers), `packaging/homebrew/`, the Homebrew DSL checks and the notarized Homebrew install test in `distribution.yml`, and the cask-only tests (`managed_cask_*`, `notarized_cask_test.go`, `homebrew_lifecycle_test.go` and the cask parts of other tests).
- `agent-job-queue.rb` as a release asset. Remove it from `scripts/distribution-publish.sh` and its tests.
- The GitHub App token step, `createCommitOnBranch`, the CAS and readback code, and the CI `paths-ignore` guard.
- Keep runtime handling of Homebrew-installed apps in `internal/backend` and `internal/remote`, and its tests. Rename nothing there.

### Delivery job (`Publish Sparkle feed and homepage`, `macos-15`)

1. Bind the public release to this run's notarized artifact (the existing `bindArtifact`), and select the newest build. An older run reports "Superseded" and deploys nothing.
2. Fetch the live `https://dunky13.github.io/agent-job-queue/latest.json`. If it names a newer build, report "Superseded". If it names the same build with identical feed content (signatures ignored), report "Already current" and skip the deploy. Only a missing site (404) counts as empty; any other fetch failure fails the job.
3. Sign the ZIP and render and sign `appcast.xml` as now. Verify both signatures against `packaging/sparkle/public-ed-key.txt` with `crypto/ed25519` as now. The key goes on stdin only.
4. Write `latest.json` with strict fields: `schemaVersion`, `version`, `build`, `commit`, `tag`, `publishedAt`, `releaseUrl`, `dmgUrl`, `dmgSha256`, `zipUrl`, `zipSha256`. Use the manifest and the release.
5. Assemble the site: copy `site/` from the automation commit, then add `appcast.xml` and `latest.json`. The assembler refuses anything outside `site/`, and refuses symlinks and files over a size bound.
6. Deploy with SHA-pinned `actions/upload-pages-artifact` and `actions/deploy-pages`. Use job permissions `pages: write` and `id-token: write`, plus `contents: read`, in the `github-pages` environment. Pass the Sparkle secret only to the signing step.
7. Read back: the live `latest.json` names this build, and the live `appcast.xml` parses and verifies against the public key. Retry the readback for a bounded time to allow for Pages propagation.

- `scripts/distribution-recover.sh` classifies the renamed job as `delivery` and keeps accepting the old job names for historical runs.
- The workflow-shape and secret-leak tests are updated for the new job.

### Docs

- Replace `docs/homebrew-delivery.md` and its HTML with `docs/pages-delivery.md` and its HTML. Cover:
  - Enabling Pages with source "GitHub Actions".
  - The EdDSA key commands.
  - Recovery.
  - What existing Homebrew users do: nothing is required, since Sparkle updates the app. `brew untap dunky13/agent-job-queue` is optional, and they must never run `brew uninstall`, which deletes the app.
- README install instructions: the homepage, the DMG and the release page.
- Update `CONTRIBUTING.md`, `apple-signing.md`, `upgrade-recovery.md` and their HTML where they mention Homebrew delivery.
- Replace `docs/handoffs/in-repo-tap.md` with `docs/handoffs/pages-delivery.md`.
- Leave historical handoffs untouched.

### Tests

- The superseded and already-current paths: a live newer build, the same build, a 404 counting as empty, and other fetch errors failing.
- The `latest.json` golden file.
- The assembler rejects files outside `site/`, symlinks and oversized files.
- Readback mismatches fail.
- Existing signing, artifact-binding and leak-guard tests are kept.

## C: Homepage (`site/`)

- A static, dependency-free homepage in `site/`: `index.html`, CSS, and images optimized from `docs/screenshots` and the app icon. No build step, no external fonts, scripts or trackers, and nothing fetched from third-party origins.
- **Content:**
  - What Agent Queue is: a macOS menu-bar queue for heavy agent commands, with adaptive scheduling, learning and phone access. Take facts only from `README.md` and `docs/`; invent no claims.
  - A primary **Download for macOS** button.
  - Requirements: macOS 15 or later, Apple Silicon and Intel.
  - Screenshots, a short "how it works", the CLI basics (`agent-heavy run ...`), and links to the GitHub repo, the docs on GitHub, the releases and the license.
- **Download button:** the button's `href` is the release page by default. A small script fetches same-origin `latest.json`, validates its shape, and then points the button at `dmgUrl`, showing the version and build. If the fetch or validation fails, the button keeps the release-page link.
- **Accessibility and quality:** semantic HTML, keyboard and screen-reader accessible, responsive from 360 px to wide desktop, light and dark via `prefers-color-scheme`, reduced motion respected, real `alt` text, good contrast, fast with no layout shift, and correct `<title>`, description and Open Graph tags.
- **Tests:** a lightweight check, in Go or the existing web test tooling, that `site/` has no external origins, every local link and image resolves, and the `latest.json` handling has fallback behavior. Use the repo's existing lint for HTML, CSS and JS where configured.

## Human-only steps (not done by agents)

1. Generate the EdDSA key, store the private key as the Actions secret `SPARKLE_ED_PRIVATE_KEY`, and give the public key for `packaging/sparkle/public-ed-key.txt`.
2. Enable GitHub Pages with source "GitHub Actions".
3. After the first successful deployment: disable the old tap's refresh workflow, add the README notice, and archive `homebrew-agent-job-queue`.
4. Acceptance gate: once two Sparkle builds are published, update Agent Queue from build N to N+1 on the SentinelOne Mac.
