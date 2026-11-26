# Sparkle updates in the app (package A)

## Result

Agent Queue now updates itself with Sparkle 2.10.1 inside the app, per [the spec](../specs/sparkle-updates.md) and [ADR 0001](../adr/0001-sparkle-updates-in-place.md). The Go `updates check|install` command and the Swift download code and tests are deleted. `agent-heavy version [--json]` remains.

- Build: `packaging/sparkle/fetch.sh` supplies the framework to `native/build.sh`, `native/build-library.sh`, the native tests and `scripts/build-app.sh`.
- Bundle: the framework is trimmed (no XPC services, headers or modules, including their top-level links), carries the feed URL, signed-feed and verify-before-extraction keys, and ships `licenses/Sparkle-LICENSE`.
- Public key: `packaging/sparkle/public-ed-key.txt` is validated (one base64 line, 32 bytes). A build with `BUILD_NUMBER` fails without it; an untracked build omits `SUPublicEDKey`.
- Signing: `Autoupdate`, `Updater.app` and the framework are signed inside out with the hardened runtime and no `--deep`. The resume and checkpoint flow inherits this through `macos_sign_app_helpers`.
- Verification: `queue-tool verify-app` allows exactly `Updater.app` as a nested app, rejects XPC services and any other nested app or framework, and requires universal Mach-O code strictly signed by the app's team.
- Runtime: `SPUStandardUpdaterController` exists only for tracked, non-test-hook, non-preview builds. The delegate refuses checks and installs while `install-in-progress.json` exists. On the user-initiated relaunch it runs `agent-heavy drain --for 4h` and records a private marker (`~/.local/state/agent-heavy/update-drain.json`) with the exact `drainUntil` and whether the queue was already paused; after the relaunch, once the queue is reachable, it runs `agent-heavy undrain` (or `undrain --keep-paused` when the queue was paused before) only if the live drain still matches the marker.

## Decisions and deviations

- Owner decision replacing the spec's install-on-quit drain: `willInstallUpdateOnQuit` returns false and never calls the block, so Sparkle installs on a real quit. Only the relaunch path drains.
- An operator-set, extended or replaced drain is never released, because the marker pins the exact `drainUntil`. A new `agent-heavy undrain --keep-paused` ends only the drain, for a queue that was paused before the update. The marker is consumed when the relaunched queue is up and left deliberately when the queue never comes up or no helpers start; it lapses with the drain.
- `verify-app` also rejects loose code anywhere in the app: libraries, bundles, plugins and any Mach-O file outside the three app executables and Sparkle's own three.
- Deleting the dangling top-level links to the trimmed directories goes beyond the spec's literal list.
- Sparkle is required in managed apps by the signing script, so signing and verify fixtures were updated.
- Sparkle's license is both a paragraph in `THIRD-PARTY-NOTICES.md` and a file in the license output.
- Setup's payload check and bundle digest accept exactly Sparkle's five framework version links; `backend.SparkleLinks()` is the one list, shared with `verify-app`. Before this, setup refused the packaged app.
- Following Kanarie, scheduled updates use Sparkle's gentle reminders: unless Sparkle reports immediate focus (recent launch or an idle Mac), **Install Update…** appears in the panel and menu-bar menu and a notification announces the version (owner choice, 2026-11-26). This adds a one-time notification permission prompt.

## Not verified here

- Real Developer ID signing and notarization.
- A real update from build N to N+1 through a published appcast.
- The SentinelOne acceptance gate on the employer-managed Mac.
- The update notification and the Install Update entries with a real scheduled check; tests cover the announcer and the store, not Sparkle's scheduler or the notification permission prompt.

## Remaining work (package B and owner steps)

1. The owner generates the EdDSA key pair, commits `packaging/sparkle/public-ed-key.txt` and stores the private key as a CI secret. Losing the private key strands installations.
2. Package B: appcast generation and signing in release CI, the in-repository tap, and publishing `appcast.xml` at the feed URL.
3. The first Sparkle build needs one manual download from existing users.

## Validation

```sh
scripts/test-native.sh
scripts/test-go-runtime.sh core-1
scripts/test-go-tooling.sh 1
scripts/test-go-tooling.sh 2
scripts/lint.sh
```
