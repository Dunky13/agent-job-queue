# Learning browse handoff

## Result

Learned workloads now opens on **Projects**, busiest first. Projects with two workloads or fewer share one **Other projects** row. Choosing a project shows one row per operation (tests, build, lint and so on) plus **All operations**. Choosing an operation opens the existing paged group list with selection, details and merge. The phone dashboard has a breadcrumb; the native app has a Back button and breadcrumb buttons. Search always covers every project and shows a flat list.

This builds on the repository grouping and catalog API from the learning grouping change (`docs/handoffs/learning-grouping.md`). That branch is this one's base.

## What changed

- `remote/static/profiles.js`, `remote/static/app.css`, `remote/static/index.html`: the three levels, breadcrumb, focus and announcements, failed-load cleanup.
- `native/AgentLearning.swift`, `native/AgentQueue.swift`: the same levels in `LearningStore`, the hierarchy transport, and a submitted query kept apart from the search box.
- `internal/backend/profiles_learning.go`, `learning.go`:
  - Group project names are compared in NFC.
  - The CLI normalises its string flags, because Foundation's `Process` passes them decomposed.
  - Search also matches member project names, ranked after label and id matches.
- `internal/preview`: fixture projects and the hierarchy flags for screenshots and browser tests.

## Behaviour worth knowing

- When a merge folds a project into Other projects, its groups load directly. Back and the project crumb then return to Projects, because that project has no operations level.
- A page past the end after a merge re-reads the last page once.
- The overview lists at most 100 projects. Search reaches groups behind that limit.

## Known limits

- The `folded` flag is set only on the way into the operations level. If a project regains its own row while you stay on its groups, Back still goes to Projects. If a merge folds it while you are on a group list, the first Back lands on its All operations list. Each costs one extra click; no data is wrong.
- The HTTP handler checks the 120-character limit before NFC normalisation. A direct API client sending a long decomposed name gets a 400. The web client is not affected.
- No browser test covers the case where the re-read of the last page is also empty.

## Evidence not covered

- The flaky "Cannot establish process identity" failures described in the grouping handoff were not seen in this branch's runs and remain undiagnosed.

## Next steps

1. Merge the measurement fix (#78), then the grouping change (#79), and retarget this PR to `main` so hosted CI runs.
2. After merge, open Learned workloads on the phone dashboard and the native app and check the project counts against `agent-heavy profiles list --hierarchy --json`.
3. Optional later work: tool and memory filters, a `--kind` flag, and a look at the 64 to 66 GiB `rewrite` peaks before switching to Adaptive mode.
