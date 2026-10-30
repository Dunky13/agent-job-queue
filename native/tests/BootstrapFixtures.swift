import Foundation
import Darwin

// Shared by headless and graphical tests. No installed paths, services or real
// executables are touched. These shell files are inspected, never executed.
struct BootstrapFixture: Sendable {
    let directory: URL
    let environment: QueueBootstrapEnvironment
    let identity: Data

    init(parent: URL, location: String = "Applications", tracked: Bool = true,
         setupMode: Bool = true, managed: Bool = false, arguments: [String] = []) throws {
        directory = try Self.canonicalDirectory(parent).appendingPathComponent(UUID().uuidString)
        let home = directory.appendingPathComponent("home")
        let applications = directory.appendingPathComponent("Applications")
        let appParent = location == "home" ? home.appendingPathComponent("Applications") : directory.appendingPathComponent(location)
        let app = appParent.appendingPathComponent("Agent Queue.app")
        environment = QueueBootstrapEnvironment(app: app, resources: app.appendingPathComponent("Contents/Resources"),
            home: home, arguments: arguments, systemApplications: applications, setupAllowed: setupMode)
        identity = try JSONEncoder().encode(BootstrapFixtureIdentity(version: "1.2.3", commit: tracked ? String(repeating: "a", count: 40) : nil,
            build: tracked ? 42 : nil))
        let binaries = managed ? app.appendingPathComponent("Contents/MacOS") : environment.source.appendingPathComponent("bin")
        try Self.write(Data("#!/bin/sh\nexit 99\n".utf8), to: binaries.appendingPathComponent("agent-heavy"), executable: true)
        try Self.write(Data("#!/bin/sh\nexit 99\n".utf8), to: binaries.appendingPathComponent("agent-remote"), executable: true)
        if managed {
            try Self.write(Data("#!/bin/sh\nexit 99\n".utf8), to: binaries.appendingPathComponent("AgentQueue"), executable: true)
            for label in ["io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-remote"] {
                let remote = label == "io.went.agent-remote"
                let command = remote ? "agent-remote" : "agent-heavy"
                let action = remote ? "serve" : (label == "io.went.agent-heavy" ? "daemon" : "scheduler")
                let definition: [String: Any] = ["Label": label, "BundleProgram": "Contents/MacOS/\(command)",
                    "ProgramArguments": [command, action]]
                try Self.write(try PropertyListSerialization.data(fromPropertyList: definition, format: .xml, options: 0),
                    to: app.appendingPathComponent("Contents/Library/LaunchAgents/\(label).plist"))
            }
        }
        try Self.write(identity, to: environment.source.appendingPathComponent("build-info.json"))
        try Self.write(identity, to: environment.resources.appendingPathComponent("build-info.json"))
        let info: [String: Any] = ["CFBundleIdentifier": "io.went.agent-queue", "CFBundleShortVersionString": "1.2.3",
            "CFBundleVersion": tracked ? "42" : "0", "AgentQueueEmbeddedPayload": true, "AgentQueueServiceManagement": managed]
        try Self.write(try PropertyListSerialization.data(fromPropertyList: info, format: .xml, options: 0),
            to: app.appendingPathComponent("Contents/Info.plist"))
    }

    func install() throws {
        try Self.write(identity, to: environment.root.appendingPathComponent("build-info.json"))
        for name in ["agent-heavy", "agent-remote"] {
            try Self.write(Data("#!/bin/sh\nexit 99\n".utf8), to: environment.root.appendingPathComponent("bin/\(name)"), executable: true)
            try Self.write(Data("#!/bin/sh\nexit 99\n".utf8), to: environment.home.appendingPathComponent(".local/bin/\(name)"), executable: true)
        }
        for (label, arguments) in [
            ("io.went.agent-heavy", [environment.root.appendingPathComponent("bin/agent-heavy").path, "daemon"]),
            ("io.went.agent-resources", [environment.root.appendingPathComponent("bin/agent-heavy").path, "scheduler"]),
            ("io.went.agent-queue", ["/usr/bin/open", "-a", environment.app.path])
        ] { try writeDefinition(label, arguments: arguments) }
        let pending = environment.root.appendingPathComponent("install-in-progress.json")
        if FileManager.default.fileExists(atPath: pending.path) { try FileManager.default.removeItem(at: pending) }
    }

    func installManaged() throws {
        try Self.write(identity, to: environment.root.appendingPathComponent("build-info.json"))
        let record = BootstrapFixtureInstallation(app: environment.app.path)
        try Self.write(try JSONEncoder().encode(record), to: environment.root.appendingPathComponent("installation.json"))
        for name in ["agent-heavy", "agent-remote"] {
            try Self.write(Data("#!/bin/sh\nexit 99\n".utf8),
                to: environment.home.appendingPathComponent(".local/bin/\(name)"), executable: true)
        }
    }

