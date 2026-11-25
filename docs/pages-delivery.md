# Verified delivery through GitHub Pages

The Sparkle update feed and the project homepage are served by GitHub Pages at `https://dunky13.github.io/agent-job-queue/` ([ADR 0003](adr/0003-drop-homebrew-feed-on-github-pages.md)). Nothing is committed to `main` for delivery. Main CI validates and retains the self-contained app. The separate distribution workflow signs, notarizes, tests and publishes that exact build as a GitHub prerelease, and only then runs its **Publish Sparkle feed and homepage** job. A failed app build or distribution attempt leaves the live site as it was.

The feed URL baked into the app is `https://dunky13.github.io/agent-job-queue/appcast.xml`. The same site serves `latest.json`, which the homepage reads, and the homepage files from `site/`.

## Installing

Open the homepage, or take the DMG or ZIP from the [latest release](https://github.com/Dunky13/agent-job-queue/releases). Drag **Agent Queue** to Applications and open it to finish setup. The app then updates itself through Sparkle.

## Existing Homebrew users

Nothing is required. The installed app keeps working and receives updates through Sparkle once it includes the new feed address.

- Optional: remove the tap with `brew untap dunky13/agent-job-queue`. This changes only Homebrew's own records.
- Do not run `brew uninstall`: it deletes the app. The cask is no longer updated, so Homebrew no longer reports new versions, which is expected.
- To reinstall by hand later, use the DMG or ZIP from the homepage or the release page.

## Delivery contract

The delivery job runs on `macos-15` from the exact automation commit of the distribution run (the prebuilt `cmd/queue-pages`, package `internal/pages`). The tool is built before any secret is in the environment, and the Sparkle key is read once and removed from the process environment. Only `gh` receives the workflow token; `sign_update` gets a minimal environment with neither secret. The job has `contents: read`, `pages: write` and `id-token: write`, runs in the `github-pages` environment and holds no App token. It:

1. Verifies the requested public release, its manifest, the app ZIP checksum and the published DMG checksum file, and finds the highest published numbered build through a bounded paginated search. It then requires this run's notarized artifact (`--artifact-dir`) to carry exactly the public manifest, ZIP and DMG, SHA-256 included, so a replaced release asset is never signed. If a newer public build exists (verified by release and source ancestry), this run is **superseded**: it reports that and deploys nothing, because only the requested build has a notarized artifact to bind, and the newer run publishes the newer build.
2. Fetches the live `https://dunky13.github.io/agent-job-queue/latest.json`. Only an HTTP 404 means no site yet; every other fetch or parse failure fails the job. A live build that is newer than this run means **Superseded** and deploys nothing. A live build that is older must be a verified public release and an ancestor of this run. The same build with identical feed content (signatures ignored) means **Already current** and skips the deploy. A live `latest.json` that differs from its own public release fails the job.
3. Signs the published ZIP with Sparkle's `sign_update --ed-key-file - -p`. The private key `SPARKLE_ED_PRIVATE_KEY` goes in on stdin only, never as an argument or in logs. Before anything is deployed, the tool verifies with `crypto/ed25519` that the enclosure signature matches the exact ZIP and that the feed signature matches the feed bytes Sparkle signs (everything before the trailing `sparkle-signatures` comment, whose `length` is that byte count), both against `packaging/sparkle/public-ed-key.txt`, the key the app trusts. A missing or malformed key file, or a secret that does not match it, fails the job visibly instead of stranding installed apps.
4. Renders `appcast.xml` with one item for that build (`sparkle:version` is the build number, `sparkle:shortVersionString` the version, `sparkle:minimumSystemVersion` 15.0, an enclosure with the immutable release ZIP URL, its length, type and `sparkle:edSignature`, and the release page as link), then signs the feed with `sign_update --ed-key-file - appcast.xml`. Sparkle reserializes the XML, so the signed feed is parsed as XML and must contain exactly the produced item and a feed signature; byte comparison is never used.
5. Writes `latest.json` with `schemaVersion`, `version`, `build`, `commit`, `tag`, `publishedAt`, `releaseUrl`, `dmgUrl`, `dmgSha256`, `zipUrl` and `zipSha256`. The links are derived from the tag and must match.
6. Assembles the site: the files under `site/` plus `appcast.xml` and `latest.json`. The assembler accepts only regular files inside `site/` and refuses symbolic links, hidden files other than `.nojekyll`, special files, files over 8 MiB, more than 1000 files or 64 MiB in total, and any file named like a generated one. A `site/` with only the homepage is enough, and a missing `site/` fails with a clear message.
7. Uploads the assembled directory with the SHA-pinned `actions/upload-pages-artifact` and deploys it with `actions/deploy-pages`. A deployment replaces the whole site, which is why the job reads the live site first.
8. Reads the live `latest.json` and `appcast.xml` back with a bounded retry (18 attempts, 10 seconds apart). The live feed must equal the deployed feed and verify against the public key. A newer build deployed meanwhile by a later run is accepted.

A missing secret fails the job visibly and points here. Pull requests receive neither Pages permissions nor the Sparkle key.

## One-time setup

Key creation and Pages settings need the repository owner's GitHub session. Keep private key contents out of chat and source files.

1. Generate the Sparkle EdDSA key pair with the pinned Sparkle tooling and store the private key as the repository Actions secret, never pasting it into chat. In a checkout, set `dir` to the output of `packaging/sparkle/fetch.sh "$TMPDIR/sparkle"`, then run these four commands in order: `"$dir/bin/generate_keys"` creates the key in your login keychain and prints the public key; `"$dir/bin/generate_keys" -x sparkle-private-key` exports the private key to a file; `gh secret set SPARKLE_ED_PRIVATE_KEY --repo Dunky13/agent-job-queue < sparkle-private-key` stores it; `rm sparkle-private-key` removes the file. Commit only the public key (the base64 line `generate_keys` printed) to `packaging/sparkle/public-ed-key.txt`. The public key file must exist on `main` before the first delivery; delivery fails without it. Expected result: `gh secret list --repo Dunky13/agent-job-queue` shows `SPARKLE_ED_PRIVATE_KEY` and the exported file is gone.
2. Enable GitHub Pages with the source set to **GitHub Actions**: in the repository, open **Settings > Pages** and choose **GitHub Actions** under **Build and deployment**. The `github-pages` environment is created with it; keep its branch rule limited to `main`. Expected result: the first delivery deploys without a "Pages not enabled" error.
3. Make sure the `site/` directory with the homepage exists on `main`. The homepage is owned separately from delivery.

The old App token needs no replacement: remove the `HOMEBREW_SYNC_APP_CLIENT_ID` variable and `HOMEBREW_SYNC_APP_PRIVATE_KEY` secret, uninstall the Homebrew-sync App from this repository and drop it as a bypass actor on the `main` and `main-ci` rulesets. Nothing else in delivery writes to `main`.

## App installation and upgrade recovery

The app is installed by dragging it from the DMG or ZIP into `/Applications`, or by a Sparkle update. Nothing runs at install: there is no installer script or setup command.

The first launch completes setup. Opening the app from Applications writes the `agent-heavy` and `agent-remote` wrappers to `~/.local/bin`, migrates state and unregisters any queue, resource or phone-access services that older versions registered. The app then starts and supervises its own queue processes; only its login item is registered with Service Management. An update replaces the app, and the next launch completes the upgrade. Normal app launch only reads installation and authorization state until setup runs; it never deletes executable code.

Services, jobs, history and credentials are removed only by the app's ownership-checked service-removal command, which must run before deleting the app. It asks a running app's queue whether it is drained and then quits the app. With active jobs it refuses and leaves the app running:

```sh
~/.local/bin/agent-heavy install --app-bundle "/Applications/Agent Queue.app" --unregister-services
```

Private history, configuration and credentials remain retained.

Setup waits for a running queue daemon's active and queued jobs before replacing it, and never starts a live handover. After a normal quit no daemon remains: running jobs continue and their outcomes are recorded without replay, attached `agent-heavy run` callers exit with status 125 within about a second, and if jobs were running the next daemon comes up paused, so queued jobs run after `agent-heavy resume` with no caller attached. Original callers keep their outcomes and exit statuses only when finishing a handover an older release left behind, using that exact build's bundle. Uncertain attempts or ownership fail closed. Removal still requires a drained queue.

Whole previous signed app generations remain visible under `~/Applications/Agent Queue Runtimes`, bound to exact filesystem and attempt identities. Only a later setup operation can reclaim them after verifying terminal attempts and dead original callers and owned workers.

macOS Login Items approval remains an operating-system decision, but it concerns only the app's login item (opening at login) and never blocks setup or the queue, because the queue processes are the app's own children rather than registered background services. Setup also retires registrations from older versions that are still awaiting approval. Setup cannot bypass this approval or endpoint-protection policy. Unsafe ownership, malformed retained configuration and uncertain job attempts are not guessed or discarded. Deliberately held jobs remain held and can require an operator decision when drain is required. Local fixtures do not establish native Service Management worker survival, Apple acceptance or SentinelOne acceptance.

### Upgrading from the installer

Users of the installer era have `~/Applications/Agent Queue.app` recorded as the managed location, with the services running from it. Setup refuses a new copy placed elsewhere (such as `/Applications`) while the recorded copy exists. Those users must first stop the services, move the old app to the Trash, then install and open the new app:

```sh
~/.local/bin/agent-heavy install --app-bundle "$HOME/Applications/Agent Queue.app" --unregister-services
```

Existing releases remain immutable. Setup can retire and back up legacy `~/Library/LaunchAgents/io.went.agent-*.plist` definitions during migration; it applies its own drain and ownership checks first.

## Recovery

Inspect the failed distribution **Publish Sparkle feed and homepage** job. Rerun failed distribution jobs to retry unchanged automation: a repeated run reads the live site first and deploys nothing when it is already current. A red delivery job with signing and acceptance green is classed as `delivery` and retried automatically by `scripts/distribution-recover.sh`, first immediately and then at least 15 minutes apart, up to attempt 48 or 7 days for each run. The observer requests at most five reruns per pass and reaches older builds first. It still recognises the earlier job names, so runs started before this change recover as before.

If the automation itself needs a fix, merge the repair and dispatch distribution against the original build run ID as documented in [Apple signing and notarization](apple-signing.md#resuming-a-pending-ci-submission). A matching immutable release is verified rather than overwritten.

Common failures:

- `Configure SPARKLE_ED_PRIVATE_KEY and enable GitHub Pages`: the secret is missing or Pages is not set to the GitHub Actions source.
- A 404 or "Pages not enabled" error from the deploy step: Pages is not enabled, or the `github-pages` environment forbids the `main` branch.
- `The live site answered HTTP ...` or `could not be read`: the live site could not be read. This is never treated as an empty site; rerun the job.
- `differs from its validated public release` or `conflicting release metadata`: the live `latest.json` disagrees with the public releases. Do not edit the site by hand; investigate the releases first.
- `did not serve the deployed build after N attempts`: the deployment finished but the site still serves something else. Check the **github-pages** environment deployments, then rerun.
- The site directory is missing: add `site/` with the homepage to `main`.

Rolling back means delivering a newer build; the feed never serves an older one. Never edit generated files by hand, and never execute code from a release to repair drift.

Publishing a release or updating the feed does not modify the operator's Mac. Version visibility requires the new app to be installed and running; open its gear menu and choose **About and updates**.

## Validation

The tests simulate pagination, requested release validation, checksum failures, conflicting identities, supersession by a newer release or live build, the already-current skip, 404 versus other fetch failures, the `latest.json` golden file, assembler refusal of symbolic links, files outside `site/` and oversized files, live read-back mismatches and signing with an inert fake `sign_update` that proves the key never appears in arguments. They do not call GitHub, deploy a site or use the live queue. From the source checkout:

```sh
go test -p 2 ./internal/pages ./internal/tooling -v
```

This guide has a complete standalone [HTML version](pages-delivery.html).
