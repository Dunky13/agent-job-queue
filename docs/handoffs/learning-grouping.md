# Learning grouping handoff

## Result

Workloads now share learning within a verified git repository, grouped by operation and tool, whatever subdirectory or worktree they ran in. Each workload family has a stored project, operation and tool, with `other` as the fallback. The catalog can filter by project and operation and offers a project hierarchy that collapses small projects into one row.

Measured on a private copy of real history at the final commit: profiles go from 1880 to 1433, 987 families sit in merged groups, a second preview finds nothing further, and the hierarchy has 49 rows (48 projects plus "Other projects" covering 72). A client built from `main` still previews and lists the regrouped database. The live database was only read.

This change has no UI. The dashboard and native browse views are the next step.

## What changed

- `internal/backend/profiles_classification.go`: the `learning_classification` table, the operation and tool vocabulary, inline `sh -c` scanning, the backfill's class rules and the merge rule.
- `internal/backend/profiles_regroup.go`: reads each metadata file into evidence, then aggregates per family, so the result cannot depend on file order. Bad records are skipped.
- `internal/backend/profiles_similarity.go`: the observation path that classifies a job when the controller sees it.
- `internal/backend/profiles_learning.go`, `learning.go`, `internal/remote/server.go`: project and operation on summaries, filters, hierarchy.
- `internal/backend/scheduler_controller.go`: a failed history pass is logged and retried after ten minutes. It no longer holds admissions, which is a deliberate change from `main`. The identity cache now expires after ten minutes.

## Known limits

- About 1,291 of the 1,433 groups still hold one family, because 1,421 of 2,722 families have no repository that can be verified. These include deleted worktrees whose stored scope maps to no live checkout and folders outside any repository; the split between them was not measured. The browse hierarchy, not this merge, is what shortens the list.
- Sharing raises estimates. 8 groups covering 424 families and 496 of 3,424 runs predict over the 13.6 GiB memory budget. Adaptive mode refuses to start these automatically until the variant has five successful runs of its own. Fixed and shadow modes are unaffected. The largest are the `rewrite` groups `other/shell` and `checks/shell`, at 64 to 66 GiB. Those peaks were not investigated.
- History keeps no command line, so an older job started through `env` or `fnm` counts as `shell` or `javascript`. New jobs are classified by what the wrapper runs.
- A path reused by an unrelated repository can assign the wrong repository through a stored scope. It only raises a conservative estimate, and an operator can ungroup. See the `shortcut:` comment.
- A failed git probe looks like "not a repository". That checkout stays unshared until the identity cache expires and a later job is observed.
- A permanently failing history pass shows only in the resource controller log.
- An invalid `learning_classification` row fails catalog reads visibly. Adding tool or operation vocabulary needs a version step.
- A family that cannot register because the registry is full still counts in the preview.

## Evidence not covered

- `TestRealDaemonHandoverRetainsClaimedWorkerWaitingForExternalSlot` and `TestQueueEdgeLostSubmitResponseRetainsCommittedPayload` failed once under load with "Cannot establish process identity" and passed on rerun. The message comes from `identityCommand` in `internal/backend/ownership.go`, when `ps` or `sysctl` fails or exceeds its 5-second limit. This change does not touch it, and the root cause was not diagnosed.

## Next steps

1. Add the project, operation and group browse hierarchy to `remote/static/profiles.js` and `native/AgentLearning.swift`, with preview fixtures and labeled before and after screenshots.
2. After merge, the updated controller runs the pass once. Confirm the group counts with `agent-heavy profiles list --hierarchy --json`.
3. Before switching to Adaptive mode, review the over-budget groups above.
