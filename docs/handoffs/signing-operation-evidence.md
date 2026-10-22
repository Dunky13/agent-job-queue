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

## Change

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
