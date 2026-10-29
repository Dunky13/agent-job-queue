# Installed version and updates

## Operator flow

Open the queue's gear menu and choose **About and updates**. The window shows the running app's bundle version immediately and reads the installed controller version locally. Opening the window does not contact the network. Choose **Check for updates** to query the public project releases. Choose **Download update** for an available update.

The updater downloads and verifies the new `Agent Queue.app`, copies it to a new folder named `~/Downloads/Agent Queue <version> (build <N>)`, and reveals it in Finder. It never installs the app itself. To finish:

1. Quit Agent Queue.
2. Drag the new `Agent Queue.app` into Applications and choose **Replace**.
3. Open it from Applications. Setup runs automatically, detects the newer build and re-registers the app's services. Compatible managed upgrades hand over queued and running jobs while replacing bundle-owned services; legacy transitions wait for the queue to drain. Settings, history and credentials are preserved.

An existing folder in Downloads is never overwritten or deleted. If the folder name is taken, the updater uses a numbered suffix such as `Agent Queue 1.2.0 (build 42) 2`.

An installation without a published build identity is explicitly marked untracked. Its version string is displayed as recorded, including legacy bundle versions. **Download published build** offers an explicit replacement with a published build rather than claiming that unknown build histories are comparable. Known newer published installations cannot be downgraded: the updater refuses to download an older or equal tracked build, and opening an older copy later is rejected by setup.

The CLI supports the same flow:

```sh
agent-heavy version
agent-heavy version --json
agent-heavy updates check
agent-heavy updates check --json
agent-heavy updates download --build 42
agent-heavy updates download --build 42 --json
```

Replace 42 with the published build returned by the check. The JSON result is:

```json
{"downloaded": true, "build": 42, "app": "/Users/me/Downloads/Agent Queue 1.2.0 (build 42)/Agent Queue.app", "message": "Quit Agent Queue, then drag the new copy into Applications and choose Replace. Opening it finishes the update automatically."}
```

If availability changes, check again and select the current build. If Finder cannot reveal the copy, the download still succeeds and the message names its path. Checks do not require GitHub authentication, change queue state, or download executable assets. Network failures and rate limits remain errors. Preview mode never checks, downloads or reveals external content.

## Version identity

Each release ZIP, `Agent-Queue-<version>-universal.zip`, contains only `Agent Queue.app` at its top level. The release manifest `agent-job-queue.json` contains repository, semantic version, monotonically increasing CI build number, full source commit, immutable tag, asset name and SHA-256. Version comparison uses the build number so consecutive source builds with the same semantic version are discoverable.

Packaging embeds matching build-info.json records in the app's `Contents/Resources` and in its runtime payload (`Contents/Resources/payload/source`) before signing. Published main-branch CI receives its build number and commit from the CI environment. Local and pull-request packages remain untracked. For an intentionally tracked test package, supply BUILD_NUMBER and SOURCE_COMMIT together; this does not publish it. A checkout name or current branch alone does not establish a published build.

Setup validates source and app metadata before replacing support files. Under the existing installation lock, it rejects older tracked builds and conflicting equal-build identities across completed, app and pending-installation identities. Identical tracked builds can be reinstalled. A durable incomplete-installation marker precedes the first support-file replacement and remains after failure or interruption. It is cleared only after native service registration and readiness, remote adoption and the completed identity commit succeed. While the marker is present, version checks and update downloads fail; open Agent Queue from Applications to finish setup. Managed replacement can use authenticated live handover when the installed daemon, target and every outstanding caller support protocol 1 with queue schema 2. Legacy releases or callers need one drained upgrade first. Unsupported capabilities or schemas retain the drain path; uncertain attempts, process ownership or changed custody fail closed. Runtime refuses to certify an incomplete installation and otherwise compares the completed marker, bundled identity and bundle version.

Managed publication records exact candidate and backup custody in private installation state. A changed candidate, newer target, substituted backup or retained backup without an owned receipt blocks publication or recovery and preserves the evidence. Do not delete these records to force an update. Setup also refuses executable locations with group- or world-writable ancestors before service changes; the default target is the account's Applications folder.

Live handover preserves queued work, pause state, the parallel limit and each original caller's outcome and exit status without replaying attempts. During the swap, new mutations can be briefly rejected before acceptance. Complete previous signed bundles needed by outstanding callers or workers remain visible under `~/Applications/Agent Queue Runtimes`, bound to exact filesystem and attempt custody. Only a later setup operation reclaims a generation after verifying terminal attempts and dead original callers and owned workers; the runtime never deletes executable code.

## Download boundary

The checker uses the fixed public project repository and selects the highest numbered published build with the expected manifest, including published prerelease builds. It uses the release list because GitHub's latest-release shortcut excludes prereleases. Pagination searches at most 1,000 releases within the operation deadline; reaching the search bound reports an incomplete search instead of claiming that a build is current. The manifest must match the release tag and full commit. Asset URLs must belong to that exact project release.

Downloads require HTTPS with redirects restricted to known GitHub release hosts. Metadata, archive size, extraction size, entry count and operation time are bounded. An absolute CLI deadline interrupts DNS, connection setup, headers and trickling bodies; a socket idle timeout alone cannot bound those operations. The updater verifies the archive SHA-256 against the manifest and GitHub's asset digest when available. It rejects traversal, symlinks, special files, case or Unicode path collisions and any top-level entry other than `Agent Queue.app` (and `__MACOSX`). It extracts into private staging, then checks both embedded build identity records, the bundle identifier `io.went.agent-queue`, the bundle version and build, and the code signature against the published release. Only then does it copy the app with `ditto` into a newly created Downloads folder, verify the copy's identity and signature again, and reveal it. A failed copy removes only the folder the updater just created.

One download runs at a time. Private staging is removed after every attempt, and leftovers from an interrupted attempt or from the earlier installer flow are cleaned up on the next one. The installed version is rechecked before the copy is made.

The public GitHub release is the distribution trust boundary. New main releases produced by the notarized pipeline use Developer ID signatures and Apple notarization, with a stapled ticket on the app and the DMG. Historical releases and local development packages can remain ad hoc signed. The updater checks signature integrity and release identity; those checks alone do not establish Apple notarization or Gatekeeper acceptance. It does not disable Gatekeeper, remove quarantine, or add a background installation service. Copying and replacing the app in Applications is left to the user, so the updater never writes code into Applications. Older archives without embedded build metadata or in the earlier installer layout remain available through their release pages; the updater refuses to present them as verified new-format downloads. Builds that predate this flow cannot update in place and need one manual download from the release page.

Homebrew's generated manifest remains compatible. Direct updates through the app do not rewrite Homebrew's receipt; Homebrew-managed installations can keep using their cask upgrade flow.

## Validation contract

Tests use artificial releases, an injected Downloads folder and inert native commands. They cover truthful offline versions, release selection, same-version build updates, unknown history, downgrade refusal, changed availability, private staging cleanup, network failures, malicious archives, digest, metadata and signature failures, the never-overwrite rule for Downloads, explicit download, duplicate actions and preview isolation. Native checks retain ARM and Intel coverage. Operating-system approval and endpoint-protection policy remain independent requirements. Public metadata can be checked read-only; tests never write to the operator's Downloads, Applications or live queue.

API behavior is documented in [GitHub REST releases](https://docs.github.com/en/rest/releases/releases), including public access, prerelease flags and release assets. Bundle display reads Apple's [infoDictionary](https://developer.apple.com/documentation/foundation/bundle/infodictionary).
