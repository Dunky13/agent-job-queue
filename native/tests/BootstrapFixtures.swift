import Foundation
import Darwin

// Shared by headless and graphical tests. No installed paths, services or real
// executables are touched. These shell files are inspected, never executed.
struct BootstrapFixture: Sendable {
    let directory: URL
    let environment: QueueBootstrapEnvironment
    let identity: Data

    init(parent: URL, location: String = "Applications", tracked: Bool = true,
         setupMode: Bool = true, managed: Bool = false) throws {
        directory = try Self.canonicalDirectory(parent).appendingPathComponent(UUID().uuidString)
        let home = directory.appendingPathComponent("home")
        let applications = directory.appendingPathComponent("Applications")
        let appParent = location == "home" ? home.appendingPathComponent("Applications") : directory.appendingPathComponent(location)
        let app = appParent.appendingPathComponent("Agent Queue.app")
        environment = QueueBootstrapEnvironment(app: app, resources: app.appendingPathComponent("Contents/Resources"),
            home: home, arguments: setupMode ? ["--setup"] : [], systemApplications: applications)
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

actor BootstrapServiceStatusFixture {
    private(set) var requests: [QueueBootstrapRequest] = []
    let status: String

    init(status: String = "enabled") { self.status = status }

    func execute(_ request: QueueBootstrapRequest) throws -> Data {
        requests.append(request)
        guard request.arguments.count == 3, request.arguments.prefix(2) == ["--service-management", "status"],
              let service = request.arguments.last else {
            throw ClientError.message("Ordinary startup attempted a service mutation.")
        }
        return try JSONEncoder().encode(BootstrapFixtureServiceStatus(service: service, status: status))
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
        if phoneAccessBlocker && !request.arguments.contains("--disable-phone-access") {
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
