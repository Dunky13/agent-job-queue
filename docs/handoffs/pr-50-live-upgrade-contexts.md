# PR 50: independent live-upgrade fixture contexts

[Pull request](https://github.com/Dunky13/agent-job-queue/pull/50)

## Two independent main failures

[Main run 40773808904](https://github.com/Dunky13/agent-job-queue/actions/runs/40773808904) failed commit policy and the Intel installer partition. The other validation jobs passed. Commit `fdd3598858864766c91be88a597f81cc74191c57` again has a verified signature, a noreply squash author, and only primary-email certifications. The current exact DCO policy correctly rejects that mismatch. A separate merge-workflow decision is pending; this PR does not change the identity policy, checker, or repository protections.

The installer failure occurred in `TestInstallerManagedLiveUpgradeRetainsRunningAttempts/register_resources`. Its new runtime command printed `new runtime outcome`, but the caller was killed while the test's shared context expired. One 60-second context governed initial queue operations, accepted callers, an injected native failure, repair, and later work verification.

## Fixture fix

Accepted callers now belong to the fixture lifecycle and retain the existing release, cancellation, and reap cleanup. Each upgrade operation has its own 60-second context. Busy retries inside one `runPackagedSetup` retain the same original context. Subsequent work verification has its own 60-second context. The initial control client retains its own bounded RPC deadline rather than inheriting the fixture's earlier operation lifetime.

The test explicitly cancels the earlier context before post-upgrade verification. This makes fast machines exercise the same lifetime boundary that failed on Intel. All six scenarios still require active and queued handover behavior, original caller exit code 23, exactly one execution, preserved runtime custody, and later installer-only reclamation. Race detection, two workers, the 12-minute test timeout, and 20-minute CI job budget remain unchanged. This modifies test fixtures only; production installer behavior is unchanged.

## Evidence

Forcing cancellation before the fix reproduced `new runtime could not run work alongside inherited attempt context canceled` in the real `register resources` flow. After the fix, all six scenarios passed locally under race detection in 151.02 seconds. Go formatting, public-source portability, and diff whitespace checks passed.

```sh
go test -p 2 -parallel 2 -race ./internal/backend -run '^TestInstallerManagedLiveUpgradeRetainsRunningAttempts$' -count=1 -v -timeout 12m
go run -p 2 ./cmd/queue-tool check-source
git diff --check
```

Hosted Apple Silicon and Intel checks on the final head remain required. The local tests use private temporary homes, actual race-built product CLI processes, and simulated native service registration; they do not establish real Service Management or release acceptance. Test-owned processes and temporary directories were cleaned up. The ignored HTML counterpart remains for review.

## Main recovery

Under the current identity policy, a recovery merge must explicitly select `marc@went.io` and retain one matching maintainer certification through the guarded command after owner approval. Supporting default web squash merges instead requires an explicit change to the maintainer certification policy to include the noreply identity. That choice is pending and is not silently made by this test fix. Successful PR checks alone do not establish the author identity of a future squash commit. Verify the actual merge metadata and subsequent main CI before declaring recovery complete. Historical failed commits and runs remain intact.