    func writeDefinition(_ label: String, arguments: [String], program: String? = nil, runAtLoad: Bool? = true) throws {
        var definition: [String: Any] = ["Label": label, "ProgramArguments": arguments]
        definition["Program"] = program
        definition["RunAtLoad"] = runAtLoad
        try Self.write(try PropertyListSerialization.data(fromPropertyList: definition, format: .xml, options: 0),
            to: environment.home.appendingPathComponent("Library/LaunchAgents/\(label).plist"))
    }

    static func write(_ data: Data, to url: URL, executable: Bool = false) throws {
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])
        try data.write(to: url)
        try FileManager.default.setAttributes([.posixPermissions: executable ? 0o700 : 0o600], ofItemAtPath: url.path)
    }

    // Foundation normalizes /private/var back to the /var symlink on macOS.
    // Use the same POSIX spelling as the backend's no-follow fixture paths.
    static func canonicalDirectory(_ url: URL) throws -> URL {
        guard let path = realpath(url.path, nil) else { throw ClientError.message("Could not resolve private fixture directory") }
        defer { free(path) }
        return URL(fileURLWithPath: String(cString: path), isDirectory: true)
    }
}

private struct BootstrapFixtureInstallation: Encodable {
    let schemaVersion = 1
    let app: String
    let serviceManagement = true
}

actor BootstrapInstallerProgressFixture {
    static let loginNote = "Agent Queue is set up. Opening it at login is waiting for approval in System Settings > General > Login Items & Extensions; allow it there if you want that."
    let fixture: BootstrapFixture
    let report: QueueBootstrapReport
    private(set) var installations = 0
    private var held: CheckedContinuation<Void, Never>?

    init(_ fixture: BootstrapFixture, report: QueueBootstrapReport = .clear) {
        self.fixture = fixture
        self.report = report
    }

    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) async throws -> Data {
        if request.arguments.suffix(2) == ["--preflight", "--json"] {
            return try JSONEncoder().encode(report)
        }
        guard request.arguments == ["install", "--app-bundle", fixture.environment.app.path] + (fixture.environment.arguments.contains("--setup") ? [] : ["--repair-and-continue"]) else {
            throw ClientError.message("Diagnostic setup used an unexpected request.")
        }
        installations += 1
        // The installer registers only the login item and never waits for its approval.
        output(Data(Self.loginNote.utf8))
        await withCheckedContinuation { held = $0 }
        try fixture.install()
        return Data("Diagnostic fixture setup complete.".utf8)
    }

    func release() { held?.resume(); held = nil }
    var isHeld: Bool { held != nil }
}

// An installer error that carries the retired approval marker is an ordinary failure:
// the app never polls for approval. Only an explicit retry runs setup again.
actor BootstrapApprovalRetryFixture {
    let fixture: BootstrapFixture
    private(set) var installations = 0
    private(set) var statusReads = 0

    init(_ fixture: BootstrapFixture) { self.fixture = fixture }

    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) async throws -> Data {
        if request.arguments.suffix(2) == ["--preflight", "--json"] {
            return try JSONEncoder().encode(QueueBootstrapReport.clear)
        }
        if request.arguments.count == 3, request.arguments.prefix(2) == ["--service-management", "status"], let service = request.arguments.last {
            statusReads += 1
            return try JSONEncoder().encode(BootstrapFixtureServiceStatus(service: service, status: "requiresApproval"))
        }
        guard request.arguments == ["install", "--app-bundle", fixture.environment.app.path, "--repair-and-continue"] else {
            throw ClientError.message("Setup used an unexpected request.")
        }
        installations += 1
        guard installations > 1 else {
            throw ClientError.message(QueueSetupProgress.approvalMarker + "\nAgent Queue setup: The queue service is requiresApproval; allow Agent Queue in System Settings before setup can finish")
        }
        try fixture.installManaged()
        return Data("Fixture setup complete.".utf8)
    }
}

private struct BootstrapFixtureServiceStatus: Encodable {
    let schemaVersion = 1
    let service: String
    let status: String
}

struct BootstrapFixtureIdentity: Encodable {
    let schemaVersion = 1
    let repository = "Dunky13/agent-job-queue"
    let version: String
    let commit: String?
    let build: Int?

    // Encode optional identity fields as JSON null, like the production metadata.
    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(schemaVersion, forKey: .schemaVersion)
        try container.encode(repository, forKey: .repository)
        try container.encode(version, forKey: .version)
        try container.encode(commit, forKey: .commit)
        try container.encode(build, forKey: .build)
    }
    enum CodingKeys: String, CodingKey { case schemaVersion, repository, version, commit, build }
}

