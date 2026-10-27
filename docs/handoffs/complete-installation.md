# Complete installation and live upgrades

## Outcome and scope

The DMG, ZIP and Homebrew complete installation before opening Agent Queue. Setup starts automatically in the installer, with no application-level Install confirmation or second runtime setup wizard. Ordinary runtime launch has no installation or repair authority. Optional agent integration opens only from its menu action.

This is a local implementation handoff. No release, Apple submission, tap publication, live installation, endpoint policy change or protected-Mac SentinelOne acceptance was performed.

## Installation channels

| Channel | Behavior |
| --- | --- |
| DMG | Contains the complete signed installer. Opening it starts setup and opens the runtime only after completion and readiness. |
| ZIP | Contains the same complete installer and bundled helpers. |
| Homebrew install and upgrade | Runs the noninteractive vendor Installer artifact directly, then delivers its installer app and CLI links. |
| Runtime app | Checks installed identity and background-service authorization. It cannot install, recover, replace helpers or register services during ordinary launch. |

The installer bundles the universal app, queue daemon, resource controller and HTTPS runtime. Installed users need no Go, Xcode, Python, Pueue, uv or separate Caddy executable. Historical drag-only disk images remain readable by verification tools; new disk images expose the complete installer.

## Automatic repairs and retained state

Compatible upgrades finish while running and queued jobs continue. The installed daemon, target executable and all outstanding callers must support handover protocol 1 and queue schema 2. Older releases and callers require one drained transition before live upgrades become available. Unsupported schemas and uncertain ownership retain the drain or refusal path. Explicit removal and external replacement preparation always require drain.

The old daemon freezes dispatch and unaccepted mutations, durably commits an exact target hash and attempt inventory, then releases leadership. The successor validates the same boot, database inode, original daemon death and immutable job identities before consuming that transfer once. Running workers finish their original attempts; original callers reconnect and receive their own output and exit status. Pause state, parallel limit and queued jobs survive. Unplanned crashes retain the existing pause policy; a consumed receipt cannot authorize another restart. New mutations can be briefly rejected before acceptance during the swap.

The installer retains whole previous signed app bundles under the visible `Applications/Agent Queue Runtimes` directory in the user's account. Private receipts bind each exact bundle and its original workers and callers, including queued callers. Runtime startup and job completion never remove executable code. A later installer transaction reclaims a generation only after its exact job outcomes are terminal and original processes are proven gone; ambiguous custody is retained.

Individual inspections, repairs and installation attempts retain finite budgets; required queue-drain and operating-system approval waits have no aggregate five-minute cutoff. Released handover custody remains valid across a long approval delay. An abandoned prepared handover expires and restores dispatch.

Supported repairs restore verified missing legacy service files, clean completed jobs through ownership-checked cleanup, start a verified stopped legacy Go service and disable incompatible phone access when migration requires it. Legacy callback replacement binds the exact original configuration revision and retains a private backup. Verified legacy service definitions are backed up before incompatible options are retired. An inactive customized Pueue callback retained by an already managed Go installation remains unchanged because that runtime does not execute it.

Interrupted managed installation completes the exact retained candidate and its publication custody before applying a newer build. Missing or substituted receipts, candidates or backups fail without discarding evidence. Changed retained backups block service changes. Uncertain command attempts are never automatically replayed.

## Homebrew trust and staging

The cask uses Homebrew's documented `installer script:` artifact, rather than a sandboxed flight-step hook. The script runs trusted system tools to copy the complete installer into private staging before executing vendor code. It preserves quarantine and shared directory permissions.

The copied installer, inner app and controller executables must satisfy Developer ID, notarization and the public publisher team requirements. Gatekeeper assesses the copied installer. The signed outer and inner bundle versions and build numbers must match the requested cask receipt before the controller runs. This prevents a different valid publisher-signed release in writable Caskroom staging from silently satisfying the requested version.

Predecessor removal during Homebrew upgrade retains the independent runtime and services. Homebrew uninstall retains runtime state under the existing removal contract; explicit runtime removal still requires its ownership-checked service-removal command.

Both trusted cask renderers and the real Homebrew DSL contract were updated together. Publisher rotation requires a reviewed template change. Companion tap tooling must be synchronized before publishing a release that uses the new template.

## Approval and failure boundaries

macOS can require background-service approval. The installer shows a Login Items shortcut, reads service status without repeatedly registering denied services and resumes after approval. A native registration exit 2 is recognized only after valid read-only approval-state confirmation. Fast approval and approval while completing an older pending candidate are covered.

Live handover preserves deliberately paused or stashed jobs without resuming them. A legacy transition needing drain can still require an operator decision for such work. Unrelated ownership, damaged configuration, uncertain processes and endpoint quarantine remain visible failures. The installer does not delete locks or ownership evidence to make progress. SentinelOne policy and quarantine remain intact. Moving setup into the installer does not establish a SentinelOne false-positive verdict or guarantee endpoint acceptance; a newly signed release still needs an approved test on the protected Mac.

## Validation

