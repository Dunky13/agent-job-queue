# Apple notarization handoff

Observed on 2026-10-22. This is the evidence record for the Developer ID and
notarization change; the [signing guide](../apple-signing.md) contains the full
research, source links, configuration and operator commands.

## Implemented behavior

Local development packages remain ad hoc signed. A main-only protected signing
job consumes validated universal binaries, imports temporary credentials, and
notarizes the payload before signing the enclosing installer. Both app bundles
and the final signed DMG receive validated stapled tickets before final hashes
and release metadata are generated. Publication also waits for fresh ARM and
Intel artifact acceptance jobs. There is no unsigned publication fallback.

Protected CI also retains immutable checkpoints of the signed bytes and Apple
submission receipts before and after each upload and after each finite wait.
Re-running failed jobs on the original main-push run restores the exact build
and continues existing submission IDs. The default wait is five minutes per
stage. A pending submission blocks publication without losing the build.
Run, repository, source, team, version and archive hashes are checked before
resuming. Unknown upload outcomes and missing or expired state fail closed.
The [signing guide](../apple-signing.md#resuming-a-pending-ci-submission)
documents recovery commands and the 30-day retention boundary.

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
- Resumption regressions cover finite timeouts, exact archive and submission ID
  reuse across attempts, ambiguous uploads, malformed IDs, interrupted stapling,
  Apple log binding, diagnostics, metadata mismatch and archive corruption.
  Checkpoint tests cover provenance, numeric attempt ordering, missing/expired
  state, both digests, pagination, unsafe archives, locks and mode/xattr transfer.
- A real Developer ID-signed universal payload survived a checkpoint round-trip
  with identical archive bytes and valid signatures for both architectures.
  Restoring a pre-upload snapshot without an ID refused a duplicate upload.
  That check simulated GitHub transport and made no new Apple submission.
- The final tooling, delivery and tap race tests passed. Independent Sol 6.1
  high review found no remaining actionable issues in the resume engine, store,
  composite action, workflow and test changes. The protected hosted signing
  and recovery path remains main-only and has not run on this PR.

The actual final notarized native app also launched with quarantine retained,
rendered its isolated Learned Workloads fixture and exited normally. Final ZIP
checksums, extracted signatures and stapled tickets, Gatekeeper distribution
checks, mounted-DMG checks, packaged private queue work and HTTPS passed.

Actual notarized Homebrew execution is an opt-in acceptance test. Its first
end-to-end run exposed an incomplete enclosing app in the test fixture; the
faithful full-installer fixture subsequently completed with quarantine retained.
The complete final acceptance batch passed: real Homebrew installation finished
in 8.87 seconds, all 204 staged entries retained quarantine and the additional
fixture attribute, and the private queue daemon remained healthy. Unsigned
content, a genuinely notarized payload inside an incomplete outer installer,
and timeout/normal-exit/cancellation descendant cleanup also passed. Tooling,
delivery and standalone tap race tests passed. The default ad hoc fixture
is rejected before staging or executing content; data-only quarantine tests
do not claim notarized runtime proof. Fresh hosted ARM/Intel acceptance jobs
remain main-only and have not run for this local artifact. Clean-Mac offline
first launch remains unverified; standalone binaries cannot carry stapled
tickets.

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

The first final notarized Homebrew test copied only the payload into an
otherwise incomplete `Agent Job Queue Installer.app` directory. macOS blocked
its signed controller in `dyld` before Go started. Copying the complete signed
and stapled installer instead made the same real Homebrew install finish in
11 seconds, with the original 60-second deadline and quarantine preserved on
every staged entry. The fixture now checks the enclosing installer before and
after staging, and rejects the former incomplete enclosure before execution.
No backend change, quarantine removal or deadline increase was needed.

The new checkpoint mode test initially assumed creating a file with mode
`0755` would override a private process umask. Reproduction showed the fixture
was already `0700` before snapshotting and remained `0700` after restore.
The test now explicitly establishes and checks `0755` before transfer, then
requires the same mode and attribute afterward. It passes under both `022`
and `077` umasks and race detection; the broad race suite subsequently passed.

Independent review caught that the new opt-in incomplete-installer regression
was excluded by the final artifact job's exact test selector. Both hosted
architectures now run all `TestNotarizedHomebrew` cases with the finalized
payload. Workflow lint passed, and independent Sol 6.1 high re-review found no
remaining actionable findings in this repair. Cross-provider review was not
available and is not claimed.

## Apple submission state

Apple accepted all three submissions, and each downloaded log reports no
issues:

- Payload: `0602c2b7-fbe6-4aec-ac69-6a0100026a9a`, uploaded
  2026-10-21 13:54 UTC, Accepted.
- Installer: `f9b9ac22-b490-4dc0-b0f0-a456860bd011`, uploaded
  2026-10-21 15:25 UTC, Accepted.
- DMG: `c8a0a5f7-e289-4621-b5e4-5f673943b870`, uploaded
  2026-10-22 03:16 UTC, Accepted.

The installer's first local six-hour wait expired while Apple continued
processing. Observation resumed against the same submission. Before resuming
packaging, all eight architecture-specific payload code hashes matched Apple's
accepted log, and the retained installer ZIP hash matched Apple's accepted
installer log. No duplicate payload or installer upload was needed. App and
installer tickets were stapled before their enclosing artifacts were sealed;
final checksums were generated after the DMG was stapled.

This is a local test artifact with null commit/build identity, not a production
release candidate. A production release must embed its reviewed commit and
build identity before signing, then pass its own notarization and fresh hosted
acceptance. CI now retains the original signed build and submission IDs across
runner attempts; rerunning failed jobs continues the same submissions. A
timeout still prevents publication until acceptance and runtime checks pass.

Submission identifiers and diagnostics are retained outside release artifacts.
Inspect pending submissions before retrying. Nothing from this change has been
merged, published or installed into the operator's live queue.

A complete standalone [HTML version](apple-notarization.html) accompanies this
handoff.
