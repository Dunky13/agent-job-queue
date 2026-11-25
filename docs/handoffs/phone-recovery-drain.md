# Phone access recovery refuses until the queue drains

`agent-heavy install --recover disable-phone-access` could disable phone access while queue jobs were still running or cleaning up. The setup window runs that recovery directly, so it did. The automatic `--repair-and-continue` policy and the `--disable-phone-access` install path already held it back until the queue drained. This is the first step of an architecture plan whose next step moves the setup window onto `--repair-and-continue`, so Go owns the approved-repair policy alone.

## Result

- **One drain check for phone access:** `requirePhoneAccessDrained` (`internal/backend/install_managed.go`) is shared by `--recover disable-phone-access` and the `--disable-phone-access` install path. Go installs read the daemon snapshot. Legacy installs inspect and match the Pueue service before reading its tasks. Both then require legacy cleanup to be finished.
- **Missing legacy configuration:** without `pueue.yml`, the legacy check refuses as `queueMigration` does, instead of crashing or guessing the queue location.
- **Setup window:** the Swift approved-repair loop now waits while jobs drain instead of choosing phone access early, matching `installApprovedNext`. The follow-up deletes that loop.
- **Wording:** both phone issues state that disabling is allowed only after jobs and cleanup have finished. `docs/upgrade-recovery.md` says manual recovery refuses until drained and automatic repair waits.
- **Glossary:** a root `GLOSSARY.md` now holds the project's domain language. "Platform port" is left for the PR that introduces it.

## Evidence

- **Reproduction:** before the fix, the new assertion in `TestInstallerRecoveryPhoneAccessPreservesLegacyAndGoCredentials` failed for both the legacy and Go installs. An independent reviewer confirmed this on `origin/main` (a3336c2).
- **New tests:** that test also asserts the Go cleanup refusal and credential preservation for both install kinds. It also asserts the legacy cleanup refusal. `TestInstallerPhoneAccessDrainRefusesLegacyQueueWithoutConfiguration` covers the missing-configuration case. A `BootstrapTests` case covers the window waiting before it disables phone access.
- **Reviews:** independent Standards and Spec reviews ran, and their confirmed findings were fixed.
- **Suites:** the backend runtime partitions and the native suite ran through the shared queue (see the PR).

## Not changed

- Preflight still offers `disable-phone-access` while jobs drain, as it does `reset-callback`; choosing it refuses with the drain or cleanup reason.
- The refusal message is shared with migration ("Queue migration requires a drained queue…"), so it was left as it is.
- `reset-callback` and `queueMigration` keep their own drain checks. Folding them into one helper would change `reset-callback` on Go installs, which is out of scope here.
- No test injects a failing `launchctl print` for the legacy queue service; that inspection is the same one `reset-callback` uses.

## Next steps

1. Merge after hosted CI passes and the operator approves.
2. (Done by the approved-repair-owner follow-up, see `approved-repair-owner.md`.) Move the setup window's repair onto `agent-heavy install --repair-and-continue`, with a flag that holds back callback replacement, and delete the Swift loop in `native/AgentBootstrap.swift`.
