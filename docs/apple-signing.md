# Apple signing and notarization

Research checked against Apple and GitHub primary documentation on 2026-10-21.
This guide describes the release implementation and its acceptance criteria.
It does not certify an artifact while Apple is still processing it.

## Signing policy

Use Developer ID Application for both app bundles, both Go executables, and the
DMG. Sign universal executables after combining their architecture slices. Each
executable receives Hardened Runtime, a secure timestamp, and a stable code
identifier. Verify both architecture slices against Apple's certificate chain
and the configured team. Sign components explicitly from the inside out; use
`--deep` for verification only. These choices follow Apple's
[distribution signing instructions](https://developer.apple.com/documentation/xcode/creating-distribution-signed-code-for-the-mac/).

Start without exception entitlements. In particular, do not add JIT, unsigned
executable memory, disabled library validation, or debugging access merely
because an example for another runtime uses them. Test the actual Swift app,
Go queue daemon and HTTPS gateway with their hardened signatures. Apple advises
using only exceptions required by the app's behavior in its
[Hardened Runtime documentation](https://developer.apple.com/documentation/security/hardened-runtime).

Developer ID notarization is separate from App Store distribution. The current
product needs no App Store upload, sandbox conversion, or Developer ID Installer
certificate for its ZIP and DMG formats. The Apple
[notarization prerequisites](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution)
require Developer ID signing, runtime protection and secure timestamps.

## Packaging order

This is a custom installer that copies another app and standalone helpers.
Apple explicitly requires payload notarization before assembling such an
installer. Our additional DMG distribution also receives its own ticket:

1. Sign the two Go helpers and the menu app. Submit the complete payload ZIP.
2. Require acceptance, fetch the log, then staple and validate the menu app.
3. Sign the outer installer, submit its ZIP, require acceptance, and staple it.
4. Recreate the distribution ZIP with the stapled installer inside it.
5. Create and sign the DMG containing that installer, notarize it, and staple it.
6. Generate final checksums and Homebrew metadata after all byte changes.

Record each submission ID before waiting. A timeout leaves a submission at
Apple; inspect it before retrying. Read successful logs for warnings too.
ZIPs and standalone executables cannot themselves carry stapled tickets. Bare
Go helpers can require access to Apple's service on first launch, so offline
first launch of the complete installed product remains a separate test.
These requirements come from Apple's
[custom notarization workflow](https://developer.apple.com/documentation/security/customizing-the-notarization-workflow).

The payload app is stapled before signing its parent because it lives under
the installer's sealed resources. Production installation copies the app with
`ditto --rsrc --extattr --qtn --acl`; verification checks the copied app's
signature, ticket and quarantine. Never change signed resource files afterward.
Apple's [packaging guide](https://developer.apple.com/documentation/xcode/packaging-mac-software-for-distribution)
describes container signing, stapling and distribution testing.

## Credentials and CI

The `apple-signing` GitHub environment is restricted to branch `main`. Store
the following secrets there, with no repository-scoped Apple secret copies:

- `APPLE_DEVELOPER_ID_P12_BASE64`: the password-protected identity, base64 encoded.
- `APPLE_DEVELOPER_ID_P12_PASSWORD`: its export password.
- `APPLE_NOTARY_KEY_P8`: the App Store Connect team API private key.

Its non-secret variables are `APPLE_SIGNING_IDENTITY`, `APPLE_TEAM_ID`,
`APPLE_NOTARY_KEY_ID`, and `APPLE_NOTARY_ISSUER_ID`. Optional
`APPLE_NOTARY_TIMEOUT` defaults to `30m` per submission. The signing job has a
120-minute outer budget for three submissions and packaging. This is a bounded
wait, not a claim about Apple's turnaround time.

PRs build and test ad hoc development artifacts without Apple credentials.
After required main validation passes, a separate hosted macOS job imports the
identity into a disposable private Keychain and signs the already-built
installer. It does not recompile code with the signing key loaded. The wrapper
removes temporary credentials on success, failure and cancellation. Publication
consumes only the finalized notarized artifact. Missing credentials, rejection,
stapling or policy failures stop publication with no ad hoc fallback.

Before publication, separate fresh Apple Silicon and Intel jobs verify final
checksums, signatures, tickets and app-copy policy, exercise the hardened queue
and HTTPS gateway, and run the actual Homebrew installer with quarantine kept.
The Homebrew test owns a private daemon and user directory, substitutes only
its private launchctl fixture, and bounds installer descendants before cleanup.
No Apple credential is passed to these acceptance jobs. Checkout precedes all
artifact downloads so its workspace cleanup cannot erase the release inputs.

GitHub recommends secret storage and disposable runner Keychains in its
[Apple certificate guide](https://docs.github.com/en/actions/how-tos/deploy/deploy-to-third-party-platforms/sign-xcode-applications).
Environment branch rules protect secret access independently of workflow
conditions, as described in
[managing environments](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments).
The existing main protection and signed-commit policy continue to apply.

Public submission receipts are retained separately from release artifacts.
Private keys, exported identities and temporary upload archives are never
included in published downloads. Local credentials stay outside the checkout.

## Local use

Ad hoc development remains the default:

```sh
agent-heavy run --label "Agent Queue installer" -- scripts/build-installer.sh
```

For Developer ID packaging, provide your installed identity, expected team,
and an existing `notarytool` Keychain profile:

```sh
SIGNING_MODE=developer-id \
APPLE_SIGNING_IDENTITY='Developer ID Application: YOUR NAME (YOURTEAMID)' \
APPLE_TEAM_ID=YOURTEAMID \
NOTARY_KEYCHAIN_PROFILE=YOUR_PROFILE \
agent-heavy run --label "Agent Queue notarized installer" -- scripts/build-installer.sh
```

The team placeholder must be replaced by the actual ten-character team ID.
Alternatively use `NOTARY_KEY_FILE`, `NOTARY_KEY_ID`, and, for a team API key,
`NOTARY_ISSUER_ID`. Profile and direct-key authentication cannot be mixed.
`NOTARY_KEYCHAIN` selects a non-default profile Keychain, while
`APPLE_SIGNING_KEYCHAIN` selects a signing Keychain. Paths are operator inputs,
never committed private material. `NOTARY_LOG_DIR` must be outside `DIST_DIR`.
Configuration is checked before compiling.

To sign an already validated extracted installer without rebuilding it:

```sh
scripts/macos-signing.sh package 'prepared-installer/Agent Job Queue Installer.app' dist "$(cat VERSION)"
```

Supply the same Developer ID environment for that command. Inspect a retained
submission with `xcrun notarytool info SUBMISSION_ID --keychain-profile YOUR_PROFILE`.
Do not submit the same pending build repeatedly to try to accelerate processing.

## Distribution and acceptance

Both the native installer and new Homebrew casks directly execute the signed
Go controller. The cask preserves quarantine. Historical ad hoc casks remain
unchanged until the next numbered release; the trusted tap helper validates
their original cask as bounded data. Merge the companion trusted tap update
before the source release, as documented in [Homebrew delivery](homebrew-delivery.md).

Acceptance requires all four executable signatures, Apple acceptance and logs,
valid stapled tickets, final ZIP extraction and mounted-DMG checks, successful
`syspolicy_check distribution` for both app bundles, and actual packaged queue
and HTTPS execution. Automated tests also exercise rejection, malformed
responses, missing or conflicting configuration, wrong teams, cleanup and
quarantine preservation.

Use `codesign --verify --check-notarization` with an explicit `notarized`
requirement for the standalone Go executables. An app-oriented `spctl` check
does not establish the correct behavior for every non-app code type. The
quarantined Homebrew test verifies both helpers' notarization before exercising
the actual signed installer entry. Apple documents these distinct checks in
[Testing a Notarised Product](https://developer.apple.com/forums/thread/130560).

Signing, notarization and Gatekeeper acceptance are distinct results. A normal
first-open confirmation can still appear for a correctly notarized download.
Apple's [trusted execution troubleshooting](https://developer.apple.com/forums/thread/706442)
recommends testing a quarantined download on a fresh Mac or restored VM,
including offline behavior where supported. Local development-Mac checks do
not establish clean-Mac or Intel runtime acceptance. Hosted Intel tests and
clean-machine installation evidence must be reported separately.

A complete standalone [HTML version](apple-signing.html) accompanies this guide.
