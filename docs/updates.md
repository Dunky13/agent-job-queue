# Installed version and updates

## Operator flow

Open the queue's gear menu and choose **About and updates**. The window shows the running app's bundle version immediately and reads the installed controller version locally. Opening the window does not contact the network. Choose **Check for updates** to query the public project releases. Choose **Download and open installer** for an available update, then choose **Install** in the existing installer to complete the change. The installer restarts the menu app while preserving the independent queue daemon, queued and running jobs, settings and credentials.

An installation without a published build identity is explicitly marked untracked. Its version string is displayed as recorded, including legacy bundle versions. **Download published installer** offers an explicit replacement with a published build rather than claiming that unknown build histories are comparable. Known newer published installations cannot be downgraded, including by an installer opened before another update completed.

The CLI supports the same flow:

```sh
agent-heavy version
agent-heavy version --json
agent-heavy updates check
agent-heavy updates check --json
agent-heavy updates install --build 42
```

Replace 42 with the published build returned by the check. Install prepares and opens the verified installer; the installer provides final confirmation and progress. If availability changes, check again and select the current build. Checks do not require GitHub authentication, change queue state, or download executable assets. Network failures and rate limits remain errors. Preview mode never checks, downloads or opens external content.

## Version identity

The existing release manifest contains repository, semantic version, monotonically increasing CI build number, full source commit, immutable tag, installer asset name and SHA-256. Version comparison uses the build number so consecutive source builds with the same semantic version are discoverable.

Packaging embeds matching build-info.json records in both app bundles and the runtime payload before signing. Published main-branch CI receives its build number and commit from the CI environment. Local and pull-request packages remain untracked. For an intentionally tracked test package, supply BUILD_NUMBER and SOURCE_COMMIT together; this does not publish it. A checkout name or current branch alone does not establish a published build.

The installer validates source and app metadata before replacing support files. Under the existing installation lock, it rejects older tracked builds and conflicting equal-build identities across completed, app and pending-installation identities. Identical tracked builds can be reinstalled. A durable incomplete-installation marker precedes the first support-file replacement and remains after failure or interruption. It is cleared only after app replacement, resource-controller reload and the completed identity commit succeed. Reloading this separate controller applies updated prediction logic while the queue daemon keeps active jobs running. Runtime refuses to certify an incomplete installation and otherwise compares the completed marker, bundled identity and bundle version. Retry the same or a newer installer to complete an interrupted update.

## Download and installation boundary

The checker uses the fixed public project repository and selects the highest numbered published build with the expected manifest, including published prerelease builds. It uses the release list because GitHub's latest-release shortcut excludes prereleases. Pagination searches at most 1,000 releases within the operation deadline; reaching the search bound reports an incomplete search instead of claiming that a build is current. The manifest must match the release tag and full commit. Asset URLs must belong to that exact project release.

Downloads require HTTPS with redirects restricted to known GitHub release hosts. Metadata, archive size, extraction size, entry count and operation time are bounded. An absolute CLI deadline interrupts DNS, connection setup, headers and trickling bodies; a socket idle timeout alone cannot bound those operations. The updater verifies the archive SHA-256 against the manifest and GitHub's asset digest when available. It rejects traversal, symlinks, special files and case or Unicode path collisions, then checks embedded build identity, both bundle versions and both code signatures before opening the installer.

One preparation runs at a time. Private staging retains at most three opened installers. Failed or interrupted preparation is cleaned up, and installed older packages can be removed on the next preparation. The installed version is rechecked before opening. The installer independently rechecks ordering under its installation lock, covering an already-open installer used later.

The public GitHub release is the distribution trust boundary. Builds remain ad hoc signed. This feature does not add Developer ID notarization or a background installation service, disable Gatekeeper, or remove quarantine. Platform approval may still be required. Older archives without embedded build metadata remain available through their release pages; the updater refuses to present those archives as verified new-format installations.

Homebrew's existing cask and generated manifest remain compatible. Direct installation through the app updater uses the vendor installer and does not rewrite Homebrew's receipt. Homebrew-managed installations can continue using their existing cask upgrade flow. When synchronizing the dedicated tap's helper scripts, copy the standalone refresh.go, its regression tests, and the reviewed workflow as documented in the tap seed instructions. The first upgrade from Pueue requires a drained queue; migration from Python also requires disabled old remote access. Subsequent embedded Go queue upgrades preserve active daemon jobs and restart enabled remote access with the embedded gateway, retaining credentials and the trusted CA.

## Validation contract

Tests use artificial releases, isolated installer directories and inert native commands. They cover truthful offline versions, release selection, same-version build updates, unknown history, downgrade refusal, changed availability, private bounded storage, network failures, malicious archives, digest/metadata/signature failures, explicit handoff, duplicate actions and preview isolation. Native and installer checks retain ARM and Intel coverage. Public metadata can be checked read-only; tests never install into the operator's live queue.

API behavior is documented in [GitHub REST releases](https://docs.github.com/en/rest/releases/releases), including public access, prerelease flags and release assets. Bundle display reads Apple's [infoDictionary](https://developer.apple.com/documentation/foundation/bundle/infodictionary).
