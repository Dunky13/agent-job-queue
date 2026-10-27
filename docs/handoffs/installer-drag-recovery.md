# Drag installation and pending-build recovery

## Task and scope

The latest installer on another Mac reported "Another candidate installation is pending". The user chose to restore drag-to-Applications with a separate explicit setup action. Baseline: `e7e75b3b1d58cb6d8fad598b7235f39fac5e49bc`. After local validation, the user authorized commit, push, PR and release. Release requires the main-branch merge and its hosted/signing gates. No affected-Mac repair or endpoint-security change was authorized.

The user then reported that build 126 failed with "The queue service definition was not found; setup is incomplete". This repair is included in the same delivery. The earlier pending state on the other Mac had already been cleaned up; its diagnostic was withdrawn.

## Cause and behavior

Managed packaging selected the outer installer as the DMG entry point. The app already contained everything needed for setup, but ordinary startup could only direct users back to that installer. The DMG now contains the signed owning app and an Applications shortcut. Copy and open the app, choose **Install and set up**, then confirm. Normal launch and **Check again** remain read-only. Declining confirmation cannot authorize setup. Missing service registrations expose the same explicit workflow; required macOS approval remains a System Settings action.

An interrupted installation pins its build identity. A newer installer correctly refused to replace it, but reported neither the required build nor an actionable recovery. A regression reproduces the same error by failing service registration and then trying a newer installer. This reproduces the failure class; the other Mac's precise pending records have not been inspected.

The pending diagnostic now identifies the build, version and commit. When the exact pending app remains installed, **Finish the retained pending installation** completes it using the current controller before upgrading. **Repair and continue** includes this operation after approval. Legacy embedded apps and managed apps use their existing installation paths. Source signatures, marker identity, private-path validation, queue drain, service ownership, publication custody, macOS approval and readiness remain authoritative. Recovery pins the selected identity and rechecks its marker before execution and under the normal installer locks.

The ZIP retains the explicit installer and can perform recovery before copying the replacement. If the pending app was already replaced or is missing, retrieve its exact build from release history and finish that installation first. Do not remove pending records or retained application backups to force an upgrade. Unknown backup custody, failed recovery, active work, source substitution and changed pending markers remain refusals.

## Implementation entry points

The build-126 error came from treating Apple's `notFound` lookup result as proof of a missing packaged plist and refusing explicit registration before asking the API. [Apple's status documentation](https://developer.apple.com/documentation/servicemanagement/smappservice/status-swift.enum/notfound) describes a framework lookup error, separately from the [bundled plist requirement](https://developer.apple.com/documentation/servicemanagement/smappservice/agent(plistname:)). The repair verifies the signed bundle and permits one explicit registration attempt from `notFound`, preserving the API's actual failure or approval state and requiring observed enabled status before readiness. Actual missing/malformed definitions still fail package verification. Replacement/removal retain their unresolved-status refusal. Disabled phone access is never registered to resolve a lookup miss; setup separately checks for a loaded remote job and retained remote cleanup before accepting an absent disabled service. Explicit phone enable/start uses the same lookup semantics while preserving credential and approval requirements.

## Build-126 retained-app review hold

The standards review found no actionable issues in its reviewed source snapshot. The spec review identified a real limit: exact pending recovery invokes the retained app's own native bridge. If that bridge is build 126 and still reports `notFound`, its old registration policy refuses the operation. The current controller cannot safely replace that signed native executable or register a different owning bundle to bypass it. A regression reproduces the old bridge refusal and proves the original pending identity, installed build and last-good backup survive unchanged. The recovery failure now explicitly reports that custody was retained.

Release remains on hold for this unresolved affected-Mac case until its current pending state is reconciled. The user was asked whether the build-126 failure's pending state was also cleaned up; their earlier cleanup statement preceded this new failure. Do not infer that the new pending state is gone, delete its records, or claim the old native bridge is repaired by the newer controller.

- `scripts/macos-signing.sh`: signed app and Applications shortcut in the managed DMG; ZIP and Homebrew installer delivery remain supported.
- `native/AgentBootstrap.swift`: confirmed setup in the copied app, missing-registration handling, pending recovery action and approved-repair sequence.
- `internal/backend/install_pending.go`: exact retained-app selection, signature verification and recovery through the existing installer.
- `internal/backend/install_preflight.go`: actionable pending report and recovery dispatch.
- `internal/backend/install.go`: detailed pending error and recovery identity revalidation.

## Validation

The regression failed before implementation with the original pending-candidate message. Focused Go recovery checks pass for managed and legacy apps, an owned previous-app backup, missing/replaced apps, active work, failed retries, unknown backups and a changed pending marker. Managed lifecycle race tests and signing/DMG fixtures passed in queue job 2206. Native tests passed for read-only startup, cancelled and confirmed setup, missing service registrations and existing recovery behavior.

The first complete batch, job 2210, stopped at missing npm dependencies (`eslint: command not found`). Job 2211 installed the lockfile dependencies and passed lint, the reachable-vulnerability scan and every Go package except one tooling assertion. That remaining assertion expected the intentionally removed installer-only managed DMG layout in the release-resume fixture; it has been updated to require the restored app and Applications shortcut. Backend and HTTPS race suites passed in 228.348 and 166.025 seconds; the tooling suite's retained failed witness took 374.090 seconds.

Job 2217 passed the affected release-resume, signing and DMG tests with race detection, developer and Linux builds, seven schema tests, all 16 browser journeys, all headless native tests, universal packaging, private packaged installation and mounted DMG verification. The actual packaged payload and in-place app installs passed in 8.87 and 6.81 seconds. Packaged HTTPS passed in 5.69 seconds; the outer installer lifecycle passed with race detection in 65.60 seconds. The verified DMG contains the standalone universal app and Applications shortcut and detached successfully.

The graphical run then failed in the existing aggregate-repair approval test: a Return event did not open the alert while the model was already reviewing. Capturing/rendering the ready window before that event resolved the failure, consistent with the event preceding the SwiftUI action's render. The fixture now captures this state, matching the other interactive paths, and retains a failure screenshot when available. No timeout was increased and no action was retried. Diagnostic job 2223 passed all five graphical modes, including the new copied-app setup and its actual approval and Cancel buttons. Final job 2225 passed all five modes again after diagnostic logging was removed. Screenshots of the ready screen and approval dialog were inspected. The complete batch is not claimed as a single green invocation; all validation stages passed across the initial run and focused corrections/remaining-stage runs.

The graphical fixture exercises the actual production setup window, its approval and Cancel buttons, and transition to a simulated queue after one accepted installation. It uses a disposable home and injected executor. Packaged acceptance uses private daemons and injected native lifecycle operations. Packaged artifacts were built from the uncommitted working tree with ad hoc signatures. These checks do not establish live Service Management on the affected Mac, hosted Intel acceptance, Developer ID signing, Apple acceptance, publication or SentinelOne acceptance.
