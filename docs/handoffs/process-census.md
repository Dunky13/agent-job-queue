# Process census

## Result

`internal/census` is now the only parser of process listings that answer an ownership or liveness question. It takes text, imports nothing from this repository, and is used by `internal/backend` and `internal/remote`. Callers keep their own execution mechanism and timeout: `identityCommand`, monitor's context and the remote injected command.

There is one column list, `pid=,pgid=,uid=,stat=,lstart=`, replacing four. It still reads no arguments, environment or paths. `census.Parse` rejects, for every caller:

- a row with too few columns, a non-numeric or out-of-range PID, PGID or UID, or a birth that is not a complete C-locale `lstart`, which also catches a row cut off inside its last column;
- a repeated PID;
- a listing that does not show its observer running in its own process group. A zombie observer row counts as not running.

Negative UIDs stay valid because macOS can print `nobody` as -2. `Table.Identify` answers the recorded PID and birth question as Absent, Reused, Defunct or Alive. `IsZombie` and `NormalizeBirth` hold the other two shared rules.

## Tightening by caller

| Reader | Added |
| --- | --- |
| `ProcessGroupMembers` | observer, duplicate, zombie observer, full `lstart` |
| `processGroupStates` | duplicate, PID and PGID range, full `lstart` |
| `resourceProcessListing` | observer group, zombie observer, full `lstart` |
| `queueHandoverProcessSnapshot` | full `lstart` |
| `leaseProcessSnapshot` | observer group, zombie observer, full `lstart` |
| remote `processBirth` | everything, see below |

Existing error text at each site is unchanged. `census.Wording` maps rejections back to it. New rejection kinds at the two `ownership.go` sites use new messages with the same `; shared slot retained` suffix. A search found no code or test matching on any of these strings.

## Fail-closed trace

`ProcessGroupMembers` callers, none of which needs an exception because each lists itself:

- `execution.go` (two sites) and `queue_daemon.go` worker start: return 125.
- `queue_handover.go` `queueHandoverIdentity`: returns the error.
- `VerifyGroupIdentity`: returns the error, retaining the slot without signalling.
- `hooks.go`: an error counts as a background process, so the group is killed at the hook deadline. A rejected census can end a hook group at its deadline, which is the existing behavior for an inspection failure.

`processGroupStates` is reached through `GroupExists`, only after a permission-denied probe. All five callers return the error or retain the lock.

## Decisions to review

- **Remote `processBirth` changed behavior.** It now runs `ps -p PID,SELF` so the observer is in the listing. Before, `ps` exit status was ignored and empty output meant "exited", so a `ps` that failed with no output for any other reason was indistinguishable from an exited process, and `reconcileChildren` would remove that record. A failed listing is now an error. This is the one reader with a narrower listing than the full table, kept narrow because it polls every 50 ms.
- **Remote keeps `ps` spacing.** Remote records hold double-spaced births, including pre-migration Python ones, so `Process.RawBirth` preserves them while backend uses the normalized `Birth`. `TestProcessBirthMatchesRecordsFromTheSinglePidListing` compares against the old listing with real `ps`.
- **`pid,command` install sites are not shared.** They substring-match command lines. Sharing would add a `command` column and lose the no-arguments property, and their parsing is a single `Fields` call.
- **Lease owner births are normalized before comparison.** `leaseProcesses.alive` compared the recorded birth exactly. `leaseOwner` has always stored the normalized form, so stored records are unaffected.
- **Boot identity is consolidated** into `census.ParseBootIdentity`, keeping each site's reader and message. Backend no longer accepts braced or URN UUID forms, which `sysctl` does not print.
- **One existing test fixture changed.** `TestResourceCensusRejectsTruncationDuplicatesAndMalformedRows` used rows such as `999 900 observer birth`, which no longer parse. Its five cases and assertions remain, written with real rows, and the expected birth is now a real normalized `lstart`. No assertion was removed.

## Evidence

Run on the uncommitted candidate that was then committed unchanged, apart from this document.

