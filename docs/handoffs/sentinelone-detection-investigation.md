# SentinelOne first-launch persistence investigation

Investigated 2026-10-26; updated after the user supplied an incident summary and clarified that the affected Mac is a different machine from the analysis workstation. The summary identifies build 94 and repeated SentinelOne quarantine during installation and first launch. Its surviving helper hashes match the public build-94 menu-app helpers independently inspected here. This is a suspected false positive, not a confirmed vendor verdict: the authoritative detection record, exact rule, agent version and policy remain unavailable. No downloaded app or installer was executed and no endpoint settings were changed.

## Updated finding: build 58 compared with build 94

The first-launch change explains why opening the new app repeats installation. The user subsequently identified the earliest quarantine as the separate installer's `--payload` path, prepared by the older updater. With later quarantines also occurring on the app's `--app-bundle` path, self-installation is not a necessary condition for the reported block. The hidden destination directory, temporary-file writer and three service definitions already existed in build 58.

Compared fixed sources `f892944dca4159ff125d2ad841661e1dfd36af96` and `fe05988e07165afd8b23c9edda95a90ff0bf2ad8`:

| Behavior | Build 58 | Build 94 | Interpretation |
| --- | --- | --- | --- |
| Ordinary app launch | Starts the menu UI and polling | Checks embedded setup and can run installation automatically | Material change: launching can now copy executable files and install services |
| Packaged Install button | Embedded controller runs `install --payload` | Same entry point remains | A separate explicit installation path still exists |
| Helper destination | Private `.local/share/agent-heavy/bin` | Same | Not newly introduced |
| `.install-*.tmp` write, chmod and rename | `installAtomic` | Identical function | Observed temporary executable writes use an existing mechanism |
| Login-service definitions | Daemon, scheduler and menu app | Identical `serviceDefinitions` function | `RunAtLoad` and restart policies were not introduced by this upgrade |
| Source reads and licenses | Older payload ownership checks | App-bundle-aware source validation and license traversal | Destination helper-copy sequence remains the same |
| Remote lifecycle, queue dispatch, Go dependencies | Existing implementations | No changes to `internal/remote/control.go`, `internal/backend/queue_daemon.go`, `go.mod` or `go.sum` in this comparison | No corresponding new remote-service or dependency mechanism found |

