# Drag-to-Applications installer handoff

The DMG contains Agent Queue.app and an Applications shortcut. Drag the app to Applications and open the copied app. First launch installs the private queue backend and background services before opening the menu. Setup failures stay visible and support Retry.

## Runtime and compatibility

The app embeds complete source support and universal Go helpers under Contents/Resources/payload. The new agent-heavy install --app-bundle command installs from that bundle without copying, replacing, chmodding, quitting, or reopening the app. Existing queue migrations, process ownership checks, downgrade fences, and private-state permissions remain enforced. Active Go daemon jobs survive setup. Legacy Python migrations still require a drained queue and disabled old phone access.

Readiness accepts supported preserved launch-service settings and checks every private parent path without following symlinks. Invalid fixture and snapshot arguments fail before inspection or installation. Version and About checks follow the registered app location. Launching outside an accepted Applications directory shows move guidance before changing the backend.

The standard legacy Python cleanup callback matcher now accepts bounded system, virtualenv, and Homebrew Python spellings with safe quoting. Script, arguments, and template remain exact. Custom callbacks fail before a transaction is published.

The ZIP retains the custom installer layout used by the updater and Homebrew. New DMGs use the drag layout; retained legacy artifacts preserve their original installer layout. Shared shell classification rejects declared missing, linked, dangling, and partial embedded payloads. Signing seals embedded helpers before the enclosing app and retains the payload, installer, and DMG notarization sequence.

The non-executing queue-tool verify-app --app PATH command requires complete embedded source, exact metadata identity, universal executables, licenses, strict signatures, and absence of private state or recursive apps. Installer verification permits legacy fallback only when the embedded payload is entirely absent and not declared. Disk-image verification validates the exact Applications shortcut and rejects mixed layouts.

Native transport now uses bounded poll readiness waits after EAGAIN. Fixed sleeps delayed draining oversized output on both the committed baseline and changed code. Output caps, cancellation, deadlines, child ownership, cleanup, and the existing one-second assertion remain intact.

## Completed local validation

The full Go race suite passed across cmd, internal, packaging/homebrew, and remote/static. Go vet, source portability, formatting, shell lint, and whitespace checks passed.

Final shared queue job 950 passed 333 native checks, six graphical setup lifecycle checks, the universal installer build, packaged classic and in-place app installation, the packaged HTTPS gateway test, and mounted drag-image verification. Packaged installation used real private daemons and scheduler jobs with simulated launch-service operations. The GUI fixture used disposable home and app directories, an injected executor, and simulated queue data. Screenshots were inspected. Source manifests matched before and after the final native, GUI, build, and artifact batch.

Ten serial transport runs passed 440 assertions. All 30 oversized-output checks completed in 248 to 337 milliseconds. Focused regression failures were reproduced before correcting callback recognition, bootstrap readiness and parent checks, diagnostic validation, and transport latency. Separate source reviews found no remaining actionable findings in the packaging, bootstrap/backend, or transport scopes.

## Reproduction and remaining boundaries

Run the documented finite validation batch through the shared heavy-job queue:

```sh
agent-heavy run --label "Agent Queue validation" -- bash scripts/validate.sh
```

For graphical setup verification with disposable fixtures:

```sh
agent-heavy run --label "Bootstrap UI verification" -- bash native/tests/run-bootstrap-ui-tests.sh build/bootstrap-ui-verification
```

Local artifacts used ad hoc signatures and untracked development build metadata. Universal slices were inspected, but Intel runtime, Developer ID signing, Apple notarization, and Gatekeeper acceptance were not established locally. Hosted checks and merge state belong to the linked PR. Release publication and production runtime require their own evidence. No live installed queue, app, or configuration was migrated during this work.

Packaging is reversible in source. A completed legacy migration retires the old Python runtime, so reverting the app alone is not a supported migration rollback. Preserve the independent daemon and retained state when diagnosing setup. Do not bypass the migration or ownership gates.
