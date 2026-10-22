# Production signing preflight follow-up

Observed on 2026-10-22. This follow-up prepares a narrow change after the first
production signing attempt. It requires a new human merge approval; the prior
approval covered tap PR 4 and source PR 15, which are already merged.

## Proven production outcome

Source PR 15 merged as `85df85392aef69a5b089e57a2209b5af7be38658` after tap PR 4
merged as `c64c0a3de8d7190afd74dc902830cadb3772e6c9`. Both merge signatures and
author-matching DCO trailers were verified, and their trees matched the reviewed
heads.

Main [run 39739825063](https://github.com/Dunky13/agent-job-queue/actions/runs/39739825063),
build 48, passed all eight validation jobs. The protected signing job failed
in its first payload preparation at 07:32 UTC with the codesign message
`The specified item could not be found in the keychain.` It had completed
credential import and reached the signing child. Submission, receipt and wait
steps were skipped. The only uploaded artifact was the validated ad hoc
installer. No Apple upload, notarized production artifact, publication or
Homebrew delivery occurred.

No failed-job rerun was attempted: this run has no surviving checkpoint and
the existing restore guard correctly refuses a fresh start on a later attempt.
The scheduled observer was disabled while this follow-up is prepared.

## Diagnosis and limits

The same existing credentials were imported into a fresh private Keychain
locally. Both the configured certificate name and its SHA-1 signed and verified
the actual universal controller successfully. The exact credential wrapper and
resumable payload preparation also passed. Neither test uploaded to Apple or
changed the host's default Keychain or search list.

Source inspection found no lost identity variable or missing quoted Keychain
argument. These checks do not reproduce the hosted failure or establish its
root cause. In particular, neither a name-only lookup problem nor a missing
issuer is proven. Apple's [code-signing certificate guidance](https://developer.apple.com/documentation/technotes/tn3161-inside-code-signing-certificates)
requires a usable identity and trust chain; GitHub's
[image setup](https://github.com/actions/runner-images/blob/7d634bd45ab574eeb0718d83954a4e0e8007cebe/images/macos/scripts/build/configure-machine.sh)
intends to install Developer ID G2. Image source is not a readback of the failed
runner's certificate inventory.

## Changes

The credential wrapper now queries the full and valid code-signing identity
inventories in its own temporary Keychain before invoking the child. Selection
requires exactly the configured full name or case-insensitive fingerprint,
validity, Developer ID Application type and the expected team. The child receives
the resolved uppercase SHA-1. An absent, invalid, ambiguous, wrong-team or
inconsistent selection fails closed without choosing another certificate.

Diagnostics print fixed classifications and counts, plus the selected public
fingerprint and team only for a ready identity. Raw inventory names, command
errors, passwords and private keys are not printed. A failed child refreshes
the inventory classification and keeps its original exit code. Host search
lists, default Keychains, trust settings and signing requirements are unchanged.

The workflow also retains an initial `s00` snapshot after state verification and
before credential setup. The snapshot permits recovery from a first import or
preparation failure without losing the reviewed installer. If a stage already
has prepared bytes without a durable ID, its existing ambiguity guard still
refuses resubmission. Missing state, expired submission checkpoints, corruption,
provenance mismatch and rejected submissions remain failures. There is no
fallback from a known expired submission checkpoint to the initial snapshot.

This closes the observed pre-sign recovery gap and makes the next hosted
identity failure distinguishable. It does not claim that the original hosted
signing error is already fixed. The next approved main run must provide that
evidence before production publication can be certified. The failed build's
workflow cannot acquire these changes through a rerun of its old commit.

## Validation contract

Tests first reproduced the missing initial-checkpoint behavior and the lack of
identity preflight at the actual wrapper seam. Coverage includes exact names,
both fingerprint cases, missing/invalid/ambiguous/wrong-team/type identities,
malformed or failed inventory queries, no alternate valid-certificate fallback,
safe diagnostics, child exit-code preservation and all existing credential
custody, process-group cleanup and cancellation checks.

Checkpoint integration restores the initial state into a new attempt and makes
exactly its first submission using private tools. Existing tests still reject
unknown upload outcomes, prior prepared state without an ID and missing state.
Workflow tests require the initial immutable upload before any signing stage;
an expired submission checkpoint cannot fall back to `s00`.

Local mocked recovery tests, real credential-import/signing checks, exact-head
hosted CI, independent review and post-merge production proof must be reported
separately. No operator installation is authorized. The standing
[signing guide](../apple-signing.md) contains the recovery commands and trust
boundaries. A standalone [HTML version](production-signing-preflight.html)
accompanies this handoff.

## Completed local checks

The queued lint batch passed, and the complete tooling race suite passed in
89.150 seconds. All existing Apple-signing cleanup and cancellation checks
remain enabled. The revised wrapper's real private-Keychain preflight reported
exactly one matching valid identity; universal controller signing and the exact
resumable payload preparation passed. Temporary credentials were removed and
the host's default Keychain and search list were unchanged. No Apple upload
was made.

Independent Sol 6.1 high review found no blocking source-level findings in the
wrapper, initial checkpoint, regression coverage or documentation. Bash 3.2
syntax and ShellCheck passed. Cross-provider review was unavailable and skipped.
Exact-head hosted PR validation remains separate from the protected main-only
signing path; this local evidence does not certify a hosted repair.
