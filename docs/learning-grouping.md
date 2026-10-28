# Workload grouping implementation handoff

## Problem and behavior

The previous matching path created one profile per strict command family and hashed almost every environment value plus HEAD into each exact variant. Agent invocation metadata and frequent commits prevented repeated measurements from accumulating. Similar tasks could share learning only after a manual merge.

The new path keeps original family identities and numeric history intact, stabilizes future exact variants, and groups compatible families by a hashed repository/directory scope, tool class and operation. Borrowing keeps the complete conservative envelope. Exact successes and recent same-machine group successes are counted separately. Source revisions move to the freshness context; setup, toolchain, arguments and semantic environment remain in exact variants.

## Owners

- `internal/backend/profiles_identity.go`: versioned stable variants and incidental environment exclusions.
- `internal/backend/profiles_similarity.go`: bounded operation vocabulary, scope hashing, wrapper recognition, automatic membership and pinned-profile preservation.
- `internal/backend/profiles_regroup.go`: historical metadata joins, read-only preview, revision-checked apply and the one-time controller backfill.
- `internal/backend/profiles_store.go`: additive private schema and description observation.
- `internal/backend/profiles_prediction.go` and `profiles_collection.go`: identical independent/batched estimates, exact counts and recent group counts.
- `internal/backend/scheduler.go`: validates the additive group count while retaining the existing exact-run familiarity guard for waiting-helper compatibility.
- `internal/backend/learning.go`: CLI and shared admission invalidation for corrections and retrospective grouping.

## Compatibility and rollout

Identity sidecars still contain exactly the original `family`, `variant` and `context` fields. Existing jobs and their stored identities are not rewritten. New exact fingerprints use `variant-v2`; historical variants remain distinct. Additive SQLite tables retain hashed scope and fixed operation categories. Old clients can still read original measurements and group membership. Read-only clients never migrate storage.

The updated controller runs its initial collection before attempting the historical pass, then rotates pending grants through the existing correction path and collects fresh estimates. Operator splits, merges, labels, resets and floors stay pinned. Active grants and explicit overrides survive. A stale preview revision cannot apply a migration.

The CLI can preview and explicitly apply grouping before a full application upgrade, because the membership schema and conservative group estimates remain compatible. Future stable fingerprints and automatic grouping require the updated runtime. Replacing or publishing a signed application remains a separate delivery step.

## Validation

Regression coverage exercises volatile invocation metadata, source revision changes, resource-control separation, wrapper recognition, scope/operation separation, shared conservative peaks, exact versus group success counts, machine isolation, operator corrections, stale revision rejection, read-only previews, byte-for-byte numeric history preservation and migration idempotence. Batched controller predictions are compared with independent reads.

Run the applicable Go runtime checks through the shared queue as documented in `CONTRIBUTING.md`. Preview real history with `agent-heavy profiles regroup --json`; apply only the reviewed revision as described in `docs/learning.md`. Keep live-state copies and validation reports outside version control.

## Inspect and correct mismatches

The native app and dashboard show per-member counts and measured peaks before expansion. Expanding a member explicitly reads only its own variants and recent runs. Filtering happens before the global bounds, so a busy sibling cannot hide a quieter member's history. Search and offset pages cover groups larger than 256 members without discarding an unsaved correction draft.

`profiles show ID --family FAMILY --json` returns `inspectedFamily`, a single member with its complete retained summary, up to 100 of its variants and its 30 latest runs. The enclosing `profile` and `prediction` remain group summaries. `--members-query`, `--members-offset` and `--members-limit` select pages of the full membership. The authenticated HTTP adapter forwards the equivalent validated query parameters. All reads remain nonmutating.

Ungroup confirmation uses the existing revision-checked split operation. It preserves original family, variant and run identities, copies reservation floors and pins both resulting groups against automatic regrouping. Both interfaces follow the returned profile and offer a return to the original group. Explicit sharing remains available to reverse an operator decision.

Coverage includes older per-member evidence hidden by the group run limit, complete retained summaries, large-group paging/search, foreign-member rejection, read-only database identity, history preservation after split, automatic grouping exclusion, cancelled/failed reads, stale revisions, draft preservation and confirmation navigation. Browser comparisons use simulated data; native model tests use private commands and fixture captures remain inert.

## Merge mixed selections

`internal/backend/profiles_combine.go` plans a bounded mixed selection of source profiles and original workload families, validates all members and the reviewed revision, and applies the plan in one SQLite transaction. Full-group and individual-family selections coalesce by family identity. The destination name is retained; reservation floors take the maximum across all participating profiles. Remaining source groups keep their unselected members and floors. Empty sources are deleted, changed groups stay pinned, and the learning revision advances once. Original runs, variants, envelopes and success counts are never rewritten. The shared correction path validates the full selection before invalidating pending admissions.

The additive CLI `combine` action and authenticated HTTP route take both `profileIds` and `families` arrays, bounded to 32 combined items. Existing single-profile `merge` remains compatible. `remote/static/profile-selection.js` owns persistent checkbox selection, destination search, explicit review, confirmation and stale-selection recovery. The native learning store uses the same revision-bound request and retains selections across navigation. Selecting a run always selects its original workload and full history, as agreed, so future matching runs follow the destination too.
