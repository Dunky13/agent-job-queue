# Unattended Apple release delivery

Implementation date: 2026-10-26. Repository: Dunky13/agent-job-queue.
Baseline: `ced25f439b10496f819a6237f5bd53942fbcb791`.
The operator authorized merge after passing checks and review, followed by real
Apple signing, same-candidate recovery, publication and Homebrew qualification.

## Project contract

| Boundary | Preserved contract |
| --- | --- |
| Build adapter | `scripts/build-installer.sh`: Go controller/gateway plus native Swift app and custom installer, built without Apple credentials |
| Architectures | Universal arm64 and x86_64 binaries, inspected in the installer and tested on native Apple Silicon and Intel hosted runners |
| Apple topology | Signed payload ZIP, Accepted diagnostic log and app ticket; signed enclosing installer ZIP and ticket; signed drag-and-drop DMG and ticket |
| Version and channel | Product `VERSION` remains unchanged. Each eligible trusted-main CI build receives the existing numbered prerelease identity `build-N-SHA` |
| Update authority | Existing repository-pinned manifests, exact checksums, Apple Developer ID team checks and trusted Homebrew tap remain authoritative |
| CI gate | `ci-required`, aggregating commit policy, lint, vulnerabilities, native ARM/Intel tests, web tests and universal installer validation |
| Release acceptance | Exact finalized artifact, signatures, tickets, Gatekeeper, copied app, packaged queue/HTTPS execution and real quarantined Homebrew installation on both architectures |
| Credentials | Only the main-restricted `apple-signing` environment has Apple secrets. Private temporary Keychains and credential cleanup remain mandatory |

No other project's pipeline, environment, credentials or stable trust policy was
changed. No signing/publication reviewer gate existed in this repository, so
none was removed. The existing automatic successful-main trigger is retained.

## Implementation

- Record and check selected Xcode 16.4 (16F6), SDK, Swift, Go, notary client and runner image before credential import. Retain build and signing toolchain receipts independently.
- Include original build number and source prefix in descriptive Apple upload names before submission; keep final consumer names compatible.
- Keep create-once REST submissions, local durable UUID-before-upload state, all stage checkpoints, origin verification and the original CI run's lock.
- Retain allowlisted small receipts, provider status and logs separately from large signed archives. Checkpoint/source/receipt retention is 90 days. Successful releases publish permanent `apple-notarization.json` linking source, submitted hashes, UUIDs, Accepted logs and final hashes.
- Observe completed distributions and scheduled reconciliation without Apple secrets. The first continuation may be immediate; later attempts have a 15-minute cooldown. Stop at attempt 48 or seven days of candidate age. Retry only a bound pending UUID, classified CloudKit connection loss, or delivery-only failure after both architecture acceptance jobs pass. Rerun the same execution's failed jobs, preserving successful work and exact artifacts.
- Fail closed on expired/missing or ambiguous custody, unknown upload outcomes, rejection, mismatched diagnostics, cleanup errors and acceptance failures. Runner loss before a checkpoint can require provider reconciliation; no receipt import or new upload is inferred from an older checkpoint.
- Create a draft, bind its whole candidate inventory, upload without replacement, and compare remote inventory, tag/source identity and every byte before exposure. Require GitHub immutable publication and repeat byte verification afterward. Resume partial drafts and verify identical published candidates read-only.
- Keep Homebrew's existing exact-build dispatch and signed tap readback as a separate final delivery gate.

## Live controls

Before source edits, source/workflows, environment settings and branch policies,
secret names, public configuration, rulesets, run inventories and releases were
captured in a local-only evidence packet. It is not committed; the chat handoff
links its actual location.

Ruleset `25872971` (`main-ci`) had an empty required-check list. Its only changed
rule now requires `ci-required` from GitHub Actions application `15368`, with
strict checks. Conditions and existing bypass actors were preserved. Repository
release immutability changed from disabled to enabled. The `apple-signing`
environment and its single `main` branch policy were read back unchanged.

## Qualification state

The complete finite local validation passed through shared queue job 2019:
source portability, formatting/vet, reachable vulnerabilities, shell/workflow lint,
Go race tests, tap helper tests, Linux tool builds, web/browser tests, native
regressions, universal installer build, private installation and mounted DMG
verification. Focused recovery, receipt, publication and toolchain failure tests
also pass. These local installer artifacts use development signatures.

Final independent review, PR/hosted CI, merged-source qualification, real
same-UUID continuation, Apple acceptance/stapling/Gatekeeper, public asset
readback and Homebrew delivery are not yet established for this implementation.
Historical build 94 is a prior release witness, not proof for the new source.

Synthetic failure tests establish source contracts only. They never establish
Apple acceptance, ticket delivery, hosted installation or publication.
