# Separate build and distribution workflow

The accepted change separates application builds from distribution repairs.
The original report concerned build 56: its notarization succeeded, then fresh
verification lost its connection to Apple's ticket service during
`stapler validate`. The existing repair thread retained ownership of that
release and retried its failed acceptance jobs using the signed artifacts.
This change does not retry or replace that release.

## Resulting behavior

CI tests the app, builds the universal installer and retains its artifact.
A separate distribution workflow consumes the verified main-build artifact,
notarizes payload, installer and DMG, runs fresh Apple Silicon and Intel
acceptance, publishes the original numbered build, and verifies Homebrew
delivery. Automatic runs require successful same-repository main-push CI.
Manual runs accept an original build run ID and use trusted main automation.

The original source SHA, build number, version and artifact identity remain
separate from the distribution execution and its automation SHA. A pipeline
repair can reuse the original build bytes. An application change requires a
new build. The source installer archive is inspected as data before signing;
no downloaded build script is executed with Apple credentials.

## Recovery contract

- Serialize distribution executions for the same source build.
- Check the source run, successful validation, exact source artifact ID/digest,
  ZIP/DMG checksums and embedded build metadata before credentials.
- Discover previous distribution checkpoints and legacy CI checkpoints for the
  same build; validate repository, workflow, source SHA, team and version.
- Preserve exact signed bytes and Apple submission IDs across workflow runs.
- Check installer production separately from effective required-validation
  success, allowing successful partial CI retries without rebuilding.
- Derive checkpoint obligations from actual signing progress across attempts;
  acceptance-only retries and complete histories proving all signing stages
  were skipped do not invent a requirement for a newer signing checkpoint.
- Order each execution by numeric attempt and stage slot, then order retained
  checkpoints across executions by verified creation time. Artifact IDs do not
  encode chronology; missing or tied creation times stop recovery.
- Bind prepared-stage custody to both execution run ID and attempt.
- Stop on missing or expired latest state, changed artifacts, corruption,
  rejection or uncertain upload outcomes. Never create a fresh submission to
  evade an unresolved earlier one.
- Publish only after all three stages and both architecture acceptance jobs
  succeed. Keep the existing immutable-release comparison and exact-build
  Homebrew verification.

## Operator entry point

The [signing guide](../apple-signing.md#resuming-a-pending-ci-submission)
contains recovery commands, expected results, retention limits and credential
boundaries. Use failed-job retries for unchanged automation. After merging an
automation fix, dispatch `distribution.yml` from `main` with the original
`build_run_id`. Neither path rebuilds application binaries.

No operator installation, live queue mutation, credential change, old
unknown-upload reconciliation or new observer is part of this change.

## Validation

- All Go packages passed race tests with at most two package/test workers.
  Source portability, Go formatting and vet, reachable-vulnerability scanning,
  Linux builds of the trusted tools, workflow lint and shell syntax checks passed.
- Review reproduced integration defects in trigger trust, partial CI retries
  and signing-state recovery. Regression coverage includes copied successful
  jobs, queued retries, setup/download failures before signing, older workflow
  executions resumed later, and nonchronological artifact IDs. Missing or
  ambiguous histories and timestamps retain explicit failure paths.
- Focused race checks passed after the fixes, including the actual credential
  wrapper and resume path with private simulated tools. Checkpoint checks retain
  refusal for unknown signing history, changed artifacts, deleted trailing
  progress and missing newer signing state.
- A read-only download of original CI artifact `12429070300` from run
  `39784200861` passed the new artifact verifier against build 56, source commit
  `c7ed359a425064514469195b3989b44fb8ebce5b` and version `0.1.0`. Its GitHub digest
  was `sha256:d2e73446d634415cf1931ee658af12a015d2084b08bc76d68bed35bb2a592b76`.
  The retained production checkpoint's initial installer matched the original
  archive's contents, modes and resource metadata.
- Native ditto/resource-metadata round trips passed under both `022` and `077`
  umasks. The complete offline HTML reports contain no external assets.

These checks do not establish a hosted distribution execution, real Apple
submission or fresh ARM/Intel acceptance through the new workflow. Those gates
remain required after merge. No live release or installed queue was changed by
local validation.

A complete standalone [HTML version](distribution-workflow.html) accompanies
this handoff.
