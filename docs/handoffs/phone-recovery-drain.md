# Phone access recovery waits for a drained queue

`agent-heavy install --recover disable-phone-access` could disable phone access while queue jobs were still running or cleaning up. The setup window runs that recovery directly, so it did. The automatic `--repair-and-continue` policy and the `--disable-phone-access` install path already held it back until the queue drained. This is PR 0 of the architecture plan recorded in the queue-popover-polish session; PR 1 gives approved repair a single owner.

## Result

- **One drain check for phone access:** `requirePhoneAccessDrained` (`internal/backend/install_managed.go`) is shared by `--recover disable-phone-access` and the `--disable-phone-access` install path. Go installs read the daemon snapshot. Legacy installs inspect and match the Pueue service before reading its tasks. Both then require legacy cleanup to be finished.
- **Missing legacy configuration:** `installYAMLValue` now accepts a nil node, so a legacy check without `pueue.yml` reads the default Pueue directory instead of crashing.
- **Setup window:** the Swift approved-repair loop now waits while jobs drain instead of choosing phone access early, matching `installApprovedNext`. PR 1 deletes that loop.
- **Wording:** both phone issues state that disabling waits for jobs and cleanup. `docs/upgrade-recovery.md` says manual recovery refuses until drained and automatic repair waits.
- **Glossary:** a root `GLOSSARY.md` now holds the project's domain language. "Platform port" is left for the PR that introduces it.

## Evidence

- **Reproduction:** before the fix, the new assertion in `TestInstallerRecoveryPhoneAccessPreservesLegacyAndGoCredentials` failed for both the legacy and Go installs. An independent reviewer confirmed this on `origin/main` (a3336c2).
- **New tests:** that test also asserts the Go cleanup refusal and credential preservation for both install kinds. `TestInstallerPhoneAccessDrainWithoutLegacyConfigurationReadsDefaultQueue` covers the missing-configuration case. A `BootstrapTests` case covers the window waiting before it disables phone access.
- **Reviews:** independent Standards and Spec reviews ran, and their confirmed findings were fixed.
- **Suites:** the backend runtime partitions and the native suite ran through the shared queue (see the PR).

## Not changed

- Preflight still offers `disable-phone-access` while jobs drain, as it does `reset-callback`. The issue text now says it waits.
- The refusal message is shared with migration ("Queue migration requires a drained queue…"), so it was left as it is.
- `reset-callback` and `queueMigration` keep their own drain checks. Folding them into one helper would change `reset-callback` on Go installs, which is out of scope here.
