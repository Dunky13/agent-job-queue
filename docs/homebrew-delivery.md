# Verified Homebrew delivery

The Homebrew tap and the Sparkle update feed live on this repository's `main` branch ([ADR 0002](adr/0002-homebrew-tap-in-source-repository.md)). Main CI validates and retains the self-contained app. The separate distribution workflow signs, notarizes, tests and publishes that exact build as a GitHub prerelease, and only then runs its **Publish Homebrew tap and Sparkle feed** job. A failed app build or distribution attempt leaves the previous cask and feed in place.

## Installing and migrating

Homebrew clones a tap's default branch, so tap this repository with its explicit URL:

```sh
brew tap dunky13/agent-job-queue https://github.com/Dunky13/agent-job-queue
brew install --cask dunky13/agent-job-queue/agent-job-queue
```

Users who tapped the old `homebrew-agent-job-queue` repository switch the remote once, keeping the tap name:

```sh
brew tap --custom-remote dunky13/agent-job-queue https://github.com/Dunky13/agent-job-queue
brew update
```

The cask declares `auto_updates true` because Sparkle updates the app in place. A bare `brew upgrade` skips casks that declare it unless `--greedy` is given; `brew upgrade --cask dunky13/agent-job-queue/agent-job-queue` names the cask and upgrades it.

## Delivery contract

The delivery job runs on `macos-15` from the exact automation commit of the distribution run (the prebuilt `cmd/queue-tap`, package `internal/tap`). The tool is built before any secret is in the environment, and the Sparkle key is read once and removed from the process environment. Only `gh` receives the App token; `brew` and `sign_update` get a minimal environment with neither secret. It:

1. Verifies the requested public release, its manifest and the app ZIP checksum, and finds the highest published numbered build through a bounded paginated search. It then requires this run's notarized artifact (`--artifact-dir`) to carry exactly the public manifest and ZIP, SHA-256 included, so a replaced release asset is never signed. If a newer public build exists (verified by release and source ancestry), this run is **superseded**: it reports that and delivers nothing, because only the requested build has a notarized artifact to bind, and the newer run publishes the newer build. A main branch that already holds a newer verified build also yields without a commit.
2. Renders `Casks/agent-job-queue.rb` from the trusted template and loads it through the real Homebrew DSL (`packaging/homebrew/test-cask.rb`). Downloaded Ruby is never evaluated.
3. Signs the published ZIP with Sparkle's `sign_update --ed-key-file - -p`. The private key `SPARKLE_ED_PRIVATE_KEY` goes in on stdin only, never as an argument or in logs.
4. Renders `appcast.xml` with one item for that build (`sparkle:version` is the build number, `sparkle:shortVersionString` the version, `sparkle:minimumSystemVersion` 15.0, an enclosure with the immutable release ZIP URL, its length, type and `sparkle:edSignature`, and the release page as link), then signs the feed with `sign_update --ed-key-file - appcast.xml`. Sparkle reserializes the XML, so the signed feed is parsed as XML and must contain exactly the produced item and a feed signature; byte comparison is never used.
5. Commits `Casks/agent-job-queue.rb`, `Casks/agent-job-queue.json` and `appcast.xml` in one GitHub-signed `createCommitOnBranch` on `main`, using the App token, a `Signed-off-by` trailer for the App bot and `expectedHeadOid` as compare-and-swap. On a head mismatch caused by a concurrent merge it re-reads `main` and retries, up to four attempts. If `main` already holds the same content (feeds are compared semantically, ignoring signatures) it verifies the commits that last changed the three files (Verified signature, App bot author, DCO trailer) and creates no commit. A template or feed change for an already delivered build is committed once; the release and its assets stay untouched.
6. Reads the commit back by SHA: Verified signature, App bot author, DCO trailer, the expected parent and the exact content of all three files.

