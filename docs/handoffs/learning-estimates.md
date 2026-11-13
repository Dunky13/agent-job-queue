# Learning estimates handoff

## Result

Two defects made estimates and grouping feel wrong, and both are fixed.

1. A workload that borrowed from its group got the largest peak any member ever measured, times 1.5. One runaway run set the estimate for the whole group. A borrowed estimate now keeps the workload's own peaks in full, counts every other workload in the group once, and sets aside the highest tenth of those. Groups with fewer than ten other workloads are unchanged.
2. The exact variant included per-session values that agents set for themselves, so the same command from another agent session was a new variant with no history. Agent namespaces are now left out of the variant.

## Evidence

Measured on a private copy of real history from one 16 GiB, 12-core machine: 4,065 runs, 3,190 workload families, 25 days. The live database was only read. The installed runtime predated the measurement fix, so its CPU figures are not used here.

Group estimates:

| | Before | After |
|---|---|---|
| Families in groups whose estimate exceeds the machine's 13.6 GiB budget | 555 | 16 |
| Runs predicted over budget, replayed in time order | 10.9% | 0.2% |
| Runs that peaked above their borrowed estimate | 2.9% | 4.3% |
| Largest group (343 families), group estimate | 16.0 GiB | 8.0 GiB |
| Group holding a 66 GiB runaway (47 families), group estimate | 99 GiB | 4.0 GiB |

The replay computed each run's estimate from earlier runs only. The 16 families that stay over budget sit in small groups whose members really are that heavy. Setting aside the highest twentieth instead gives 0.8% over budget and 3.1% above the estimate; the highest tenth was chosen because a refused job never starts on its own, while a low estimate is corrected by live sampling and the pressure gates.

Variants:

- 3,635 of 3,800 variants had exactly one run. No variant had five recent successes, so no estimate ever reached medium confidence.
- When the same command ran again, 83% of the runs landed on a new variant.
- Across six live job payloads from five agent sessions, `CLAUDE_CODE_MESSAGING_SOCKET`, `CLAUDE_CODE_MESSAGING_TOKEN`, `CLAUDE_PID` and `T3CODE_THREAD_ID` each had five distinct values. All four fed the variant.

Within one family, peak memory varies by 1.9 times on average. Within one group it varies by 17 times. That is why a workload's own peaks count in full while the others are trimmed.

## What changed

- `internal/backend/profiles_prediction.go`: the shared-peak rule in `profileEstimate`, used by both the independent and the batched read. `profilePrediction` takes the member that locates the group apart from the family being estimated.
- `internal/backend/profiles_learning.go`: the group estimate describes a workload the group has not seen, instead of treating an arbitrary member as its own.
- `internal/backend/profiles_identity.go`: `CLAUDECODE`, `AI_AGENT` and the `CLAUDE_`, `CODEX_` and `T3CODE_` namespaces are incidental environment.
- `docs/learning.md`, `docs/learning.html`: the estimate and variant rules.

Stored data, schemas, identity sidecars and the prediction contract are unchanged. A job submitted from an agent session gets a new variant once.

## Found and not changed

These need a decision or a larger change. Each is confirmed against the same copy.

1. **Evicted history becomes a machine-wide floor.** When `learning_retained` passes 8,192 rows, the oldest row's peak is folded into `floors`, which raises every estimate on that machine. The table is empty today. At about 150 new variants a day it starts filling in roughly six weeks. Replaying the eviction order on the copy, the floor reaches 3.4 GiB after the first 50 rows and 8.2 GiB after 500, and it only ever rises. This is tested, deliberate behaviour (`TestProfileOuterRetentionFallsBackToMachineAndGlobalFloors`), so it was left alone. Recommended: drop evicted rows, so a forgotten workload starts again like one never seen.
2. **The cold-start prior is 1 GiB.** About four in ten jobs had no history to borrow from, and 29% of those peaked above 1 GiB. A prior learned from the machine's own history would fit better.
3. **Overlap needs three successes of the exact variant.** Three variants in 25 days qualified. The variant fix should raise that for repeated commands; one-off commands stay unfamiliar and never overlap in Adaptive mode.
4. **CPU estimates can exceed the core count.** The 1.5 margin applies to a peak that is already bounded by the machine, and a job predicted above 80% of cores is refused rather than run alone.
5. **1,802 of 3,190 families have no verifiable repository**, mostly deleted worktrees (3,289 of 4,630 metadata directories are gone). They cannot join a group. New jobs record their repository while the checkout exists, so this shrinks as history ages out.
6. **Half of all families have operation `other`**, because job titles are free text. They share one group per repository and tool. The shared-peak rule is what makes those groups usable.

## Validation

- `TestProfileSimilarityUnusualSiblingDoesNotSetALargeGroupsEstimate` fails under the old rule (96 GiB estimate) and passes now. It also checks the ten-workload boundary, a workload keeping its own peak in a new variant, batched and independent reads agreeing, and the group estimate.
- `TestWorkloadIdentityIgnoresAgentInvocationMetadata` covers the agent namespaces and still requires `GOMAXPROCS` to change the variant.
- `scripts/lint.sh` and the `core-1` and `core-2` runtime partitions ran locally with race detection. The other partitions, the web, native and installer checks were left to hosted CI, because this change touches only `internal/backend` and documentation.

## Next steps

1. Install a build with this change, then compare `agent-heavy profiles show ID --json` group estimates with the table above.
2. Decide on the machine-wide floor before `learning_retained` fills.
3. Re-measure variant reuse after a week: the share of repeated commands that land on an existing variant should rise well above 17%.
