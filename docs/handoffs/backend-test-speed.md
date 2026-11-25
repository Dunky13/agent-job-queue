# Backend test speed handoff

## Result

The slowest `internal/backend` tests were slow mostly because of one fixed cost, not because of the work they do. A race-built Go binary sleeps about one second before it exits (`atexit_sleep_ms`, default 1000). Every real-daemon, installer and removal test starts several race-built children: the daemon, each `run` client, each worker and each restart. So each test paid roughly one second per child exit.

## Change

- `TestMain` (`queue_test.go`) appends `atexit_sleep_ms=0` to `GORACE` for the processes this test binary starts. The test process and every child stay race-instrumented and still report races when they find them. Only the pause at exit goes away. Children that are built by `go build -race` (the installer live fixtures) inherit it through `envMap()`.
- `fixtureUntil` polls every 10 ms instead of 40 ms. Its deadlines are unchanged.
- `hookTimeout` is a variable so the four hook-deadline tests can run with a 4 s or 5 s deadline instead of waiting 10 s. The log line now reports the configured seconds. Production still uses 10 s.
- `Cancel on a Done reading row reports no false cancel` ends its cleanup window after 3 s instead of 12 s (`queueCleanupWindow` was already a variable). The test still asserts the unconfirmed-cleanup error, that the row is unchanged and that the process lives.

No timeout, budget or worker count was raised. No assertion was removed.

## Evidence

Measured locally on an Apple Silicon host with `go test -race -c` binaries run with `-test.v`, base (`origin/main`) against this branch, one after the other through the shared queue. The hosted Intel runner is slower in absolute terms, so expect larger savings there.

| Test | Before (s) | After (s) |
| --- | --- | --- |
| TestRealDaemonJobRunsPastUnreadableRowAtLimitOne | 31.5 | 8.4 |
| TestCorruptedRowWithLiveProcessIsNotSettledAway | 15.9 | 7.0 |
| TestRealDaemonResourcePressureRecoveryAndShadowMode | 15.0 | 11.9 |
| TestRealDaemonPATHDefaultsRelativeEntriesAndErrors | 13.3 | 6.3 |
| TestHookBackgroundChildCannotOutliveTimeout | 10.0 | 4.0 |
| TestHookDeadlineSurvivesSlowGroupInspection | 10.1 | 4.1 |
| TestHookRetainsGroupLeaderUntilFinalSignal | 10.0 | 5.0 |
| TestHookTimeoutTerminatesProcessGroup | 10.0 | 4.0 |
| TestManagedRemovalOfRunningAppQuitsItOnceDrained | 8.5 | 5.7 |
| TestManagedRemovalRefusalAfterTheQuitSaysToReopen | 8.5 | 5.6 |
| TestInstallerAutomaticSetupStopsForARunningApp | 8.1 | 6.0 |
| TestRealDaemonConcurrentSubmittersRemainExclusive | 8.0 | 4.7 |
| TestManagedRemovalRefusesUnownedLockHolder | 7.8 | 6.0 |
| TestInstallerNewerCopyBesideRunningRecordedAppStops | 7.6 | 5.2 |
| TestManagedRemovalOfRunningAppRefusesActiveJobs | 7.3 | 4.5 |
| TestManagedRemovalRefusedQuitLeavesARunningAppComplete | 6.5 | 4.5 |
| TestRealDaemonUndrainWithoutADrainLeavesAPauseAlone | 6.3 | 0.1 |
| TestManagedRemovalHeldLifetimeLockNamesIt | 6.2 | 3.4 |
| TestManagedSetupAndPreflightNameARunningApp | 5.9 | 3.9 |

Whole package, sum of top-level test times, 715 tests, every test passing in both runs: 840.6 s before, 656.3 s after. That run predates the 10 ms poll change. The sum includes `TestInstallerManagedLiveResumeRetainsRunningAttempts` (161 s before, 124 s after), which the hosted core shards do not run. Without it the sum went from 679 s to 532 s.

## Not changed, and why

- `TestProfileBackfillSkipsAFamilyWhenTheRegistryIsFull` (about 10 s) and `TestProfileCollectionLargeFamilyQueryHasNoHostParameterLimit` (about 8 s) spend their time in SQLite over 8192 and 32768 families, which is the product work under test.
- `TestInstallerStandardCleanupCallbackSpellings` (about 17 s) runs 52 full installer fixtures at about 0.33 s each.
- `TestInstallerManagedLiveResumeRetainsRunningAttempts` runs ten real installs, about 9 to 15 s each, and most of that is subprocess time.
- `queue_corrupted_test.go` keeps its two one-second sleeps. Each proves that a row stays `running` while its process ignores SIGINT, and nothing event-driven can prove an absence, so shortening them would weaken the assertion.
- `lanes_test.go` and `queue_worker_gone_test.go` keep their 300 ms sleeps for the same reason.
- No `t.Parallel()` was added. The fixtures share `t.Setenv`, process-wide variables such as `queueCleanupWindow` and `hookTimeout`, and `HOME`, so parallel runs would risk flakiness. The package still has no parallel tests.

## Next steps

- Compare hosted Intel core-1 and core-2 times against run 45655515344 once this merges.
- If the installer shard is the next bottleneck, share one built bundle across the ten live-resume scenarios.
- Convert the remaining table tests that start one daemon per row to one daemon per table, where the rows do not depend on a fresh daemon.