A missing secret, App installation or App permission fails the job visibly and points here. The feed URL baked into the app is `https://raw.githubusercontent.com/Dunky13/agent-job-queue/main/appcast.xml`.

CI ignores pushes that change only `Casks/**` and `appcast.xml` (`paths-ignore` on the `push` trigger), so a delivery commit cannot start another release. Pull requests are unfiltered so required checks still report.

## App installation and upgrade recovery

The cask is a plain app cask. Its only artifacts are `app "Agent Queue.app"`, which Homebrew installs into `/Applications`, and `uninstall quit: "io.went.agent-queue"`. Nothing runs at `brew install` or `brew upgrade`: there is no installer script, flight step, binary link or setup command. The cask keeps quarantine so macOS assesses the notarized app.

The first launch completes setup. Opening the app from Applications writes the `agent-heavy` and `agent-remote` wrappers to `~/.local/bin`, migrates state and unregisters any queue, resource or phone-access services that older versions registered. The app then starts and supervises its own queue processes; only its login item is registered with Service Management. A later `brew upgrade` replaces the app, and the next launch completes the upgrade. Normal app launch only reads installation and authorization state until setup runs; it never deletes executable code.

Homebrew uninstall asks a running app to quit, which stops its queue processes while running job processes continue, then removes the app. Services, jobs, history and credentials are removed only by the app's ownership-checked service-removal command, which the cask caveats name and which must run before deleting the app. It asks a running app's queue whether it is drained and then quits the app. With active jobs it refuses and leaves the app running:

```sh
~/.local/bin/agent-heavy install --app-bundle "/Applications/Agent Queue.app" --unregister-services
```

No destructive `zap` action is supplied. Private history, configuration and credentials remain retained.

Setup waits for a running queue daemon's active and queued jobs before replacing it, and never starts a live handover. After a normal quit no daemon remains: running jobs continue and their outcomes are recorded without replay, attached `agent-heavy run` callers exit with status 125 within about a second, and if jobs were running the next daemon comes up paused, so queued jobs run after `agent-heavy resume` with no caller attached. Original callers keep their outcomes and exit statuses only when finishing a handover an older release left behind, using that exact build's bundle. Uncertain attempts or ownership fail closed. Removal still requires a drained queue.

Whole previous signed app generations remain visible under `~/Applications/Agent Queue Runtimes`, bound to exact filesystem and attempt identities. Only a later setup operation can reclaim them after verifying terminal attempts and dead original callers and owned workers.

macOS Login Items approval remains an operating-system decision, but it now concerns only the app's login item (opening at login) and never blocks setup or the queue, because the queue processes are the app's own children rather than registered background services. Setup also retires registrations from older versions that are still awaiting approval. Setup cannot bypass this approval or endpoint-protection policy. Unsafe ownership, malformed retained configuration and uncertain job attempts are not guessed or discarded. Deliberately held jobs remain held and can require an operator decision when drain is required. Local fixtures do not establish native Service Management worker survival, Apple acceptance or SentinelOne acceptance.

### Upgrading from the installer cask

Users of the installer-era cask have `~/Applications/Agent Queue.app` recorded as the managed location, with the services running from it, and the old cask left that app in place. Setup refuses a new copy placed elsewhere (such as `/Applications` from the new cask) while the recorded copy exists. Those users must first stop the services, move the old app to the Trash, then upgrade and open the new app:

```sh
~/.local/bin/agent-heavy install --app-bundle "$HOME/Applications/Agent Queue.app" --unregister-services
```

Existing releases and casks remain immutable, and the first transition from a historical cask still runs that predecessor's own removal rules. Setup can retire and back up legacy `~/Library/LaunchAgents/io.went.agent-*.plist` definitions during migration; it applies its own drain and ownership checks first. Publish a new validated numbered release to move users from the historical cask.

## One-time setup

App and key creation need the repository owner's GitHub session. Keep private key contents out of chat and source files.

