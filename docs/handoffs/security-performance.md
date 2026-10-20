# Security and performance implementation

Date: 2026-10-20. Baseline: `175841d61e67832cf8731df4cfd31b294a318b20`, source PR #11. This implements S1, P1, P2, P3, P4, and H1 from the preceding assessment. The queue daemon remains independent, arbitrary commands are never automatically replayed, and process ownership remains authoritative. No production installation, merge, or release is established by this handoff.

## Authentication admission: S1

The gateway wraps Caddy's existing authentication handler with bounded admission using `golang.org/x/time/rate`. Authentication, cost-14 bcrypt, password caching, challenges, TLS, and CSRF retain their existing owners. Slots cover actual authentication work and are released before proxy work. Cancellation while bcrypt is executing retains the slot until the comparison returns.

Requests use the actual socket peer IP, with IPv4-mapped addresses normalized. Forwarded headers cannot select an identity. Idle entries expire in last-seen order; a full table rejects new peers instead of resetting existing limiters. Excess requests receive HTTP 429 and `Retry-After`. A bounded waiting queue accommodates cold asset bursts without creating unbounded background work.

| Configuration field | Default | Accepted range |
| --- | --- | --- |
| `max_concurrent` | 2 | 1-32 |
| `requests_per_second` | 4 | 0.1-100 per peer |
| `burst` | 64 | 1-512; also the global waiting bound |
| `max_peers` | 1024 | 1-65536 |
| `peer_ttl_seconds` | 600 | 1-86400; at least burst / rate |

