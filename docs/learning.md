# Inspect and correct workload learning

## Use

Choose **Learned workloads** from the native app's gear menu or the phone dashboard navigation. Search by name, select a workload, and inspect its measured peaks, successful and incomplete run counts, variants and recent numeric measurements. A job's detail view links to its current learning profile.

Open **Group members** to compare each workload's successful and incomplete run counts, peaks and last run. Expand a member to load its own variants and recent measurements, including runs too old to appear in the group's recent list. Search and page within the group to find members beyond the first 256.

For a mismatch, choose **Ungroup workload…** inside its expanded details and confirm. All of that workload's variants and retained measurements move together into an independent profile. Its minimum reservations are preserved. The separated workload and remaining group stay excluded from automatic grouping, so another learning pass cannot undo your correction. Use **Back to group** to continue reviewing. You can explicitly share their learning again later.

To combine several groups or workloads, use the selection checkboxes in the catalog and group members. **Select this run’s workload** adds the original workload family, including its other retained measurements and future matching runs. Selections persist across searches, member pages and group navigation, with a maximum of 32 items. In **Merge selected groups and workloads**, review the items, search for a **Destination group**, choose **Merge selected…**, and confirm once. A selected destination is excluded from the source group list. Save any correction draft first.

Bulk merging is one transaction and one learning revision. It keeps the destination name, preserves the highest minimum CPU and memory reservations, and retains every original numeric measurement and identity. Selected workloads leave their unselected siblings in the original group. Groups emptied by the move are removed. The destination and remaining source groups are pinned against automatic regrouping. A stale revision, missing source, invalid selection or database failure rejects the whole membership change. Failed confirmations preserve the selection for review; clear and rebuild stale selections after refreshing.

Give a profile a useful name and save minimum RAM or CPU reservations when you know a job needs more headroom. Blank minimums return to automatic estimation. Sharing learning combines the original families under the selected target profile, keeping the larger reservations from both profiles. Separate a family when it should learn independently. Reset measurements only when the old evidence is obsolete; reset removes those measurements permanently while preserving your corrections.

Related workloads now share learning automatically within the same repository and repository-relative working directory. Grouping recognizes the tool and operation, with separate categories for broad, race, native and browser tests. Simple `env` and `fnm exec` wrappers are recognized for new jobs. Opaque scripts can use their safe task descriptions; mixed or unclassified batches remain independent. Project names alone never establish a repository match. Automatic groups retain the largest measured peaks rather than averaging dissimilar runs down.

Agent session IDs, per-call IDs and authentication socket locations no longer reset exact-variant learning. Source revisions are recorded as freshness context instead of creating a different exact variant on every commit. Arguments, resource controls, setup fingerprints and executable identity continue to distinguish variants. These changes affect learning only; commands and their execution environments are unchanged.

Group estimates deliberately have no exact-variant successful-run count: they describe the conservative envelope a new variant can borrow. Each actual job still has its own prediction and confidence. Measured history, the group estimate and operator minimums are displayed separately. Run history and variant peaks can include multiple machines; each run identifies its machine with an opaque hash, while the group estimate uses the current machine only.

The CLI has the same inspection and correction behavior:

```sh
agent-heavy profiles list --json
agent-heavy profiles list --query Tests --offset 0 --limit 100 --json
```

Use the returned profile ID with `agent-heavy profiles show ID --json`. To inspect one original workload, use `agent-heavy profiles show ID --family FAMILY --json`. To search or page all members, use `agent-heavy profiles show ID --members-query "tests" --members-offset 0 --members-limit 256 --json`. Member search matches safe names, project, executable and opaque family identity. Family inspection and member search/paging are separate reads. Edit, merge, combine, split and reset take the profile ID and a JSON object on private standard input. Include the latest `revision` from the detail response. Edit requires `label`, `memoryFloorBytes` and `cpuFloorCores`, with null clearing a minimum. Merge requires `targetId`; split requires `family`; reset requires `confirm: true`. Bulk `combine` takes the destination profile ID and `{ "revision": 7, "profileIds": ["SOURCE_GROUP_ID"], "families": ["WORKLOAD_FAMILY_ID"] }` on standard input, replacing the placeholders with returned opaque identities. Both arrays are required, either may be empty, and their combined size must be 1 to 32. The authenticated HTTP equivalent is POST `/api/profiles/DESTINATION_ID/combine` with the same JSON object and existing CSRF protection. The response is the saved profile's new detail and revision. JSON failures contain an error message and exit with status 125.

Preview retroactive grouping without changing the database:

```sh
agent-heavy profiles regroup --json
```

The response gives `revision`, `profilesBefore`, `profilesAfter`, `familiesGrouped`, proposed `groups` and `applied`. Review it, put that revision in a private `regroup-request.json` file as `{"revision": REVIEWED_REVISION}`, then apply it:

```sh
agent-heavy profiles regroup --apply --json < regroup-request.json
```

An updated resource controller also performs one retroactive pass after its first successful collection. Retained numeric runs are joined to private safe metadata without requiring original commands or surviving worktrees. A deleted checkout can still group within its original directory; the algorithm does not guess which repository owned it. Missing metadata and conflicting classifications leave families independent. Running the command again is safe and can incorporate additional retained metadata.

## Contract

The native app, phone dashboard and CLI expose learned workload profiles: their names, member job families, variants, successful and incomplete observations, measured resource peaks and recent runs. When a local machine identity is available, profile details also show the current resource prediction and its confidence. Unknown telemetry remains unavailable rather than inventing an estimate.

