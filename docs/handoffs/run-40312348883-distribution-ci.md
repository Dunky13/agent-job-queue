# Distribution acceptance failure in run 40312348883

## Scope and evidence

The local fix addresses Homebrew acceptance in [distribution run 40312348883](https://github.com/Dunky13/agent-job-queue/actions/runs/40312348883), for source commit `24cc7528a0c4fc26e549d205def68b26e4d64a0e` and build `build-92-24cc752`. That run completed signing, Apple notarization, final checksum/signature/ticket verification, and packaged queue/HTTPS acceptance. Homebrew acceptance failed on both architectures, so publication and Homebrew delivery were skipped.

## Causes and changes

- Apple Silicon reached signed first-launch setup, then failed with `Cannot inspect background service ownership: Installer command launchctl failed: exit status 1`. The fixture returned an empty generic error for absent services. The installer requires exit 113 and macOS's exact missing-service diagnostic. The fixture now supplies that evidence for the queue, sampler, and remote service labels. Unexpected commands still fail.
- Intel stopped before Homebrew with `insufficient remaining time for the bounded Homebrew command owner`. Preliminary verification took about 53 seconds, exceeding the 45 seconds available before a shared two-minute deadline violated the required 75-second reserve. Trust verification and installation now have separate bounded contexts. Both respect the existing suite deadline, with ten seconds reserved for command cleanup. The workflow timeout, Ruby command deadline, cleanup reserve, and trust checks remain in place.
- A credential-free regression invokes the actual service stub for all three labels. It failed against the original fixture and passes with the correction, so ordinary PR CI can catch this mismatch without a notarized payload.

Only the acceptance test and its fixture change. Production service ownership checks, quarantine, signing, notarization, publication dependencies, and installed user state retain their existing behavior.

## Validation

- The original fixture regression failed for all three labels; the corrected regression passes.
- Source portability, Go formatting, tooling vet, and `git diff --check` passed.
- The complete tooling race suite passed in 284.464 seconds. Backend preflight and exact missing-service evidence regressions passed with race detection in 2.305 seconds. Shared queue job 1984 completed successfully.
- The retained final artifact's ZIP and DMG matched their supplied SHA-256 checksums. Artifact ID: `12646132267`.
- The distribution command using that exact signed payload passed locally on Apple Silicon in 28.434 seconds. Homebrew app relocation and signed first-launch setup completed; all 389 staged entries retained quarantine and the fixture attribute, installation readback matched the build identity, and the private daemon stayed healthy. Unsigned payload rejection, incomplete-installer rejection, timeout cleanup, normal-exit cleanup, outer-cancellation cleanup, and the new service fixture regression passed. Shared queue job 1985 completed successfully.
- The signed artifact was downloaded and verified without signing credentials or a new Apple submission. All installer execution used a private test home and service stub.

At preparation, hosted checks against this fix, Intel acceptance, public release, and Homebrew tap delivery have not run. The original hosted run remains failed and publication remains blocked.

## Reproduce

Follow CONTRIBUTING.md's shared queue policy. The focused credential-free regression is:

```sh
go test -p 2 -parallel 2 ./internal/tooling -run '^TestNotarizedHomebrewServiceFixture$' -count=1 -v
```

For real signed acceptance, download the existing `Agent-Job-Queue-Notarized-Installer-1` artifact from run 40312348883 into an ignored local directory, verify its supplied checksums, and extract the installer ZIP with `ditto -x -k`. Set `AGENT_QUEUE_TEST_NOTARIZED_PAYLOAD` to the absolute extracted `Agent Job Queue Installer.app/Contents/Resources/payload` directory and `APPLE_TEAM_ID` to the expected public team identifier. Then run the same finite command used by distribution, through the shared queue:

```sh
agent-heavy run --label "Retained signed Homebrew acceptance" -- \
  go test -p 2 -parallel 2 ./internal/tooling -run '^TestNotarizedHomebrew' -count=1 -v -timeout 3m
```

## Delivery boundary

Delivery is authorized through a signed commit, push, pull request, required hosted checks and review, and merge. Main's existing CI and distribution workflows may subsequently run; their signing, Intel acceptance, publication, and Homebrew delivery results remain separate gates. Local Apple Silicon acceptance cannot qualify hosted Intel execution. An unchanged rerun of the failed run would still use its original broken test code; any recovery must deliberately select trusted automation containing this fix while preserving the retained build and checkpoints.
