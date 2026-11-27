# Installed version and updates

Agent Queue updates itself with [Sparkle](https://sparkle-project.org). Sparkle downloads a new build, verifies it, installs it in place and relaunches the app. See [ADR 0001](adr/0001-sparkle-updates-in-place.md) for the decision and [the spec](specs/sparkle-updates.md) for the design.

## Operator flow

Open the queue's gear menu and choose **About and updates**. The window shows the running app's bundle version immediately and reads the installed controller version locally. Opening the window does not contact the network.

The first time Sparkle is due to check, it asks once whether Agent Queue may check for updates automatically. After you agree it checks on its own schedule. Sparkle opens its own window for an update found by a scheduled check only when Agent Queue was just launched or the Mac was idle, so the window cannot open unseen behind other apps. Instead **Install Update…** appears in the queue panel and the menu-bar menu, and a notification announces the version; macOS asks once for permission to show it. Clicking either opens Sparkle's install dialog. **Check for Updates…** in the window asks Sparkle to check now and shows Sparkle's own dialogs for the release notes, the download and the install. The button is disabled while Sparkle is busy or may not act.

When you choose to relaunch for an update, the app first closes queue intake with the bundled `agent-heavy drain --for 4h`. New `agent-heavy run` calls then wait instead of failing, and callers of running or queued jobs survive the restart. An update that Sparkle installs when you quit the app does not drain and never restarts the app mid-session: the app declines Sparkle's immediate-install offer, so the update installs at the next real quit. If the drain cannot be set, the app logs the failure and installs anyway, because the drain only protects callers.

The app records the drain it started in a private marker file, `~/.local/state/agent-heavy/update-drain.json` (mode 0600 in a 0700 directory, created without following links and read back only if it is a regular file owned by you). The marker holds the drain's exact expiry and whether the queue was already paused. After the relaunch, once setup reports the queue up, the app reads the marker strictly (unknown or duplicate keys, other owners and loose permissions all make it void), consumes it, and runs `agent-heavy undrain` only if the live drain still has the recorded expiry. A drain you set, extended or replaced yourself has a different expiry and is never released, and a marker whose drain has lapsed is removed without running anything. If the queue was paused before the update, or its status could not be read as running, the app runs `agent-heavy undrain --keep-paused`, which ends only the drain and never resumes your pause. Otherwise `undrain` also resumes the queue, which ends the pause a restarted queue starts with.

The marker is consumed when the relaunched queue is up, whether or not the release succeeds. It is left in place on purpose when the queue never comes up within a minute of launch and when the app runs without helpers (fixture, setup-disabled and unmanaged launches): a later launch releases it if the drain is still live, and removes it otherwise. See [upgrade recovery](upgrade-recovery.md) for the drain itself.

The CLI reports only the installed version:

```sh
agent-heavy version
agent-heavy version --json
```

There is no `agent-heavy updates` command. A test build launched with `--fixture` is a test-hook build, so it never checks or installs.

### When updates are unavailable

The updater exists only in a published build. These builds never check or install:

- Untracked builds (local and pull-request builds, or a build whose release metadata cannot be verified).
- Test-hook builds, including any launched with `--fixture`.

The window says which applies and links to the releases page. A tracked build also refuses checks and installs while the incomplete-installation marker (`install-in-progress.json`) exists. Open Agent Queue from Applications to finish setup first.

Installations older than the first Sparkle build cannot update in place. Their own download announces the new build but then fails with "Update archive contains unsafe paths or entries": it accepts no symbolic links, and the signed `Sparkle.framework` needs them. Install the DMG from the homepage or the release page once instead: quit Agent Queue, drag the app from the DMG into Applications, choose Replace and open it. Sparkle updates it from then on.

## Version identity

Each release ZIP, `Agent-Queue-<version>-universal.zip`, contains only `Agent Queue.app` at its top level. The release manifest `agent-job-queue.json` contains repository, semantic version, monotonically increasing CI build number, full source commit, immutable tag, asset name and SHA-256. The Sparkle feed `appcast.xml` carries the build number as `sparkle:version` and the semantic version as `sparkle:shortVersionString`, so consecutive source builds with the same semantic version are comparable.

Packaging embeds matching build-info.json records in the app's `Contents/Resources` and in its runtime payload (`Contents/Resources/payload/source`) before signing. Published main-branch CI receives its build number and commit from the CI environment. Local and pull-request packages remain untracked. For an intentionally tracked test package, supply BUILD_NUMBER and SOURCE_COMMIT together; this does not publish it, and it requires `packaging/sparkle/public-ed-key.txt`. A checkout name or current branch alone does not establish a published build.

Setup validates source and app metadata before replacing support files. Under the existing installation lock, it rejects older tracked builds and conflicting equal-build identities across completed, app and pending-installation identities. Identical tracked builds can be reinstalled. A durable incomplete-installation marker precedes the first support-file replacement and remains after failure or interruption. It is cleared only after service registration is verified (for the app, only its login item remains registered and older queue, resource and phone-access registrations are retired), remote adoption and the completed identity commit succeed. While the marker is present, version checks fail and Sparkle refuses checks and installs; open Agent Queue from Applications to finish setup. Setup never starts a live handover: before replacing a running daemon it waits for that daemon's jobs. Uncertain attempts, process ownership or changed custody fail closed. Runtime refuses to certify an incomplete installation and otherwise compares the completed marker, bundled identity and bundle version.

Managed publication records exact candidate and backup custody in private installation state. A changed candidate, newer target, substituted backup or retained backup without an owned receipt blocks publication or recovery and preserves the evidence. Do not delete these records to force an update. Setup also refuses executable locations with group- or world-writable ancestors before service changes; the default target is the account's Applications folder.

Setup never starts a live handover. Finishing one an older release left behind preserves queued work, pause state, the parallel limit and each original caller's outcome and exit status without replaying attempts. During the swap, new mutations can be briefly rejected before acceptance. Complete previous signed bundles needed by outstanding callers or workers remain visible under `~/Applications/Agent Queue Runtimes`, bound to exact filesystem and attempt custody. Only a later setup operation reclaims a generation after verifying terminal attempts and dead original callers and owned workers; the runtime never deletes executable code.

## Trust

An update is trusted through these layers:

1. Sparkle's EdDSA signature on the archive, verified before extraction (`SUVerifyUpdateBeforeExtraction`).
2. Sparkle's EdDSA signature on the feed itself (`SURequireSignedFeed`).
3. Sparkle's check that the new app is code-signed by the same Developer ID team.
4. Apple notarization, with a stapled ticket on the app and the DMG.
5. Setup's build-identity, downgrade and incomplete-installation checks when the relaunched app runs setup. Setup rejects an older tracked build.

The app carries these `Info.plist` keys:

| Key | Value |
| --- | --- |
| `SUFeedURL` | `https://dunky13.github.io/agent-job-queue/appcast.xml` |
| `SUPublicEDKey` | The public key in `packaging/sparkle/public-ed-key.txt`. Required for a tracked build, optional for an untracked one. |
| `SURequireSignedFeed` | true |
| `SUVerifyUpdateBeforeExtraction` | true |
| `SUEnableAutomaticChecks` | Unset, so Sparkle asks for consent once. |

The EdDSA private key is a CI secret. Losing it strands every installation on its current build until a manual download.

The app is not sandboxed, so the bundled `Sparkle.framework` omits its XPC services, headers and modules. It contains `Autoupdate` and `Updater.app`, signed inside out with the hardened runtime, and the app is notarized with them. `queue-tool verify-app` allows exactly that one nested app and framework layout, requires universal Mach-O files signed by the app's team, and rejects any other nested app or framework and any XPC service. Sparkle's license is shipped as `source/licenses/Sparkle-LICENSE`.

This work qualifies Sparkle's installer mechanism on an employer-managed Mac that previously quarantined Agent Queue, not Agent Queue's own binaries. The open acceptance gate is a Sparkle update of Agent Queue from build N to N+1 on that Mac. See [the investigation](handoffs/sentinelone-detection-investigation.md).

Agent Queue is no longer distributed through Homebrew. An app installed earlier through the Homebrew tap updates itself through Sparkle like any other installation. Never run `brew uninstall` on it, which deletes the app; `brew untap dunky13/agent-job-queue` is optional. See [Pages delivery](pages-delivery.md).

## Validation contract

Native tests cover the tracked, untracked and test-hook decision for creating the updater, the refusal while the incomplete-installation marker exists, the drain running before the install block on the relaunch path, the install-on-quit offer being declined without a drain, the failed-drain continuation, and the rule that `undrain` runs only for a marker whose recorded expiry still matches the live drain (operator drains, extended drains and prior pauses included). Go tests cover the installed-version command and `queue-tool verify-app` for the Sparkle layout, signatures, architectures, `Info.plist` keys and licenses. Signing tests pin the inside-out order, hardened runtime and the absence of `--deep`. Real Developer ID signing, notarization and an actual update from one build to the next cannot run in these tests; tests never write to the operator's Applications or live queue. Operating-system approval and endpoint-protection policy remain independent requirements.

Sparkle behavior is documented in [Sparkle's documentation](https://sparkle-project.org/documentation/). Bundle display reads Apple's [infoDictionary](https://developer.apple.com/documentation/foundation/bundle/infodictionary).