actor BootstrapExecutorFixture {
    let fixture: BootstrapFixture
    let phoneAccessBlocker: Bool
    private(set) var requests: [QueueBootstrapRequest] = []
    private var held: CheckedContinuation<Void, Never>?
    private var releases = 0

    init(_ fixture: BootstrapFixture, phoneAccessBlocker: Bool = false) {
        self.fixture = fixture
        self.phoneAccessBlocker = phoneAccessBlocker
    }

    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) async throws -> Data {
        if request.arguments.suffix(2) == ["--preflight", "--json"] {
            return try JSONEncoder().encode(QueueBootstrapReport.clear)
        }
        requests.append(request)
        output(Data("Preparing private fixture services…".utf8))
        await withCheckedContinuation { held = $0 }
        if releases == 1 {
            if phoneAccessBlocker {
                throw ClientError.message("Agent Queue installer: Phone access from the previous installation must be disabled before setup can continue. Credentials and configuration will be preserved\nAGENT_QUEUE_SETUP_RECOVERY:disable-phone-access\n")
            }
            throw ClientError.message("Fixture service could not start. Correct the issue, then Retry.")
        }
        if phoneAccessBlocker && !request.arguments.contains("--disable-phone-access") && !request.arguments.contains("--repair-and-continue") {
            throw ClientError.message("Fixture recovery did not request disabling phone access.")
        }
        try fixture.install()
        return Data("Fixture setup complete.".utf8)
    }

    func release() { releases += 1; held?.resume(); held = nil }
    var isHeld: Bool { held != nil }
}

actor BootstrapRetainedExecutorFixture {
    let fixture: BootstrapFixture
    private(set) var calls = 0

    init(_ fixture: BootstrapFixture) { self.fixture = fixture }

    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) throws -> Data {
        if request.arguments.suffix(2) == ["--preflight", "--json"] {
            return try JSONEncoder().encode(QueueBootstrapReport.clear)
        }
        calls += 1
        try fixture.install()
        let binary = fixture.environment.root.appendingPathComponent("bin/agent-heavy").path
        try fixture.writeDefinition("io.went.agent-heavy", arguments: [binary, "daemon"], program: binary, runAtLoad: false)
        try fixture.writeDefinition("io.went.agent-resources", arguments: [binary, "scheduler"], program: binary, runAtLoad: nil)
        return Data("Retained fixture definitions.".utf8)
    }
}

actor BootstrapCountingExecutorFixture {
    private(set) var calls = 0

    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) -> Data {
        calls += 1
        return Data()
    }
}

actor BootstrapAutomaticSetupFixture {
    let fixture: BootstrapFixture
    let report: QueueBootstrapReport
    private var status: String
    private(set) var requests: [QueueBootstrapRequest] = []

    init(_ fixture: BootstrapFixture, report: QueueBootstrapReport = .clear, status: String = "notRegistered") {
        self.fixture = fixture
        self.report = report
        self.status = status
    }

    func execute(_ request: QueueBootstrapRequest) throws -> Data {
        requests.append(request)
        if request.arguments.suffix(2) == ["--preflight", "--json"] {
            return try JSONEncoder().encode(report)
        }
        if request.arguments.prefix(2) == ["--service-management", "status"], let service = request.arguments.last {
            return try JSONEncoder().encode(BootstrapFixtureServiceStatus(service: service, status: status))
        }
        guard request.arguments == ["install", "--app-bundle", fixture.environment.app.path, "--repair-and-continue"] else {
            throw ClientError.message("Automatic setup did not use the backend's verified repair contract")
        }
        try fixture.installManaged()
        status = "notRegistered"
        return Data("Verified setup complete".utf8)
    }
}

