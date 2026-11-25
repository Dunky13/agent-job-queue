# CI cache and backend shard balance handoff

## Result

On PR #89, `macOS tests (macos-15-intel, core-1)` hit its 20-minute budget (run 45658284838, job 139360878597). It ran about 20% slower than main, but main was already using 1018 s of that budget. Three causes stacked up, and this change fixes each at its root. No test, race instrumentation, worker count, `go test -timeout` or job budget changed.

| Cause | Evidence | Change |
| --- | --- | --- |
| setup-go stored one Go cache per OS, architecture, Go version and `go.sum`. Whichever job finished first wrote it, and nothing ever updated it. On main, the Intel entry (184 MB) came from tooling shard 1, so core-1 race-compiled Caddy, `cmd/*` and the backend test binary cold on every run. | On Nov 4 the non-backend phase took 33 s with a 575 MB cache. On Nov 25 it took 284 s, of which `cmd/agent-heavy` to `cmd/agent-remote` alone took 204 s. Job 139350989928 logged "Cache saved with the key ... 7279fcc". PR #88's first run after its `go.sum` change had no cache, and its core-1 took 1129 s. | Each Go job restores its own cache, keyed by job, matrix entry, `go.sum` and commit, and falls back to the newest earlier entry. Only successful pushes to main save, so PRs write no caches. Before this change, PR-ref setup-go caches used about 2.1 GB of the 10 GB. |
| Backend shards alternated sorted names, so adding tests moved half the suite between shards. | Intel serial test time: shard 1 went from 373 s to 580 s and shard 2 from 479 s to 369 s, while 74 tests were added. | Greedy split by measured duration, from `scripts/backend-test-weights.tsv`. On Nov 25 timings it simulates to 474.6 s / 474.6 s. |
| Core-1 also ran every other runtime package, adding their cold race compile on top of the heavier shard. | 284 s on main and 334 s on the PR run, cut off. | `https` runs those packages in a second invocation after `internal/remote`, reusing its race-built Caddy dependencies. The gateway still runs alone, because its 30 s admission budget assumes no competing test binary (see `https-admission-flake.md`). |

Local `core` now means the unsharded backend only. `core` + `https` + `installer` covers the original scope exactly once, just as the four CI partitions do.

## Expected effect (projected, Intel, main speed)

- **core-1:** about 90 s setup, about 20 s inventory compile, and a backend shard of about 475 s. That is about 590 s, against 1018 s before. The shard uses about 66% of the 12-minute test timeout, against 81% before and 95% on the PR #89 runner.
- **https:** about 330–450 s, against 291 s before and a 1200 s budget.
- **Runner minutes:** PR runs save several macOS runner-minutes. On main the gain is headroom, not minutes, because the cache saves cost about as much time as the compiles they avoid.

## Validation

- `go test -p 2 ./internal/tooling -run 'TestMacOS|TestWorkflow|TestValidateBatch|TestDistribution|TestNotarization|TestGoRuntime|TestStage'` passed.
- New tests cover the measured-weight split, the per-shard unweighted count, stale and missing weights, and refusal of malformed weights before any test runs. Updated tests pin the two `https` invocations and the backend-only core.
- `actionlint`, `shellcheck` and `queue-tool check-source` passed.
- A stub `go` run over the real 717-test inventory gave shards of 359 and 358 tests, weighted 474 585 ms and 474 584 ms, with 0 unweighted.
- Lint and both tooling shards: see the PR description for the queued result.

## Remaining work

1. **After merge,** compare per-phase times on the second main run, the first one that restores a main-saved cache, on both architectures. The PR's own run is cold, because no main cache exists yet under the new keys.
2. **Refresh `scripts/backend-test-weights.tsv`** from both core shard logs when the printed weight sums drift apart, or once the backend speedup work lands.
3. **Watch total backend time.** It grew about 16 s per shard per week between Nov 4 and Nov 25. The remaining headroom on a slow Intel runner (about 150 s) lasts about 9 weeks unless the slow tests get faster. Faster tests are tracked separately as the backend test speed work.
4. **`distribution.yml`** still uses setup-go's cache. It has 39% headroom, so it was left unchanged.
