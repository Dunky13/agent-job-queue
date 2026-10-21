# Apple notarization handoff

Observed on 2026-10-21. This is the evidence record for the Developer ID and
notarization change; the [signing guide](../apple-signing.md) contains the full
research, source links, configuration and operator commands.

## Implemented behavior

Local development packages remain ad hoc signed. A main-only protected signing
job consumes validated universal binaries, imports temporary credentials, and
notarizes the payload before signing the enclosing installer. Both app bundles
and the final signed DMG receive validated stapled tickets before final hashes
and release metadata are generated. Publication also waits for fresh ARM and
Intel artifact acceptance jobs. There is no unsigned publication fallback.

New casks keep quarantine and directly run the signed Go controller. The native
installer uses the same Go entry. The companion tap update must merge before
the source change. Historical paired cask/manifest records are preserved until
a newer source release is published; generated `Casks/` files are unchanged.

## Credential evidence

The existing Apple setup from the maintainer's signing thread was verified
live. This Mac has a valid Developer ID Application identity for team
`U9GXK89565`, and its existing notarization Keychain profile authenticates.
Encrypted local backups supplied the protected source repository environment;
no credential values were printed or committed.

The `apple-signing` GitHub environment has exactly branch `main` as its allowed
deployment policy, three Apple secrets and four public identity variables.
There are no repository-scoped Apple secret copies. A real CI-style smoke
imported the identity into a disposable Keychain, signed the universal Go
controller with Hardened Runtime and timestamp, verified both architecture
slices, authenticated with Apple using the API key, and removed credentials.
The host default Keychain and search list were identical before and afterward.

## Local validation

- A universal installer build completed. Both signature integrity and mounted
  DMG architecture, executable-mode and metadata checks passed.
- The actual Developer ID-signed, hardened native app launched with public
  fixture data and rendered Learned Workloads. It exited normally.
- The hardened packaged Go queue and HTTPS gateway smoke tests passed against
  isolated temporary state, including real daemon work and preserved exit codes.
- Tooling, delivery and tap race tests, shell/workflow lint, native regressions,
  packaged installation/upgrade and mounted-DMG checks passed together.
- Wrong or missing configuration, unsafe credential paths, rejected or malformed
  notarizations, failed signatures/staples, output checksums, secret cleanup and
  cancellation have automated regression coverage.

Actual notarized Homebrew execution is an opt-in acceptance test. The default
ad hoc fixture is rejected before staging or executing content; data-only
quarantine tests do not claim notarized runtime proof. Fresh hosted ARM/Intel
jobs run the real acceptance test only after Apple accepts the final artifacts.
Clean-Mac offline first launch remains unverified; standalone binaries cannot
carry stapled tickets.

## Issues found during validation

The real signing test caught a missing `=` prefix on a literal codesign
requirement before any upload. The command and stub regression now enforce
macOS's literal-versus-file requirement syntax.

Race testing exposed a nominal five-second cleanup loop that could exceed its
budget under scheduling delay. A deterministic delayed-poll fixture reproduced
the original failure; elapsed-time cleanup passes with the original test
deadline unchanged.

The real Homebrew fixture showed that a quarantined unsigned shell-script entry
can be killed by macOS. New casks and the native installer directly enter the
signed Go controller. Review also required ownership of Homebrew's separate
installer process groups before private fixture tools can be removed.

That review finding is closed: the fixture tracks Homebrew's separate command
groups, bounds cancellation and inherited output waits, and retains private
tools if cleanup cannot be proven. Real Homebrew regressions cover timeout,
normal parent exit with a surviving child, and outer cancellation. Independent
Sol 6.1 high review of the final source found no remaining actionable issues.

The latest pre-change main run passed all required validation but failed
publication because checkout cleaned the previously downloaded `dist` assets.
The signing change puts checkout before downloads and adds a workflow
regression. That prior release was not published successfully.

## Apple submission state

Payload submission `0602c2b7-fbe6-4aec-ac69-6a0100026a9a` was uploaded at
2026-10-21 13:54 UTC. Apple reports **In Progress**. Acceptance, payload
stapling, outer-installer/DMG notarization and final Gatekeeper acceptance are
pending. A successful signature or upload is not notarization acceptance.

The original account-setup probe from the separate signing thread was also
still processing when checked. This does not establish a cause for Apple's
delay. Submission identifiers and diagnostics are retained outside release
artifacts. Inspect pending submissions before retrying. Nothing from this
change has been merged, published or installed into the operator's live queue.

A complete standalone [HTML version](apple-notarization.html) accompanies this
handoff.
