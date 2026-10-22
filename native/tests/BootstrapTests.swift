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
        let incomplete = QueueBootstrapModel(environment: noMutation.environment, execute: { _, _ in Data() }) {
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
    }

    @MainActor private static func waitUntil(_ predicate: () async -> Bool) async throws {
        let deadline = ProcessInfo.processInfo.systemUptime + 5
        while !(await predicate()) {
            guard ProcessInfo.processInfo.systemUptime < deadline else { throw ClientError.message("Bootstrap fixture did not make progress") }
            try await Task.sleep(for: .milliseconds(5))
        }
    }
}