- Final local lint, public-source portability, formatting/vet, shell/workflow/Ruby checks and reachable-vulnerability scan passed. The scan found no reachable vulnerable calls.
- The complete Go race scope passed, including backend (544.424 seconds), tooling (566.422 seconds), delivery, remote, native-service metadata and the standalone trusted tap packages.
- Six real-binary private installer scenarios passed with race detection: active work, queued-only work in a paused queue, lost commit acknowledgement, failure before commit, failure after release and failure after successor adoption. They verify completed replacement while original callers remain alive, subsequent new work, original output and exit code, no replay, full previous bundle custody and later installer cleanup.
- Queue handover race regressions passed for socket outages, queued work, pause/limit, exact target and one-use custody, arbitrary released-receipt approval delay, terminal live callers, caller tampering, prepared expiry and workers waiting for a shared lease. The second scheduling-read outage was reproduced before its fix and verified with both positive and fail-closed cases afterward.
- Runtime-generation regressions passed for queued callers, active workers and outcomes without workers; exact caller birth, terminal outcome consistency, interrupted transfers, multiple generations, substituted evidence and safe later reclamation. The final broad race scope covers the shared strict journal decoder and the last-caller-finishes-before-prepare regression.
- Earlier graphical fixtures verified automatic installer start and read-only runtime status using a harmless private payload. They did not register native services. The final source review resolved the caller-retention, worker-admission and empty-preparation journal findings and reported no remaining findings in its bounded closure check. Cross-provider review was not performed.
- The complete `scripts/validate.sh` batch passed with exit 0. Developer and Linux tool compilation, all seven web-schema and sixteen browser tests, full native regressions, universal ZIP/DMG build and verification, packaged payload and app-bundle installation, packaged HTTPS gateway and the actual Homebrew Installer artifact with private native lifecycle adapters all passed. The freshly packaged helper also passed running, paused queued-only and interrupted-after-adoption upgrades (19.41 seconds). These fixture adapters do not establish real native registration or Apple trust acceptance.
- Hosted ARM/Intel acceptance is tracked by the exact PR-head checks. Real Service Management worker survival, Developer ID release signing, Apple notarization, Gatekeeper release qualification, public release, tap publication and SentinelOne acceptance remain unverified. Local installer artifacts are development builds with ad-hoc signing.

The earlier complete-installation work also corrected a recovery-test fixture that recreated timestamped execution metadata. It now reuses the exact advertised executions; the production immutability guard is unchanged.

## PR 44 Intel fixture follow-up

The first hosted Intel core run on `9817e5e` failed in the paused-queue upgrade fixture at its post-upgrade resume with `Queue control deadline expired`. The scenario reused one control client whose absolute 15-second RPC deadline had been created before the upgrade. The 60-second scenario context remained active; the failure was not the overall test timeout. Production installer controls already construct a fresh bounded client per request.

The fixture now uses a new per-operation control budget, bounded by the unchanged scenario context, for controls and snapshot polling. A deterministic regression explicitly expires the earlier client and confirms that its read is rejected while fresh post-handover controls succeed. Neither production deadline guards nor test timeout values are relaxed. Expiring the original cached client reproduced the failure locally; the corrected queued scenario passed with race detection in 27.212 seconds, and bounded source review reported no remaining findings. All six corrected installer scenarios then passed with race detection (142.97 seconds), followed by the packaged paused-queue scenario (10.67 seconds) and the public-source boundary check. The original failure log remains attached to the [Intel core job](https://github.com/Dunky13/agent-job-queue/actions/runs/40526143087/job/122266148881).

## PR 44 Intel fixture cost and phase follow-up

The next Intel run on `36fd7d0` reached the fixture's 60-second scenario deadline in two cases and later exhausted the backend package's 12-minute budget while a subsequent ordinary test had run for only three seconds. This was a different witness from the stale RPC deadline. The [job log](https://github.com/Dunky13/agent-job-queue/actions/runs/40529719251/job/122281903140) is retained as the failing evidence.

A measured successful fixture case performed 45 full bundle captures and digests. Replacing its copied 24,055,250-byte backend test executable with the actual stripped, race-instrumented product CLI reduced the helper to 20,219,298 bytes. One paired local case changed from 28.40 to 24.78 seconds; this measurement does not establish Intel performance. The CLI is built once per top-level fixture before private HOME changes, with a finite build context. Packaged acceptance still uses the actual helper from the installer archive.

Fresh installation now owns a separate 60-second readiness phase, explicitly cancelled after successful readiness before accepted jobs are created. The live-upgrade phase retains one unchanged 60-second context across all controls, retries, upgrade and original/new caller verification. Production code, ownership checks, race detection and timeout values remain intact.

CI runs the complete six-scenario managed live-upgrade test in a required `installer` runtime matrix scope on both architectures. Core runs every other backend test plus its original runtime packages; HTTPS remains separate. The driver validates the target test inventory before partial execution, and its exact skip applies only to the backend command. Default `all` still runs the original complete runtime scope. Every partition retains two workers, race detection, the 12-minute Go budget and the 20-minute job budget; `ci-required` continues to aggregate every matrix instance. The final phase-owned fixture passed all six race scenarios locally (177.33 seconds) and the actual packaged queued scenario (13.69 seconds). The actual core driver then passed with the remaining backend scope completing in 335.010 seconds. Focused driver/workflow race contracts passed (10.409 seconds), full lint/vet and source portability passed, and bounded review reported no remaining findings. These local results do not establish hosted Intel acceptance.

Run the repository's required finite batch through the shared resource queue:

```sh
agent-heavy run --label "Complete installer validation" -- bash scripts/validate.sh
```

The self-contained HTML handoff and screenshots are local review artifacts supplied in chat and are excluded from version control. The screenshots use private fixtures and simulated jobs.
