import Foundation
import Darwin

// Shared by headless and graphical tests. No installed paths, services or real
// executables are touched. These shell files are inspected, never executed.
struct BootstrapFixture: Sendable {
    let directory: URL
    let environment: QueueBootstrapEnvironment
    let identity: Data

    init(parent: URL, location: String = "Applications", tracked: Bool = true) throws {
        directory = try Self.canonicalDirectory(parent).appendingPathComponent(UUID().uuidString)
        let home = directory.appendingPathComponent("home")
        let applications = directory.appendingPathComponent("Applications")
        let appParent = location == "home" ? home.appendingPathComponent("Applications") : directory.appendingPathComponent(location)
        let app = appParent.appendingPathComponent("Agent Queue.app")
        environment = QueueBootstrapEnvironment(app: app, resources: app.appendingPathComponent("Contents/Resources"),
            home: home, systemApplications: applications)
        identity = try JSONEncoder().encode(BootstrapFixtureIdentity(version: "1.2.3", commit: tracked ? String(repeating: "a", count: 40) : nil,
            build: tracked ? 42 : nil))
        try Self.write(Data("#!/bin/sh\nexit 99\n".utf8), to: environment.source.appendingPathComponent("bin/agent-heavy"), executable: true)
        try Self.write(Data("#!/bin/sh\nexit 99\n".utf8), to: environment.source.appendingPathComponent("bin/agent-remote"), executable: true)
        try Self.write(identity, to: environment.source.appendingPathComponent("build-info.json"))
        try Self.write(identity, to: environment.resources.appendingPathComponent("build-info.json"))
        let info: [String: Any] = ["CFBundleIdentifier": "io.went.agent-queue", "CFBundleShortVersionString": "1.2.3",
            "CFBundleVersion": tracked ? "42" : "0", "AgentQueueEmbeddedPayload": true]
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
    private(set) var requests: [QueueBootstrapRequest] = []
    private var held: CheckedContinuation<Void, Never>?
    private var releases = 0

    init(_ fixture: BootstrapFixture) { self.fixture = fixture }

    func execute(_ request: QueueBootstrapRequest, output: @escaping @Sendable (Data) -> Void) async throws -> Data {
        requests.append(request)
        output(Data("Preparing private fixture services…".utf8))
        await withCheckedContinuation { held = $0 }
        if releases == 1 { throw ClientError.message("Fixture service could not start. Correct the issue, then Retry.") }
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
