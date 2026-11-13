# Learning estimates handoff

## Result

Three defects are fixed: two in how an estimate is calculated, one in how runs of the same command are matched to an exact variant. Group membership itself is unchanged; its limits are measured under "Found and not changed".

1. A workload that borrowed from its group got the largest peak any member ever measured, times 1.5. One runaway run set the estimate for the whole group. A borrowed estimate now keeps the workload's own peaks in full, counts every other workload in the group once, and sets aside the highest tenth of those. Groups with fewer than ten other workloads are unchanged.
2. The exact variant included per-session values that agents set for themselves, so the same command from another agent session was a new variant with no history. Agent namespaces and the macOS launch values are now left out of the variant.

3. History that fell out of the outer retention bound became a machine-wide floor under every estimate, and that floor could only rise. That history is now forgotten: its workload starts again like one never seen. The owner chose this after the first two fixes were reviewed.

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

Retention: `learning_retained` holds 8,192 rows and is empty today. At about 150 new variants a day it starts filling in roughly six weeks. Replaying the eviction order on the copy, the old floor reached 3.4 GiB after the first 50 evicted rows and 8.2 GiB after 500.

Within one family, peak memory varies by 1.9 times on average. Within one group it varies by 17 times. That is why a workload's own peaks count in full while the others are trimmed.

## What changed

- `internal/backend/profiles_prediction.go`: the shared-peak rule in `profileEstimate`, used by both the independent and the batched read. `profilePrediction` takes the member that locates the group apart from the family being estimated.
- `internal/backend/profiles_prediction.go`, `Record`: rows past the `learning_retained` bound are deleted. They are no longer folded into `floors`, and machine floors are no longer folded into the `'*'` row, which nothing could trigger any more. `maxProfileMachines` is removed with it.
- `internal/backend/profiles_learning.go`: the group estimate describes a workload the group has not seen, instead of treating an arbitrary member as its own.
- `internal/backend/profiles_identity.go`: `CLAUDECODE`, `AI_AGENT`, the `CLAUDE_`, `CODEX_` and `T3CODE_` namespaces and four macOS launch values are incidental environment. `XPC_SERVICE_NAME` carries per-launch process ids, so it reset every variant on each app relaunch.
- `native/AgentLearning.swift`: the caption under the group estimate says what the figure describes. Text only; no screenshot was taken.
- `docs/learning.md`, `docs/learning-grouping.md`: the estimate and variant rules. `docs/learning.html` gets the estimate sentences it already had.

Schemas, identity sidecars and the prediction contract are unchanged, and no stored row is rewritten. A job submitted from an agent session gets a new variant once. Rows already in `floors` on an existing store are still read and still raise estimates there; resetting a group makes it ignore them, as before.

`go.mod` and `CONTRIBUTING.md` move to Go 1.26.11. The hosted vulnerability scan failed on this branch on GO-2026-6859 in `net/http` of Go 1.26.10, the version `main` pins too.

## Found and not changed

These need a decision or a larger change. Each is confirmed against the same copy.

1. **The cold-start prior is 1 GiB.** About four in ten jobs had no history to borrow from, and 29% of those peaked above 1 GiB. A prior learned from the machine's own history would fit better.
2. **Overlap needs three successes of the exact variant.** Three variants in 25 days qualified. The variant fix should raise that for repeated commands; one-off commands stay unfamiliar and never overlap in Adaptive mode.
3. **CPU estimates can exceed the core count.** The 1.5 margin applies to a peak that is already bounded by the machine, and a job predicted above 80% of cores is refused rather than run alone.
4. **1,802 of 3,190 families have no verifiable repository**, mostly deleted worktrees (3,289 of 4,630 metadata directories are gone). They cannot join a group. New jobs record their repository while the checkout exists, so this shrinks as history ages out.
5. **Half of all families have operation `other`**, because job titles are free text. They share one group per repository and tool. The shared-peak rule is what makes those groups usable.

## Behaviour worth knowing

- Lower borrowed estimates admit more. An unfamiliar job in a group of ten or more now reserves the trimmed figure, so in Adaptive mode it can start beside a held lane reservation that the group's largest peak used to block. The three-success overlap guard, budget check, live sampling, operator minimums and machine floors are unchanged.
- Adding a workload to a group can now lower other members' estimates, when it takes the group from nine to ten other workloads. Before, a new member could only raise them.
- Groups an operator shared are trimmed like automatic ones. A minimum reservation on the group is the way to hold every member above a figure.
- The group estimate describes a workload new to the group. A member of a group with exactly ten measured workloads has nine others, so it still borrows the largest peak while the group estimate shows the trimmed one.
- A job that is itself an agent run (`codex exec`, `claude -p`) is no longer told apart by `CODEX_HOME`, `CLAUDE_CONFIG_DIR` or sandbox settings. Its envelope keeps the largest peak of either configuration.
- Variables an agent sets outside its namespace, such as `GIT_EDITOR`, still feed the variant. The same command from a different kind of launcher can stay a separate variant, each with its own history.
- A workload whose history was forgotten borrows from its group again, or gets the 1 GiB prior when the group has nothing else. A heavy job that runs less often than 10,240 other variants are recorded on that store starts low once and is corrected by live sampling.
- `docs/learning.html` was already missing ten paragraphs of `docs/learning.md` before this change, including the variant paragraph. It was not regenerated.

## Validation

- `TestProfileSimilarityUnusualSiblingDoesNotSetALargeGroupsEstimate` fails under the old rule (90 GiB estimate) and passes now. It also checks nine, ten, eleven and twenty other workloads, CPU trimmed on its own, a workload keeping its own peak in a new variant, batched and independent reads agreeing, and the group estimate.
- `TestWorkloadIdentityIgnoresAgentInvocationMetadata` covers the agent namespaces, `AI_AGENT` and the launch values, and still requires `GOMAXPROCS` to change the variant.
- `TestProfileOuterRetentionForgetsTheOldestHistory` replaces `TestProfileOuterRetentionFallsBackToMachineAndGlobalFloors`. It fails on the old rule (8 GiB and 6 cores from the floor) and passes now with the 1 GiB, 2-core cold floors and an empty `floors` table.
- `govulncheck` as CI runs it reports GO-2026-6859 on Go 1.26.10 and no vulnerabilities on 1.26.11.
- `scripts/lint.sh`, the `core-1` and `core-2` runtime partitions with race detection and `scripts/test-native.sh` passed locally. The other partitions, the web and installer checks were left to hosted CI, because this change touches only `internal/backend`, one native caption and documentation.

## Next steps

1. Install a build with this change, then compare `agent-heavy profiles show ID --json` group estimates with the table above.
2. Re-measure variant reuse after a week: the share of repeated commands that land on an existing variant should rise well above 17%.
