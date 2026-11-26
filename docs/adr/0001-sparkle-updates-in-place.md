---
status: accepted
---

# Sparkle installs updates in place

The app used to deliver a verified copy of each update to Downloads and leave the Finder replacement to the user. That way, it never wrote code into Applications and needed no installer helper. We now use Sparkle 2 to download, verify and install updates in place and relaunch the app. Setup on relaunch still completes the upgrade, and the custom Go and Swift download updater and `agent-heavy updates` are removed.

The deciding evidence is a SentinelOne test on the employer-managed Mac where Agent Queue was quarantined before (see `docs/handoffs/sentinelone-detection-investigation.md`). Kanarie, an unrelated Sparkle-based app, installed 0.1.1 and then updated itself to 0.1.2 through Sparkle on that Mac without a detection. That test qualifies Sparkle's installer mechanism under that policy. It does not qualify Agent Queue's own binaries. The open acceptance gate is a Sparkle update of Agent Queue from build N to N+1 on that Mac.

## Consequences

- Trust moves from the custom checks to these layers:
  - Sparkle's EdDSA signature on the archive and on the feed (`SURequireSignedFeed`), verified before extraction (`SUVerifyUpdateBeforeExtraction`).
  - Sparkle's check that the update is code-signed by the same Developer ID team.
  - Apple notarization.
  - Setup's build-identity, downgrade and incomplete-installation checks on relaunch.

  The previous checks were the GitHub manifest, the digest, the archive shape and quarantine on the copy.
- The app checks for updates automatically after Sparkle's standard opt-in prompt. Before an update installs, the app opens a time-bounded drain so running callers survive the restart, and setup releases it after the relaunched queue is up.
- Untracked (local and pull-request) builds, test-hook builds, preview mode and an incomplete installation never check or install updates.
- Installations older than the first Sparkle build need one last manual download from the release page.
- The EdDSA private key is a CI secret and the public key is in `Info.plist`. Losing the private key strands every installation on its current build until a manual download.

## Considered options

- Keep the download-only updater. It is safer under endpoint protection, but every update stays a manual drag.
- Add a custom install step to the existing updater. That rebuilds part of Sparkle with less review.
