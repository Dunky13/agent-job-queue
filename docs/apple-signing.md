# Apple signing and notarization

Research checked against Apple and GitHub primary documentation on 2026-10-26.
This guide describes the release implementation and its acceptance criteria.
It does not certify an artifact while Apple is still processing it.

## Signing policy

Use Developer ID Application for the app bundle, its two bundled Go executables
(`agent-heavy` and `agent-remote` in `Contents/MacOS`), and the DMG. Sign universal executables after combining their architecture slices. Each
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

The release is one self-contained `Agent Queue.app`. No installer or payload is
assembled around it, so the app itself is the notarized unit and the DMG is an
additional container that receives its own ticket. Notarization has two stages,
`app` then `dmg`:

1. Sign the two Go helpers in the app's `Contents/MacOS`, then sign the app. Managed service definitions remain in the app's `Contents/Library/LaunchAgents` and use `BundleProgram`. Submit the app as a ZIP (`ditto -c -k --sequesterRsrc --keepParent`).
2. Require acceptance, fetch the log, then staple and validate the app.
3. Recreate the distribution ZIP from the stapled app. It contains only `Agent Queue.app` at its top level.
4. Package the stapled app and an `Applications` symlink in the DMG, sign the DMG, notarize it, and staple it.
5. Generate final checksums and Homebrew metadata after all byte changes.

Final release assets are `Agent-Queue-VERSION-universal.zip` and
`Agent-Queue-VERSION-universal.dmg`, each with a `.sha256` file, plus
`agent-job-queue.json`, `agent-job-queue.rb` and `apple-notarization.json`.
Hosted Apple submissions are named `Agent-Queue-VERSION-build-N-SHA-universal.zip`
(app stage) and `Agent-Queue-VERSION-build-N-SHA-universal.dmg` (dmg stage). `N` is the original
CI build number and `SHA` is its seven-character source prefix. Local packaging
keeps version-only names. Product, version, build and stage are visible before upload. Resumable checkpoints retain their
existing internal filenames; submission copies preserve the exact archive bytes
and SHA-256. Already recorded submissions are resumed by ID without resubmitting.