The first-launch behavior was introduced by [commit 0b5ec48, drag-to-Applications installation](https://github.com/Dunky13/agent-job-queue/commit/0b5ec48c67bf4cb24cb758d48fb575f68871c889). It added the app's embedded setup payload and `AgentBootstrap.swift`, then inserted bootstrap before the normal menu UI starts. [Build-58 startup](https://github.com/Dunky13/agent-job-queue/blob/f892944dca4159ff125d2ad841661e1dfd36af96/native/AgentQueue.swift#L1668), [build-94 startup](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/native/AgentQueue.swift#L1721).

Function bodies extracted from both commits have matching SHA-256 values:

```text
installAtomic:
577bcb03e10f8bf72bdc9080685d07066ab849fd81fdf5b706f3ba974b3b4c1c
serviceDefinitions:
e8385c0891bf8f00acef37487620ca93e57bceb93955b928be79ef4b871ef54f
```

This is a source-level comparison, not an EDR bisection. Build 58 having installed successfully previously does not guarantee that reinstalling it under today's policy would succeed. Source, signatures/hashes, process ancestry, earlier events and endpoint policy are separate variables. Attribution of the earliest event to the separate installer comes from the user's affected-Mac log analysis, not independent access to those logs here. A policy/model change or a verdict involving the new binaries' identity/reputation remains plausible, not established.

## Can setup happen only during installation?

Yes. An explicit installation or upgrade operation can install the app and helpers, register approved services, verify readiness and commit the completed identity. Ordinary app startup can then inspect readiness and show the queue. If installation is missing or incomplete, it should show a clear repair instruction without automatically copying executable files or repeating setup. Any app-owned registration required by the selected native service API can run in an explicitly invoked setup mode; it need not run during ordinary launch.

This changes the installation experience: a Finder drag alone would no longer complete backend setup. Users would need a clear separate Install/Setup operation, and the Homebrew flow would need a corresponding explicit completion path. Preserve the independent daemon and existing running-job behavior.

The expected effect is fewer unexpected writes and no new repair attempt merely because someone opens the app. It does not resolve the observed SentinelOne block by itself: the separate installer already encountered quarantine while performing the same helper writes. Changing when that work happens does not remove the blocked operation. Native bundle-contained service packaging is a separate improvement, and neither change is evidence of scanner acceptance until tested under the employer's policy.

Recommendation: separate installation from normal startup for reliability and user control, while investigating the SentinelOne verdict as its own acceptance issue. Obtain the authoritative threat record and pursue a suspected false-positive review. Do not present a blanket signer/directory exclusion or moving setup earlier as a proven fix. The user has now authorized implementation of installer-only setup and the supported native service lifecycle. The original incident remains separate from local implementation evidence.

## Correlation with the supplied incident

The user reports quarantined destination helpers, missing app launchers, an incomplete-install marker and surviving older daemon/controller processes. Treat those as evidence from the affected Mac, not as observations made on the analysis workstation. Private incident identifiers and raw endpoint logs are intentionally excluded from this portable report.

The helper-copy events map to `support()`. It writes `agent-heavy` first, then `agent-remote`, and only afterward copies documents and licenses. In `execute()`, this entire stage precedes `serviceDefinitions()` and `launchctl bootstrap`. Therefore a failure returned during helper copying stops that attempt before its later service-registration code. The available timeline identifies executable writes as a concrete event boundary; it does not prove newly registered services caused that attempt's alert. Prior service state or preceding actions could still contribute to a behavioral decision. [Copy order](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/internal/backend/install.go#L1497), [stage order](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/internal/backend/install.go#L1876).

The app-bundle installation path skips `publishApp()`. It is intended to leave the launching app bundle in place. Missing launchers or bundle metadata cannot be attributed to a deliberate self-delete in that path from the source inspected. The user's SentinelOne evidence supports security remediation as the working explanation; the console record is needed to attribute each removed file. [App-publication guard](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/internal/backend/install.go#L1938).

The reported approximately two-second scan failures do not prove repeated installations every two seconds. Both queue UI polling and the resource-controller loop use a two-second cadence. Distinguishing the originating process requires the process tree and actual event records. [UI polling](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/native/AgentQueue.swift#L462), [controller loop](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/internal/backend/scheduler_controller.go#L686).

## Recovery and containment implications

1. Do not relaunch or reinstall on the affected Mac while the block is active. Preserve the incident records for IT. This investigation performs no repair and does not invoke the affected CLI.
2. Preserve `install-in-progress.json` pending approved recovery. It records incomplete installation and participates in version protections. The installer removes it after completing the successful path. Deleting it alone neither restores executable files nor proves a coherent installation. [Marker creation](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/internal/backend/install.go#L1876), [success cleanup](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/internal/backend/install.go#L1976).
3. Preserve current processes and job state while IT reviews the incident; do not restart the remaining services as a diagnostic. An alive daemon is not proof that new work can start: dispatch launches `execute-attempt` through the daemon's executable path, which can fail if that executable is missing. Existing worker processes and restartability require separate checks. [Dispatch](https://github.com/Dunky13/agent-job-queue/blob/fe05988e07165afd8b23c9edda95a90ff0bf2ad8/internal/backend/queue_daemon.go#L524).
4. Ask IT to review the actual threats and determine whether a vendor correction or narrowly supported exception is appropriate. The listed paths identify investigation targets; they are not a recommendation for broad recursive exclusions. A queue or ancestor-process exception must not silently exempt arbitrary queued children.
5. After approval, recover from a complete verified signed release bundle. Surviving helpers are comparison evidence, not a reason to execute around the quarantine. Preserve private queue data and recovery evidence, use an ownership-checked recovery path, verify installed bytes and services, and let successful installation clear its marker. Do not manually reconstruct a signed app from partial files.

Required product work has two parts: a transparent native service lifecycle, and failure containment. The latter should stop automatic repair attempts after an interrupted installation until the user reviews the failure, and avoid publishing a mixture of helper generations. A per-file rename is atomic for one pathname; it does not make the multi-file upgrade transactional or prevent later EDR quarantine. No staging strategy can guarantee protection from security remediation, and retaining an unapproved copy would not be a valid recovery mechanism.

The initial investigation did not run heavy validation. Later implementation uses the healthy analysis workstation queue; the affected machine's queue is reported broken; the investigation does not bypass the shared queue policy or use the still-running daemon as a test target. Future runtime testing belongs on an approved test machine with protection enabled, not by repeating the incident on the affected Mac.

## Scope and concrete source findings

The target is Agent Job Queue for macOS. The initial authorized endpoint was investigation and a resolution proposal, with documentation changes only. Subsequent user authorization covers local installer-only setup and bundle-owned service implementation. No real installation, service registration, security exception, sample submission, release, commit or PR has been performed; implementation validation is documented in [Bundle-owned services](bundle-owned-services.md).

Source inspected: `ced25f439b10496f819a6237f5bd53942fbcb791`. The implementation files cited here are unchanged from published source `fe05988e07165afd8b23c9edda95a90ff0bf2ad8`, verified with a scoped Git comparison.

1. First launch calls `bootstrap.start()`. When setup is needed and preflight passes, it automatically runs the embedded controller with `install --app-bundle APP_PATH`. A progress window appears, but there is no separate initial choice to enable background persistence. The ZIP installer's Install button is a different entry point. [Startup](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/native/AgentQueue.swift#L1721), [automatic setup](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/native/AgentBootstrap.swift#L443), [ZIP installer](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/native/Installer.swift#L16).
2. Setup copies `agent-heavy` and `agent-remote` into the user's private `.local/share/agent-heavy/bin` directory, outside the app bundle, and creates shell entry points in `.local/bin`. These are product runtime locations, not evidence of concealment by themselves. [Support installation](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/internal/backend/install.go#L1497).
3. Setup writes three definitions into the user's `Library/LaunchAgents` directory, with these fresh-install defaults. Existing daemon/controller choices can be retained on upgrade. [Definitions](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/internal/backend/install.go#L1571).

| Service | Action | Fresh-install policy |
| --- | --- | --- |
| `io.went.agent-heavy` | Private `agent-heavy daemon` | `RunAtLoad=true`; restart after unsuccessful exit |
| `io.went.agent-resources` | Private `agent-heavy scheduler` | `RunAtLoad=true`; `KeepAlive=true` |
| `io.went.agent-queue` | `/usr/bin/open -a APP_PATH` | `RunAtLoad=true`; no keepalive |

4. After copying support files, setup starts the daemon and controller using `launchctl bootstrap`, or restarts an existing controller with `launchctl kickstart -k`. An already-ready daemon is retained on ordinary upgrades. These are later installation stages; a helper-copy failure can prevent reaching them. Their presence alone does not identify which operation SentinelOne observed. [Activation](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/internal/backend/install.go#L1901).
5. Optional phone access can add `io.went.agent-remote` with login/restart behavior. It is not enabled by default. An already-enabled service may be migrated during an upgrade, so it is a conditional lead. [Remote definition](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/internal/remote/control.go#L675), [enable requirements](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/internal/remote/control.go#L730).

The inspected baseline contained no `SMAppService` or `SMLoginItem` registration and no literal `persistence_deception` rule or executable. SentinelOne describes user LaunchAgents as a persistence mechanism its monitoring inspects. That supports investigating this sequence; it does not classify every LaunchAgent as malicious. [SentinelOne's macOS persistence explanation](https://www.sentinelone.com/blog/how-malware-persists-on-macos/).

## Ranked explanations

| Rank | Explanation | Distinguishing evidence |
| --- | --- | --- |
| 1 | The executable-write/process chain is classified as persistence behavior | The incident attributes helper writes and subsequent quarantine to installation. Obtain the detection timeline and compare the normal-install and first-launch entry points under the same policy on an approved test machine. |
| 2 | Build identity/reputation, prior events or changed endpoint policy contributes | Surviving helper hashes match build 94, weakening an ad hoc or altered-helper explanation for those files. Compare authoritative detections, policy and signer details across the original successful and failing events. |
| 3 | Upgrade-time phone-service migration or another child action contributed | Inspect actual ancestry and operations. Merely copying `agent-remote` is not evidence that phone access was enabled or its service started. |

There is no employer SentinelOne console or incident export in this workspace, so no detector feedback loop was established. These are ranked explanations, not a reproduced root cause.

## Published reference package: independently checked

The latest public prerelease at initial inspection was [build-94-fe05988](https://github.com/Dunky13/agent-job-queue/releases/tag/build-94-fe05988), version `0.1.0`, bundle build `94`, published 2026-10-26. The user subsequently identified build 94 in the incident and supplied surviving helper hashes matching the reference menu-app copies below. The affected outer archive itself has not been compared. The reference ZIP directly downloaded here matches both its checksum file and release JSON manifest:

```text
SHA-256 aaa89d93bb4249e909036d9ed0f7a0d7ac5536af907f6fad176bb6cb8f679a30
```

| Check | Observed result |
| --- | --- |
| Metadata | Expected installer/menu bundle identifiers, version `0.1.0`, build `94` |
| Signatures | `codesign --verify --strict --all-architectures` passed for both apps and all four helper copies |
| Both architecture identities | Installer, menu app, and app-contained helpers identify Developer ID Application: Marc Went, Team ID `U9GXK89565`, timestamps and hardened runtime on `arm64` and `x86_64` |
| Stapled tickets | `xcrun stapler validate` passed for both apps |
| Gatekeeper | Both apps accepted by `spctl --assess --type execute`, source `Notarized Developer ID` |
| Hosted distribution | [Run 40325950843](https://github.com/Dunky13/agent-job-queue/actions/runs/40325950843) reports successful signing/notarization, Apple Silicon acceptance, Intel acceptance, publication and Homebrew delivery |
| SentinelOne execution | Not performed; neither downloaded app was launched |
| DMG verification | Not performed in this investigation |

Reference helper hashes differ between independently signed app and installer copies. Match the incident's actual executable path:

| Copy | SHA-256 |
| --- | --- |
| Menu-app `agent-heavy` | `9a87dc99e07f92c4fcf3af128498027e52a6eab1d8a5049fa0b317787c51263c` |
| Menu-app `agent-remote` | `2c351a3659a3c6ddb9ece3bd18bd0c2a833e3cd243749117ab9837728e6744e3` |
| Installer `agent-heavy` | `6e6a1c9e0ad721e29f9f811d3a1d3069b4aa2c3434abd8124584c7dc2510ecbc` |
| Installer `agent-remote` | `facaafcec106c6ab31cbfd2ee13dabb194ae7dfe71e2abc3034e14cec10ec85d` |

Missing Developer ID signing is therefore not established as a problem with this reference release. These checks do not prove the employer's alert is false.

## Product remediation options

| Option | Concrete change | Tradeoff and verdict |
| --- | --- | --- |
| A. Native service lifecycle, recommended | Use `SMAppService.mainApp` for the menu-app login preference. Put queue/controller helpers and agent definitions in an owning signed app bundle and register with `SMAppService.agent`. Add explicit enablement, status, disable and removal controls. | Improves attribution, integrity and user control. Requires careful migration of independent services and live jobs. Durable direction, subject to a protected retest. |
| B. Retain registration, improve consent and removal | Add a clear first-run enable action, retained launch preferences and ownership-checked service removal while keeping the private daemon layout. | Smaller change with fewer migration risks, but direct LaunchAgent writes/activation remain and may still trigger the heuristic. |
| C. Explicit foreground-only mode | Offer a separately selected mode without automatic login-service installation. | Removes persistence installation from that mode but changes unattended and reboot/login behavior. A product choice, not a silent fix. |

Option A is not a mechanical API substitution. The daemon must remain independent of the menu UI. Preserve state, process ownership checks and running jobs; establish how app replacement/deletion affects helper executable locations. The current Homebrew uninstall intentionally preserves the queue, jobs and enabled remote access. A new explicit complete-uninstall action should review active work and remove owned services after the chosen drain/cancel policy, while ordinary UI exit and upgrade retain independence. [Existing uninstall contract](https://github.com/Dunky13/agent-job-queue/blob/ced25f439b10496f819a6237f5bd53942fbcb791/packaging/homebrew/refresh.go#L759).

Do not implement cosmetic filename/path changes to suppress a detector, remove quarantine, or disable protection. The objective is transparent supported lifecycle and vendor-reviewed acceptance. This describes the investigation recommendation; local implementation is now authorized and in progress. Scanner acceptance has not been established.

## What the reported name establishes

PaperCut's own support article, updated April 13, 2026, reports SentinelOne warnings named `persistence_deception` while installing its macOS client manually or through MDM. This is a documented precedent involving legitimate software. It does not establish that the current package is benign, define SentinelOne's internal rule, or identify the responsible executable. [PaperCut: PaperCut Hive and SentinelOne](https://www.papercut.com/kb/PaperCutPocketHive/papercut-hive-and-sentinelone/).

No public SentinelOne rule definition for the exact string was found in this research. Treat the string as an alert value until the actual executable path, command line, hash, and incident details identify its meaning in this event. Inferring a specific triggering API solely from this name would exceed the evidence.

## Why persistence behavior deserves inspection

SentinelLabs explains that its behavioral detection considers a sequence of process actions. Its macOS.GMERA analysis specifically discusses writing a LaunchAgent, concealing its filename, and executing encoded scripts. Those combined behaviors motivate suspicion; the article does not say that every legitimate LaunchAgent is malicious or disclose the rule behind `persistence_deception`. [SentinelLabs: Detecting macOS.GMERA Malware Through Behavioral Inspection](https://www.sentinelone.com/labs/detecting-macos-gmera-malware-through-behavioral-inspection/).

For this incident, correlate the first-launch process tree with persistence creation, helper locations, approval state and any upgrade-time migration. These remain investigation targets until matched to the actual event.

## Supported macOS implementation path

Apple documents `SMAppService` on macOS 13 and later for registering bundle-contained login items, LaunchAgents, and LaunchDaemons. Agent registration replaces manually installing agent plists into shared LaunchAgents directories. [Apple: SMAppService](https://developer.apple.com/documentation/servicemanagement/smappservice).

Apple's installer sample places helper property lists inside a fully signed app bundle so modifications invalidate its signature and users can identify the owning app in System Settings. This is a concrete packaging and attribution improvement to evaluate. It is not a vendor promise that SentinelOne will accept the result. [Apple: Updating your app package installer to use the new Service Management API](https://developer.apple.com/documentation/servicemanagement/updating-your-app-package-installer-to-use-the-new-service-management-api).

Registration remains subject to approval. A registered LaunchAgent is bootstrapped immediately and at subsequent logins. `requiresApproval` also represents revoked consent. A migration must preserve the independent queue daemon and job lifecycle while respecting disabled services, rather than tying daemon lifetime to the menu-bar app or silently restoring consent. [Apple: register()](https://developer.apple.com/documentation/servicemanagement/smappservice/register%28%29), [Apple: requiresApproval](https://developer.apple.com/documentation/servicemanagement/smappservice/status-swift.enum/requiresapproval).

Apple's deployment guide provides enterprise background-task management by application identity, service label, or Developer Team ID. Such an MDM policy governs Apple's background-item approval; it does not approve the executable in SentinelOne. [Apple: Manage login items and background tasks on Mac](https://support.apple.com/guide/deployment/depdca572563/web).

## Signing and notarization limits

Apple describes notarization as automated malicious-content and signing checks that produce a ticket used by Gatekeeper. It is not App Review. The documentation does not establish approval by independent endpoint protection products. Therefore valid Developer ID signing, notarization, and stapling are useful artifact evidence, but they cannot prove that the reported SentinelOne event is a false positive or guarantee the absence of future detections. [Apple: Notarizing macOS software before distribution](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).

## Resolution through the employer's security team

SentinelOne recommends examining behavior and incident indicators, submitting suspected false positives for review, and consulting support before Interoperability or Performance exclusions. It advises keeping exclusions narrow. Its customer portal supports tickets. [SentinelOne FAQ: How does SentinelOne handle false positives?](https://www.sentinelone.com/faq/), [SentinelOne customer support](https://www.sentinelone.com/global-services/get-support-now/).

Recommended evidence for that investigation: exact release filename and download source; outer artifact and detected executable SHA-256 values; signing identity and Team ID; notarization evidence; detection time and install stage; process ancestry and command lines; written persistence items; related network events; agent version; policy; and the queued command, if any. The employer's team can retrieve private console details and submit an authorized vendor case. Do not publicly upload private packages or incident logs without authorization.

PaperCut's article describes product-specific path exclusions. Do not copy that workaround to this queue app: a general command runner can execute many unrelated programs, so broad directory or descendant-process exclusions would create a larger protection gap. Prefer a vendor-reviewed correction, transparent supported packaging, and an employer-approved controlled retest with protection enabled.

## Verification boundary

Package signature, ticket and Gatekeeper assessments were performed only on the identified public reference ZIP. No SentinelOne-protected reproduction, vendor case, allow rule or MDM change was performed. A successful Gatekeeper check does not reproduce a behavioral first-launch alert.

Before calling this resolved:

1. Retrieve the original incident. After IT review, reproduce the same first-launch flow on an employer-approved test Mac with SentinelOne enabled under the same relevant policy. Do not retry on the affected production Mac. Record exact package/helper hashes and versions. If quarantined files are unavailable, retain the incident evidence rather than silently substituting another build.
2. Compare the unchanged baseline with a signed/notarized candidate, changing one relevant behavior at a time. Record alerts, process termination and quarantine. A quiet static scan is not this behavioral test.
3. Verify fresh first launch, existing-install upgrade, login/reboot, disabled services, menu-app quit and representative queued work. Exercise refused registration without repeated automatic re-registration. Check phone access only when explicitly enabled.
4. Verify Apple Silicon and Intel, supported installation channels, running-job survival across upgrade, and explicit complete removal. Keep signature/ticket/Gatekeeper results separate from SentinelOne results.

The result that can be established is acceptance of identified bytes under an identified scanner version and policy, with vendor review as needed. No packaging change guarantees zero detections across virus scanners or future policies.