Supply all five fields in optional `auth_admission` configuration JSON on private stdin to `agent-remote configure`, alongside its required password and optional port. Omitting the object retains existing limits; a new installation uses defaults. The native password form preserves custom limits. `agent-remote status --json` exposes the effective limits in `authAdmission`. Passwords remain absent from argv and status output. See the [operator documentation](../../README.md#phone-access).

Concurrent distinct four-character guesses also reproduced a data race in the pinned Caddy v2.11.7 password-cache key construction. Its direct bcrypt-string provisioning left spare byte capacity, allowing concurrent append operations to share storage. Generated gateway configuration now uses Caddy's supported base64 hash representation, whose decoded bcrypt slice has exact capacity. Durable stored hashes remain unchanged. Neither Caddy source nor its authentication implementation was replaced. No authentication bypass was demonstrated.

The unmitigated real-provider probe failed the race detector. Supported encoding passed the equivalent probe. Permanent tests cover 500 distinct short guesses against known and unknown accounts, exact cache results, a real public four-character bcrypt credential, and sixteen concurrent cold asset requests requiring one real comparison. Rate, peer capacity/expiry, spoofed headers, waiting cancellation, slot ownership, panic cleanup, and release before proxying have deterministic regressions. The final full remote race suite passed in 194.117 seconds, including actual private TLS, HTTP/2, CSRF, credential rotation, and service lifecycle acceptance. Environment-driven optional tests retain their explicit skips.

## Daemon operational queries: P1

Schema version 2 adds a partial index over `phase != 'done'`. Scheduling, restart reconciliation, and shutdown checks use a dedicated unfinished-job query. `queued`, `starting`, `running`, and `outcome` remain operational; outcomes leave that set only after ownership-checked cleanup. Public snapshots retain completed history. The [SQLite partial-index documentation](https://www.sqlite.org/partialindex.html) describes the indexed subset used here.

Version 1 migration uses a serialized transaction, rereads the version under its write lock, and publishes the index and version together. Tests cover concurrent openers, failed migration rollback, all four unfinished phases, stable ID order, query-plan index use, retained snapshots, future/invalid schema, and private-file validation. The lifecycle tables and worker/outcome fences are unchanged.

Matched measurements used identical fixtures and benchmark code against the baseline and implementation, three rounds of fifty iterations each on Apple M2 Pro, with Go workers limited to two. Values below are medians of the rounds. Two fabricated unfinished controls accompany retained completed history; they never launch commands.

| Retained completed jobs | Tick before | Tick after |
| --- | --- | --- |
| 0 | 0.052 ms | 0.057 ms |
| 100 | 0.587 ms | 0.053 ms |
| 1000 | 5.265 ms | 0.052 ms |
| 10000 | 49.194 ms | 0.059 ms |

At 10,000 retained jobs, allocations per tick fell from 34.388 MB to 10.425 KB. A separate actual private-daemon benchmark spread small limit RPCs across reconciliation intervals: mean latency fell from 9.308 ms to 0.279 ms, and p95 from 46.912 ms to 0.579 ms. History snapshots still cost work proportional to their contents. These results establish the measured paths under these fixtures, not a whole-application speed multiplier. A focused lifecycle and operational-query race run passed in 105.166 seconds.

## Profile collections: P2

Each controller collection owns one finite database connection. Unchanged descriptions are checked without acquiring a write transaction; changed descriptions are written in internal batches of at most 128. Prediction inputs and matches are read together after terminal samples have been recorded. Query family sets use a JSON parameter and SQLite `json_each`, avoiding per-family host-parameter limits.

The collection retains no global prediction cache. Its same-connection SQLite `data_version` invalidates both operator corrections and newly recorded samples. Before admission, a validated read transaction serializes the durable grant write against profile changes. It is released before queue RPC. Read-only consumers cannot write; reusable sessions recheck directory/database identities and sidecars. Existing full synchronization, DELETE journaling, and bounded busy waits remain in place.

Regressions cover merged groups, reset and retained history, machine and wildcard floors, read-only legacy stores, unsafe replacements/permissions/sidecars, more than 32,766 families, the 128-description boundary, concurrent corrections and completions, and actual controller rejection of stale predictions. A same-tick large completion changes its waiting sibling's prediction before selection. The database read lock was tested against a real competing writer.

Three matched rounds of one hundred iterations used fabricated identities and measured actual driver opens:

| Profile fixture | Before | After | Connection opens before / after |
| --- | --- | --- | --- |
| One repeated identity | 0.785 ms | 0.552 ms | 3 / 1 |
| Thirty repeated identities | 23.043 ms | 0.628 ms | 90 / 1 |
| Thirty distinct identities | 25.361 ms | 1.876 ms | 90 / 1 |

For thirty distinct identities, allocation fell from 1.727 MB to 624 KB. The one-item fixture allocated slightly more, 56.6 KB to 60.4 KB. Existing independent-call performance remained approximately unchanged. A separate complete controller-tick fixture, including real metadata/filesystem/lock/status/grant work with simulated queue snapshots and resource counters, improved from 48.629 ms to 25.398 ms for thirty jobs; driver opens fell from 92 to one. The optimized measurements were reconfirmed in three fixed rounds after the final production-helper correction. These are local fixture timings with normal host variability, not a live host-resource benchmark.

## Native request lifetime and capture: P3 and H1

Learning selection retains its task, cancels superseded reads, and waits for their private clients to be reaped before starting another. Generation checks remain. Only explicitly discardable status/profile reads observe cancellation. Accepted mutations retain their response and deadline behavior; UI cancellation neither cancels independent daemon commands nor retries a mutation.

The private controller transport drains combined stdout/stderr through a nonblocking pipe while the child runs. It enforces the existing 2 MiB cap during collection, with no response capture file on disk. Bounded input writes and output draining proceed together. Every exit path reaps the direct private controller child, escalating termination when necessary, while independently running daemon work remains separate. A final drain closes the race between an empty read and observing child exit.

Actual private-process regressions cover cancellation before launch and in flight, termination resistance, rapid-selection overlap, stdout/stderr/combined overflow before deadline, an exact-cap response, final bytes, stalled and continuous output, interleaved stdin, empty-input EOF, handle cleanup, and inherited descriptors held by an independent daemon. Mutation cancellation was tested for accepted completion. The latest focused transport run passed all 32 checks.

## Native snapshot preparation: P4

Snapshot decode, content comparison, and section preparation execute in a detached utility task. The main actor publishes one prepared value after repeating cancellation and mutation-revision checks. Refresh coalescing, unchanged-content suppression, selection/position ordering, and atomic publication remain covered. Debug validation asserts that decoding is off the main thread.

Synthetic response-to-publication measurements used the original and changed QueueStore with identical 1,000-job payloads. Three alternating paired rounds measured medians of 14.315 ms before and 14.387 ms afterward. An initial slower sample was not sustained; a separate priority comparison found no demonstrated benefit from raising utility priority. Total preparation latency is approximately unchanged. The intended improvement is removal of this work from the main thread, not a claimed decode CPU speedup. Small normal snapshots remain inexpensive.

Two 12-second Time Profiler recordings exercised the actual native app binaries with a private controller serving the same artificial 1,000-job, 860,957-byte response through normal status polling. Before the change, JSON decoder stacks appeared in 142 main-thread samples and no worker samples. Afterward, they appeared in zero main-thread samples and 73 worker samples. Sampling weight was one millisecond. The recordings establish the thread move in a running app; their polling phases and sample counts are not normalized and do not establish a CPU percentage, frame-rate gain, or response-time multiplier. The isolated profiling apps were stopped after capture.

## Integrated verification and delivery boundaries

The focused native suites passed 158 checks before the final drain refinement; the latest transport rerun and universal Apple Silicon/Intel build passed afterward. An independent Sol 6.1 high source review covered all six findings and the final batching changes, with no remaining material finding. It ran whitespace checks, not duplicate test suites. A production-only build failure caused by a test-helper reference was caught by integrated validation and corrected to the standard library's map cloning; the explicit production build and focused races then passed. Final integrated results and actual native app checks are recorded when complete below.

The final complete `scripts/validate.sh` batch passed source/workflow/shell lint, Go vet, the vulnerability scan, every Go race suite, developer and Linux tool builds, seven dashboard schema tests, sixteen browser tests, all 170 native checks, universal packaging, and installer fixtures. Backend and remote race suites passed in 128.715 and 158.566 seconds. The scanner found zero reachable vulnerabilities; uncalled advisories remained reported, with none suppressed. Actual packaged queue execution passed in 7.66 seconds and packaged HTTPS in 4.85 seconds. The DMG checksum, read-only mounted signatures/architectures/identities, and normal detach passed.

The isolated running app and its private daemon/scheduler were then refreshed with the new binaries. Its schema 1 database migrated to version 2 with the operational index, preserving both existing jobs and the concurrency setting. Eleven real background/text/padding/gap selection checks passed against the updated controller. The native app displayed one new running and one waiting command, then their successful completion; its detail view displayed the second command's actual output. The updated app, daemon, and scheduler remain running for review. These are local runtime results; hosted CI on the eventual PR head is separate evidence.

Reproduce the complete validation through the shared queue:

```sh
agent-heavy run --label "Agent Queue validation" -- bash scripts/validate.sh
```

Finite developer benchmarks are `BenchmarkQueueHistoryTick`, `BenchmarkQueueHistoryRPC`, `BenchmarkProfileCollection`, and `BenchmarkProfileControllerTick` in `internal/backend`. Use fixed rounds through the shared queue and retain fixture shapes, worker limits, raw results, and before/after revisions. Synthetic model timings and a single profiler recording are not a substitute for live workload profiling.

All local fixtures use private temporary state and artificial workload data. The installed queue and unrelated jobs are preserved. Existing signing/DCO, mandatory CI, companion tap ordering, and the human merge gate remain required. Main-only artifact publication, Homebrew delivery, production installation, and Apple notarization are separate evidence.

This handoff has a complete self-contained [HTML version](security-performance.html). The earlier [Go migration handoff](go-migration.md) records the migration and prior CI evidence.