Record each submission ID before waiting. A timeout leaves a submission at
Apple; inspect it before retrying. Read successful logs for warnings too.
ZIPs and standalone executables cannot themselves carry stapled tickets. Bare
Go helpers can require access to Apple's service on first launch, so offline
first launch of the complete installed product remains a separate test.
These requirements come from Apple's
[custom notarization workflow](https://developer.apple.com/documentation/security/customizing-the-notarization-workflow).

The app carries support metadata and documents without duplicate helper binaries; its only executables are the helpers in `Contents/MacOS`. Opening the app from `/Applications` or `~/Applications` runs setup (`agent-heavy install --app-bundle`) from inside the bundle. Setup registers the bundle-owned services, writes the CLI wrappers to `~/.local/bin` and never modifies the installed bundle or copies executables out of it. Helpers are signed before sealing the app. The ZIP and the DMG deliver the same stapled app, preserving signatures, tickets and quarantine. Homebrew delivers the app through a plain `app` cask; nothing runs at install time and the first launch completes setup.

The app is stapled only after its notarization is accepted, and no signed
resource file changes afterward. Verification copies the app with
`ditto --rsrc --extattr --qtn --acl` and checks the copy's signature, ticket and
quarantine. Apple's [packaging guide](https://developer.apple.com/documentation/xcode/packaging-mac-software-for-distribution)
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
45-minute outer budget for two submissions, checkpoint transfers and packaging. This is a bounded
wait, not a claim about Apple's turnaround time.

PRs build and test ad hoc development artifacts without Apple credentials.
After required main validation passes, a separate distribution workflow imports the
identity into a disposable private Keychain and signs the already-built
app. It does not recompile code with the signing key loaded. The wrapper
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
configuration and app restoration, so a failure can be attributed to
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
checksums, signatures, tickets and app-copy policy (a quarantine-preserving copy of the extracted app), exercise the hardened queue
and HTTPS gateway, and exercise actual Homebrew delivery with quarantine kept.
The runtime test then runs setup from inside the extracted, signed app. It owns
private daemon processes and user directories,
substitutes native lifecycle and launchctl readback, and bounds descendants
before cleanup. These checks do not qualify real Service Management registration,
approval, subsequent login or reboot; those need an isolated login-session test.
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
universal app (artifact `Agent-Queue-App-N`). `distribution.yml` automatically follows a successful
same-repository main-push CI run. It can also be dispatched manually from `main`
with the original `build_run_id`, so a merged automation repair can process the
retained app without recompiling it.

The build run ID, artifact-producing attempt, build number, source commit,
artifact ID, artifact digest and embedded version identify the original build.
The workflow checks API provenance, the successful artifact-producing build
job and the effective successful required validation gate for the same run and
source commit, the outer GitHub artifact digest, ZIP/DMG checksums and the
embedded build identities before credentials are used. The app may come
from an earlier attempt when a failed-job retry passed the remaining checks. It reads the version from the original
artifact, even when current main has a different `VERSION`.

Manual recovery also supports a completed legacy CI run whose app and
required validation passed before its old distribution jobs failed. It does not
make a failed application build eligible. Automatic distribution requires the
whole triggering CI run to have succeeded.

The automation commit is pinned separately across distribution jobs. Release
tags, manifests and target commits keep the original build identity. Changing
application source requires a new CI build; changing distribution automation can
reuse the existing artifact.

The protected signing job uses `scripts/macos-resume.sh`; local one-shot
`macos-signing.sh package` remains available. Each app and DMG
stage persists an immutable checkpoint before uploading, immediately after
recording its submission ID, and after every attempted preparation, including
failures before submission and failed waits. Checkpoint slots are ordered: `s00` is the initial state, `01` to `03` are the app stage (prepared, submitted, finished), `07` to `09` are the dmg stage, and `s10` is the final candidate (`04` to `06` are unused). The last snapshot retains the
latest available state even when preparation never reaches its first upload.
Missing required checkpoints still stop recovery; this does not authorize an
older state after an uncertain attempt. An
initial `s00` checkpoint precedes credential setup and signing. Checkpoints
contain the original prepared app, exact signed upload archives, their
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
a complete job history proving both signing stages were skipped does not
require a new one, including setup failures before the initial checkpoint.
Missing state after signing could have begun still stops recovery.

Recovery continues accepted stages and existing submission IDs without signing
or uploading them again. Accepted Apple logs must match the submission ID and
exact archive hash. Only the two completed stages (app and dmg) can export release assets. The final distribution artifact is `Agent-Queue-Notarized-App-N`; the workflow extracts its ZIP into `prepared-app/` and checks it with `queue-tool verify-app --app`. Installer-era checkpoints and builds (payload or installer stages, or a non-self-contained app layout) are refused by this automation and cannot be resumed or published.
Fresh Apple Silicon and Intel acceptance jobs still precede publication and
Homebrew delivery. Final artifacts use execution-specific names; downstream
jobs consume the successful signing job's exact artifact.

Checkpoints and original CI artifacts are retained for 90 days. Independent small
receipt/diagnostic artifacts are also retained for 90 days. Successful releases
include permanent `apple-notarization.json` evidence binding both Accepted
logs, submitted hashes, source metadata and final hashes. Missing or
expired latest state, invalid provenance, corruption, a changed source artifact
or signing team, rejection and an uncertain upload stop recovery. There is no
fallback to older state or a fresh upload. Whole-runner loss before durable
persistence can require provider reconciliation. Legacy checkpoints with an
unknown upload outcome remain blocked; a new distribution run does not clear
that uncertainty. Inspect preserved receipts and Apple's submission history
with the original credentials before deciding how to recover. Unknown uploads require operator reconciliation; automatic receipt import and
duplicate resubmission are not provided.

GitHub failed-job retries retain the original automation revision, ref and
actor privileges, and are available for 30 days with at most 50 attempts.
Dispatching a new distribution run is the path for using a repaired workflow.
See GitHub's [rerun rules](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs),
[workflow-run trigger](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#workflow_run)
and [artifact API](https://docs.github.com/en/rest/actions/artifacts).

## Automatic continuation and publication

`Distribution recovery` observes completed distribution workflows and runs every
15 minutes. It uses Actions write access to request a same-run failed-job retry;
it has no Apple secrets or publication permission. A valid GitHub artifact
digest, exact source/build/execution binding, newest checkpoint, and matching
UUID/status/logs are required. Pending status must come from a finite REST wait
timeout in the current invocation. The outer signing wrapper must separately
prove that its temporary credentials were cleaned up successfully. Preparation
clears inherited retry status before accessing credentials. Malformed or
mismatched later polls cannot retain an earlier pending result. Only explicit pending provider status or the
classified CloudKit connection-loss failure can retry signing. Rejection,
missing or ambiguous custody, malformed evidence and acceptance failures stop.
Delivery-only failures can retry after both architecture acceptance jobs passed.
The first continuation can run immediately; later attempts wait at least 15
minutes after completion. Automatic recovery stops at attempt 48 or a candidate
age of seven days, with exact refusal details in the observer log and summary. The original build lock and UUID are retained through every retry. Export-time ticket verification has its
own final checkpoint and classified continuation while preserving completed bytes.

Build and signing jobs explicitly select Xcode 16.4 (16F6), verify that selection
before credentials, and retain the actual SDK, Swift, Go, notary client and runner
image identity. Hosted qualification is required for this pin; a local receipt
from another Xcode is not its proof.

Each required toolchain probe must exit successfully and return an identity
before a receipt is written. A legacy checkpoint receives schema-2 artifact
binding only after its existing source/run/team metadata is verified and the
trusted verifier proves its initial archive matches the original CI archive's
contents, modes and resource metadata. This upgrade changes neither signed
archives nor submission records. Missing or mismatched origin proof blocks it.

The channel remains numbered main-build prereleases, with `VERSION` as the
product version and `build-N-SHA` as the immutable release identity. No stable
version or updater trust authority changes. `ci-required` remains the aggregate
CI gate. The protected environment stays main-only without recurring reviewers.

The final candidate checkpoint retains `publication.json` before export. That
file records the first finalization execution and toolchain, and later executions
verify and reuse its exact bytes as `apple-notarization.json`. Per-attempt
toolchain receipts remain separate. A whole-workflow rerun therefore preserves
every public asset instead of regenerating execution-dependent audit bytes.
Retained audit evidence must match the producer's sorted JSON serialization;
unknown fields, multiple documents, incomplete toolchains and duplicate-key
ambiguities fail verification before publication.

Publication creates a private draft, binds its entire candidate inventory in the
draft notes, and uploads without overwriting assets. It verifies exact remote
inventory, source/tag identity and downloaded bytes before exposure. GitHub
release immutability is enabled as repository policy. Published release metadata
must confirm immutability, alongside the exact inventory and byte readback. The
workflow uses its existing contents-write Actions token; no additional App or
administration credential is required. The same complete assets are checked again
after publication. An existing identical public release is verified read-only;
a conflicting draft, tag or asset fails closed. Unfinished provider uploads are
retained for explicit reconciliation. Homebrew delivery remains a separate
exact-build gate using the existing narrowly scoped App token and trusted tap.

## Local use

Ad hoc development remains the default:

```sh
agent-heavy run --label "Agent Queue app" -- scripts/build-app.sh
```

For Developer ID packaging, provide your installed identity, expected team,
and an existing `notarytool` Keychain profile:

```sh
SIGNING_MODE=developer-id \
APPLE_SIGNING_IDENTITY='Developer ID Application: YOUR NAME (YOURTEAMID)' \
APPLE_TEAM_ID=YOURTEAMID \
NOTARY_KEYCHAIN_PROFILE=YOUR_PROFILE \
agent-heavy run --label "Agent Queue notarized app" -- scripts/build-app.sh
```

The team placeholder must be replaced by the actual ten-character team ID.
Alternatively use `NOTARY_KEY_FILE`, `NOTARY_KEY_ID`, and, for a team API key,
`NOTARY_ISSUER_ID`. Profile and direct-key authentication cannot be mixed.
`NOTARY_KEYCHAIN` selects a non-default profile Keychain, while
`APPLE_SIGNING_KEYCHAIN` selects a signing Keychain. Paths are operator inputs,
never committed private material. `NOTARY_LOG_DIR` must be outside `DIST_DIR`.
Configuration is checked before compiling.

To sign an already validated extracted app without rebuilding it:

```sh
scripts/macos-signing.sh package 'prepared-app/Agent Queue.app' dist "$(cat VERSION)"
```

Supply the same Developer ID environment for that command. Inspect a retained
submission with `xcrun notarytool info SUBMISSION_ID --keychain-profile YOUR_PROFILE`.
Do not submit the same pending build repeatedly to try to accelerate processing.

## Distribution and acceptance

The notarized app runs setup itself on first launch and executes its signed Go
helpers from inside the bundle. New Homebrew casks deliver only the app, with no
installer script or setup hook; the user opens it to finish setup. The cask
preserves quarantine. Historical ad hoc casks remain unchanged until the next numbered release; the trusted tap
helper validates their original cask as bounded data. Merge the companion trusted
tap update before the source release, as documented in [Homebrew delivery](homebrew-delivery.md).

Acceptance requires valid signatures on the app, both bundled helpers and the DMG, Apple
acceptance and logs for both stages, valid stapled tickets on the app and DMG,
final ZIP extraction and mounted-DMG checks (exactly `Agent Queue.app` and an
`Applications` link), successful `syspolicy_check distribution` for the app,
and actual packaged queue and HTTPS execution. Automated tests also exercise
rejection, malformed responses, missing or conflicting configuration, wrong
teams, cleanup and quarantine preservation.

Use `codesign --verify --check-notarization` with an explicit `notarized`
requirement for the standalone Go executables. An app-oriented `spctl` check
does not establish the correct behavior for every non-app code type. The
quarantined Homebrew test delivers the extracted release app through the real
cask DSL and verifies the app's and both helpers' notarization. It registers no
Service Management jobs; signing and packaged job execution
cannot certify live Service Management or endpoint-security acceptance. Apple documents these distinct checks in
[Testing a Notarised Product](https://developer.apple.com/forums/thread/130560).

Signing, notarization and Gatekeeper acceptance are distinct results. A normal
first-open confirmation can still appear for a correctly notarized download.
Apple's [trusted execution troubleshooting](https://developer.apple.com/forums/thread/706442)
recommends testing a quarantined download on a fresh Mac or restored VM,
including offline behavior where supported. Local development-Mac checks do
not establish clean-Mac or Intel runtime acceptance. Hosted Intel tests and
clean-machine installation evidence must be reported separately.

The committed guide describes the source contract. Qualification handoffs record
actual source, workflow, submission and public asset witnesses separately.
