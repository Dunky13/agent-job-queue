# Inspect and correct workload learning

## Use

Choose **Learned workloads** from the native app's gear menu or the phone dashboard navigation. Search by name, select a workload, and inspect its measured peaks, successful and incomplete run counts, variants and recent numeric measurements. A job's detail view links to its current learning profile.

Give a profile a useful name and save minimum RAM or CPU reservations when you know a job needs more headroom. Blank minimums return to automatic estimation. Sharing learning combines the original families under the selected target profile, keeping the larger reservations from both profiles. Separate a family when it should learn independently. Reset measurements only when the old evidence is obsolete; reset removes those measurements permanently while preserving your corrections.

Group estimates deliberately have no exact-variant successful-run count: they describe the conservative envelope a new variant can borrow. Each actual job still has its own prediction and confidence. Measured history, the group estimate and operator minimums are displayed separately. Run history and variant peaks can include multiple machines; each run identifies its machine with an opaque hash, while the group estimate uses the current machine only.

The CLI has the same inspection and correction behavior:

```sh
agent-heavy profiles list --json
agent-heavy profiles list --query Tests --offset 0 --limit 100 --json
```

Use the returned profile ID with `agent-heavy profiles show ID --json`. Edit, merge, split and reset take the profile ID and a JSON object on private standard input. Include the latest `revision` from the detail response. Edit requires `label`, `memoryFloorBytes` and `cpuFloorCores`, with null clearing a minimum. Merge requires `targetId`; split requires `family`; reset requires `confirm: true`. The response is the saved profile's new detail and revision. JSON failures contain an error message and exit with status 125.

## Contract

The native app, phone dashboard and CLI expose learned workload profiles: their names, member job families, variants, successful and incomplete observations, measured resource peaks and recent runs. When a local machine identity is available, profile details also show the current resource prediction and its confidence. Unknown telemetry remains unavailable rather than inventing an estimate.

Job families remain independent of linked-worktree checkout paths and source revisions. A profile groups one or more families for statistical learning. Automatic groups initially contain one family. Operator corrections can rename a profile, share learning between profiles, separate a family into its own profile, set minimum CPU/RAM reservations, or reset obsolete measurements. A job's details link to the profile used for its prediction.

Sharing learning changes future lookup for existing and future variants of those families; it does not rewrite a command or merge exact-variant success counts. Machine-specific evidence stays machine-specific. Family transfer remains conservative until an exact variant has enough recent successful evidence. Separating a family restores its independent history because measurements retain their original family and variant identities.

Minimum reservations are explicit operator floors applied above the statistical estimate. They do not fabricate successful measurements or reduce observed peaks. Clearing a floor returns to the learned envelope. Resetting measurements is explicitly confirmed, preserves naming/grouping corrections and returns affected workloads to conservative cold-start learning. Active jobs continue and can contribute new measurements when they finish.

## Consistency and privacy

Every mutation carries the revision the operator reviewed. Stale edits are rejected and require refresh. Changes invalidate pending automatic admission decisions and are applied by the independent controller on its next fresh pass. Existing active work and explicit manual overrides are preserved. A controller pass using predictions from before an edit cannot publish a new automatic grant afterward.

Adaptive corrections require a fresh acknowledgement from an updated controller. A still-running older controller and older waiting helpers cannot safely interpret the new grouping, so finish the installation and wait for its controller before editing in Adaptive mode, or deliberately select Fixed scheduling. Installation reloads only the resource controller before marking the upgrade complete. A failed reload leaves installation visibly incomplete and can be retried; the independent queue daemon keeps active jobs running.

The inspection API uses bounded summaries and numeric history. Names come from the existing safe job descriptions or explicit operator labels; raw arguments, inline programs, environment values and credentials are not retained by this feature. Read-only listing does not create or migrate a database. Private SQLite storage, bounded history, ownership checks and existing authenticated/CSRF browser boundaries remain in force.

## Interface contract

The CLI entry point is agent-heavy profiles. List and show return JSON when requested. Changes use an explicit expected revision; reset additionally requires confirmation. The native and phone interfaces call the same CLI-owned behavior.

List responses contain revision, profiles, total, offset and limit. Each profile summary contains id, label, memberCount, variantCount, successCount, incompleteCount, peakMemoryBytes, peakCpuCores, lastRunAt, memoryFloorBytes and cpuFloorCores. An empty collection is a normal cold-start state.

Detail responses contain revision, profile, members, variants, runs, prediction, predictionMachine and truncated. A member contains family, label, project and executable. A variant contains id, family, successCount, incompleteCount, lastRunAt, peakMemoryBytes and peakCpuCores. A run contains id, family, variant, machine, sampledAt, memoryBytes, cpuCores, cpuSeconds, durationSeconds and success. Run records with success false are labelled incomplete or failed rather than being counted as successful evidence.

Prediction uses the existing memoryBytes, cpuCores, durationSeconds, samples, confidence and source fields, or null when machine identity is unavailable. predictionMachine is the opaque machine identifier or null. truncated contains boolean members, variants and runs flags where detail limits apply. All identities are opaque hashes; presentation does not claim that a hash is a reproducible-build cache key.

Catalog pages contain at most 100 profiles. A detail view contains at most 256 original families, 100 variants and 30 recent runs. Truncation is explicit and the same detail limits apply to the CLI. Bounded numeric retention can leave an upper envelope after individual run records expire; displayed successful and incomplete counts describe retained run records. Explicit naming and grouping corrections are pinned and never silently evicted to make room for automatic metadata.

Both response forms include controllerAppliedRevision and controllerStatus. Saving a correction can succeed before the controller acknowledges it; refresh learning to see that acknowledgement. Queue polling does not replace drafts or fetch profile history. Destructive confirmations bind to the reviewed profile and revision; a refresh cancels an old confirmation.
