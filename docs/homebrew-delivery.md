# Verified Homebrew delivery

The tap follows the latest successfully published main build. Main CI validates and retains the self-contained app. The separate distribution workflow signs, notarizes, tests and publishes that exact build before its **Verify Homebrew delivery** job starts. A failed app build or distribution attempt leaves the prior Homebrew build available.

## Delivery contract

The source job verifies the published manifest and checks that the tap's trusted helpers match this source revision. It dispatches the tap's refresh workflow with the exact release tag and source commit, then waits for the run ID returned by GitHub. A missing credential, failed refresh, timeout or mismatched readback fails delivery visibly. Successful app validation and release publication remain distinct from successful Homebrew delivery.

The tap validates the requested public release, selects the highest published numbered build through a bounded paginated search, downloads the release app ZIP, verifies its checksum, regenerates the cask through its trusted local template, and checks the real Homebrew DSL. It publishes using a GitHub-signed API commit with an author-matching DCO trailer and expected-head compare-and-swap. Downloaded Ruby is never evaluated.

The source independently checks the tap's signed commit, manifest and exact rendered cask at one pinned branch head. Overlapping builds cannot downgrade the tap. A newer build counts as **superseded** delivery only after its public release and source ancestry have been verified. The source reports **current** when the exact requested manifest is present.

Refresh jobs serialize separately from PR validation. A 15-minute schedule and manual dispatch reconcile missed delivery. GitHub can delay or drop scheduled events; the release-driven job is the primary path. No-op refreshes still verify current public metadata, app ZIP and cask, so drift cannot hide behind an unchanged build number.

When trusted templates change before a new app release, the tap retains a cask that matches its own historical public artifact, checked only as bounded data. It reports that the template change awaits a newer release. The next numbered build uses the updated trusted template. This preserves provenance across repeated tooling changes without rewriting a cask at the same build number.

A tap that still holds metadata for a release built with the earlier installer wrapper is accepted as the current state and checked against its public release and archive checksum. Its cask must match either the frozen trusted managed-app template or the bounded cask attached to that historical public release. Both are compared only as data, never executed or republished. This covers taps whose trusted template advanced after the old release was published. The first app ZIP release replaces that predecessor; arbitrary cask drift and a latest public release that still uses the installer asset name fail visibly.

## App installation and upgrade recovery

The cask is a plain app cask. Its only artifacts are `app "Agent Queue.app"`, which Homebrew installs into `/Applications`, and `uninstall quit: "io.went.agent-queue"`. Nothing runs at `brew install` or `brew upgrade`: there is no installer script, flight step, binary link or setup command. The cask keeps quarantine so macOS assesses the notarized app.

The first launch completes setup. Opening the app from Applications registers its bundle-owned services with Service Management and writes the `agent-heavy` and `agent-remote` wrappers to `~/.local/bin`. A later `brew upgrade` replaces the app, and the next launch completes the upgrade. Normal app launch only reads installation and authorization state until setup runs; it never deletes executable code.

Homebrew uninstall asks a running app to quit, then removes the app. Services, jobs, history and credentials are removed only by the app's ownership-checked service-removal command, which the cask caveats name and which must run before deleting the app:

```sh
~/.local/bin/agent-heavy install --app-bundle "/Applications/Agent Queue.app" --unregister-services
```

No destructive `zap` action is supplied. Private history, configuration and credentials remain retained.

Compatible managed upgrades keep running and queued jobs across service replacement when the installed daemon, target and every outstanding caller support handover protocol 1 and queue schema 2. Original callers keep their outcomes and exit statuses, pause and parallel-limit settings survive, and no attempt is replayed. New mutations can be briefly rejected before acceptance during the swap. Legacy releases or callers need one drained upgrade; unsupported versions or schemas retain the drain path, and uncertain attempts or ownership fail closed. Removal and external app-replacement preparation still require drain.

Whole previous signed app generations remain visible under `~/Applications/Agent Queue Runtimes`, bound to exact filesystem and attempt identities. Only a later setup operation can reclaim them after verifying terminal attempts and dead original callers and owned workers.

macOS background-service approval remains an operating-system decision. While approval is pending, automatic setup reads status without re-registering denied services and continues after approval. Setup cannot bypass this approval or endpoint-protection policy. Unsafe ownership, malformed retained configuration and uncertain job attempts are not guessed or discarded. Deliberately held jobs remain held and can require an operator decision when drain is required. Local fixtures do not establish native Service Management worker survival, Apple acceptance or SentinelOne acceptance.

### Upgrading from the installer cask

Users of the installer-era cask have `~/Applications/Agent Queue.app` recorded as the managed location, with the services running from it, and the old cask left that app in place. Setup refuses a new copy placed elsewhere (such as `/Applications` from the new cask) while the recorded copy exists. Those users must first stop the services, move the old app to the Trash, then upgrade and open the new app:

```sh
~/.local/bin/agent-heavy install --app-bundle "$HOME/Applications/Agent Queue.app" --unregister-services
```

