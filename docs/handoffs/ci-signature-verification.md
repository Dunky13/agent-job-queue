# CI signature verification follow-up

## Refreshed baseline

The historical Intel core timeout belonged to PR #57 at
`9d64fb573799438b7f84ea880c5b9bc5159a7a95`, run
[41077165345](https://github.com/Dunky13/agent-job-queue/actions/runs/41077165345).
The package exhausted its 12-minute budget. The named cached-description test
had been running for zero seconds, so its presence in the timeout stack does
not establish a test-specific deadlock.

PR #57 merged on 2026-10-30 at 18:15:01 UTC. Refreshed main is
`367836835eeb419fc3d523ebb9231c17690ee334`, tree
`598b86a74f86c452bae058654ca32cc7b1802094`. Its tree equals the final PR head
`f5a243be7acfabae4a0b6a8f09511bf22a1884d9`. That final head passed all 20 jobs in
[41097148906](https://github.com/Dunky13/agent-job-queue/actions/runs/41097148906),
including both core shards, installer and native tests on both architectures,
and `ci-required`.

The landed core driver sorts and alternates the complete discovered ordinary
backend test, example and fuzz-seed inventory. It retains independent installer
and HTTPS partitions, complete runtime package discovery, race detection, two
workers, the 12-minute test timeout and the 20-minute job budget. No additional
timeout or coverage change is needed for the historical failure.

## Remaining failure and repair

The new main run
[41105282352](https://github.com/Dunky13/agent-job-queue/actions/runs/41105282352)
failed ARM tooling shard 2 in
`TestMacOSResumeRetainsExactArtifactsAndIDsAcrossAttempts`. Its log reports
`printf: write error: Broken pipe`, followed by a false missing-signature-metadata
error while checking the Intel slice of a private fixture helper.

`macos_verify_signer_details` fed captured metadata through `printf | grep -q`.
An early successful match lets grep close its input before printf finishes;
with `pipefail`, the valid match can therefore reject the signature. A new
regression with metadata larger than a pipe buffer rejects valid code and disk
image metadata before the repair. This reproduces the mechanism independently
of provider credentials, real signing and scheduling luck.

Here-strings now feed all four checks without a concurrent producer. The exact
team, Developer ID authority, secure timestamp and hardened-runtime predicates
are unchanged. Per-architecture inspection, codesign verification, notarization
receipts and publication gates remain intact. Regression cases also require
missing mandatory metadata to fail and retain the disk-image runtime exception.

## Startup and helper behavior

The landed app presents its status item before preparation. Ordinary setup
retires old helper registrations and registers only the login item; pending
login approval does not block startup. The app starts and supervises its
helpers after preparation. Private regression fixtures exercise pending
approval, stopped-daemon recovery with unfinished jobs, crash-loop migration,
removal/relocation drain checks, lifetime locks and helper lifelines.

Real Service Management registration, installed-app interaction, Intel runtime
execution on the ARM development host, Developer ID signing, fresh Apple
submissions, release publication and deployment are separate acceptance gates.
This follow-up authorizes none of those actions, nor a merge.

## Reproduction and validation

Run sequentially from the repository root under the shared resource policy:

```sh
go test -p 2 -parallel 2 -race ./internal/tooling -run '^(TestMacOSSignerDetailsConsumeCompleteMetadata|TestMacOSResumeRetainsExactArtifactsAndIDsAcrossAttempts|TestMacOSSigning.*|TestGoRuntime.*|TestMacOSGoAndNativeValidationHaveIndependentBudgets)$' -count=1 -timeout 12m
bash scripts/test-go-tooling.sh 1
bash scripts/test-go-tooling.sh 2
go test -p 2 -parallel 2 -race ./internal/backend -run '^(TestExplicitGenericLabelSurvivesCachedDescription|TestManagedAppBundle.*|TestManagedLoadedBundleProgramAcceptsCrashLoopWithoutProcess|TestManagedRelocationRequiresStoppedQueueDrained|TestRealDaemonEndsWhenItsLifelineCloses|TestRealSchedulerEndsWhenItsLifelineCloses|TestLifelineIsNeverForwardedToControlEnvironments)$' -count=1 -timeout 12m
bash scripts/test-native.sh
go vet -p 2 ./internal/tooling
go run -p 2 ./cmd/queue-tool check-source
shellcheck -x scripts/macos-signing.sh
bash -n scripts/macos-signing.sh
git diff --check
```

Local checks passed: focused tooling race tests (69.134 seconds), complete
tooling shard 1 (276.918 seconds), complete tooling shard 2 (202.890 seconds),
focused backend race tests (20.348 seconds), the full native suite (including
118 supervisor checks and compilation for both distributed architectures),
tooling vet, the public-source boundary check, ShellCheck, Bash syntax and diff
whitespace checks. The two notarized-Homebrew release-app acceptance tests
skipped because no final notarized release app was supplied. The development
host is ARM; compiled Intel slices are not local Intel runtime acceptance.

Both native jobs also passed on exact current main. Main CI remained in
progress with the separately diagnosed ARM tooling shard failure when local
validation finished. The exact follow-up candidate, independent Sol 6.1 XHigh
review and fresh hosted CI state are recorded in the follow-up PR.
Private logs and the offline HTML
handoff remain ignored local evidence. Fixtures own their temporary homes and
children and remove them at completion. No installed queue or real provider is
used by these checks.
