# Spec: Sparkle updates and in-repository Homebrew tap

Decisions: [ADR 0001](../adr/0001-sparkle-updates-in-place.md) and [ADR 0002](../adr/0002-homebrew-tap-in-source-repository.md). The pinned Sparkle 2.10.1 download is `packaging/sparkle/fetch.sh`, which verifies its SHA-256 and prints the extracted directory. Both packages use it, so don't add another download path.

This work ships as two pull requests that are developed in parallel and merged in this order:

1. **B (tap and feed)**.
2. **A (Sparkle in the app)**. Merging A first would ship builds whose feed returns 404.

## Rules for both packages

- Follow `AGENTS.md` and `CONTRIBUTING.md`: Conventional Commits, signed commits with DCO sign-off, no em dashes, and no machine-local paths.
- Run heavy validation through `agent-heavy run --label '...' -- <cmd>`, with at most 2 Go workers (`-p 2`).
- Keep `agent-job-queue.json`, the release asset names and the publish script's fixed `names=(...)` list unchanged. Installed builds use them for their last manual hop to the first Sparkle build.
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
  - `SUFeedURL` = `https://raw.githubusercontent.com/Dunky13/agent-job-queue/main/appcast.xml`.
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

## B: Tap and feed on main

### Repository layout

- Add `Casks/agent-job-queue.rb` and `Casks/agent-job-queue.json` with the current content of `Dunky13/homebrew-agent-job-queue` at branch `tap`. Fetch them with `gh api`.
- The cask template gains `auto_updates true`.

### Delivery

- Replace the tap dispatch (the `Verify Homebrew delivery` job, `cmd/queue-delivery` and `internal/delivery`) with an in-repository delivery job on `macos-15`.
- Reuse `packaging/homebrew/refresh.go` logic, moved into a normal package with its tests. Delete the parity and sync tooling, which no longer has a second repository to match.
- The job:
  1. Verifies the published release and ZIP checksum, as `refresh.go` does.
  2. Selects the highest published build and never regresses.
  3. Renders the cask and checks it through the real Homebrew DSL.
  4. Signs the published ZIP with `sign_update --ed-key-file - -p`. The secret `SPARKLE_ED_PRIVATE_KEY` goes in on stdin only, never as an argument or in logs.
  5. Renders `appcast.xml` with one item for that build:
     - `sparkle:version` = build number.
     - `sparkle:shortVersionString` = version.
     - `sparkle:minimumSystemVersion` = `15.0`.
     - `enclosure` with the immutable release ZIP URL, its `length`, `type="application/octet-stream"` and `sparkle:edSignature`.
     - The release page as `link`.
  6. Signs the feed with `sign_update --ed-key-file - <appcast.xml>`.
  7. Commits all three files in one GitHub-signed `createCommitOnBranch` on `main`. The commit uses the App token (`contents: write` on this repository only), the DCO trailer and `expectedHeadOid`.
     - On a head mismatch caused by a concurrent merge, it re-reads `main` and retries a bounded number of times.
     - If `main` already has identical content, it verifies and makes no commit.
  8. Reads the commit back by SHA: content, signature verified, author.
- A missing secret, App configuration or App permission fails the job visibly, with a pointer to the docs.
- `scripts/distribution-recover.sh` keeps its retry classification for the renamed delivery job.

### CI loop guard

`.github/workflows/ci.yml` push to `main` uses `paths-ignore: ['Casks/**', 'appcast.xml']`. Leave `pull_request` unfiltered so required checks still report on PRs.

### Docs

- Rewrite `docs/homebrew-delivery.md`, and its HTML, covering:
  - The new tap command.
  - `brew tap --custom-remote` migration.
  - App setup: install the App on this repository, and add it as bypass actor on the `main` and `main-ci` rulesets.
  - The EdDSA secret.
  - Recovery.
- README install instructions.
- Add a handoff in `docs/handoffs/in-repo-tap.md`.

### Tests

- Appcast rendering against a golden file.
- The signature step never puts the key on argv. Test this with an inert fake `sign_update`.
- CAS retry on head mismatch.
- No commit on identical content.
- Workflow shape tests updated.

## Human-only steps (not done by agents)

1. Generate the EdDSA key and commit the public key to `packaging/sparkle/public-ed-key.txt`.
2. Store the private key as the Actions secret `SPARKLE_ED_PRIVATE_KEY`.
3. Install the Homebrew-sync App on this repository and add it as a bypass actor on the `main` and `main-ci` rulesets.
4. After B is live: disable the old tap's refresh workflow, then archive `homebrew-agent-job-queue`.
5. The acceptance gate: once two Sparkle builds are published, update Agent Queue from build N to N+1 on the SentinelOne Mac.
