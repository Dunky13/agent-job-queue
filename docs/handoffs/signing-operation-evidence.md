# Signing operation evidence follow-up

Observed on 2026-10-22 after approved PR 16 merged. This change improves
diagnosis of the remaining hosted signing failure. It does not claim a
demonstrated production signing repair and requires a new human merge approval.

## Build 50 outcome

PR 16 merged as `9af9b3213d846afb13a9cde53150d59e38616e7c`; its signature,
author-matching DCO and reviewed-tree equality were verified. Main
[run 39746557811](https://github.com/Dunky13/agent-job-queue/actions/runs/39746557811),
build 50, passed all eight validation checks, including ARM and Intel tests.

The protected signing [job 119719458277](https://github.com/Dunky13/agent-job-queue/actions/runs/39746557811/job/119719458277)
retained its initial checkpoint, then failed during the payload prepare child.
Its preflight and child-failure diagnostics both reported exactly one matching
valid identity, the expected public certificate fingerprint and team. The
intervening error was `The specified item could not be found in the keychain.`
The available log does not identify the exact child operation that emitted it.

The initial immutable artifact is `Apple-notarization-39746557811-a1-s00`, ID
`12412505175`, outer SHA-256
`fa53fdac94bb4432d114f7975fc07356101775d37bd579c0ac1518faff0df19b`.
The downloaded outer bytes and enclosed checkpoint checksum were verified.
The inner state metadata matches repository, commit, team, version 0.1.0,
run 39746557811 and build 50. Its original installer archive SHA-256 is
`df4a078e93231a12a6f1747d01203a53946fff8072aec7be5cb60b911679f7ab`
and was independently recomputed. The state has seven entries, no symlinks,
empty stage/archive/receipt/completed directories and no active work. No
downloaded code was executed.

Submission and later signing stages were skipped. There is no Apple
submission ID to query, no pending notarization, and no notarized production
publication or Homebrew delivery. A blind rerun was not attempted. The existing
production observer was disabled during investigation.

## Evidence and uncertainty

Source inspection passes the selected fingerprint and private Keychain to
`codesign`. The matching inventory staying valid does not prove successful
private-key access. Earlier local Developer ID checks also had the same
certificate in the default identity search, so they did not prove independence
from that existing identity. The local machine runs macOS 27.0.1; the failed
hosted runner used macOS 15.7.9, image 20260907.0337.1.

A unique synthetic identity in an unlisted private Keychain signed
successfully locally without trust or search-list changes. Thus missing
default-list visibility alone did not reproduce the failure on the local OS.
Installed `codesign` documentation says certificate-chain lookup can consult
the normal search list even with an explicit Keychain. This is a boundary to
investigate, not evidence that a missing intermediate caused this failure.
No trust, credentials, default Keychain or search list was changed to get green.

## Original diagnostic change at 7e03951

Fixed operation markers distinguish credential configuration, installer
restoration, each known helper/app/installer signing step and its verification,
and DMG signing/verification. They emit begin and success/failure with the
original status; they do not emit command arguments, paths or credential
selectors. Reusing prepared bytes does not repeat preparation or markers.

Wrapped shell functions explicitly return failing command statuses so Bash's
conditional-call behavior cannot hide configuration or restoration failures.
Signing, verification requirements, credential cleanup and upload ambiguity
guards remain enabled. No search-list or trust workaround is added.

A native macOS test generates a unique short-lived synthetic signing identity
in an owned disposable Keychain. It confirms that the identity is absent from
default lookup, signs a copied system executable with the explicit Keychain
and fingerprint, verifies all architecture signatures, and verifies that the
embedded certificate has that exact fingerprint. It checks unchanged host
Keychain settings and deletes its owned Keychain. It requires no production
credentials, trust changes, Apple upload or target execution. This gives both
hosted macOS architectures a test independent of the operator's identities;
it cannot certify a Developer ID trust chain or notarization.

Regression tests at the real prepare seam inject failures before signing,
during restoration, for each payload signing target, and during verification.
They require the correct marker and original status, no next operation, no
prepared archive or stage record, and no submission. Ordered success and
prepared-state reuse are covered separately.

## Continuation boundary

The shared queued lint batch passed. The complete tooling race suite passed
in 98.449 seconds, including the native synthetic identity probe. New prepare
tests first failed because operation markers were missing, then passed after
implementation. Configuration, canonicalization, credential containment,
restore, each payload signing target, verification, success order and prepared
reuse were covered. Existing lifecycle, packaging and ad hoc checks passed.
Independent Sol 6.1 high review found no blocking source findings. Cross-provider
review was unavailable and skipped. Standalone HTML content and asset checks
passed.

Inspect exact-head hosted ARM and Intel results before merge. A synthetic hosted
failure must be diagnosed before asking for merge. If the probe passes,
operation markers still require a new approved main run to locate the remaining
production-only error. Do not report the hosted cause or repair as proven.

The old run's workflow cannot acquire these changes through a rerun. Preserve
its initial checkpoint and require human approval for this new PR. Any later
production run still requires full Developer ID/notarization, fresh ARM/Intel
runtime, publication and exact-build Homebrew delivery evidence. No operator
installation is authorized. A [standalone HTML version](signing-operation-evidence.html)
accompanies this handoff.

## Hosted reproduction and approved CI boundary

PR 17 head `7e03951a72e2e88f97226eb0db12a5cafcd70495` ran the unique-identity
probe on macOS 15.7.9 in [run 39750558419](https://github.com/Dunky13/agent-job-queue/actions/runs/39750558419).
Both [ARM job 119729779635](https://github.com/Dunky13/agent-job-queue/actions/runs/39750558419/job/119729779635)
and [Intel job 119729779654](https://github.com/Dunky13/agent-job-queue/actions/runs/39750558419/job/119729779654)
failed at the actual explicit-Keychain `codesign` invocation with
`The specified item could not be found in the keychain.` Creation, import,
authorization, explicit identity lookup and absence from default lookup had
already passed. No cleanup or host-setting comparison failure was reported.

This is a credential-free hosted reproduction of the error. It narrows the
failure to native signing under the unlisted-identity condition, but does not
yet prove which signing lookup or synthetic-certificate policy caused it.
Search-list visibility is the leading hypothesis; certificate policy and
OS-specific key access remain alternatives.

GitHub's [Apple signing guidance](https://docs.github.com/en/actions/how-tos/deploy/deploy-to-third-party-platforms/sign-xcode-applications)
registers the signing Keychain in the runner search list. Its
[variables reference](https://docs.github.com/en/actions/reference/workflows-and-actions/variables)
documents `RUNNER_ENVIRONMENT=github-hosted`. The follow-up uses
those platform boundaries to test registration while keeping the certificate,
signing arguments and trust policy unchanged.

The wrapper appends only the owned temporary Keychain after preserving the
original ordered list, restores that list on success/failure/cancellation,
then deletes owned credentials. It does not change default Keychains, trust
settings, local operator search lists or self-hosted runners. The native probe
uses the same registration helper only on disposable GitHub-hosted runners.
Local validation uses mocked setters and the original unlisted native probe.

The native probe also owns its signing process group. Cancellation sends TERM
and bounds the wait and output pipes, then kills any remaining owned
descendants before Keychain cleanup. A hard timeout can prevent Bash's EXIT
trap from running; Go cleanup then deletes its owned Keychain, which removes
that appended entry, and checks the exact final search list and default
Keychain. Mock-only regressions cover normal completion, cancellation with a
TERM-ignoring descendant holding output pipes, and a TERM-ignoring parent.
The production credential wrapper already stops its child group before
restoring settings and deleting credentials.

Local validation passed: the complete tooling race suite took
114.751 seconds, followed by final queued lint and targeted native/process
race checks taking 11.972 seconds after the process-lifecycle correction.
Mocked registration coverage includes partial writes, readback failures,
conflicting additions, cancellation, empty/space-containing lists and reuse.
Independent Sol 6.1 high review found no remaining blocking source findings.
Cross-provider review remained unavailable and skipped. These results do not
establish that real macOS 15 registration repairs signing.

The observer instruction initially excluded `credential/search-list/trust
overrides`. After reviewing the local patch, the operator approved the scoped
exception with “As recommended”: activate registration only on disposable
GitHub-hosted runners and validate updated PR 17. No operator-Mac or self-hosted
settings changes, trust overrides, production uploads or merge are authorized
by that exception. Exact-head hosted ARM and Intel results are still required.
Do not weaken the test or bypass the separate human merge gate after validation.

## Registration readback repair

At approved registration head `621a230917a08e571e33b09f2c0eeb395c443854`,
[Intel job 119748005067](https://github.com/Dunky13/agent-job-queue/actions/runs/39755991806/job/119748005067)
failed before signing with `phase=register classification=readback-mismatch`
and `phase=restore classification=conflict`. It did not report a final cleanup
or host-setting comparison failure. The failed runner's paths were withheld,
so its exact mismatch is not established by that log.

Apple's published implementation
[canonicalizes database paths](https://github.com/apple-oss-distributions/Security/blob/db15acbe6a7f257a859ad9a3bb86097bfe0679d9/OSX/libsecurity_cdsa_utilities/lib/cssmdbname.cpp#L55-L100)
and [returns the canonical database name](https://github.com/apple-oss-distributions/Security/blob/db15acbe6a7f257a859ad9a3bb86097bfe0679d9/OSX/libsecurity_cdsa_client/lib/dlclient.cpp#L253-L267).
This supports a physical-path alias as a plausible mechanism; it does not
prove the failed runner's incident cause or exact source/binary correspondence.

A regression uses a real symlinked temporary parent and a mocked settings
tool returning the physical owned path. Before the repair, it reproduces the
same readback-mismatch and restore-conflict sequence. After the repair, it
registers and restores successfully, preserving an existing aliased entry
byte-for-byte and in its original position. Another regression rejects a
symlinked final Keychain file before any settings tool is invoked.

The helper resolves only the newly owned file's existing parent using physical
directory traversal, retains the filename, and validates it again. Original
captured paths are never normalized. Ordered exact readback and conflicting
change refusal remain mandatory. The fixed `owned-path-normalized` diagnostic
exposes no paths. Wrapper tests model the physical owned path separately from
the caller's path. No operator settings, trust or credentials were used.

The focused wrapper/helper suite passed in 33.493 seconds. Queued lint and the
complete tooling race suite passed, with race tests taking 121.776 seconds.
Standalone HTML and diff checks passed. Independent Sol 6.1 high source review
found no blocking findings in the narrow patch; cross-provider review remained
unavailable and skipped. Exact-head hosted ARM and Intel results are still
required before separate merge approval; production signing and delivery
remain unverified.
