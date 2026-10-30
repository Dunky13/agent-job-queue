# PR #57 review repairs

PR: https://github.com/Dunky13/agent-job-queue/pull/57

Reviewed original candidate: `f2a411d6958727c71c648fffbb82c2843e03bc6d`.
Base: `2295abe1e786dd8b6b4984356c2b51583558d2d3`.
Authorized endpoint: signed repair commit pushed to the existing PR. Merge,
release, publication and deployment require separate authorization.

## Findings and repairs

| Finding | Repair | Regression evidence |
| --- | --- | --- |
| P1: App-bundle service removal and relocation skipped stopped-store drain checks. | Pass the managed plan into offline inspection. Only ordinary supervised setup at the same location may preserve unfinished work for daemon recovery. Removal, relocation and replacement retain drain and attempt checks. | Both removal and relocation accepted queued, starting, running and outcome work before the repair. Tests now require refusal without changing installation or queue custody. |
| P2: Terminal phone reconfiguration could leave enabled access stopped when old-service shutdown outlasted the settings debounce. | Reconcile current enabled settings after a clean phone-service exit, retaining restart throttling and explicit stop/shutdown guards. | The supervisor regression failed after the settings event settled while the old service was still running. It now requires a restart without another filesystem change and verifies that disabled access remains stopped. |
| P2: A failed lifeline allocation allowed helpers to start without crash cleanup. | Refuse helper launch without a lifeline descriptor, record a launch failure and retry allocation through the existing throttle. | Injected allocation failure previously reached the spawner. The regression requires zero launches until allocation recovers, then verifies the descriptor is supplied. |
| P3: Restart documentation incorrectly said the daemon stayed stopped after a clean exit. | Align README, CONTRIBUTING and upgrade recovery with the queue, scheduler and phone-service policies. | Source comparison against the supervisor state transitions. |

## Validation

- Before repairs: both Go drain regressions failed across all four unfinished
  phases; the native regressions failed for missing lifelines and delayed phone
  reconfiguration.
- After repairs: Go race tests PASS for backend (564.979s), remote (171.279s),
  lifeline (11.664s) and appservices (1.261s), including both new drain regressions.
- Full native regression suite: PASS, including 103 supervisor checks and
  compilation of the supervisor for both distributed architectures.
- Go vet for the four affected packages: PASS.
- Public-source portability/boundary check: PASS.
- Independent Standards review: no remaining actionable findings.
- Independent Spec review: no remaining actionable findings.
- Both final repair reviews pinned diff SHA-256
  `4fd7f196eabd74800b56309225dc4368d01bac6a8392a0e31b15ef2f64cba918`
  against the original candidate, before this evidence-only handoff was added.

Reproduce the relevant local checks from the repository root:

```sh
go test -p 2 -parallel 2 -race ./internal/backend ./internal/remote ./internal/lifeline ./internal/appservices -timeout 12m
bash scripts/test-native.sh
go run -p 2 ./cmd/queue-tool check-source
git diff --check
```

These tests use private homes, fake native adapters and isolated helper
processes. The supervisor runner compiles both arm64 and x86_64 and executes the
host architecture. Local validation ran on macOS 27.0.1, arm64, Go 1.26.10 and
Apple Swift 6.4. The repair changes no rendered UI; its evidence is command
and regression output.

## Native CI follow-up

The CI run on `d5dd64c1423ca7e68c72763575b14c3438a24968` passed all jobs
except both native matrices and their aggregate. Both native jobs failed
`non-zero exit waits out the throttle`. A delayed original fixture reproduced
that assertion with a 480 ms-old helper and a 300 ms throttle: the correct
fourth launch had already occurred before the test expected a waiting state.

The repair records waiting transitions synchronously and checks durable exit
results with both prompt and 350 ms-late observers. A controllable restart clock
now verifies exact timer boundaries and cancellation: a fast failure requests
500 ms, cannot restart at 499 ms, restarts at 500 ms, and a helper that outlived
its throttle requests no additional timer. Shutdown must cancel the pending
waiter before advancing time can launch anything. Spawn failures retain their
recorded cause even if the retry has already run.

