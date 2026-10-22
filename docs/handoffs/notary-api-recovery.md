# Notary API recovery repair

## Problem and scope

Production [run 39761705194](https://github.com/Dunky13/agent-job-queue/actions/runs/39761705194),
build 54, attempt 1, targets commit
`ab2e066ae6886a0bf9e0aa99fef48ffa64f78cc9`. All eight validation jobs and
Developer ID payload signing succeeded. Native `notarytool submit` then exited
with `Trace/BPT trap: 5` before returning a submission ID. No notarized runtime
acceptance, production publication or Homebrew delivery followed.

The verified latest immutable checkpoint retains the signed payload ZIP with
SHA-256 `84b53513edee9aa3cd9570456fd8417b7c2fb3592268f8d75bd1f23c85f96601`.
Its stage is `uploading`, with an empty ID and no acceptance receipt. Authorized
read-only Apple history and nearby log inspection found no matching archive.
That bounded absence does not prove Apple received nothing. Build 54 stays
blocked; this repair does not rewrite its checkpoint or rerun its upload.

## Evidence and implementation choice

Network-denied local probes of valid and malformed synthetic keys did not
reproduce the native crash. The exact preserved ZIP and a harmless comparison
ZIP also failed normally at the proven network-denial boundary. This does not
establish complete archive preflight or the hosted crash's root cause. There
is no verified Xcode-version or S3-acceleration workaround for this failure.

The repair replaces the durable CI raw-key client path with Apple's supported
[Notary API](https://developer.apple.com/documentation/notaryapi/submitting-software-for-notarization-over-the-web).
Apple returns a submission ID and temporary S3 credentials before archive
upload. The client persists that ID first, removing the native submission
process from this CI path and reducing the ambiguity window. Local
Keychain-profile and one-shot packaging commands keep their existing behavior.

JWT authentication uses the existing `go-jose` dependency; upload uses the
official AWS Go SDK. Production endpoints are fixed. Provider errors are
reported as safe classifications rather than raw responses, tokens, signed
URLs or private paths. API-key authentication is tested before signing with a
read-only history request. The client is compiled before credentials are loaded.

## Recovery contract

1. Validate the exact six-field stage record, attempt and archive hash. Reject
   a symlinked record or archive and serialize submit operations.
2. Authenticate before changing state. Write and sync `uploading` before one
   create request. Do not retry an uncertain create response.
3. Persist a valid Apple ID as `submitted` before parsing upload details or
   sending archive bytes. A persistence failure prevents upload. If an ID is
   known, also print its public JSON on failure for operator evidence.
4. Upload the immutable archive once using the returned temporary credentials.
   A failed or interrupted upload retains its ID and never creates another
   submission automatically.
5. Resume by querying the recorded submission. Acceptance still requires the
   exact ID and archive SHA-256 in Apple's log, then stapling and policy checks.
   Payload, installer and DMG remain ordered; fresh ARM and Intel distribution
   checks still precede publication and Homebrew delivery.
6. A legacy `uploading` stage with an empty ID, including build 54, remains a
   manual-reconciliation failure. No checkpoint fallback or automatic receipt
   import is added.

The synced local ID survives a client failure and is retained by the following
immutable CI snapshot. It cannot guarantee recovery if the entire runner is
lost before that snapshot uploads. Temporary S3 credentials stay private and
are not checkpointed. A known submission whose archive upload did not complete
may therefore need operator resolution; querying it is safe, but does not
prove that Apple has received or processed its bytes.

## Validation and remaining boundary

The native-path regression first failed against a private copy of the original
script. The expanded shell tests pass for all three stages, public recovery ID
output after failure, guarded unknown outcomes, missing or unusable clients,
and mismatched wait IDs and acceptance logs. The complete focused resume suite
passed in 58.043 seconds. A regression using the actual prebuilt CLI reached
exactly two local CONNECT-rejecting proxy requests for wait and log, with no
Apple request. It verifies ordinary macOS temporary key and output paths.

The Go client race suite passed in 2.045 seconds. It covers JWT signature and
claims, malformed keys, stage and hash validation, unknown create responses,
single-create behavior, synced ID ordering, failed writes and uploads, the
official S3 client's auth and checksum, redirects, bounded responses, timeouts,
rejection, cancellation and private output paths. Broader tooling validation
passed in 140.645 seconds, alongside lint. Independent review then identified
two additional defects: an HTTP/2 transport could replay a create POST, and
bare-second timeouts accepted by the native engine were rejected by Go's
duration parser. Both were repaired before PR submission.

Real TLS/ALPN HTTP/2 frame regressions reproduced two complete create POSTs
and one upload on the original client for `PROTOCOL_ERROR`, `REFUSED_STREAM`
and GOAWAY. Clearing the create request's `GetBody` now yields one POST, no
upload, an unknown-outcome record and refusal on a later submit. HTTP/2 stays
enabled, and a separate safe GET retry still succeeds. These focused HTTP/2
tests passed in 0.299 seconds, and the full serial API suite in 0.560 seconds.

The timeout regression reproduced the actual CLI failure before the fix:
`NOTARY_TIMEOUT=300` reached only one local proxy connection because wait
rejected its argument. Normalization now passes the real CLI flow with two
local connections. Explicit units, the 24-hour boundary and oversized integers
are covered; unsupported budgets fail before signing or authentication. The
focused timeout and actual-CLI tests passed in 9.523 seconds. Final queued lint,
Go API race tests (2.092 seconds), complete tooling race tests (150.520 seconds),
and Linux/macOS release-tool builds passed in shared queue job 834. The second
independent Sol 6.1 high review verified both repairs and found no remaining
actionable defects. Cross-provider review was unavailable and skipped; it is
not counted as a clean review. Hosted PR checks remain a separate gate.

The expanded vulnerability scan found no reachable vulnerabilities in the app
helpers or release tool. It also reported one finding in an imported package
and one in a required module whose vulnerable code was not called. Linux and
macOS release-tool builds passed. One live read-only preflight authenticated
successfully against Apple using the existing protected local team key. Its
public key and issuer selectors match CI; that does not verify the contents of
the hosted private-key secret. The temporary plaintext key was removed and
exact before/after Keychain search-list and default settings matched. No
submission was created and no archive was uploaded.

Local unit and fixture results do not establish live S3 upload, Apple
acceptance, hosted signing or production delivery. A new repair PR retains the
human merge gate. After an approved merge, observation must target the new
exact main run, head and build; build 54 cannot acquire these changes through
a rerun. The existing production observer remains disabled at its unresolved
submission boundary.

The complete signing guide is [Markdown](../apple-signing.md) and
[standalone HTML](../apple-signing.html). This handoff also has a
[standalone HTML version](notary-api-recovery.html).
