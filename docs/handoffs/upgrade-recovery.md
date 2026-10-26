# Guided upgrade recovery handoff

The requested follow-up replaces repeated generic retries with a read-only preflight summary and specific recovery choices. It follows PR #26's phone-access recovery and covers the other installer blockers identified in the source assessment.

## Resulting flow

1. The native app validates its embedded bundle and installation target, then asks the bundled controller for a schema-versioned preflight report.
2. The window displays independent blockers together, including legacy jobs and cleanup, coordination locks, stopped or uncertain services, phone access, customized configuration, private-path safety, and package/location problems.
3. The operator can inspect jobs and files, wait with a stop control, cancel a selected job after confirmation, run verified cleanup, start a verified stopped Go service, or explicitly disable phone access. Custom callback replacement requires review, a separate confirmation, a matching configuration revision, and a private backup of the original.
4. Every recovery revalidates the current backend state and ownership. A fresh clean preflight permits installation; the installer repeats authoritative checks and the app verifies installed-state readback before normal queue startup.

Normal Go upgrades preserve active jobs. A loaded service or uncertain attempt is not blindly restarted. Missing configuration, arbitrary custom service definitions, unsafe ownership, and unidentified processes remain guided operator repairs. App movement uses Finder guidance; downloads open the official releases page. No repair changes another installation, bypasses signatures, deletes leases or locks, or executes unvalidated old Python source.

See [the operator and controller contract](../upgrade-recovery.md) for exact command forms.

## Evidence and gates

Focused backend installer/preflight/recovery tests pass, including read-only filesystem witnesses, multiple blockers, selected-job actions, stale configuration rejection, safe service ownership, credentials/CA preservation, and original install/upgrade cases. Native headless tests and all three real AppKit bootstrap fixture scenarios pass. Transport tests use real private controller processes and show that cancellation stops preflight reads while retaining accepted operations. The shared JSON fixture checks the Go/Swift boundary; Unicode job labels have explicit coverage. Screenshots of the production setup summary were inspected at its existing window width.

The final independent standards and spec review is CLEAN on the corrected source. Five additional regressions reproduced and locked down review findings: state-preparing readiness checks, executable-parent symlinks, non-executable backend repair, missing legacy configuration, and ineligible custom-callback job actions. Cleanup eligibility also excludes active or missing tasks, and callback replacement parses the exact revision-validated bytes. The complete local validation batch is running and remains required before delivery. Hosted Apple Silicon/Intel CI, universal packaging, protected merge, notarized publication, and Homebrew readback remain separate delivery gates. Test fixtures never change the installed operator queue or phone service.

## Delivery context

PR #26 merged as `30b046f43edf5f5121367ae49b0f131b60e37c84` with passing checks, a GitHub-verified signature, and matching DCO certification. Its original main build passed, but Apple ticket stapling encountered a CloudKit connection failure. Distribution recovery reuses the retained exact build and existing accepted submission instead of submitting a duplicate. Publication must not be claimed until the required ticket, acceptance, and delivery checks pass.
