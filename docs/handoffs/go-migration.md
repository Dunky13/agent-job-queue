# Go migration and embedded queue handoff

Date: 2026-10-20

## Outcome and scope

The backend, queue daemon, and repository delivery tools use Go. The native macOS app remains Swift and the dashboard remains JavaScript. An independent Go daemon owns the queue through a private Unix socket and a separate SQLite database. Caddy's Go modules provide HTTPS and Basic authentication inside the remote server.

Installed users need neither Pueue, Python, uv, Homebrew, a separate Caddy executable, nor a Go toolchain. The standalone installer works offline after its artifact is downloaded. Homebrew remains an optional distribution route.

This handoff records local implementation and validation. Source review and the companion Homebrew tooling update are separate delivery steps. It does not establish production installation, merge, or release.

## Queue package evaluation

Two delegated GPT-6.1 Sol high investigations examined released source and the actual queue contracts. Goqite v0.4.0 was tested through its Go interface using the existing modernc SQLite driver. After consumer B received an expired claim, consumer A could still extend and delete that same message:

```text
A and B received same ID: true; received=2; stale A Extend succeeded
stale A Delete succeeded; B's durable row count=0
MaxReceive=1 redelivery after expiry: false
```

Goqite supports enqueue-time priority and transactions. Its Extend and Delete operations identify a message by queue and ID, without a claim generation. MaxReceive=1 prevents redelivery, but leaves durable command attempts, cancellation, reordering, retained outcomes, and process reconciliation to this application. See the [release source](https://github.com/maragudk/goqite/blob/v0.4.0/goqite.go) at commit 471f9d49ce356737fc756a287007d7b4c54c61e1.

Backlite v0.8.0 requires a positive recovery timeout. Its selection query can return expired claims regardless of attempt count, and its claim/completion operations do not fence a claim generation. It also lacks mutable priorities. This conclusion is based on source inspection; Backlite was not runtime-tested. See its [configuration validation](https://github.com/mikestefanello/backlite/blob/v0.8.0/client.go) and [queue queries](https://github.com/mikestefanello/backlite/blob/v0.8.0/internal/query/query.go), commit ce7da9d35d56f217ec9c9b6fa9aa232811d28b76.

The implementation therefore reuses the existing pure Go SQLite driver for one authoritative command lifecycle. Neither queue package is a runtime dependency. This choice fits arbitrary commands that cannot safely be replayed; those libraries remain useful for idempotent background tasks.

## Execution and recovery

- Submission tokens identify one durable job. Lost acknowledgements retry the same token; an unresolved response preserves the potentially accepted payload.
- The daemon records an immutable attempt before spawning a worker. The worker must atomically claim that attempt before executing any user command. Duplicate and stale workers are rejected.
- Workers run in independent process groups and record their own exit outcomes, including while the daemon is unavailable. Daemon recovery cannot overwrite a newer worker outcome.
- Commands retain literal argv, cwd, environment, stdin EOF, merged output, and exit status. Caller or UI death does not release accepted work.
- Private execution payloads encode argv, cwd, and environment names and values as byte strings. Failed executable starts can try the next PATH candidate; a successfully started command's result is final, including exits 126 and 127.
- Process cleanup retains boot and process birth checks, descendant teardown, shared leases, and resource admission. Cleanup runs outside the daemon mutex to preserve lock ordering with the resource controller.
- An unclean restart pauses automatic dispatch. An uncertain attempt is never automatically replayed. Unknown outcomes fail conservatively; unresolved process ownership retains exclusion.
- Queue state, database sidecars, sockets, locks, payloads, and logs retain private ownership checks. Unsafe links, public files, and special log files are rejected.

Existing CLI names and public JSON fields remain compatible with the native app and dashboard. Priorities, stable ordering, manual starts, limits, cancellation, paused jobs, and adaptive scheduling retain their contracts. The new `agent-heavy pause` and `agent-heavy resume` commands control future automatic starts without stopping active work.

## Installation and migration

The first migration from Pueue requires all running and waiting jobs to finish, including process cleanup after a Done result. Migration from Python also requires old remote access to be disabled. The installer verifies service ownership and repeats its drain and ownership checks after shutdown, before publishing support files.

The completed legacy snapshot is imported in a transaction. Completed IDs, results, original logs, and the retained ID range survive. The installer requires valid legacy group settings before stopping services or publishing files, including the real paused state and concurrency limit. Learning history, legacy lease configuration, remote credentials, and certificate storage remain authoritative. New installs create no Pueue configuration. Existing legacy configuration is retained as inert migration data; unrelated Homebrew packages are not uninstalled.

Subsequent embedded queue upgrades retain active independent workers and the running daemon. Installation markers, build identities, downgrade protection, atomic executable publication, quarantine preservation, and safe retry behavior remain in place. Enabled Go remote services adopt the embedded gateway through their ownership-checked lifecycle, preserving the bcrypt hash, CSRF secret, and phone-trusted CA.

New state uses `queue.sqlite3`, `run/queue.sock`, and `logs/` within the private state directory. Imported completed logs remain in their original `pueue/task_logs/` directory. The dashboard accepts both owned log locations.

## Validation evidence

An earlier complete queued validation batch passed: source and workflow checks, vulnerability scanning, all Go race tests, developer builds and Linux tool cross-compilation, seven dashboard schema tests, sixteen browser tests, native headless regressions, universal installer inspection, and packaged queue and HTTPS gateway tests. The vulnerability scanner found zero reachable vulnerabilities; it also reported uncalled package/module advisories. No advisory was suppressed.

A later final backend race run exposed an intermittent process-group cleanup failure. Controlled tests then reproduced two real macOS observation races: the last zombie was reaped between a denied kernel probe and a complete process snapshot, and a zombie group shrank between complete snapshots. Both regressions failed before the fix and passed afterward. An empty observation now requires a fresh ESRCH; surviving zombies must retain the same PID and birth identities. Live, new, reused, incomplete, and uncertain identities continue to retain ownership. The original crash test and deterministic safety regressions passed ten repetitions under race detection in 26.482 seconds. The exact branch behind the original intermittent failure was not individually traced.

Before the delivery review, a queued batch passed source checks and the complete backend race suite in 97.563 seconds, rebuilt both universal installer formats, and passed all installer fixtures in 8.335 seconds. Artifact inspection verified both architectures, bundle identities, ad-hoc signatures, bundled licenses, and exclusion of private state. The packaged queue flow passed in 8.23 seconds and the packaged HTTPS gateway in 4.96 seconds.

The delivery review reproduced two further command compatibility defects: PATH search stopped at a denied executable, and JSON serialization replaced non-UTF-8 argument bytes. Both were corrected. Focused actual-daemon regressions passed in 9.865 seconds, including failed-start PATH fallback, final exits 126/127 without replay, raw argv and environment names/values, payload rejection, scheduler and metadata decoding, and stable existing UTF-8 workload hashes. Exact private payload version spelling also has a failing-before/passing-after regression.

The standalone tap tests initially failed after copying outside the app module because they imported internal app packages. Source-only parity tests now stay separate, and an offline copy test enforces the shipped boundary. Source packaging, delivery and tooling race tests passed; the actual standalone tap tests passed offline under race detection in 1.531 seconds.

The complete final Go race run passed across all commands, backend, delivery, preview, HTTPS, tooling, standalone tap and embedded assets. The backend suite passed in 141.317 seconds and the HTTPS suite in 185.897 seconds. Final lint, developer builds and Linux preview/tool cross-builds passed. Both universal installer formats were rebuilt; all installer fixtures passed in 8.501 seconds, actual packaged queue execution in 5.01 seconds, and actual packaged HTTPS in 5.07 seconds. Universal architectures, identities, ad-hoc signatures, licenses and private-state exclusion were verified again.

Focused checks have passed for literal arguments and environment, exit status, EOF and final output, concurrent exclusion, caller death, descendant cancellation, external legacy ownership, manual starts and durable limits, paused queues, daemon crash recovery, worker completion during an outage, unclaimed attempts, duplicate workers, lost acknowledgements, stale writes, corrupt schema, numeric ID overflow, private files and sockets, unsafe log targets, and cleanup lock ordering.

Installer fixtures cover fully drained migration, offline compressed legacy state, cleanup receipts and markers, late state changes, retries, retained settings and credentials, active embedded upgrades without external tools, and fresh installs without legacy configuration. Actual private launchctl output was inspected and tested, including paths containing spaces. Homebrew cask renderers agree and declare no runtime formula dependencies.

Independent GPT-6.1 Sol high reviews covered daemon recovery, stale writes, installer ownership, package suitability, process liveness, and delivery tooling. The final Standards review found no material violation; the Spec review's two confirmed command defects were corrected and reviewed again without a remaining material execution or ownership finding. The tap review's outdated recovery instructions were also corrected. Source review is separate from runtime evidence.

## Local installer artifacts

These version 0.1.0 artifacts include the reviewed command compatibility fixes. They were built from the local working tree for verification. Their release commit and build fields are null; they are not published releases.

- ZIP: `dist/Agent-Job-Queue-Installer-0.1.0-universal.zip`
- ZIP SHA-256: `7e8b8ca7ae67ba288c3b418e176e56985ba712fab2daa3224263c5b127ea2d76`
- DMG: `dist/Agent-Job-Queue-Installer-0.1.0-universal.dmg`
- DMG SHA-256: `13c36b3f759a6fe36e66108dd84030e2c7709bbc72f8698f11c82dad99cc4caf`

## Verification boundaries

Installer payload tests exercise the Go installation flow with a test-owned launchctl fixture and an extracted universal payload. They then run the actual packaged daemon, client, and scheduler with system-only PATH. This does not establish execution of the packaged installer GUI or its full InstallMain entry point.

Remote payload tests run the actual universal remote executable and its embedded HTTPS gateway. Native regressions are headless fixtures. Intel binaries are cross-built and inspected; actual Intel runtime and hosted CI require their separate runners. Local tests do not establish production installation, Apple notarization, release publication, or Homebrew delivery.

This Mac's filesystem rejects non-UTF-8 directory names with EILSEQ, so the raw-byte cwd/executable/filename success test is explicitly skipped here. Private codec roundtrips preserve those bytes, and actual-daemon tests verify that a raw cwd cannot silently alias a Unicode replacement-character directory. Actual raw argv and environment names/values were verified through the daemon. A successful raw-path execution still requires a filesystem that accepts those pathnames.

The companion Homebrew update is [tap PR #3](https://github.com/Dunky13/homebrew-agent-job-queue/pull/3), with a GitHub-verified signed commit, author-matching DCO and successful hosted validation. Its Go helper and template match this source exactly. Merge the reviewed tap update before the source change; trusted-tooling parity remains enforced. The PR remains a draft, with no live refresh or installation performed.

## Verification entry point

```sh
agent-heavy run --label "Agent Queue validation" -- bash scripts/validate.sh
```

Inspect `agent-heavy status` after a paused recovery before explicitly choosing `agent-heavy resume`. Do not remove locks or discard an unknown process owner to resume work.

This handoff has a complete self-contained [HTML version](go-migration.html).
