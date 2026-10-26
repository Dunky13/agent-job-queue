import Foundation
import Darwin

@main
struct BootstrapTests {
    @MainActor static func main() async throws {
        func check(_ value: Bool, _ name: String) throws {
            print("\(value ? "PASS" : "FAIL") \(name)")
            if !value { throw ClientError.message(name) }
        }
        let temporary = try BootstrapFixture.canonicalDirectory(FileManager.default.temporaryDirectory)
            .appendingPathComponent("agent-queue-bootstrap-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: temporary, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: temporary) }
        func fixture(_ location: String = "Applications", tracked: Bool = true) throws -> BootstrapFixture {
            try BootstrapFixture(parent: temporary, location: location, tracked: tracked)
        }
        func needsSetup(_ environment: QueueBootstrapEnvironment) throws -> Bool {
            if case .install = try environment.inspect() { return true }
            return false
        }
        func rejects(_ environment: QueueBootstrapEnvironment, containing text: String) -> Bool {
            do { _ = try environment.inspect(); return false }
            catch { return error.localizedDescription.contains(text) }
        }

        let fresh = try fixture()
        let decision = try fresh.environment.inspect()
        if case let .install(request, _) = decision {
            try check(request.executable == fresh.environment.source.appendingPathComponent("bin/agent-heavy") &&
                request.arguments == ["install", "--app-bundle", fresh.environment.app.path],
                "Fresh launch runs only its own bundled installer targeting the running app")
        } else { throw ClientError.message("Fresh package did not require setup") }
        try check(!FileManager.default.fileExists(atPath: fresh.environment.root.path), "Inspection makes no backend changes")
        try fresh.install()
        try check(try fresh.environment.inspect() == .ready, "Current complete installation skips setup")
        let nonexecutable = fresh.environment.root.appendingPathComponent("bin/agent-heavy")
        try FileManager.default.setAttributes([.posixPermissions: 0o601], ofItemAtPath: nonexecutable.path)
        try check(try needsSetup(fresh.environment), "An execute bit for other users cannot authorize an owner-inexecutable command")
        try fresh.install()
        let retained = try fixture()
        let retainedExecutor = BootstrapRetainedExecutorFixture(retained)
        var retainedCompletions = 0
        let retainedModel = QueueBootstrapModel(environment: retained.environment, execute: { request, output in
            try await retainedExecutor.execute(request, output: output)
        }) { retainedCompletions += 1 }
        await retainedModel.start()?.value
        let retainedCalls = await retainedExecutor.calls
        try check(retainedModel.phase == .complete && retainedCompletions == 1 && retainedCalls == 1,
            "One setup succeeds with backend-retained Program and RunAtLoad choices")
        for label in ["io.went.agent-heavy", "io.went.agent-resources"] {
            let command = label == "io.went.agent-heavy" ? "daemon" : "scheduler"
            let binary = retained.environment.root.appendingPathComponent("bin/agent-heavy").path
            for runAtLoad in [false, nil, true] {
                try retained.writeDefinition(label, arguments: [binary, command], program: binary, runAtLoad: runAtLoad)
                try check(try retained.environment.inspect() == .ready,
                    "Accepted \(label) explicit Program and retained RunAtLoad \(String(describing: runAtLoad)) authorize startup")
            }
            try retained.writeDefinition(label, arguments: [binary, command], program: "/unverified/command")
            try check(try needsSetup(retained.environment), "Customized \(label) Program cannot authorize startup")
            try retained.writeDefinition(label, arguments: [binary, command])
        }
        try retained.writeDefinition("io.went.agent-queue", arguments: ["/usr/bin/open", "-a", retained.environment.app.path],
            program: "/usr/bin/open", runAtLoad: false)
        try check(try needsSetup(retained.environment), "App launch definition still requires the backend canonical rewrite")
        let systemExecutable = URL(fileURLWithPath: "/usr/bin/true")
        try check(try QueueBootstrapFiles.safeExecutable(systemExecutable, scope: .init(root: URL(fileURLWithPath: "/usr"), bundled: true)),
            "Immutable root-owned resources are accepted within the app resource scope")
        var privateRefusedRoot = false
        do { try QueueBootstrapFiles.validateFile(systemExecutable, scope: .init(root: URL(fileURLWithPath: "/usr"))) }
        catch { privateRefusedRoot = true }
        try check(privateRefusedRoot, "Root ownership allowance never extends to the private account scope")
        let sticky = URL(fileURLWithPath: "/private/tmp/agent-queue-bootstrap-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: sticky, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: sticky) }
        let stickyFixture = try BootstrapFixture(parent: sticky)
        try check(try needsSetup(stickyFixture.environment), "Root-owned sticky temporary ancestor permits private fixtures")
        for path in [".local/share/agent-heavy", ".local/share/agent-heavy/bin", ".local/share", ".local/bin", ".local",
                     "Library/LaunchAgents", "Library", ".local/state/agent-heavy", ".config/agent-heavy"] {
            for kind in ["symlink", "group-writable", "world-writable"] {
                let unsafe = try fixture()
                try unsafe.install()
                let directory = unsafe.environment.home.appendingPathComponent(path)
                try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true,
                    attributes: [.posixPermissions: 0o700])
                if kind == "symlink" {
                    let outside = unsafe.directory.appendingPathComponent("outside")
                    try FileManager.default.moveItem(at: directory, to: outside)
                    try FileManager.default.createSymbolicLink(at: directory, withDestinationURL: outside)
                } else {
                    try FileManager.default.setAttributes([.posixPermissions: kind == "group-writable" ? 0o770 : 0o707],
                        ofItemAtPath: directory.path)
                }
                try check(rejects(unsafe.environment, containing: "Unsafe setup path"),
                    "\(kind) private \(path) cannot authorize ready")
                // The same unsafe path must block a fresh/mismatched identity
                // before the bundled installer could perform any setup.
                try FileManager.default.removeItem(at: unsafe.environment.root.appendingPathComponent("build-info.json"))
                let counter = BootstrapCountingExecutorFixture()
                var completed = false
                let blocked = QueueBootstrapModel(environment: unsafe.environment, execute: { request, output in
                    await counter.execute(request, output: output)
                }) { completed = true }
                await blocked.start()?.value
                let calls = await counter.calls
                try check(blocked.phase == .failed && !completed && calls == 0,
                    "\(kind) private \(path) blocks installer execution")
            }
        }
        for path in [".local/share/agent-heavy/build-info.json", ".local/share/agent-heavy/bin/agent-heavy",
                     ".local/bin/agent-heavy", "Library/LaunchAgents/io.went.agent-heavy.plist"] {
            for kind in ["symlink", "writable", "hardlink"] {
                let unsafe = try fixture()
                try unsafe.install()
                let file = unsafe.environment.home.appendingPathComponent(path)
                let outside = unsafe.directory.appendingPathComponent("outside")
                if kind == "symlink" {
                    try FileManager.default.moveItem(at: file, to: outside)
                    try FileManager.default.createSymbolicLink(at: file, withDestinationURL: outside)
                } else if kind == "hardlink" {
                    try FileManager.default.linkItem(at: file, to: outside)
                } else {
                    try FileManager.default.setAttributes([.posixPermissions: 0o777], ofItemAtPath: file.path)
                }
                let counter = BootstrapCountingExecutorFixture()
                let blocked = QueueBootstrapModel(environment: unsafe.environment, execute: { request, output in
                    await counter.execute(request, output: output)
                }) { }
                await blocked.start()?.value
                let calls = await counter.calls
                try check(blocked.phase == .failed && calls == 0 && blocked.output.contains("Unsafe setup path"),
                    "\(kind) private \(path) fails before installer execution")
            }
        }
        for path in ["Contents", "Contents/Resources", "Contents/Resources/payload", "Contents/Resources/payload/source",
                     "Contents/Resources/payload/source/bin", "Contents/Resources/payload/source/build-info.json",
                     "Contents/Info.plist"] {
            for kind in ["symlink", "writable"] {
                let unsafe = try fixture()
                let entry = unsafe.environment.app.appendingPathComponent(path)
                if kind == "symlink" {
                    let outside = unsafe.directory.appendingPathComponent("outside")
                    try FileManager.default.moveItem(at: entry, to: outside)
                    try FileManager.default.createSymbolicLink(at: entry, withDestinationURL: outside)
                } else {
                    try FileManager.default.setAttributes([.posixPermissions: 0o777], ofItemAtPath: entry.path)
                }
                let counter = BootstrapCountingExecutorFixture()
                let blocked = QueueBootstrapModel(environment: unsafe.environment, execute: { request, output in
                    await counter.execute(request, output: output)
                }) { }
                await blocked.start()?.value
                let calls = await counter.calls
                try check(blocked.phase == .failed && calls == 0,
                    "\(kind) app \(path) fails before installer execution")
            }
        }
        let reordered = Data("{\n\"build\":42,\"commit\":\"\(String(repeating: "a", count: 40))\",\"version\":\"1.2.3\",\"repository\":\"Dunky13/agent-job-queue\",\"schemaVersion\":1}\n".utf8)
        try BootstrapFixture.write(reordered, to: fresh.environment.root.appendingPathComponent("build-info.json"))
        try check(try fresh.environment.inspect() == .ready, "Build identity equality ignores JSON formatting and key order")
        for identity in [BootstrapFixtureIdentity(version: "1.2.2", commit: String(repeating: "a", count: 40), build: 42),
                         BootstrapFixtureIdentity(version: "1.2.3", commit: String(repeating: "b", count: 40), build: 42),
                         BootstrapFixtureIdentity(version: "1.2.3", commit: String(repeating: "a", count: 40), build: 41)] {
            try BootstrapFixture.write(try JSONEncoder().encode(identity), to: fresh.environment.root.appendingPathComponent("build-info.json"))
            try check(try needsSetup(fresh.environment), "Version, commit and build changes require setup")
        }
        try fresh.install()
        let marker = fresh.environment.root.appendingPathComponent("install-in-progress.json")
        try BootstrapFixture.write(Data("unfinished".utf8), to: marker)
        try check(try needsSetup(fresh.environment), "Any incomplete-install marker forces safe backend retry")
        try FileManager.default.removeItem(at: marker)
        for name in ["agent-heavy", "agent-remote"] {
            for path in [fresh.environment.root.appendingPathComponent("bin/\(name)"), fresh.environment.home.appendingPathComponent(".local/bin/\(name)")] {
                try FileManager.default.removeItem(at: path)
                try check(try needsSetup(fresh.environment), "Missing \(name) stable binary or entry point requires repair")
                try fresh.install()
            }
        }
        for label in ["io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-queue"] {
            let path = fresh.environment.home.appendingPathComponent("Library/LaunchAgents/\(label).plist")
            try FileManager.default.removeItem(at: path)
            try check(try needsSetup(fresh.environment), "Missing \(label) launch definition requires setup")
            try fresh.writeDefinition(label, arguments: ["/unverified/app"])
            try check(try needsSetup(fresh.environment), "Stale \(label) command cannot authorize startup")
            try fresh.install()
        }
        try fresh.writeDefinition("io.went.agent-queue", arguments: ["/usr/bin/open", "-a", "/Applications/Older Copy.app"])
        try check(try needsSetup(fresh.environment), "App launch definition must target this running copy")
        let userApp = try fixture("home")
        try userApp.install()
        try check(try userApp.environment.inspect() == .ready, "Direct home Applications install is supported")
        let untracked = try fixture(tracked: false)
        try untracked.install()
        try check(try untracked.environment.inspect() == .ready, "Equal untracked development identities do not rerun every launch")
        for location in ["Downloads", "Volumes/Agent Queue", "private/AppTranslocation/Agent Queue", "Applications/Nested"] {
            let misplaced = try fixture(location)
            try check(rejects(misplaced.environment, containing: "Move Agent Queue"), "\(location) receives move guidance before mutation")
            try check(!FileManager.default.fileExists(atPath: misplaced.environment.root.path), "Disallowed location remains untouched")
        }
        for path in ["source/build-info.json", "source/bin/agent-heavy", "source/bin/agent-remote"] {
            let broken = try fixture()
            try broken.install()
            try FileManager.default.removeItem(at: broken.environment.resources.appendingPathComponent("payload/\(path)"))
            try check(rejects(broken.environment, containing: "embedded setup payload"), "Missing \(path) fails closed even with current backend")
        }
        let missing = try fixture()
        try FileManager.default.removeItem(at: missing.environment.resources.appendingPathComponent("payload"))
        try check(rejects(missing.environment, containing: "embedded setup payload"), "Package marker detects an entirely missing payload")
        let badIdentity = try fixture()
        try BootstrapFixture.write(Data("{}".utf8), to: badIdentity.environment.source.appendingPathComponent("build-info.json"))
        try check(rejects(badIdentity.environment, containing: "embedded setup payload"), "Invalid source identity fails clearly")
        let appMismatch = try fixture()
        try BootstrapFixture.write(try JSONEncoder().encode(BootstrapFixtureIdentity(version: "2.0.0", commit: nil, build: nil)),
            to: appMismatch.environment.resources.appendingPathComponent("build-info.json"))
        try check(rejects(appMismatch.environment, containing: "identities differ"), "App/source identity mismatch cannot start installed backend")

        var inert = missing.environment
        inert.arguments = ["--fixture", "private-fixture.json"]
        try check(try inert.inspect() == .legacy, "Fixture arguments are inert even for a damaged packaged app")
        inert.arguments = ["--fixture"]
        try check(rejects(inert, containing: "requires a JSONFILE"), "Malformed fixture selection fails closed before real queue startup")
        let diagnosticFresh = try fixture()
        for arguments in [["--snapshot", "output.png"], ["--snapshot"], ["--fixture"], ["--fixture", ""],
                          ["--fixture", "--snapshot", "output.png"], ["--fixture", "--another-flag"],
                          ["--fixture", "fixture.json", "--fixture"],
                          ["--fixture", "fixture.json", "--snapshot"],
                          ["--fixture", "fixture.json", "--snapshot", "--snapshot-menu"],
                          ["--snapshot-menu"]] {
            var environment = diagnosticFresh.environment
            environment.arguments = arguments
            let counter = BootstrapCountingExecutorFixture()
            var completed = false
            let model = QueueBootstrapModel(environment: environment, execute: { request, output in
                await counter.execute(request, output: output)
            }) { completed = true }
            await model.start()?.value
            let calls = await counter.calls
            try check(model.phase == .failed && !completed && calls == 0,
                "Invalid diagnostic \(arguments.joined(separator: " ")) rejects before any installer call")
        }
        for arguments in [["--fixture", "fixture.json"], ["--fixture", "fixture.json", "--snapshot", "output.png"]] {
            var environment = diagnosticFresh.environment
            environment.arguments = arguments
            let counter = BootstrapCountingExecutorFixture()
            var completed = false
            let model = QueueBootstrapModel(environment: environment, execute: { request, output in
                await counter.execute(request, output: output)
            }) { completed = true }
            await model.start()?.value
            let calls = await counter.calls
            try check(model.phase == .complete && completed && calls == 0 && !FileManager.default.fileExists(atPath: environment.root.path),
                "Valid fixture diagnostic remains inert on a fresh packaged app")
        }
        let legacyURL = temporary.appendingPathComponent("Legacy.app")
        let legacy = QueueBootstrapEnvironment(app: legacyURL, resources: legacyURL.appendingPathComponent("Contents/Resources"), home: temporary)
        try check(try legacy.inspect() == .legacy, "Legacy apps without embedded payload preserve existing startup")

        let lifecycle = try fixture()
        let executor = BootstrapExecutorFixture(lifecycle)
        var completions = 0
        var shown = 0
        let model = QueueBootstrapModel(environment: lifecycle.environment, execute: { request, output in
            try await executor.execute(request, output: output)
        }) { completions += 1 }
        model.onSetupRequired = { shown += 1 }
        let first = model.start()
        try await waitUntil { await executor.isHeld }
        try check(model.phase == .installing && model.isRunning && completions == 0 && shown == 1,
            "Installing gates normal startup and termination while presenting setup progress")
        try check(model.start() == nil, "Repeated startup cannot launch a second installer")
        first?.cancel()
        await executor.release()
        await first?.value
        try check(model.phase == .failed && !model.isRunning && completions == 0 && model.output.contains("Fixture service"),
            "Failure retains a readable error, permits quit and leaves normal startup gated")
        try check(!model.canDisablePhoneAccess && model.start(disablePhoneAccess: true) == nil,
            "Unrelated setup failures cannot authorize disabling phone access")
        let retry = model.start()
        try await waitUntil { await executor.isHeld }
        await executor.release()
        await retry?.value
        try check(model.phase == .complete && completions == 1 && !model.isRunning,
            "Retry verifies installed state then continues normal startup exactly once")
        let requests = await executor.requests
        try check(model.start() == nil && requests.count == 2 && requests[0] == requests[1],
            "Retry uses the same verified bundled executable and completion is idempotent")

        let noMutation = try fixture()
        let phoneFailure = QueueBootstrapModel(environment: noMutation.environment, execute: { _, _ in
            throw ClientError.message("Agent Queue installer: Disable phone access before migrating.\nAGENT_QUEUE_SETUP_RECOVERY:disable-phone-access\n")
        }) { completions += 1 }
        await phoneFailure.start()?.value
        try check(phoneFailure.phase == .failed && phoneFailure.message.contains("Phone access") &&
            phoneFailure.canDisablePhoneAccess && !phoneFailure.output.contains("AGENT_QUEUE_SETUP_RECOVERY"),
            "Legacy phone access failure offers an actionable recovery without exposing protocol diagnostics")
        let recoveryFixture = try fixture()
        let recoveryExecutor = BootstrapExecutorFixture(recoveryFixture, phoneAccessBlocker: true)
        var recoveryCompletions = 0
        let recovery = QueueBootstrapModel(environment: recoveryFixture.environment, execute: { request, output in
            try await recoveryExecutor.execute(request, output: output)
        }) { recoveryCompletions += 1 }
        try check(recovery.start(disablePhoneAccess: true) == nil,
            "Disabling phone access requires a prior recoverable failure")
        let blocked = recovery.start()
        try await waitUntil { await recoveryExecutor.isHeld }
        await recoveryExecutor.release()
        await blocked?.value
        try check(recovery.canDisablePhoneAccess && recoveryCompletions == 0,
            "Phone access blocker keeps normal startup gated until the user chooses recovery")
        let continued = recovery.start(disablePhoneAccess: true)
        try await waitUntil { await recoveryExecutor.isHeld }
        try check(!recovery.canDisablePhoneAccess && recovery.isRunning && recovery.start(disablePhoneAccess: true) == nil,
            "Recovery consumes consent and prevents duplicate installers")
        await recoveryExecutor.release()
        await continued?.value
        let recoveryRequests = await recoveryExecutor.requests
        try check(recovery.phase == .complete && recoveryCompletions == 1 && recoveryRequests.count == 2 &&
            recoveryRequests[0].arguments == ["install", "--app-bundle", recoveryFixture.environment.app.path] &&
            recoveryRequests[1].arguments == recoveryRequests[0].arguments + ["--disable-phone-access"] &&
            recoveryRequests[0].executable == recoveryRequests[1].executable,
            "Explicit recovery uses the verified bundled installer and completes only after installation readback")
        let failedRecovery = QueueBootstrapModel(environment: noMutation.environment, execute: { request, _ in
            if request.arguments.contains("--disable-phone-access") { throw ClientError.message("Phone access shutdown could not verify process ownership.") }
            throw ClientError.message("AGENT_QUEUE_SETUP_RECOVERY:disable-phone-access")
        }) { recoveryCompletions += 1 }
        await failedRecovery.start()?.value
        await failedRecovery.start(disablePhoneAccess: true)?.value
        try check(failedRecovery.phase == .failed && !failedRecovery.canDisablePhoneAccess && recoveryCompletions == 1 &&
            failedRecovery.output.contains("ownership"),
            "Failed phone access shutdown remains gated, retains its diagnostic and clears recovery consent")
        let unmarkedFailure = QueueBootstrapModel(environment: noMutation.environment, execute: { _, _ in
            throw ClientError.message("Disable phone access. Invalid embedded AGENT_QUEUE_SETUP_RECOVERY:disable-phone-access text.")
        }) { recoveryCompletions += 1 }
        await unmarkedFailure.start()?.value
        try check(!unmarkedFailure.canDisablePhoneAccess,
            "Similar prose and embedded marker text cannot enable the recovery action")
        let incomplete = QueueBootstrapModel(environment: noMutation.environment, execute: { request, _ in
            request.arguments.contains("--preflight") ? try JSONEncoder().encode(QueueBootstrapReport.clear) : Data()
        }) {
            completions += 1
        }
        await incomplete.start()?.value
        try check(incomplete.phase == .failed && completions == 1 && incomplete.output.contains("before the installation was complete"),
            "Successful process exit without required installation readback never starts normal UI")
        let longFailure = QueueBootstrapModel(environment: noMutation.environment, execute: { _, _ in
            throw ClientError.message(String(repeating: "x", count: 20_000) + " actionable tail")
        }) { completions += 1 }
        await longFailure.start()?.value
        try check(longFailure.output.count <= 8_000 && longFailure.output.hasSuffix("actionable tail"),
            "Failure output stays bounded and retains the actionable tail")

        let jobIssue = QueueBootstrapIssue(id: "legacy-jobs", title: "Existing jobs need attention", detail: "Finish or explicitly cancel these jobs before migrating.",
            actions: [.init(id: "review-jobs", title: "Review jobs", destructive: false),
                      .init(id: "cancel-job", title: "Cancel job", destructive: false),
                      .init(id: "wait", title: "Wait and continue", destructive: false)],
            jobs: [.init(id: 7, label: "Private fixture job", status: "Paused")])
        let phoneIssue = QueueBootstrapIssue(id: "legacy-phone-access", title: "Disable phone access", detail: "Credentials and certificates will be preserved.",
            actions: [.init(id: "disable-phone-access", title: "Disable phone access and continue", destructive: false)])
        let multiple = QueueBootstrapReport(schemaVersion: 1, issues: [jobIssue, phoneIssue])
        let summaryFixture = try fixture()
        let summaryExecutor = BootstrapPreflightExecutorFixture(summaryFixture, report: multiple,
            afterRecovery: .init(schemaVersion: 1, issues: [phoneIssue]))
        var summaryCompletions = 0
        let summary = QueueBootstrapModel(environment: summaryFixture.environment, execute: { request, output in
            try await summaryExecutor.execute(request, output: output)
        }) { summaryCompletions += 1 }
        await summary.start()?.value
        try check(summary.phase == .reviewing && summary.issues.count == 2 && summaryCompletions == 0 && !summary.isRunning,
            "Preflight lists independent blockers together before any installation")
        try check(summary.perform(actionID: "cancel-job", issueID: jobIssue.id, jobID: 7) == nil &&
            summary.perform(actionID: "cancel-job", issueID: jobIssue.id, jobID: 8, confirmed: true) == nil &&
            summary.perform(actionID: "unknown", issueID: jobIssue.id, confirmed: true) == nil,
            "Local action allowlist, listed job identity and mandatory cancellation consent cannot be bypassed by report flags")
        await summary.perform(actionID: "cancel-job", issueID: jobIssue.id, jobID: 7, confirmed: true)?.value
        let summaryRequests = await summaryExecutor.requests
        let summaryInstalls = await summaryExecutor.installations
        try check(summary.phase == .reviewing && summary.issues == [phoneIssue] && summaryInstalls == 0 &&
            summaryRequests.contains(where: { $0.arguments.suffix(4) == ["--recover", "cancel-job", "--job-id", "7"] }),
            "Explicit job cancellation is followed by an independent preflight and leaves remaining blockers visible")
        await summary.perform(actionID: "disable-phone-access", issueID: phoneIssue.id)?.value
        // This fixture deliberately retains its phone blocker, proving actions do
        // not infer permission to install from process success alone.
        try check(summary.phase == .reviewing && summaryCompletions == 0,
            "Successful recovery with a remaining blocker never starts the queue")

        let staleFixture = try fixture()
        let staleExecutor = BootstrapPreflightExecutorFixture(staleFixture, report: .init(schemaVersion: 1, issues: [phoneIssue]))
        await staleExecutor.setStaleRecovery()
        let stale = QueueBootstrapModel(environment: staleFixture.environment, execute: { request, output in
            try await staleExecutor.execute(request, output: output)
        }) { summaryCompletions += 1 }
        await stale.start()?.value
        await stale.perform(actionID: "disable-phone-access", issueID: phoneIssue.id)?.value
        let staleInstalls = await staleExecutor.installations
        try check(stale.phase == .reviewing && stale.issues == [phoneIssue] && staleInstalls == 0,
            "An empty recovery response cannot bypass a fresh preflight that still reports a blocker")

        let configIssue = QueueBootstrapIssue(id: "custom-callback", title: "Review custom callback", detail: "Back up the original and replace its custom behavior with the standard cleanup callback.",
            actions: [.init(id: "review-configuration", title: "Review configuration", destructive: false),
                      .init(id: "reset-callback", title: "Back up and replace callback", destructive: false)])
        let configFixture = try fixture()
        let revision = String(repeating: "a", count: 64)
        let configExecutor = BootstrapPreflightExecutorFixture(configFixture, report: .init(schemaVersion: 1, issues: [configIssue], revision: revision))
        let configModel = QueueBootstrapModel(environment: configFixture.environment, execute: { request, output in
            try await configExecutor.execute(request, output: output)
        }) { summaryCompletions += 1 }
        await configModel.start()?.value
        try check(configModel.perform(actionID: "reset-callback", issueID: configIssue.id, confirmed: true) == nil,
            "Replacing a callback requires reviewing its current proposal first")
        configModel.review(configIssue.id)
        try check(configModel.perform(actionID: "reset-callback", issueID: configIssue.id) == nil,
            "Callback replacement always requires explicit confirmation despite a false destructive field")
        await configModel.perform(actionID: "reset-callback", issueID: configIssue.id, confirmed: true)?.value
        let configRequests = await configExecutor.requests
        try check(configModel.phase == .complete && configRequests.contains(where: {
            $0.arguments.suffix(4) == ["--recover", "reset-callback", "--expected-revision", revision]
        }), "Reviewed configuration recovery carries its exact approved revision and verifies installation")
        let noRevisionFixture = try fixture()
        let noRevisionExecutor = BootstrapPreflightExecutorFixture(noRevisionFixture, report: .init(schemaVersion: 1, issues: [configIssue]))
        let noRevision = QueueBootstrapModel(environment: noRevisionFixture.environment, execute: { request, output in
            try await noRevisionExecutor.execute(request, output: output)
        }) { summaryCompletions += 1 }
        await noRevision.start()?.value
        noRevision.review(configIssue.id)
        try check(noRevision.perform(actionID: "reset-callback", issueID: configIssue.id, confirmed: true) == nil,
            "A callback proposal without a verified current revision can never be applied")

        let missingDefinitions = QueueBootstrapIssue(id: "service-definitions-missing", title: "Restore missing background service files", detail: "Restore standard service files after verifying the loaded services. Running jobs stay active.",
            actions: [.init(id: "restore-service-definitions", title: "Restore verified service files", destructive: false)])
        let restoreFixture = try fixture()
        let restoreExecutor = BootstrapPreflightExecutorFixture(restoreFixture,
            report: .init(schemaVersion: 1, issues: [missingDefinitions]),
            afterRecovery: .init(schemaVersion: 1, issues: [phoneIssue]))
        let restoreModel = QueueBootstrapModel(environment: restoreFixture.environment, execute: { request, output in
            try await restoreExecutor.execute(request, output: output)
        }) { summaryCompletions += 1 }
        await restoreModel.start()?.value
        try check(restoreModel.perform(actionID: "restore-service-definitions", issueID: missingDefinitions.id) == nil,
            "Service file restoration always requires consent even when a report omits its destructive flag")
        await restoreModel.perform(actionID: "restore-service-definitions", issueID: missingDefinitions.id, confirmed: true)?.value
        let restoreRequests = await restoreExecutor.requests
        let restoreInstalls = await restoreExecutor.installations
        try check(restoreRequests.contains(where: { $0.arguments.suffix(2) == ["--recover", "restore-service-definitions"] }) &&
            restoreModel.phase == .reviewing && restoreModel.issues == [phoneIssue] && restoreInstalls == 0,
            "Confirmed service file restoration rechecks remaining migration requirements before installing")

        let repairIssue = QueueBootstrapIssue(id: "service-not-ready", title: "Queue service could not start", detail: "Review diagnostics and repair the stopped service.",
            actions: [.init(id: "repair-service", title: "Repair service and continue", destructive: false)])
        let failureFixture = try fixture()
        let failureExecutor = BootstrapPreflightExecutorFixture(failureFixture, report: .clear)
        await failureExecutor.failInstallation(with: .init(schemaVersion: 1, issues: [repairIssue]))
        let freshFailure = QueueBootstrapModel(environment: failureFixture.environment, execute: { request, output in
            try await failureExecutor.execute(request, output: output)
        }) { summaryCompletions += 1 }
        await freshFailure.start()?.value
        try check(freshFailure.phase == .failed && freshFailure.issues == [repairIssue] && freshFailure.output.contains("could not become ready"),
            "An install failure immediately refreshes available repairs while retaining its original diagnostic")
        await freshFailure.perform(actionID: "repair-service", issueID: repairIssue.id)?.value
        try check(freshFailure.phase == .complete, "Explicit stopped-service repair rechecks requirements and verifies normal setup")

        let waitIssue = QueueBootstrapIssue(id: "installer-busy", title: "Another installation is running", detail: "Wait for it to finish.",
            actions: [.init(id: "wait", title: "Wait and continue", destructive: false)])

        let cleanupIssue = QueueBootstrapIssue(id: "pending-cleanup", title: "Wait for queue work and cleanup", detail: "A completed job has verified cleanup available.",
            actions: [.init(id: "cleanup-job", title: "Run verified cleanup", destructive: false),
                      .init(id: "wait", title: "Wait and continue", destructive: false)],
            jobs: [.init(id: 9, label: "Fixture completed job", status: "Cleanup pending")])
        let approvedFixture = try fixture()
        let activeWaitIssue = QueueBootstrapIssue(id: "legacy-jobs", title: "Queue work is running", detail: "Wait for the active job to finish.",
            actions: [.init(id: "wait", title: "Wait", destructive: false), .init(id: "cancel-job", title: "Cancel", destructive: true)],
            jobs: [.init(id: 10, label: "Active fixture job", status: "Running")])
        let approvedExecutor = BootstrapPreflightExecutorFixture(approvedFixture,
            report: .init(schemaVersion: 1, issues: [missingDefinitions]))
        await approvedExecutor.queueRecoveryReports([
            .init(schemaVersion: 1, issues: [cleanupIssue]),
            .init(schemaVersion: 1, issues: [repairIssue]),
            .init(schemaVersion: 1, issues: [phoneIssue]),
            .init(schemaVersion: 1, issues: [activeWaitIssue]),
        ])
        var approvedCompletions = 0
        let approved = QueueBootstrapModel(environment: approvedFixture.environment, execute: { request, output in
            try await approvedExecutor.execute(request, output: output)
        }, waitInterval: .milliseconds(1), waitLimit: 1) { approvedCompletions += 1 }
        await approved.start()?.value
        try check(approved.canRepairAndContinue && approved.repairAndContinue() == nil,
            "Automatic repair requires explicit consent independently of report destructive flags")
        let approvedRepair = approved.repairAndContinue(confirmed: true)
        try await waitUntil { await approvedExecutor.mutations == 4 && approved.phase == .waiting }
        let checksBeforeWaiting = await approvedExecutor.checks
        try await waitUntil { await approvedExecutor.checks >= checksBeforeWaiting + 3 }
        try check(approved.isRunning && approved.canStopWaiting && approved.start() == nil &&
            approved.repairAndContinue(confirmed: true) == nil,
            "One approval waits beyond the manual wait budget and prevents concurrent setup attempts")
        await approvedExecutor.update(.clear)
        await approvedRepair?.value
        let approvedRequests = await approvedExecutor.requests
        let approvedInstalls = await approvedExecutor.installations
        let approvedActions = approvedRequests.compactMap { request -> String? in
            guard let index = request.arguments.firstIndex(of: "--recover"), index + 1 < request.arguments.count else { return nil }
            return request.arguments[index + 1]
        }
        try check(approvedActions == ["restore-service-definitions", "cleanup-job", "repair-service", "disable-phone-access"] &&
            approvedRequests.contains(where: { $0.arguments.suffix(4) == ["--recover", "cleanup-job", "--job-id", "9"] }) &&
            approved.phase == .complete && approvedInstalls == 1 && approvedCompletions == 1,
            "A single approval repairs every verified stage, rechecks, installs once and continues normal startup")

        let coordinatedFixture = try fixture()
        let coordinatedExecutor = BootstrapPreflightExecutorFixture(coordinatedFixture,
            report: .init(schemaVersion: 1, issues: [missingDefinitions, waitIssue]))
        let coordinated = QueueBootstrapModel(environment: coordinatedFixture.environment, execute: { request, output in
            try await coordinatedExecutor.execute(request, output: output)
        }, waitInterval: .milliseconds(1)) { approvedCompletions += 1 }
        await coordinated.start()?.value
        let coordinatedTask = coordinated.repairAndContinue(confirmed: true)
        try await waitUntil { coordinated.phase == .waiting }
        let busyMutations = await coordinatedExecutor.mutations
        try check(busyMutations == 0, "Approved recovery waits for coordination locks before attempting any repair")
        await coordinatedExecutor.update(.init(schemaVersion: 1, issues: [missingDefinitions]))
        await coordinatedTask?.value
        let coordinatedMutations = await coordinatedExecutor.mutations
        try check(coordinated.phase == .complete && coordinatedMutations == 1,
            "Approval survives normal coordination waiting and completes without another prompt")

        let acceptedFixture = try fixture()
        let acceptedExecutor = BootstrapPreflightExecutorFixture(acceptedFixture, report: .init(schemaVersion: 1, issues: [phoneIssue]))
        let accepted = QueueBootstrapModel(environment: acceptedFixture.environment, execute: { request, output in
            try await acceptedExecutor.execute(request, output: output)
        }) { approvedCompletions += 1 }
        await accepted.start()?.value
        await acceptedExecutor.holdRecovery()
        let acceptedTask = accepted.repairAndContinue(confirmed: true)
        try await waitUntil { await acceptedExecutor.isRecoveryHeld }
        accepted.cancelWaiting()
        try check(accepted.phase == .recovering && !accepted.canStopWaiting,
            "Stopping waiting cannot cancel an accepted repair mutation")
        await acceptedExecutor.releaseRecovery()
        await acceptedTask?.value
        let acceptedMutations = await acceptedExecutor.mutations
        let acceptedInstalls = await acceptedExecutor.installations
        try check(accepted.phase == .complete && acceptedMutations == 1 && acceptedInstalls == 1,
            "An accepted repair completes, rechecks and installs exactly once")

        let repeatedFixture = try fixture()
        let repeatedExecutor = BootstrapPreflightExecutorFixture(repeatedFixture,
            report: .init(schemaVersion: 1, issues: [phoneIssue]), afterRecovery: .init(schemaVersion: 1, issues: [phoneIssue]))
        let repeated = QueueBootstrapModel(environment: repeatedFixture.environment, execute: { request, output in
            try await repeatedExecutor.execute(request, output: output)
        }) { approvedCompletions += 1 }
        await repeated.start()?.value
        await repeated.repairAndContinue(confirmed: true)?.value
        let repeatedMutations = await repeatedExecutor.mutations
        let repeatedInstalls = await repeatedExecutor.installations
        try check(repeated.phase == .failed && repeatedMutations == 1 && repeatedInstalls == 0 && repeated.output.contains("did not clear"),
            "An unchanged approved repair stops with a diagnostic rather than repeating a mutation")

        let unknownWait = QueueBootstrapIssue(id: "unknown-ownership", title: "Unknown service", detail: "Ownership cannot be verified.",
            actions: [.init(id: "wait", title: "Wait", destructive: false)])
        for unsafeIssue in [configIssue, unknownWait,
            .init(id: "legacy-jobs", title: "Cancel job", detail: "Only cancellation is offered.",
                  actions: [.init(id: "cancel-job", title: "Cancel job", destructive: false)], jobs: jobIssue.jobs),
            .init(id: "pending-cleanup", title: "Cleanup", detail: "No eligible job is listed.",
                  actions: [.init(id: "cleanup-job", title: "Cleanup", destructive: false)])] {
            let unsafeFixture = try fixture()
            let unsafeExecutor = BootstrapPreflightExecutorFixture(unsafeFixture, report: .init(schemaVersion: 1, issues: [unsafeIssue]))
            let unsafe = QueueBootstrapModel(environment: unsafeFixture.environment, execute: { request, output in
                try await unsafeExecutor.execute(request, output: output)
            }) { approvedCompletions += 1 }
            await unsafe.start()?.value
            try check(!unsafe.canRepairAndContinue && unsafe.repairAndContinue(confirmed: true) == nil,
                "Aggregate approval excludes unknown ownership, callback replacement, job cancellation and ineligible cleanup")
        }

        for changesBeforeApproval in [true, false] {
            let changedFixture = try fixture()
            let changedExecutor = BootstrapPreflightExecutorFixture(changedFixture,
                report: .init(schemaVersion: 1, issues: [missingDefinitions]), afterRecovery: .init(schemaVersion: 1, issues: [configIssue]))
            let changed = QueueBootstrapModel(environment: changedFixture.environment, execute: { request, output in
                try await changedExecutor.execute(request, output: output)
            }) { approvedCompletions += 1 }
            await changed.start()?.value
            if changesBeforeApproval { await changedExecutor.update(.init(schemaVersion: 1, issues: [configIssue])) }
            await changed.repairAndContinue(confirmed: true)?.value
            let changedMutations = await changedExecutor.mutations
            let changedInstalls = await changedExecutor.installations
            try check(changed.phase == .reviewing && changed.issues == [configIssue] &&
                changedMutations == (changesBeforeApproval ? 0 : 1) && changedInstalls == 0,
                "A newly discovered custom blocker revokes the repair approval before further changes")
        }

        let cancelledRepairFixture = try fixture()
        let cancelledRepairExecutor = BootstrapPreflightExecutorFixture(cancelledRepairFixture, report: .init(schemaVersion: 1, issues: [waitIssue]))
        let cancelledRepair = QueueBootstrapModel(environment: cancelledRepairFixture.environment, execute: { request, output in
            try await cancelledRepairExecutor.execute(request, output: output)
        }, waitInterval: .milliseconds(1)) { approvedCompletions += 1 }
        await cancelledRepair.start()?.value
        await cancelledRepairExecutor.holdPreflight()
        let cancelledApprovedTask = cancelledRepair.repairAndContinue(confirmed: true)
        try await waitUntil { await cancelledRepairExecutor.isHeld }
        cancelledRepair.cancelWaiting()
        await cancelledRepairExecutor.update(.clear)
        await cancelledRepairExecutor.releasePreflight()
        await cancelledApprovedTask?.value
        let cancelledRepairInstalls = await cancelledRepairExecutor.installations
        try check(cancelledRepair.phase == .reviewing && cancelledRepairInstalls == 0,
            "Stopping an approved read-only check revokes approval and fences its late clean result")

        let repairErrorFixture = try fixture()
        let repairErrorExecutor = BootstrapPreflightExecutorFixture(repairErrorFixture, report: .init(schemaVersion: 1, issues: [phoneIssue]))
        let repairError = QueueBootstrapModel(environment: repairErrorFixture.environment, execute: { request, output in
            try await repairErrorExecutor.execute(request, output: output)
        }) { approvedCompletions += 1 }
        await repairError.start()?.value
        await repairErrorExecutor.setFailure()
        await repairError.repairAndContinue(confirmed: true)?.value
        let repairErrorMutations = await repairErrorExecutor.mutations
        try check(repairError.phase == .failed && repairError.output.contains("failed ownership verification") && repairErrorMutations == 1,
            "A failed approved repair retains its ownership diagnostic and requires a new approval")

        let waitFixture = try fixture()
        let waitExecutor = BootstrapPreflightExecutorFixture(waitFixture, report: .init(schemaVersion: 1, issues: [waitIssue]))
        let waiting = QueueBootstrapModel(environment: waitFixture.environment, execute: { request, output in
            try await waitExecutor.execute(request, output: output)
        }, waitInterval: .milliseconds(1), waitLimit: 2) { summaryCompletions += 1 }
        await waiting.start()?.value
        await waitExecutor.holdPreflight()
        let cancelledWait = waiting.waitAndContinue()
        try await waitUntil { await waitExecutor.isHeld }
        try check(waiting.phase == .waiting && waiting.start() == nil, "Waiting prevents duplicate setup attempts")
        waiting.cancelWaiting()
        await waitExecutor.update(.clear)
        await waitExecutor.releasePreflight()
        await cancelledWait?.value
        let cancelledInstalls = await waitExecutor.installations
        try check(waiting.phase == .reviewing && cancelledInstalls == 0,
            "Stopping waiting prevents a late clean report from installing or completing startup")
        await waiting.waitAndContinue()?.value
        try check(waiting.phase == .complete, "Waiting continues into verified installation only after a fresh clean report")

        let boundedFixture = try fixture()
        let boundedExecutor = BootstrapPreflightExecutorFixture(boundedFixture, report: .init(schemaVersion: 1, issues: [waitIssue]))
        let bounded = QueueBootstrapModel(environment: boundedFixture.environment, execute: { request, output in
            try await boundedExecutor.execute(request, output: output)
        }, waitInterval: .milliseconds(1), waitLimit: 2) { summaryCompletions += 1 }
        await bounded.start()?.value
        await bounded.waitAndContinue()?.value
        let boundedChecks = await boundedExecutor.checks
        try check(bounded.phase == .reviewing && boundedChecks == 3 && bounded.message.contains("still waiting"),
            "Automatic waiting has a finite check budget and returns control without weakening a blocker")

        let stoppedFixture = try fixture()
        let stoppedExecutor = BootstrapPreflightExecutorFixture(stoppedFixture, report: .clear)
        await stoppedExecutor.holdPreflight()
        let stopped = QueueBootstrapModel(environment: stoppedFixture.environment, execute: { request, output in
            try await stoppedExecutor.execute(request, output: output)
        }) { summaryCompletions += 1 }
        let stoppedCheck = stopped.start()
        try await waitUntil { await stoppedExecutor.isHeld }
        stopped.cancelWaiting()
        await stoppedExecutor.releasePreflight()
        await stoppedCheck?.value
        let stoppedInstalls = await stoppedExecutor.installations
        try check(stopped.phase == .failed && !stopped.isRunning && stoppedInstalls == 0,
            "Stopping the initial read-only preflight fences its eventual clean response")

        let moveFixture = try fixture("Downloads")
        let moveCounter = BootstrapCountingExecutorFixture()
        let moveModel = QueueBootstrapModel(environment: moveFixture.environment, execute: { request, output in
            await moveCounter.execute(request, output: output)
        }) { summaryCompletions += 1 }
        await moveModel.start()?.value
        let moveCalls = await moveCounter.calls
        try check(moveModel.phase == .failed && moveModel.issues.first?.actions.first?.id == "move-app" && moveCalls == 0,
            "An app outside Applications offers Finder guidance before executing any payload")

        let wireFixture = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .appendingPathComponent("fixtures/bootstrap-preflight.json")
        let wireReport = try QueueBootstrapReport.decode(Data(contentsOf: wireFixture))
        try check(wireReport.issues.count == 2 && wireReport.issues.first?.jobs?.first?.id == 8 &&
            wireReport.issues.last?.actions.last?.id == "reset-callback" && wireReport.revision == String(repeating: "a", count: 64),
            "The shared backend wire fixture decodes into native job actions and configuration revision")
        let unicodeReport = QueueBootstrapReport(schemaVersion: 1, issues: [.init(id: "unicode-jobs", title: "Jobs",
            detail: "Review the selected job.", actions: [], jobs: [.init(id: 8, label: String(repeating: "😀", count: 120), status: "Paused")])])
        try check(try QueueBootstrapReport.decode(JSONEncoder().encode(unicodeReport)) == unicodeReport,
            "Native reports accept the backend's bounded Unicode job labels without confusing bytes with characters")

        for malformed in [Data(), Data(#"{"schemaVersion":2,"issues":[]}"#.utf8),
                          Data(#"{"schemaVersion":1,"issues":[{"id":"unsafe","title":"Unsafe","detail":"Review","actions":[{"id":"execute-shell","title":"Continue","destructive":false}]}]}"#.utf8),
                          Data(#"{"schemaVersion":1,"issues":[{"id":"jobs","title":"Jobs","detail":"Review","actions":[],"jobs":[{"id":-1,"label":"Invalid","status":"Queued"}]}]}"#.utf8)] {
            let malformedFixture = try fixture()
            let counter = BootstrapRawPreflightFixture(malformed)
            let malformedModel = QueueBootstrapModel(environment: malformedFixture.environment, execute: { request, output in
                await counter.execute(request, output: output)
            }) { summaryCompletions += 1 }
            await malformedModel.start()?.value
            let calls = await counter.calls
            try check(malformedModel.phase == .failed && calls == 1,
                "Malformed preflight or unknown actions fail before any mutation")
        }
    }

    @MainActor private static func waitUntil(_ predicate: () async -> Bool) async throws {
        let deadline = ProcessInfo.processInfo.systemUptime + 5
        while !(await predicate()) {
            guard ProcessInfo.processInfo.systemUptime < deadline else { throw ClientError.message("Bootstrap fixture did not make progress") }
            try await Task.sleep(for: .milliseconds(5))
        }
    }
}