Job families remain independent of linked-worktree checkout paths and source revisions. A profile groups one or more families for statistical learning. Automatic groups combine compatible families; their job details identify the match as **Automatic similar workloads**. Operator corrections can rename a profile, share learning between profiles, separate a family into its own profile, set minimum CPU/RAM reservations, or reset obsolete measurements. Corrections pin those profiles so automatic grouping cannot undo them or add new members. A job's details link to the profile used for its prediction.

Sharing learning changes future lookup for existing and future variants of those families; it does not rewrite a command or merge exact-variant success counts. Machine-specific evidence stays machine-specific. Family transfer remains conservative until an exact variant has enough recent successful evidence. Separating a family restores its independent history because measurements retain their original family and variant identities.

Predictions distinguish `samples`, successful runs of the exact variant, from `familySamples`, successful group runs on this machine in the last 30 days. Group history supplies the full conservative envelope, including its 1.5 multiplier and cold-start floors. Exact-variant evidence still controls confidence and the three-success overlap familiarity check, so existing waiting helpers remain compatible through live upgrades. Failed, incomplete, stale or other-machine observations cannot inflate recent group successes. Operator concurrency limits, host pressure, headroom, active measurement and process ownership gates remain in force.

Minimum reservations are explicit operator floors applied above the statistical estimate. They do not fabricate successful measurements or reduce observed peaks. Clearing a floor returns to the learned envelope. Resetting measurements is explicitly confirmed, preserves naming/grouping corrections and returns affected workloads to conservative cold-start learning. Active jobs continue and can contribute new measurements when they finish.

## Consistency and privacy

Every mutation carries the revision the operator reviewed. Stale edits are rejected and require refresh. Changes invalidate pending automatic admission decisions and are applied by the independent controller on its next fresh pass. Existing active work and explicit manual overrides are preserved. A controller pass using predictions from before an edit cannot publish a new automatic grant afterward.

Retroactive grouping preserves every retained run token, original family, exact variant, machine, success flag and measured peak. It changes profile membership and adds only hashed directory/repository scopes and fixed operation categories. It never reconstructs exact fingerprints from old hashes or upgrades incomplete observations into successful runs. Its changes commit atomically and are idempotent. Read-only previews do not add tables or mark the history migrated.

Adaptive corrections require a fresh acknowledgement from an updated controller. A still-running older controller and older waiting helpers cannot safely interpret the new grouping, so finish the installation and wait for its controller before editing in Adaptive mode, or deliberately select Fixed scheduling. Installation reloads only the resource controller before marking the upgrade complete. A failed reload leaves installation visibly incomplete and can be retried; the independent queue daemon keeps active jobs running.

The inspection API uses bounded summaries and numeric history. Names come from the existing safe job descriptions or explicit operator labels; raw arguments, inline programs, environment values and credentials are not retained by this feature. Read-only listing does not create or migrate a database. Private SQLite storage, bounded history, ownership checks and existing authenticated/CSRF browser boundaries remain in force.

## Interface contract

The CLI entry point is agent-heavy profiles. List and show return JSON when requested. Changes use an explicit expected revision; reset additionally requires confirmation. The native and phone interfaces call the same CLI-owned behavior.

List responses contain revision, profiles, total, offset and limit. Each profile summary contains id, label, memberCount, variantCount, successCount, incompleteCount, peakMemoryBytes, peakCpuCores, lastRunAt, memoryFloorBytes and cpuFloorCores. An empty collection is a normal cold-start state.

Detail responses contain revision, profile, members, variants, runs, prediction, predictionMachine and truncated. A member contains family, label, project, executable and measurements. Its measurements contain variantCount, successCount, incompleteCount, peakMemoryBytes, peakCpuCores and lastRunAt for that member's full retained history. memberPage contains total, offset, limit and query. An inspectedFamily response contains exactly that member and filters variants and runs before applying their limits; profile and prediction remain explicitly group summaries. A variant contains id, family, successCount, incompleteCount, lastRunAt, peakMemoryBytes and peakCpuCores. A run contains id, family, variant, machine, sampledAt, memoryBytes, cpuCores, cpuSeconds, durationSeconds and success. Run records with success false are labelled incomplete or failed rather than being counted as successful evidence.

Prediction uses memoryBytes, cpuCores, durationSeconds, samples, confidence and source, with an additive familySamples count, or null when machine identity is unavailable. predictionMachine is the opaque machine identifier or null. truncated contains boolean members, variants and runs flags where detail limits apply. All identities are opaque hashes; presentation does not claim that a hash is a reproducible-build cache key.

Catalog pages contain at most 100 profiles. A detail view contains at most 256 original families, 100 variants and 30 recent runs. Member paging and search cover the entire group. Truncation is explicit and the same detail limits apply to the CLI. GET `/api/profiles/ID` accepts `family` for individual inspection, or `membersQuery`, `membersOffset` and `membersLimit` for member pages. These reads never change history or grouping. Bounded numeric retention can leave an upper envelope after individual run records expire; displayed successful and incomplete counts describe retained run records. Explicit naming and grouping corrections are pinned and never silently evicted to make room for automatic metadata.

Both response forms include controllerAppliedRevision and controllerStatus. Saving a correction can succeed before the controller acknowledges it; refresh learning to see that acknowledgement. Queue polling does not replace drafts or fetch profile history. Destructive confirmations bind to the reviewed profile and revision; a refresh cancels an old confirmation.