The production clock still uses `ContinuousClock.now` and `Task.sleep`; its
10-second throttle, process ownership, restart policy and job behavior are
unchanged. No CI timeout or required check was changed.

Final local native suite: PASS, including 118 supervisor checks and compilation
for arm64 and x86_64. Independent Standards and Spec rechecks found no remaining
actionable findings in the repair diff against `d5dd64c`, SHA-256
`3957baf6edbb8a5c4611fbf838f02d086b104014045962c4776b3701f372adb4`.
Both hosted native jobs subsequently passed on `9d64fb5`. Its remaining Intel
core failure is covered below. The delayed fixture's scratch source and binary
were removed; its failure log is retained locally.

## Intel core CI budget follow-up

Run `41077165345` on `9d64fb573799438b7f84ea880c5b9bc5159a7a95` passed
the native matrices and all other jobs except Intel core and its aggregate.
The backend package exhausted its twelve-minute test budget while
`TestExplicitGenericLabelSurvivesCachedDescription` had only just started.
That test passes alone in 120 ms. A full local ordinary-backend race run passed
in 524.487 seconds, with per-test timings showing cumulative work across the
suite. The aggregate correctly refused this incomplete validation.

The CI core workload is now two independently required shards on each
architecture. The driver discovers the complete backend inventory, sorts and
alternates its 486 ordinary tests, examples and fuzz seeds, with 243 selected
per shard. It excludes only the managed live-upgrade test, which retains its
separate required installer job. Core-1 also runs the remaining dynamically
discovered runtime packages, and core-2 runs only its backend selection. HTTPS
is unchanged. Legacy unsharded `core` and default `all` remain available.

Selection rejects missing installer coverage, duplicate or malformed records,
incomplete summaries, unsafe names and empty shards before execution. Fixture
regressions prove exact-once selection, future-package coverage, anchored
installer-prefix handling, malformed-inventory refusal and preservation of
backend and later-package failure exit codes. They pass with race detection.
Bash 3.2 syntax, ShellCheck and Actionlint pass. Both actual shard commands passed locally: core-1 backend in 289.732 seconds
and core-2 backend in 253.847 seconds. Core-1's other runtime package batch also
passed. Comparing actual top-level execution records with the full baseline
proved 486 entries covered exactly once, 243 per shard, with no omissions or
overlaps. Reproduce from the repository root:

```sh
bash scripts/test-go-runtime.sh core-1
bash scripts/test-go-runtime.sh core-2
```

Every CI shard keeps race detection, two package/test workers, the existing
12-minute test timeout and 20-minute job budget. The complete matrix remains a
mandatory input to `ci-required`; no required check or budget was weakened.
Independent Standards and Spec reviews found no actionable findings, pinned to
repair diff SHA-256
`713df2248abf3a9fa35925640698bb7b75324d2067d9bd1ba60263c128d3ffb9`
against `9d64fb5`, before this evidence-only handoff update. Fresh hosted Intel
execution is still required to establish the new workload fits its budget.

## Hosted and platform evidence

The original head's Intel core job was cancelled at its existing 20-minute job
budget. Go setup consumed over 11 minutes before the test step began. This was
not a reported test assertion failure. Updated-head CI remains a separate gate;
do not reuse the original head's results as proof for the repaired head.

Graphical startup acceptance, live Service Management registration, Intel
runtime execution on this host, universal packaging, Developer ID signing and
notarization, SentinelOne acceptance, publication and deployment were not run as
part of these local repairs. No installed queue or real login-item registration
was changed by the tests. Optional cross-provider review was not requested.

## Next steps and cleanup

Inspect required CI on the pushed PR head and repair any source-supported
failures. Obtain owner approval before merging; retain the repository's guarded
squash merge, signature and author-matching DCO checks. Release and platform
acceptance remain separate work.

Private test fixtures have automatic teardown. Retain the local offline HTML
copy and validation logs only as review evidence; they are excluded from Git.
