# Apple signing and notarization

Research checked against Apple and GitHub primary documentation on 2026-10-22.
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

1. Sign the two Go helpers in both the outer payload and the menu app's embedded setup payload, then sign the menu app. Submit the complete payload ZIP.
2. Require acceptance, fetch the log, then staple and validate the menu app.
3. Sign the outer installer, submit its ZIP, require acceptance, and staple it.
4. Recreate the distribution ZIP with the stapled installer inside it.
5. Copy the stapled, self-contained menu app into a DMG with an Applications shortcut, sign the DMG, notarize it, and staple it. Retained older builds without an embedded setup payload keep their original installer DMG layout.
6. Generate final checksums and Homebrew metadata after all byte changes.

Record each submission ID before waiting. A timeout leaves a submission at
Apple; inspect it before retrying. Read successful logs for warnings too.
ZIPs and standalone executables cannot themselves carry stapled tickets. Bare
Go helpers can require access to Apple's service on first launch, so offline
first launch of the complete installed product remains a separate test.
These requirements come from Apple's
[custom notarization workflow](https://developer.apple.com/documentation/security/customizing-the-notarization-workflow).

The menu app carries its own source payload for first-launch setup after a Finder copy. Its helpers are signed before sealing the app; setup never modifies the installed bundle. The ZIP remains a custom installer for existing updater and Homebrew consumers. The DMG contains the same app, copied with its signature, ticket and quarantine preserved.

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
`APPLE_NOTARY_TIMEOUT` defaults to `5m` per submission. The signing job has a
45-minute outer budget for three submissions, checkpoint transfers and packaging. This is a bounded
wait, not a claim about Apple's turnaround time.

PRs build and test ad hoc development artifacts without Apple credentials.
After required main validation passes, a separate distribution workflow imports the
identity into a disposable private Keychain and signs the already-built
installer. It does not recompile code with the signing key loaded. The wrapper
removes temporary credentials on success, failure and cancellation. Publication
consumes only the finalized notarized artifact. Missing credentials, rejection,
stapling or policy failures stop publication with no ad hoc fallback. Distribution
uses trusted main automation and the original build's verified artifacts. Its
workflow run number and automation commit never replace the source build identity.

Before invoking a signing child, the wrapper checks both the matching and valid
code-signing identity inventories in its owned Keychain. It requires exactly
one configured full certificate name or fingerprint, a Developer ID Application
name and the expected team. It passes the resolved SHA-1 to the child. Missing,
invalid, ambiguous or inconsistent identities fail before signing. Public
diagnostics contain fixed classifications, matching/valid counts and, on
success, the selected fingerprint and team. Raw inventories and tool errors
are withheld; a child failure refreshes this diagnostic without replacing the
child's exit status. This check establishes identity selection, while the
subsequent signature and Apple-chain verification remain required.

Signing and verification operations emit fixed begin/success/failure markers
with the original tool exit status. Preparation also marks credential
configuration and installer restoration, so a failure can be attributed to
one operation without tracing command arguments. The markers do not include
paths, credential selectors or command arguments. Inventory `ready` means
one valid matching identity was enumerated; it does not establish successful
private-key use by `codesign`.

The macOS test suite includes a native probe that creates a new synthetic
identity in a disposable private Keychain, signs a copied system executable
with its explicit fingerprint, and verifies the certificate embedded in that
signature. A unique identity prevents an existing operator identity from
satisfying lookup. It uses no production credentials, notarization uploads,
trust overrides and checks cleanup and unchanged final Keychain settings.
The hosted-runner wrapper temporarily appends its
owned Keychain to the existing search list, then restores the original list;
local runs keep the unlisted probe and never change search settings. Registration
requires `GITHUB_ACTIONS=true` and `RUNNER_ENVIRONMENT=github-hosted`, rejecting
self-hosted runners. The operator authorized this CI-only registration scope;
the operator's local search list and trust settings remain untouched.
Only the newly owned Keychain's existing parent is resolved to its physical
path before registration. Captured original entries and order remain exact;
readback and concurrent-change checks are not relaxed. A symlinked final file
is rejected. The fixed `owned-path-normalized` classification reports this
case without exposing paths.
The probe is separate from Developer ID acceptance
and never executes its signed target.

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

Public submission receipts and exact signed intermediate archives are retained
in CI checkpoints, separately from release downloads. Private keys and exported
identities are excluded. Local credentials stay outside the checkout.

## CI notarization client

The resumable CI path uses a prebuilt Go client for Apple's
[Notary API](https://developer.apple.com/documentation/notaryapi/submitting-software-for-notarization-over-the-web).
It creates a submission, records the returned ID durably, then uploads the exact
prepared archive using Apple's temporary S3 credentials. JWT signing uses
`go-jose`; S3 upload uses the official AWS Go SDK. API authentication is checked
with a read-only history request before signing or recording submission intent.
History contents, JWTs, private keys and S3 credentials are never printed.

CI builds `queue-tool` before loading credentials and passes its absolute
executable path in `QUEUE_NOTARY_TOOL`. Direct API-key authentication in the
resumable engine requires that binary and a team key with an issuer ID. There
is no native-client fallback. Local Keychain-profile commands and one-shot
packaging retain their existing `notarytool` path.
The REST engine accepts bare-second timeout values such as `300` and explicit
`s`, `m` or `h` units, with a maximum of 24 hours. It validates this budget before
signing or creating a submission. The CI job's outer budget still applies.

Each create request is attempted once. Before it, the client validates the
stage, current attempt and archive hash, then records upload intent. Once
Apple returns a valid ID, the client saves it before S3 upload. A failed upload
with a recorded ID can only query that submission; it never creates another.
An interrupted create request can still have an unknown result without an ID.
That state requires manual reconciliation and refuses automatic resubmission.
The local synced record becomes recoverable on another runner after the
immutable checkpoint upload succeeds. Loss of the entire runner before that
upload can still require reconciliation. Temporary S3 credentials are never
checkpointed; an incomplete upload with a known ID may require operator
resolution rather than an automatic retry.

## Resuming a pending CI submission

Ticket stapling and validation retry only a positively identified CloudKit connection loss:
stapler exit 68 together with `NSURLErrorDomain -1005` and the exact Apple ticket lookup
endpoint in that error's own fields. Each operation permits six attempts, with
5-, 10-, 20-, 40- and 80-second delays (155 seconds of backoff in total). Invalid,
revoked, missing, or otherwise failed ticket checks remain fatal, as does an
exhausted retry. Stapling retries attach the ticket to the same working copy;
they never sign or submit code. A successful staple still requires a separate
successful validation. Validation retries remain read-only and never substitute
a local ticket for the required successful validation. If retries are exhausted,
the accepted receipt and immutable submitted archive remain recoverable through
the existing checkpoints. The 45-minute signing job budget still applies.

Build CI and distribution run independently. `ci.yml` validates and retains the
universal installer. `distribution.yml` automatically follows a successful
same-repository main-push CI run. It can also be dispatched manually from `main`
with the original `build_run_id`, so a merged automation repair can process the
retained installer without recompiling it.

The build run ID, artifact-producing attempt, build number, source commit,
artifact ID, artifact digest and embedded version identify the original build.
The workflow checks API provenance, the successful artifact-producing installer
job and the effective successful required validation gate for the same run and
source commit, the outer GitHub artifact digest, ZIP/DMG checksums and the
embedded build identities before credentials are used. The installer may come
from an earlier attempt when a failed-job retry passed the remaining checks. It reads the version from the original
artifact, even when current main has a different `VERSION`.

Manual recovery also supports a completed legacy CI run whose installer and
required validation passed before its old distribution jobs failed. It does not
make a failed application build eligible. Automatic distribution requires the
whole triggering CI run to have succeeded.

The automation commit is pinned separately across distribution jobs. Release
tags, manifests and target commits keep the original build identity. Changing
application source requires a new CI build; changing distribution automation can
reuse the existing artifact.

The protected signing job uses `scripts/macos-resume.sh`; local one-shot
`macos-signing.sh package` remains available. Each payload, installer and DMG
stage persists an immutable checkpoint before uploading, immediately after
recording its submission ID, and after every attempted preparation, including
failures before submission and failed waits. The last snapshot retains the
latest available state even when preparation never reaches its first upload.
Missing required checkpoints still stop recovery; this does not authorize an
older state after an uncertain attempt. An
initial `s00` checkpoint precedes credential setup and signing. Checkpoints
contain the original prepared installer, exact signed upload archives, their
SHA-256 values, stage records and public Apple receipts. ZIP wrapping preserves
executable modes and macOS resource forks.

If Apple remains pending after the finite wait, publication stops. Once the
existing submission is ready, rerun failed jobs on the **distribution run** to
reuse unchanged automation:

```sh
gh run rerun DISTRIBUTION_RUN_ID --failed --repo Dunky13/agent-job-queue
```

If the workflow or scripts needed a repair, merge that repair first, then
substitute the original numeric CI run ID for `BUILD_RUN_ID`:

```sh
gh workflow run distribution.yml --repo Dunky13/agent-job-queue \
  --ref main -f build_run_id=BUILD_RUN_ID
```

The expected result is a new **Distribute build BUILD_RUN_ID** run using main's
corrected automation and the original source artifact. Inspect it with:

```sh
gh run list --repo Dunky13/agent-job-queue --workflow distribution.yml --limit 5
```

Distribution executions share the original CI run's concurrency lock, including
legacy signing retries, without cancelling an active signing job. Recovery
discovers earlier executions for that build and
includes the original CI run's legacy checkpoints. New checkpoint names include
build run ID, distribution run ID, attempt and stage slot. Recovery verifies run
provenance, the source artifact binding, both archive digests, team, version and
stage receipts. Numeric attempt and slot order select state within an execution;
verified checkpoint creation times order separate executions. Artifact IDs are
opaque. Missing, malformed or tied creation times stop recovery. A prepared
stage also records its execution run ID, so a new
run with attempt number `1` cannot accidentally repeat an uncertain upload from
an older run's attempt `1`.

Checkpoint obligations follow actual signing jobs across attempts. A retry of
acceptance or publication can retain an earlier successful signing checkpoint;
a complete job history proving all three signing stages were skipped does not
require a new one, including setup failures before the initial checkpoint.
Missing state after signing could have begun still stops recovery.

Recovery continues accepted stages and existing submission IDs without signing
or uploading them again. Accepted Apple logs must match the submission ID and
exact archive hash. Only three completed stages can export release assets.
Fresh Apple Silicon and Intel acceptance jobs still precede publication and
Homebrew delivery. Final artifacts use execution-specific names; downstream
jobs consume the successful signing job's exact artifact.

Checkpoints and original CI artifacts are retained for 30 days. Missing or
expired latest state, invalid provenance, corruption, a changed source artifact
or signing team, rejection and an uncertain upload stop recovery. There is no
fallback to older state or a fresh upload. Whole-runner loss before durable
persistence can require provider reconciliation. Legacy checkpoints with an
unknown upload outcome remain blocked; a new distribution run does not clear
that uncertainty. Inspect preserved receipts and Apple's submission history
with the original credentials before deciding how to recover. Automatic receipt
import or duplicate resubmission is not provided.

GitHub failed-job retries retain the original automation revision, ref and
actor privileges, and are available for 30 days with at most 50 attempts.
Dispatching a new distribution run is the path for using a repaired workflow.
See GitHub's [rerun rules](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs),
[workflow-run trigger](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#workflow_run)
and [artifact API](https://docs.github.com/en/rest/actions/artifacts).

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