actor BootstrapPreflightExecutorFixture {
    let fixture: BootstrapFixture
    private(set) var requests: [QueueBootstrapRequest] = []
    private(set) var checks = 0
    private(set) var mutations = 0
    private(set) var installations = 0
    private var report: QueueBootstrapReport
    private var afterRecovery: QueueBootstrapReport
    private var recoveryReports: [QueueBootstrapReport] = []
    private var holdNext = false
    private var held: CheckedContinuation<Void, Never>?
    private var holdNextRecovery = false
    private var recoveryHeld: CheckedContinuation<Void, Never>?
    var failRecovery = false
    var returnOnlyClearedRecovery = false
    private var failedInstallationReport: QueueBootstrapReport?

    init(_ fixture: BootstrapFixture, report: QueueBootstrapReport, afterRecovery: QueueBootstrapReport = .clear) {
        self.fixture = fixture
        self.report = report
        self.afterRecovery = afterRecovery
    }

    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) async throws -> Data {
        requests.append(request)
        if request.arguments.suffix(2) == ["--preflight", "--json"] {
            checks += 1
            if holdNext {
                holdNext = false
                await withCheckedContinuation { held = $0 }
            }
            return try JSONEncoder().encode(report)
        }
        if request.arguments.contains("--recover") {
            mutations += 1
            if holdNextRecovery {
                holdNextRecovery = false
                await withCheckedContinuation { recoveryHeld = $0 }
            }
            if failRecovery { throw ClientError.message("Fixture recovery failed ownership verification.") }
            if !recoveryReports.isEmpty { afterRecovery = recoveryReports.removeFirst() }
            if !returnOnlyClearedRecovery { report = afterRecovery }
            return try JSONEncoder().encode(afterRecovery)
        }
        installations += 1
        if let failedInstallationReport {
            self.failedInstallationReport = nil
            report = failedInstallationReport
            throw ClientError.message("Fixture daemon could not become ready.")
        }
        try fixture.install()
        return Data("Fixture setup complete.".utf8)
    }

    func update(_ report: QueueBootstrapReport) { self.report = report }
    func updateAfterRecovery(_ report: QueueBootstrapReport) { afterRecovery = report }
    func queueRecoveryReports(_ reports: [QueueBootstrapReport]) { recoveryReports = reports }
    func holdPreflight() { holdNext = true }
    func releasePreflight() { held?.resume(); held = nil }
    var isHeld: Bool { held != nil }
    func holdRecovery() { holdNextRecovery = true }
    func releaseRecovery() { recoveryHeld?.resume(); recoveryHeld = nil }
    var isRecoveryHeld: Bool { recoveryHeld != nil }
    func setFailure() { failRecovery = true }
    func setStaleRecovery() { returnOnlyClearedRecovery = true }
    func failInstallation(with report: QueueBootstrapReport) { failedInstallationReport = report }
}

actor BootstrapRawPreflightFixture {
    let data: Data
    private(set) var calls = 0
    init(_ data: Data) { self.data = data }
    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) -> Data {
        calls += 1
        return data
    }
}

// Models an already accepted finite mutation: cancellation must wait for its
// response, rather than abandoning it or starting a competing retry.
actor BootstrapQuitMutationFixture {
    let fixture: BootstrapFixture
    private var held: CheckedContinuation<Void, Never>?
    private(set) var completed = false
    var isHeld: Bool { held != nil }
    init(_ fixture: BootstrapFixture) { self.fixture = fixture }
    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) async throws -> Data {
        if request.arguments.suffix(2) == ["--preflight", "--json"] {
            return try JSONEncoder().encode(QueueBootstrapReport.clear)
        }
        guard request.arguments.last == "--repair-and-continue" else { throw ClientError.message("Unexpected quit fixture mutation") }
        output(Data("Preparing private fixture services…".utf8))
        await withCheckedContinuation { held = $0 }
        try fixture.install()
        completed = true
        return Data("Accepted mutation completed".utf8)
    }
    func release() { held?.resume(); held = nil }
}

/// A supervisor spawner that starts no process. A fake child ends on SIGTERM only after
/// `releaseExits()`, so a test can observe termination waiting for its children.
final class BootstrapFakeSpawner: @unchecked Sendable {
    private struct Record {
        let name: QueueHelper.Name
        var signals: [Int32] = []
        var onExit: (@Sendable (QueueHelper.Exit) -> Void)?
    }

    private let lock = NSLock()
    private var records: [Record] = []
    private var holdsExits = true

    var spawner: QueueHelper.Spawner {
        { launch, onExit in
            let index = self.lock.withLock { () -> Int in
                self.records.append(Record(name: launch.name, onExit: onExit))
                return self.records.count - 1
            }
            return QueueHelper.Child(pid: 90_000 + Int32(index), signal: { signal in
                let release = self.lock.withLock { () -> Bool in
                    self.records[index].signals.append(signal)
                    return signal == SIGKILL || !self.holdsExits
                }
                if release { self.finish(index) }
            })
        }
    }

    var launched: [QueueHelper.Name] { lock.withLock { records.map(\.name) } }
    func signals(_ name: QueueHelper.Name) -> [Int32] { lock.withLock { records.filter { $0.name == name }.flatMap(\.signals) } }

    func releaseExits() {
        let pending = lock.withLock { () -> [Int] in
            holdsExits = false
            return records.indices.filter { records[$0].onExit != nil && records[$0].signals.contains(SIGTERM) }
        }
        pending.forEach(finish)
    }

    private func finish(_ index: Int) {
        let callback = lock.withLock { () -> (@Sendable (QueueHelper.Exit) -> Void)? in
            defer { records[index].onExit = nil }
            return records[index].onExit
        }
        callback?(.signal(SIGTERM))
    }
}
