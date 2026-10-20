# Verified Homebrew delivery

The tap follows the latest successfully published main build. Main CI validates the app and publishes its immutable installer before a separate **Verify Homebrew delivery** job starts. A failed app build leaves the prior Homebrew build available.

## Delivery contract

The source job verifies the published manifest and checks that the tap's trusted helpers match this source revision. It dispatches the tap's refresh workflow with the exact release tag and source commit, then waits for the run ID returned by GitHub. A missing credential, failed refresh, timeout or mismatched readback fails delivery visibly. Successful app validation and release publication remain distinct from successful Homebrew delivery.

The tap validates the requested public release, selects the highest published numbered build through a bounded paginated search, downloads the installer, verifies its checksum, regenerates the cask through its trusted local template, and checks the real Homebrew DSL. It publishes using a GitHub-signed API commit with an author-matching DCO trailer and expected-head compare-and-swap. Downloaded Ruby is never evaluated.

The source independently checks the tap's signed commit, manifest and exact rendered cask at one pinned branch head. Overlapping builds cannot downgrade the tap. A newer build counts as **superseded** delivery only after its public release and source ancestry have been verified. The source reports **current** when the exact requested manifest is present.

Refresh jobs serialize separately from PR validation. A 15-minute schedule and manual dispatch reconcile missed delivery. GitHub can delay or drop scheduled events; the release-driven job is the primary path. No-op refreshes still verify current public metadata, installer and cask, so drift cannot hide behind an unchanged build number.

When trusted templates change before a new app release, the tap retains a cask that matches its own historical public artifact, checked only as bounded data. It reports that the template change awaits a newer release. The next numbered build uses the updated trusted template. This preserves provenance across repeated tooling changes without rewriting a cask at the same build number.

## One-time GitHub App setup

Complete this before merging the source workflow change. The companion tap tooling PR must also merge first. App creation and installation require the repository owner's GitHub session.

1. Register a private GitHub App under Dunky13. Use the app repository as its homepage. Disable webhooks; callback URLs, OAuth and device flow are unnecessary. Grant only repository **Actions: Read and write**, plus GitHub's automatic metadata access. Restrict installation to the owner account.
2. Install it on **homebrew-agent-job-queue only**. The source repository does not need the App installed. Keep all optional content-write, administration and organization permissions disabled.
3. In the source repository's Actions variables, save its public Client ID as `HOMEBREW_SYNC_APP_CLIENT_ID`. Under the App's **Credentials > Key pairs**, choose **New key**. Store the downloaded PEM directly as the source repository's encrypted Actions secret `HOMEBREW_SYNC_APP_PRIVATE_KEY`. Keep private key contents out of chat and source files.

The workflow uses the SHA-pinned official `actions/create-github-app-token` action, limits its installation token to the tap repository and Actions write permission, and revokes it after the job. Public/source readback uses the source workflow's read-only token. The tap uses its own contents-write Actions token for signed cask publication. PRs receive neither App credential.

## Recovery and tooling changes

Inspect the failed source **Verify Homebrew delivery** job and its linked exact tap run. Correct the reported problem, then rerun failed source jobs. A matching immutable release is verified rather than overwritten. A repeated refresh verifies an already-current cask without creating another commit.

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

Publishing a release or updating the tap does not install the app on the operator's Mac. Version visibility requires the new app to be installed and running; open its gear menu and choose **About and updates**.

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
