# Verified Homebrew delivery

The tap follows the latest successfully published main build. Main CI validates and retains the installer. The separate distribution workflow signs, notarizes, tests and publishes that exact build before its **Verify Homebrew delivery** job starts. A failed app build or distribution attempt leaves the prior Homebrew build available.

## Delivery contract

The source job verifies the published manifest and checks that the tap's trusted helpers match this source revision. It dispatches the tap's refresh workflow with the exact release tag and source commit, then waits for the run ID returned by GitHub. A missing credential, failed refresh, timeout or mismatched readback fails delivery visibly. Successful app validation and release publication remain distinct from successful Homebrew delivery.

The tap validates the requested public release, selects the highest published numbered build through a bounded paginated search, downloads the installer, verifies its checksum, regenerates the cask through its trusted local template, and checks the real Homebrew DSL. It publishes using a GitHub-signed API commit with an author-matching DCO trailer and expected-head compare-and-swap. Downloaded Ruby is never evaluated.

The source independently checks the tap's signed commit, manifest and exact rendered cask at one pinned branch head. Overlapping builds cannot downgrade the tap. A newer build counts as **superseded** delivery only after its public release and source ancestry have been verified. The source reports **current** when the exact requested manifest is present.

Refresh jobs serialize separately from PR validation. A 15-minute schedule and manual dispatch reconcile missed delivery. GitHub can delay or drop scheduled events; the release-driven job is the primary path. No-op refreshes still verify current public metadata, installer and cask, so drift cannot hide behind an unchanged build number.

When trusted templates change before a new app release, the tap retains a cask that matches its own historical public artifact, checked only as bounded data. It reports that the template change awaits a newer release. The next numbered build uses the updated trusted template. This preserves provenance across repeated tooling changes without rewriting a cask at the same build number.

## App installation and upgrade recovery

The cask runs the vendor's noninteractive installer during `brew install` and `brew upgrade`. It uses the documented `installer script:` artifact with no sudo. Unlike flight-step hooks, Homebrew runs this artifact outside its hook sandbox. The vendor controller owns live handover or queue drain, verified repairs, app publication, Service Management registration, and readiness. No separate app-level confirmation is required.

Caskroom ancestors may be group-writable. The trusted cask script copies the complete installer into private staging, retaining quarantine, resource forks, and signatures. Before executing the copied controller it verifies the outer installer, inner app, and controller signatures against the public product publisher team `U9GXK89565`, Developer ID, notarization requirements, and Gatekeeper. It removes its private staging on completion or failure. It does not change shared directory permissions, remove quarantine, or change the Homebrew sandbox. Publisher rotation requires a reviewed change to both trusted renderers and the companion tap.

The Installer artifact runs before Homebrew moves the delivered installer and creates CLI links. Failure propagates to Homebrew. The runtime app remains independent of Homebrew's delivery artifacts so predecessor removal during upgrade cannot delete the running app or unregister its helpers. The required uninstall companion is an explicit no-op for runtime services. Homebrew uninstall removes its delivered installer and links; use the runtime's ownership-checked service-removal command for full runtime removal. Private history, configuration and credentials remain retained.

Compatible managed upgrades keep running and queued jobs across service replacement when the installed daemon, target and every outstanding caller support handover protocol 1 and queue schema 2. Original callers keep their outcomes and exit statuses, pause and parallel-limit settings survive, and no attempt is replayed. New mutations can be briefly rejected before acceptance during the swap. Legacy releases or callers need one drained upgrade; unsupported versions or schemas retain the drain path, and uncertain attempts or ownership fail closed. Removal and external app-replacement preparation still require drain.

Whole previous signed app generations remain visible under `~/Applications/Agent Queue Runtimes`, bound to exact filesystem and attempt identities. Only a later installer operation can reclaim them after verifying terminal attempts and dead original callers and owned workers. Normal app launch only reads installation and authorization state; it neither installs services nor deletes executable code.

macOS background-service approval remains an operating-system decision. While approval is pending, automatic setup reads status without re-registering denied services and continues after approval. The installer cannot bypass this approval or endpoint-protection policy. Unsafe ownership, malformed retained configuration and uncertain job attempts are not guessed or discarded. Deliberately held jobs remain held and can require an operator decision when drain is required. Local fixtures do not establish native Service Management worker survival, Apple acceptance or SentinelOne acceptance.

The first transition from a historical cask still executes that predecessor's own removal rules. A historical app artifact may be removed, but its daemon executable lives separately and active queue work must remain untouched. The new automatic installer independently applies drain and ownership checks before migration. It can retire a loaded legacy menu job whose deleted definition was removed by the historical cask only when its executable and complete arguments match the canonical old command. Existing releases and casks remain immutable; merge the companion tap tooling before the source repair and publish a new validated numbered release.

## One-time GitHub App setup

Complete this before merging the source workflow change. The companion tap tooling PR must also merge first. App creation and installation require the repository owner's GitHub session.

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

Publishing a release or updating the tap does not modify the operator's Mac. Running the updated cask's install or upgrade command performs runtime installation automatically. Version visibility requires the new app to be installed and running; open its gear menu and choose **About and updates**.

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
