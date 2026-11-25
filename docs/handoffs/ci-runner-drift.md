# CI runner drift: Homebrew cask path loading and OpenTelemetry advisory

On 2026-11-25 every branch failed hosted CI for two reasons outside the repository's code. The tap's refresh workflow failed for the first of them.

## Result

- **Homebrew:** Homebrew stopped loading cask files by path through `Cask::CaskLoader.load` on 2026-11-24, and added `Cask::CaskLoader.from_path`. `packaging/homebrew/test-cask.rb` now has `load_delivery_cask`, which uses `from_path` when Homebrew has it and `load` otherwise. The script and both tooling tests that load casks use it. Paths outside a tap pass Homebrew's tap trust check unchanged.
- **OpenTelemetry:** GO-2026-6974 is fixed in `go.opentelemetry.io/otel/sdk` v1.47.0. The indirect `otel`, `log`, `metric`, `sdk`, `sdk/log`, `sdk/metric` and `trace` modules move to v1.47.0.

## Evidence

- **Before:** on `main` (a3336c2) with Homebrew 7.0.16-36, `TestHomebrewNativeAppLifecycle`, `TestHomebrewAppCaskPreservesQuarantineWithFixtureApp` and `TestManagedCaskLoaderRejectsHookDrift` fail with "Loading casks from paths, URLs or Ruby source is not supported". `govulncheck` reports GO-2026-6974 as called.
- **After:** the three tests pass, and the drift test still rejects every changed cask. `govulncheck` reports 0 affecting vulnerabilities. `go build ./...`, `go vet` on the tooling, remote and command packages, and the `internal/remote` tests pass.

## Next steps

1. Open the companion tap PR copying `test-cask.rb` to `Dunky13/homebrew-agent-job-queue`, whose refresh workflow fails since 2026-11-24 for the same reason. Merge it before this PR, as `docs/homebrew-delivery.md` requires.
2. Rebase open PRs onto `main` after this merges.
