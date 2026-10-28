# Distribution fixture exit delay

## Failure

[Distribution run 40546973238](https://github.com/Dunky13/agent-job-queue/actions/runs/40546973238)
processed source `2c1bac6220ce4d85256e701f69f458d14de061d8` from
[CI run 40543052537](https://github.com/Dunky13/agent-job-queue/actions/runs/40543052537).
Signing and all three notarization stages passed. Apple Silicon acceptance passed.
Intel acceptance failed in `TestManagedInstallerPackagedNativeFixture` after
90.04 seconds with `Installer command service-management timed out`. Publication
and Homebrew delivery were skipped. Successful recovery observer runs did not
retry this failed acceptance gate.

## Cause and change

Each private `service-management` and `launchctl` wrapper executes the same Go
test binary that runs the acceptance fixture. With `-race`, successful exits
include the race runtime's default one-second sleep. Repeated successful
queries accumulate this overhead inside the fixture's 90-second deadline.
See [Go's race detector options](https://go.dev/doc/articles/race_detector).

The generated wrappers append `atexit_sleep_ms=0` to their own `GORACE` setting.
They preserve other inherited options and retain race instrumentation and error
exit codes. Parent test settings, deadlines, retry limits, private process
ownership, trust checks and publication gates are unchanged. No production
installer code changes.

The existing private lifecycle regression now supplies a 30-second child exit
delay against each command's existing ten-second bound. Both generated wrappers
must override that delay while preserving exact lifecycle replies, rejection
codes, ownership readback and process reaping.

Broader validation also reproduced a private Homebrew descendant-cleanup timeout.
Each fixture creates a fresh home, so building Homebrew's cold Bootsnap cache
delays its only invocation without providing later reuse. The fixture now sets
Homebrew's supported `HOMEBREW_NO_BOOTSNAP=1` option. The real DSL, subprocess
execution, command deadlines and cleanup checks remain unchanged.

## Validation

A focused reproduction returned its successful reply but failed during the exit
sleep at its three-second deadline before the fix, then passed after the fix.
The private lifecycle test took 6.08 seconds before and 0.71 seconds after the
wrapper change on Apple Silicon. Final validation results are recorded in the
pull request.

The descendant timeout reproduced at its unchanged 20-second bound before the
cache change. Afterward, its timeout, normal-exit and outer-cancel scenarios all
passed. Formatting, source portability and Go vet passed. The initial unsharded
tooling run failed this descendant check and later reached its 12-minute suite
limit. That failed result remains evidence; the documented two-shard tooling
driver is used for subsequent complete validation.

The original build 133 ZIP and DMG checksums matched, and packaged structural,
signature, architecture and build-identity verification passed. Installer
regressions, retained packaged live upgrades and packaged payload execution
passed. Local retained-artifact acceptance remains **HOLD** on macOS 27.0.1:

- The app-bundle test reported a changed bundle identity after setup.
- The managed installer fixture refused `Managed application changed while recording custody`.
- Comparison of the retained app against the signed archive found all 186
  files/links unchanged, no added/missing paths, matching modes and a valid strict
  signature. Only the bundle root had a later change time, alongside macOS
  metadata annotations. The exact before/after metadata delta was not retained,
  so the metadata writer is not proven.

Production custody checks remain strict. Subsequent notarized Homebrew acceptance
was not run after this refusal. This local hold must not be reported as successful
release qualification or used to justify relaxing ownership checks. Hosted Intel
and Apple Silicon CI and release acceptance remain separate gates.

Run the focused regression with:

```sh
go test -race -p 2 -parallel 2 ./internal/tooling \
  -run '^TestManagedCaskPrivateNativeLifecycle$' -count=1 -v -timeout 2m
```

Apply the shared heavy-job queue policy when running broader validation from
`CONTRIBUTING.md`. Local Apple Silicon checks cannot establish hosted Intel
acceptance. Real Service Management registration and a new Apple submission are
outside this fixture test.

## Release hold

The failed distribution remains blocked. This repair does not authorize merging,
publication or Apple submissions. Preserve its final artifact and checkpoints.
After an approved merge, use the documented trusted-main recovery procedure in
`docs/apple-signing.md` to process the retained build. Never run a full replay that
can discard checkpoints, rebuild the retained candidate, or create duplicate
Apple submissions to test this repair. Both architecture acceptance jobs and
the publication gates must pass before calling the release complete.
