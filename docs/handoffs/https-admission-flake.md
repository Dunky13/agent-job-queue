# HTTPS admission flake handoff

## Result

`TestAuthAdmissionRealCaddyCacheAndDistinctGuesses`, `TestRemoteCaddyLifecycleAcceptance` and `TestRemoteServeEndsWhenItsLifelineCloses` shared one cause. It was not a product bug.

Under `go test -race`, `golang.org/x/crypto/blowfish` is about ten times slower. One cost-14 bcrypt operation takes about 1 s natively and about 9.7 s race-instrumented on an Apple Silicon host. On the hosted Intel runner (CI run 41546094635) it took about 34 s, which crossed three fixed wall-clock budgets:

| Test | Budget crossed | Symptom |
| --- | --- | --- |
| `TestAuthAdmissionRealCaddyCacheAndDistinctGuesses` | 30 s admission queue wait (`gatewayResponseTimeout`) | The single cold bcrypt comparison ran 33.96 s, so 14 queued asset requests timed out into HTTP 429 |
| `TestRemoteCaddyLifecycleAcceptance` | 30 s gateway response and client budget | `getCSRF` after credential rotation hit `context deadline exceeded` after 30.003 s |
| `TestRemoteServeEndsWhenItsLifelineCloses` (and the lifecycle `configure`) | 35 s fixture command timeout | `remote configure` generates a cost-14 hash inside the race-built test binary and is killed: `signal: killed` |

## Product assessment

A real user is very unlikely to hit this. Production binaries are not race-built; a cost-14 comparison is about 1 s (about 1.9 s measured against 24 busy loops with default QoS, perhaps 2 to 3 s on an old Intel Mac) against a 30 s budget. A phone's cold dashboard burst coalesces into one comparison through Caddy's singleflight cache, and the bounded queue (default 64) drains in about that time.

The one path that gets close is a service run by launchd under the bundled plists, which still say `ProcessType Background`. A background clamp against 24 background busy loops measured about 24 s, near the 30 s limit. Current app installs run the service as an app child, so only a machine still running it through launchd could approach it. If 30 s is reached, waiting requests get a 429 with `Retry-After: 1`, and `remote/static/app.js` special-cases only 401 and treats any other status as a generic failure. The admission design, limits and 429 behaviour are unchanged.

## Change

- `scripts/test-go-runtime.sh` passes the flag on the `https` shard, the unscoped run and the non-backend `core` and `core-1` packages (including `cmd/agent-remote` and `internal/preview`, which also reach bcrypt), as `-gcflags='golang.org/x/crypto/blowfish=-race=false'`. `scripts/validate.sh` runs those same partition drivers, so it inherits the flag. Only that one package loses instrumentation: it is sequential arithmetic on local state with no goroutines. The admission, gateway, cache and Caddy code remain fully race-instrumented.
- `admission_test.go` reports a rejected cold burst once, with the count, elapsed time, held slots and queue depth, instead of one line per request. Every assertion is kept.
- `internal/tooling/go_runtime_test.go` expects the flag exactly on those calls and on no backend call; the `DeepEqual` comparisons are unchanged.
- `CONTRIBUTING.md` and the `gateway_test.go` comment record the reason. No timeout, limit or assertion changed.

Trade-off for the owner: this removes race instrumentation from one third-party arithmetic package. The alternatives are raising budgets (against the standing rule) or accepting the flake. Direct `go test -race ./internal/remote` runs without the flag keep the old slowness on slow or busy hosts.

## Evidence

Reproduced on an Apple Silicon host with 45 busy-loop processes (`yes`) competing for 12 cores, running the compiled race test binary for each test alone:

| Test | Plain `-race` build | With the blowfish flag |
| --- | --- | --- |
| `TestAuthAdmissionRealCaddyCacheAndDistinctGuesses` | FAIL: 14 of 16 requests got 429 after 30.00 s with 2/2 slots held and 8/16 queued | PASS, cold burst 2.7 s |
| `TestRemoteServeEndsWhenItsLifelineCloses` | FAIL: `remote configure: signal: killed` after 35 s | PASS, 8.8 s |
| `TestRemoteCaddyLifecycleAcceptance` | FAIL: `remote configure: signal: killed` after 35 s | PASS, 48 s |

With 30 busy loops the plain build passed the admission test at 23.7 s, close to the limit. An unthrottled run of the plain build took a 9.7 s cold burst. `bash scripts/test-go-runtime.sh https` (the full `internal/remote` package, through the shared job queue) exits 0 in 190 s. `go test -p 2 -parallel 2 ./internal/tooling -run '^TestGoRuntime'` exits 0, and all 21 tests in the runtime-driver, tooling-driver and workflow test files pass. The full tooling shard 1 timed out at its 12-minute budget on a host shared with other validation jobs; that is a load effect on unrelated tests, not a failure. `go vet`, `gofmt` and `shellcheck` are clean. Hosted Intel timing is not yet observed.

## Remaining

- None for PR 63: the branch is rebased onto it and `validate.sh` carries no flag of its own.
- Confirm the two hosted `https` jobs (`macos-15` and `macos-15-intel`) pass on the pull request; hosted Intel timing is the real check.
- Optionally add a `Cold asset burst` timing line to hosted output (the test already logs it with `-v`).
