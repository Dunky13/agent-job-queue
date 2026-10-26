# Installed version and updates

## Operator flow

Open the queue's gear menu and choose **About and updates**. The window shows the running app's bundle version immediately and reads the installed controller version locally. Opening the window does not contact the network. Choose **Check for updates** to query the public project releases. Choose **Download and open installer** for an available update, then choose **Install** in the existing installer to complete the change. Finish queued and running jobs before choosing Install. Managed upgrades verify the drained queue before unregistering and replacing bundle-owned services; settings, history and credentials are preserved.

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

The installer validates source and app metadata before replacing support files. Under the existing installation lock, it rejects older tracked builds and conflicting equal-build identities across completed, app and pending-installation identities. Identical tracked builds can be reinstalled. A durable incomplete-installation marker precedes the first support-file replacement and remains after failure or interruption. It is cleared only after bundle publication, native service registration and readiness, remote adoption and the completed identity commit succeed. Managed replacement requires a drained queue because unregistering a helper terminates it. Opening the app only reads installation and authorization state; it does not retry setup. Runtime refuses to certify an incomplete installation and otherwise compares the completed marker, bundled identity and bundle version. Complete a managed installation using its exact pending build before selecting another candidate. Unmarked legacy installers retain their existing version-ordering rules.

Managed publication records exact candidate and backup custody in private installation state. A changed candidate, newer target, substituted backup or retained backup without an owned receipt blocks publication or recovery and preserves the evidence. Do not delete these records to force an update. Installation also refuses executable locations with group- or world-writable ancestors before service changes; the default target is the account's Applications folder.

## Download and installation boundary

The checker uses the fixed public project repository and selects the highest numbered published build with the expected manifest, including published prerelease builds. It uses the release list because GitHub's latest-release shortcut excludes prereleases. Pagination searches at most 1,000 releases within the operation deadline; reaching the search bound reports an incomplete search instead of claiming that a build is current. The manifest must match the release tag and full commit. Asset URLs must belong to that exact project release.

Downloads require HTTPS with redirects restricted to known GitHub release hosts. Metadata, archive size, extraction size, entry count and operation time are bounded. An absolute CLI deadline interrupts DNS, connection setup, headers and trickling bodies; a socket idle timeout alone cannot bound those operations. The updater verifies the archive SHA-256 against the manifest and GitHub's asset digest when available. It rejects traversal, symlinks, special files and case or Unicode path collisions, then checks embedded build identity, both bundle versions and both code signatures before opening the installer.

One preparation runs at a time. Private staging retains at most three opened installers. Failed or interrupted preparation is cleaned up, and installed older packages can be removed on the next preparation. The installed version is rechecked before opening. The installer independently rechecks ordering under its installation lock, covering an already-open installer used later.

The public GitHub release is the distribution trust boundary. New main releases produced by the notarized pipeline use Developer ID signatures and Apple notarization, with stapled tickets on both app bundles and the DMG. Historical releases and local development packages can remain ad hoc signed. The updater checks signature integrity and release identity; those checks alone do not establish Apple notarization or Gatekeeper acceptance. It does not disable Gatekeeper, remove quarantine, or add a background installation service. The self-contained payload needs no Go toolchain, but offline first launch of its standalone Go executables has not been verified. Older archives without embedded build metadata remain available through their release pages; the updater refuses to present those archives as verified new-format installations.

Homebrew's generated manifest remains compatible. The cask for new notarized releases retains quarantine. The trusted refresh helper preserves a historical cask for an unchanged published build and applies the new template only when a newer release is available; it never executes historical released Ruby. Direct installation through the app updater uses the vendor installer and does not rewrite Homebrew's receipt. New casks deliver the explicit installer; after a cask upgrade, open it and choose Install to update the runtime. Homebrew's hook sandbox cannot manage the queue's private IPC or native services. When synchronizing the dedicated tap's helper scripts, copy the standalone refresh.go, its regression tests, and the reviewed workflow as documented in the tap seed instructions. The first upgrade from Pueue requires a drained queue; migration from Python also requires disabled old remote access. Subsequent managed upgrades also wait for a drained queue and retain remote credentials and the trusted CA. The remote controller reads the latest enabled choice under its own lock; final installation readback cannot restore an earlier enabled choice after a concurrent disable.

## Validation contract

Tests use artificial releases, isolated installer directories and inert native commands. They cover truthful offline versions, release selection, same-version build updates, unknown history, downgrade refusal, changed availability, private bounded storage, network failures, malicious archives, digest/metadata/signature failures, explicit handoff, duplicate actions and preview isolation. Native and installer checks retain ARM and Intel coverage. Public metadata can be checked read-only; tests never install into the operator's live queue.

API behavior is documented in [GitHub REST releases](https://docs.github.com/en/rest/releases/releases), including public access, prerelease flags and release assets. Bundle display reads Apple's [infoDictionary](https://developer.apple.com/documentation/foundation/bundle/infodictionary).