| Check | Result |
| --- | --- |
| `go test ./internal/census` | pass |
| `TestRealDaemon`, group liveness, hook and lease owner tests, direct | pass |
| `go test -race ./internal/remote`, direct | pass, 176.8s |
| Queue job 2745, `validate.sh` | exit 127, no `node_modules` in the new worktree; fixed with `npm ci` |
| Queue job 2751, `validate.sh` | exit 1. Lint and govulncheck passed. Backend and tooling hit the 12-minute budget, with 7s and 2s tests running at the alarm. Three remote tests failed |
| Queue job 2772, CI partitions and remaining steps | exit 1. Passed: `core-1`, `installer`, `tooling-1`, `tooling-2`, Go and Linux builds, web, browser, native, app build, installer, DMG. Failed: `core-2`, `https`, isolated remote tests |

Hosted CI is the next signal. `scripts/validate.sh` has not passed as a whole on this candidate.

## Failures seen

**Remote tests, queue jobs only.** `TestAuthAdmissionRealCaddyCacheAndDistinctGuesses`, `TestRemoteCaddyLifecycleAcceptance` and `TestRemoteServeEndsWhenItsLifelineCloses` exceed their 30s and 35s limits. The same three fail in queue jobs 2765, 2766 and 2756 (the last had two of them) on branches without `internal/census`, and the whole package passes directly on this branch. Queue jobs run at scheduler priority 4, against 31 for an interactive shell, but the cause is not established: `taskpolicy -b` on main did not reproduce the failure for the two tests tried.

**`TestRealDaemonCancellationRemovesDescendants`, intermittent.** `Cancel` fails with "Process group ownership cannot be verified". A capture at the failure showed a correct listing: the group held only the `sleep` descendant, and both recorded members had already exited. `VerifyGroupIdentity` cannot verify that state. The failure reproduces on unmodified main.

| Tree, under `taskpolicy -b` with `-race` | Failures |
| --- | --- |
| Main at 7b4b27c | 1 of 390 |
| This branch | 4 of 260 |

The branch rate is higher. The runs were not all concurrent and the sample is small, so this neither shows a regression nor rules out that the census changed the timing. Direct runs without throttling passed 15 of 15 on both trees. A delay added to `Cancel` on main did not widen the window, so the exact interleaving is not known. The verification logic is unchanged. A fix belongs with the job transition work and is out of scope here.

## Remaining work

1. Check hosted CI on the draft PR, especially both core shards and the HTTPS partition.
2. Review independently, including whether the cancellation rate above needs measuring under identical concurrent load before merge.
3. Decide who owns the cancellation race: a group whose recorded members have all exited while an unrecorded descendant remains.
4. Find out why the remote tests fail only inside queue jobs.

## Next steps

```sh
go test -p 2 -parallel 2 ./internal/census
go test -p 2 -parallel 2 ./internal/backend -run 'TestRealDaemon|Liveness|LeaseOwner'
go test -p 2 -parallel 2 -race ./internal/remote
```

## Review follow-up

Independent review of `b0553ef` found no blocking defect. Its findings are fixed:

- `processBirth` now treats any nonzero `ps` exit as a failed listing. With the observer listed, `ps` exits 0 whether or not the target exists, so a nonzero exit with a usable row can no longer read as an exited process.
- A record holding a PID macOS could never assign reads as exited, because macOS `ps -p` refuses the whole command above 99999. It would otherwise wedge `reconcileChildren` on a corrupt record.
- The `leaseProcess` and `queueHandoverProcess` aliases, used only by tests, are gone.
- Each caller's census wording is named, and `TestCensusCallersKeepTheirEstablishedMessages` asserts the preserved text at every site.

Still open: the `TestRealDaemonCancellationRemovesDescendants` race and `execution.go:596` dropping the real exit code on a rejected census. Both predate this PR. The review judged the census adds no new outcome to the race and is unlikely to explain most of the 4 of 260 versus 1 of 390 gap, without ruling out a small timing contribution.