Existing releases and casks remain immutable, and the first transition from a historical cask still runs that predecessor's own removal rules. Setup can retire and back up legacy `~/Library/LaunchAgents/io.went.agent-*.plist` definitions during migration; it applies its own drain and ownership checks first. Merge the companion tap tooling before the source change and publish a new validated numbered release.

## Syncing tap tooling before merging this change

The app change replaces the tap's trusted tooling and cask template. The tap must receive it first, through a companion PR:

1. From `packaging/homebrew/`, copy `refresh.go`, `refresh_test.go`, `test-cask.rb` and `.github/workflows/refresh.yml` into the tap checkout, together with the root `LICENSE`. Keep them byte-identical to this source revision; do not copy `SEED.txt`.
2. In the tap checkout, run `go test -p 2 refresh.go refresh_test.go` and `go vet refresh.go` (Go 1.26 or later).
3. Open a PR against the tap's `tap` branch with signed, DCO-signed-off commits, and merge it.
4. Only then merge the source change. The delivery job compares the tap's helpers byte for byte (`internal/delivery/sync.go`), so a missing or stale sync fails delivery visibly.

## One-time GitHub App setup

Complete this before merging the source workflow change. The companion tap tooling PR (see above) must also merge first. App creation and installation require the repository owner's GitHub session.

1. Register a private GitHub App under Dunky13. Use the app repository as its homepage. Disable webhooks; callback URLs, OAuth and device flow are unnecessary. Grant only repository **Actions: Read and write**, plus GitHub's automatic metadata access. Restrict installation to the owner account.
2. Install it on **homebrew-agent-job-queue only**. The source repository does not need the App installed. Keep all optional content-write, administration and organization permissions disabled.
3. In the source repository's Actions variables, save its public Client ID as `HOMEBREW_SYNC_APP_CLIENT_ID`. Under the App's **Credentials > Key pairs**, choose **New key**. Store the downloaded PEM directly as the source repository's encrypted Actions secret `HOMEBREW_SYNC_APP_PRIVATE_KEY`. Keep private key contents out of chat and source files.

The workflow uses the SHA-pinned official `actions/create-github-app-token` action, limits its installation token to the tap repository and Actions write permission, and revokes it after the job. Public/source readback uses the source workflow's read-only token. The tap uses its own contents-write Actions token for signed cask publication. PRs receive neither App credential.

## Recovery and tooling changes

Inspect the failed distribution **Verify Homebrew delivery** job and its linked exact tap run. Rerun failed distribution jobs to retry unchanged automation. If the automation itself needs a fix, merge the repair and dispatch distribution against the original build run ID as documented in [Apple signing and notarization](apple-signing.md#resuming-a-pending-ci-submission). A matching immutable release is verified rather than overwritten. A repeated refresh verifies an already-current cask without creating another commit.

An owner can reconcile the latest published build without supplying release inputs:

```sh
gh workflow run refresh.yml --repo Dunky13/homebrew-agent-job-queue --ref tap
```

To inspect the workflow runs:

```sh
gh run list --repo Dunky13/homebrew-agent-job-queue --workflow refresh.yml --limit 5
```

Changes to the trusted tap files need a reviewed companion tap PR. Copy the standalone `refresh.go`, `refresh_test.go`, `test-cask.rb`, and `.github/workflows/refresh.yml` from `packaging/homebrew/`, together with the root `LICENSE`. The tap helper uses only the Go standard library and never downloads application source or dependencies to execute. Merge the tap PR before the corresponding source change. Mandatory byte comparison makes future helper drift a delivery error. Never edit generated `Casks/` files manually or execute arbitrary code from a release to repair drift.

After verified delivery, Homebrew users can upgrade normally:

```sh
brew update
brew upgrade --cask dunky13/agent-job-queue/agent-job-queue
```

Publishing a release or updating the tap does not modify the operator's Mac. Running the updated cask's install or upgrade command places the app only; open it to use the menu bar while services prepare automatically in the background. Version visibility requires the new app to be installed and running; open its gear menu and choose **About and updates**.

## Validation

Source-side tests simulate exact-run dispatch, failures, deadline expiry, token routing, signed readback, canonical helper/cask comparison, conflicting identities and verified supersession. Tap tests simulate pagination, requested release validation, checksum failures, stale heads, idempotence, ancestry and signed publication. They do not call GitHub, install an app or use the live queue.

From the app source checkout:

```sh
go test -p 2 ./internal/delivery ./internal/tooling ./packaging/homebrew -v
```

From the standalone tap checkout, using Go 1.26 or later:

```sh
go test -p 2 refresh.go refresh_test.go
go vet refresh.go
```

This guide has a complete standalone [HTML version](homebrew-delivery.html). API and setup references: [workflow dispatch](https://docs.github.com/en/rest/actions/workflows#create-a-workflow-dispatch-event), [App registration](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app), [App keys](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps), and [schedule behavior](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#schedule).
