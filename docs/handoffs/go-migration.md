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

## CI follow-up and actual app verification

[Source CI run 39477254885](https://github.com/Dunky13/agent-job-queue/actions/runs/39477254885) passed commit policy, lint, vulnerability scanning, web tests, and the Apple Silicon runtime suite. The Intel runtime suite failed during enabled credential reconfiguration: remote shutdown exhausted its tool deadline and reported a busy service registry. The universal installer compiled and signed successfully, then failed with `hdiutil: create failed - Resource busy`.

An initial probe using real cost-14 hashing exhausted a short lifecycle budget before shutdown began and reproduced the tool-deadline error. The permanent regression injects lifecycle expiration during preparation, making the ordering check independent of CPU speed. It failed before the fix and passes for enabled and disabled configuration, a concurrent disable, and invalid input. Configure now prepares credentials before starting the existing 30-second service deadline and acquiring the control lock. It rereads authoritative enabled state under that lock before publishing configuration. Password hash cost and service deadlines remain unchanged. The exact registry contention branch from the hosted failure was not individually traced.

The packaging log did not identify the process holding the resource. Packaging now flushes staged writes, explicitly selects HFS+, disables image indexing, and retains verbose phase diagnostics. These are contention mitigations, not proof of the original holder. The [runner-images investigation](https://github.com/actions/runner-images/issues/7522) documents this class of platform failure. No security service is stopped and no timeout or automatic retry is added. An alternative mount-free hybrid image was tested and rejected because it broke strict installer signatures.

CI and the local validation batch now explicitly require a DMG check. It verifies checksums, mounts the image read-only at a private mountpoint, and verifies executable permissions, both architectures, build identities, and strict code signatures of its enclosed installer. Normal detachment is required; failed detach retains the temporary directory and fails validation. Strict signature verification caught the rejected hybrid-image regression that image checksums alone missed. Script regressions cover successful cleanup, verifier failure, failed detach, and an attachment that fails after exposing its simulated mount.

The actual packaged native app was also launched with an isolated user home and real Go queue daemon and scheduler. Its native UI displayed running and waiting jobs, cancelled a running command through confirmation, displayed the next job's real completed output, and applied then restored the queue limit with CLI readback. The installed queue was preserved. This verifies those native interactions in the isolated running app; it does not establish installation or upgrade of the user's existing app.

The final CI-fix validation passed lint and all remote and tooling race tests in 133.098 and 3.138 seconds respectively. The remote suite includes the real enabled reconfiguration flow from the failed Intel job. Both universal formats rebuilt successfully. Installer fixtures passed in 9.375 seconds, actual packaged queue execution in 5.43 seconds, and the actual packaged HTTPS gateway in 5.18 seconds. The explicit mounted-DMG check verified the rebuilt installer and detached successfully. An independent Sol 6.1 high follow-up review found no material issue in the deadline, ownership, packaging, or cleanup changes. Hosted results on the new commit remain separate evidence.

[Source CI run 39481863397](https://github.com/Dunky13/agent-job-queue/actions/runs/39481863397) subsequently passed on commit `5ec350a487b435d671853e449ab7533226dc906e`, including Apple Silicon and Intel runtime/native suites, universal installer inspection, and the aggregate required check. Publication and Homebrew delivery were skipped as expected for a pull request. This run predates the native selection correction below.

## Learned workload row selection

Actual point clicks reproduced a native interaction defect: clicking the empty background of the second workload left the first selected, while clicking its text selected the second. The plain SwiftUI button now defines a rectangular content shape after the full-width frame and padding, covering the whole row label.

The universal native executable rebuilt for Apple Silicon and Intel, and all 134 existing native regression checks passed. After replacing and signing only the isolated app's executable, eleven actual point-click checks passed: empty backgrounds on both rows, left/top/bottom padding, an already selected background, text on both rows, and unchanged selection in the gap and below the rows. Each check read back the displayed detail title. The same empty-background coordinate that missed before the fix selected the second row afterward. The real private Go daemon, scheduler, jobs, and learned data were retained, and the updated app was left running for review. This establishes local interaction behavior; hosted CI on the follow-up commit is separate evidence.

## Local installer artifacts

These version 0.1.0 artifacts include the reviewed command compatibility and CI fixes, and predate the later native row-selection correction. They were built from the local working tree for verification. Their release commit and build fields are null; they are not published releases.

- ZIP: `dist/Agent-Job-Queue-Installer-0.1.0-universal.zip`
- ZIP SHA-256: `4774a9b6f6c24e3e58bbe59b6dccd99304aaa074cfd5fe796ea1f5839539745a`
- DMG: `dist/Agent-Job-Queue-Installer-0.1.0-universal.dmg`
- DMG SHA-256: `298ec754d589e9e0e6107b5c145b55e98a8188369ec0ad59a7f8e8f7ce397df4`

## Verification boundaries

Installer payload tests exercise the Go installation flow with a test-owned launchctl fixture and an extracted universal payload. They then run the actual packaged daemon, client, and scheduler with system-only PATH. This does not establish execution of the packaged installer GUI or its full InstallMain entry point.

Remote payload tests run the actual universal remote executable and its embedded HTTPS gateway. Automated native regressions are headless fixtures; the separate actual native app interactions are recorded above. Intel binaries are cross-built and inspected locally; actual Intel runtime was verified by the pinned hosted run above. Later changes require their own hosted results. Local tests do not establish production installation, Apple notarization, release publication, or Homebrew delivery.

This Mac's filesystem rejects non-UTF-8 directory names with EILSEQ, so the raw-byte cwd/executable/filename success test is explicitly skipped here. Private codec roundtrips preserve those bytes, and actual-daemon tests verify that a raw cwd cannot silently alias a Unicode replacement-character directory. Actual raw argv and environment names/values were verified through the daemon. A successful raw-path execution still requires a filesystem that accepts those pathnames.

The companion Homebrew update is [tap PR #3](https://github.com/Dunky13/homebrew-agent-job-queue/pull/3), with a GitHub-verified signed commit, author-matching DCO and successful hosted validation. Its Go helper and template match this source exactly. Merge the reviewed tap update before the source change; trusted-tooling parity remains enforced. The PR remains a draft, with no live refresh or installation performed.

## Verification entry point

```sh
agent-heavy run --label "Agent Queue validation" -- bash scripts/validate.sh
```

Inspect `agent-heavy status` after a paused recovery before explicitly choosing `agent-heavy resume`. Do not remove locks or discard an unknown process owner to resume work.

This handoff has a complete self-contained [HTML version](go-migration.html).