1. Generate the Sparkle EdDSA key pair with the pinned Sparkle tooling and store the private key as the repository Actions secret, never pasting it into chat. In a checkout, set `dir` to the output of `packaging/sparkle/fetch.sh "$TMPDIR/sparkle"`, then run these four commands in order: `"$dir/bin/generate_keys"` creates the key in your login keychain and prints the public key; `"$dir/bin/generate_keys" -x sparkle-private-key` exports the private key to a file; `gh secret set SPARKLE_ED_PRIVATE_KEY --repo Dunky13/agent-job-queue < sparkle-private-key` stores it; `rm sparkle-private-key` removes the file. Commit only the public key (the base64 line `generate_keys` printed) to `packaging/sparkle/public-ed-key.txt`. Expected result: `gh secret list --repo Dunky13/agent-job-queue` shows `SPARKLE_ED_PRIVATE_KEY` and the exported file is gone.
2. Register or reuse the private Homebrew-sync GitHub App under Dunky13. Grant repository **Contents: Read and write** plus GitHub's automatic metadata access, and install it on **this repository** (`Dunky13/agent-job-queue`). The old tap repository no longer needs it.
3. Save the App's public Client ID as the Actions variable `HOMEBREW_SYNC_APP_CLIENT_ID` and its private key PEM as the Actions secret `HOMEBREW_SYNC_APP_PRIVATE_KEY`.
4. Add the App as a bypass actor on the `main` and `main-ci` rulesets. This is a deliberate, owner-approved exception limited to that App; the rulesets still require signed commits, no force pushes and no deletion. Human changes keep going through pull requests.
5. After the first delivery is live, disable the old tap's scheduled refresh workflow, then archive `homebrew-agent-job-queue` with a pointer to this repository.

The workflow uses the SHA-pinned official `actions/create-github-app-token`, limits its installation token to this repository with `contents: write`, and revokes it after the job. PRs receive neither App credential nor the Sparkle key.

## Recovery

Inspect the failed distribution **Publish Homebrew tap and Sparkle feed** job. Rerun failed distribution jobs to retry unchanged automation: a repeated run verifies already-delivered content without creating another commit. A red delivery job with signing and acceptance green is classed as `delivery` and retried automatically by `scripts/distribution-recover.sh`, first immediately and then at least 15 minutes apart, up to attempt 48 or 7 days for each run. The observer requests at most five reruns per pass and reaches older builds first.

If the automation itself needs a fix, merge the repair and dispatch distribution against the original build run ID as documented in [Apple signing and notarization](apple-signing.md#resuming-a-pending-ci-submission). A matching immutable release is verified rather than overwritten.

Common failures:

- `Configure HOMEBREW_SYNC_APP_CLIENT_ID, HOMEBREW_SYNC_APP_PRIVATE_KEY and SPARKLE_ED_PRIVATE_KEY`: a secret or variable is missing.
- Token creation fails: the App is not installed on this repository or lacks **Contents: Read and write**.
- `Publishing to main failed`: the App is not a bypass actor on the `main` or `main-ci` ruleset, or its permission was reduced.
- `would downgrade` or `conflicting release metadata`: `main` holds a newer or different build than the public releases show. Do not edit `Casks/` or `appcast.xml` by hand; investigate the releases first.

Never edit generated `Casks/` files or `appcast.xml` manually, and never execute code from a release to repair drift.

Publishing a release or updating the feed does not modify the operator's Mac. Version visibility requires the new app to be installed and running; open its gear menu and choose **About and updates**.

## Validation

The tests simulate pagination, requested release validation, checksum failures, conflicting identities, verified supersession, signed readback, expected-head retries, identical content, appcast rendering against a golden file and signing with an inert fake `sign_update` that proves the key never appears in arguments. They do not call GitHub, install an app or use the live queue. From the source checkout:

```sh
go test -p 2 ./internal/tap ./internal/tooling -v
```

This guide has a complete standalone [HTML version](homebrew-delivery.html).
