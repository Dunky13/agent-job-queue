import AppKit
import Darwin
import Foundation
import SwiftUI

// This is a read-only startup decision. The backend remains the authority for
// installation, migration, ownership checks and preserving existing settings.
struct QueueBootstrapEnvironment: Sendable {
    let app: URL
    let resources: URL
    let home: URL
    var arguments: [String] = []
    var systemApplications = URL(fileURLWithPath: "/Applications", isDirectory: true)

    static func current(bundle: Bundle = .main) -> Self {
        Self(app: bundle.bundleURL, resources: bundle.resourceURL ?? bundle.bundleURL.appendingPathComponent("Contents/Resources"),
             home: FileManager.default.homeDirectoryForCurrentUser, arguments: CommandLine.arguments)
    }

    var root: URL { home.appendingPathComponent(".local/share/agent-heavy") }
    var source: URL { resources.appendingPathComponent("payload/source") }

    func validateDiagnostics() throws {
        let fixtureIndices = arguments.indices.filter { arguments[$0] == "--fixture" }
        if !fixtureIndices.isEmpty || arguments.contains(where: { $0.hasPrefix("--fixture=") }) {
            guard fixtureIndices.count == 1, let index = fixtureIndices.first,
                  arguments.indices.contains(index + 1), !arguments[index + 1].isEmpty,
                  !arguments[index + 1].hasPrefix("-"),
                  !arguments.contains(where: { $0.hasPrefix("--fixture=") }) else {
                throw ClientError.message("--fixture requires a JSONFILE argument. No backend changes have been made.")
            }
        }
        if arguments.contains(where: { $0.hasPrefix("--snapshot") }) {
            guard !fixtureIndices.isEmpty else { throw ClientError.message("--snapshot requires --fixture JSONFILE. No backend changes have been made.") }
        }
        let snapshotIndices = arguments.indices.filter { arguments[$0] == "--snapshot" }
        if !snapshotIndices.isEmpty || arguments.contains(where: { $0.hasPrefix("--snapshot=") }) {
            guard snapshotIndices.count == 1, let index = snapshotIndices.first,
                  arguments.indices.contains(index + 1), !arguments[index + 1].isEmpty,
                  !arguments[index + 1].hasPrefix("-"),
                  !arguments.contains(where: { $0.hasPrefix("--snapshot=") }) else {
                throw ClientError.message("--snapshot requires a PNGFILE argument. No backend changes have been made.")
            }
        }
    }

    func inspect() throws -> QueueBootstrapDecision {
        try validateDiagnostics()
        // Fixtures and developer builds must never launch installation commands.
        if arguments.contains("--fixture") { return .legacy }
        let infoURL = app.appendingPathComponent("Contents/Info.plist")
        let bundleScope = QueueBootstrapFiles.Scope(root: app, bundled: true)
        try QueueBootstrapFiles.validateFile(infoURL, scope: bundleScope)
        let payloadExists = Self.exists(resources.appendingPathComponent("payload"))
        let marked = (try? Self.readPlist(QueueBootstrapPackaging.self, at: infoURL, scope: bundleScope))?.embeddedPayload == true
        guard payloadExists || marked else { return .legacy }
        let info = try? Self.readPlist(QueueBootstrapBundle.self, at: infoURL, scope: bundleScope)

        let parent = app.deletingLastPathComponent().standardizedFileURL.path
        let allowed = [systemApplications, home.appendingPathComponent("Applications")].map(\.standardizedFileURL)
        guard app.pathExtension == "app", allowed.map(\.path).contains(parent),
              allowed.map({ $0.resolvingSymlinksInPath().path }).contains(app.resolvingSymlinksInPath().deletingLastPathComponent().path),
              !Self.isSymlink(app) else {
            throw ClientError.message("Move Agent Queue to Applications before opening it. Quit this copy, drag Agent Queue.app into /Applications or your home Applications folder, then open that copy. If it is on a disk image, eject the image afterwards. No backend changes have been made.")
        }
        guard let info, info.identifier == "io.went.agent-queue" else {
            throw ClientError.message("The app's package metadata is missing or invalid. Download Agent Queue again.")
        }
        // Validate the full source even on a current installation. Never silently
        // use an older installed backend when the running package is incomplete.
        let identity: QueueBootstrapIdentity
        do {
            guard resources.path == app.appendingPathComponent("Contents/Resources").path else {
                throw ClientError.message("Unexpected app resource location.")
            }
            identity = try Self.readIdentity(source.appendingPathComponent("build-info.json"), scope: bundleScope)
            let bundled = try Self.readIdentity(resources.appendingPathComponent("build-info.json"), scope: bundleScope)
            guard identity == bundled, identity.version == info.version,
                  String(identity.build ?? 0) == info.build else {
                throw ClientError.message("App and payload build identities differ.")
            }
            for name in ["agent-heavy", "agent-remote"] {
                let binary = source.appendingPathComponent("bin/\(name)")
                guard try QueueBootstrapFiles.safeExecutable(binary, scope: bundleScope) else {
                    throw ClientError.message("The bundled \(name) executable is missing or unsafe.")
                }
            }
        } catch {
            throw ClientError.message("Agent Queue's embedded setup payload is incomplete or invalid. Download the app again. \(error.localizedDescription)")
        }
        let request = QueueBootstrapRequest(executable: source.appendingPathComponent("bin/agent-heavy"),
            arguments: ["install", "--app-bundle", app.path])
        let cli = home.appendingPathComponent(".local/bin")
        let launch = home.appendingPathComponent("Library/LaunchAgents")
        let privateScope = QueueBootstrapFiles.Scope(root: root)
        let cliScope = QueueBootstrapFiles.Scope(root: cli)
        let launchScope = QueueBootstrapFiles.Scope(root: launch)
        // Check every existing destination before returning an install request,
        // even when version metadata is absent or an installation is incomplete.
        // Missing destinations are left for the backend to create privately.
        for directory in [root, root.appendingPathComponent("bin"), cli, launch,
                          home.appendingPathComponent(".local/state/agent-heavy"), home.appendingPathComponent(".config/agent-heavy")] {
            try QueueBootstrapFiles.validateDirectory(directory, scope: .init(root: directory))
        }
        for name in ["build-info.json", "install-in-progress.json", "bin/agent-heavy", "bin/agent-remote"] {
            try QueueBootstrapFiles.validateFile(root.appendingPathComponent(name), scope: privateScope)
        }
        for name in ["agent-heavy", "agent-remote"] {
            try QueueBootstrapFiles.validateFile(cli.appendingPathComponent(name), scope: cliScope)
        }
        for label in ["io.went.agent-heavy", "io.went.agent-resources", "io.went.agent-queue"] {
            try QueueBootstrapFiles.validateFile(launch.appendingPathComponent("\(label).plist"), scope: launchScope)
        }
        if Self.exists(root.appendingPathComponent("install-in-progress.json")) {
            return .install(request, "Finishing an incomplete installation…")
        }
        guard let installed = try? Self.readIdentity(root.appendingPathComponent("build-info.json"), scope: privateScope), installed == identity else {
            return .install(request, "Setting up this version for your macOS account…")
        }
        for name in ["agent-heavy", "agent-remote"] {
            guard try QueueBootstrapFiles.safeExecutable(root.appendingPathComponent("bin/\(name)"), scope: privateScope) else {
                return .install(request, "Repairing missing backend components…")
            }
            // The menu-bar client uses the stable CLI entry points too.
            let entry = home.appendingPathComponent(".local/bin/\(name)")
            guard try QueueBootstrapFiles.safeExecutable(entry, scope: cliScope) else {
                return .install(request, "Repairing missing command entry points…")
            }
        }
        let definitions = [
            ("io.went.agent-heavy", [root.appendingPathComponent("bin/agent-heavy").path, "daemon"]),
            ("io.went.agent-resources", [root.appendingPathComponent("bin/agent-heavy").path, "scheduler"]),
            ("io.went.agent-queue", ["/usr/bin/open", "-a", app.path])
        ]
        for (label, arguments) in definitions {
            // The installer preserves embedded daemon/controller definitions,
            // including RunAtLoad. Only the app launch definition is rewritten.
            let appLaunch = label == "io.went.agent-queue"
            guard let definition = try? Self.readPlist(QueueBootstrapLaunch.self, at: launch.appendingPathComponent("\(label).plist"), scope: launchScope),
                  definition.label == label, definition.arguments == arguments,
                  definition.program == nil || (!appLaunch && definition.program == arguments.first),
                  !appLaunch || definition.runAtLoad == true else {
                return .install(request, "Updating background services and the app launch location…")
            }
        }
        return .ready
    }

    private static func exists(_ url: URL) -> Bool {
        var value = stat()
        return lstat(url.path, &value) == 0
    }

    private static func isSymlink(_ url: URL) -> Bool {
        var value = stat()
        return lstat(url.path, &value) == 0 && value.st_mode & S_IFMT == S_IFLNK
    }

    // Nonblocking/no-follow reads avoid hanging on a FIFO or trusting a symlink.
    private static func read(_ url: URL, scope: QueueBootstrapFiles.Scope) throws -> Data {
        guard let descriptor = try QueueBootstrapFiles.openFile(url, scope: scope) else {
            throw ClientError.message("Could not read \(url.lastPathComponent).")
        }
        let handle = FileHandle(fileDescriptor: descriptor, closeOnDealloc: true)
        defer { try? handle.close() }
        let data = try handle.read(upToCount: 16_385) ?? Data()
        guard data.count <= 16_384 else { throw ClientError.message("Setup metadata exceeds its size limit.") }
        return data
    }

    private static func readPlist<T: Decodable>(_ type: T.Type, at url: URL, scope: QueueBootstrapFiles.Scope) throws -> T {
        try PropertyListDecoder().decode(type, from: read(url, scope: scope))
    }

    private static func readIdentity(_ url: URL, scope: QueueBootstrapFiles.Scope) throws -> QueueBootstrapIdentity {
        let identity = try JSONDecoder().decode(QueueBootstrapIdentity.self, from: read(url, scope: scope))
        let versionPattern = #"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)\.)*(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$"#
        guard identity.schemaVersion == 1, identity.repository == "Dunky13/agent-job-queue",
              identity.version.range(of: versionPattern, options: .regularExpression) != nil,
              identity.commit == nil || identity.commit?.range(of: "^[0-9a-f]{40}$", options: .regularExpression) != nil,
              identity.build == nil || ((1...999_999_999).contains(identity.build ?? 0) && identity.commit != nil) else {
            throw ClientError.message("Invalid setup build identity.")
        }
        return identity
    }
}

// A descriptor-relative boundary matching the installer's bundle/private split.
// Opening each component pins its parent and never follows directory symlinks.
enum QueueBootstrapFiles {
    struct Scope {
        let root: URL
        var bundled = false
    }

    private static func unsafe(_ url: URL) -> ClientError {
        .message("Unsafe setup path: \(url.lastPathComponent). Resolve its ownership, permissions or symlinks before retrying.")
    }

    private static func parent(_ url: URL, scope: Scope) throws -> Int32? {
        let path = url.path
        let boundary = scope.root.path
        let parts = path.split(separator: "/")
        guard path.hasPrefix("/"), boundary.hasPrefix("/"), path.hasPrefix(boundary + "/"),
              path.utf8.count < Int(PATH_MAX), parts.count <= 128,
              !parts.contains("."), !parts.contains(".."),
              path == "/" + parts.joined(separator: "/"),
              boundary == "/" + boundary.split(separator: "/").joined(separator: "/") else {
            throw unsafe(url)
        }
        var descriptor = open("/", O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard descriptor >= 0 else { throw unsafe(url) }
        var transferred = false
        defer { if !transferred { close(descriptor) } }
        var current = ""
        for part in parts.dropLast() {
            current += "/" + part
            let child = openat(descriptor, String(part), O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
            if child < 0 {
                if errno == ENOENT { return nil }
                throw unsafe(url)
            }
            close(descriptor)
            descriptor = child
            var value = stat()
            guard fstat(descriptor, &value) == 0, value.st_mode & S_IFMT == S_IFDIR else { throw unsafe(url) }
            let inside = current == boundary || current.hasPrefix(boundary + "/")
            let owned = value.st_uid == getuid() || (value.st_uid == 0 && (!inside || scope.bundled))
            let sharedTemporary = !inside && value.st_uid == 0 && value.st_mode & S_ISVTX != 0
            let sharedApplications = scope.bundled && !inside && current == "/Applications" &&
                value.st_uid == 0 && value.st_mode & 0o002 == 0
            guard owned, value.st_mode & 0o022 == 0 || sharedTemporary || sharedApplications else { throw unsafe(url) }
        }
        transferred = true
        return descriptor
    }

    static func validateDirectory(_ url: URL, scope: Scope) throws {
        if let descriptor = try parent(url.appendingPathComponent(".bootstrap-probe"), scope: scope) { close(descriptor) }
    }

    // nil means absent, after validating every existing ancestor. All other
    // unsafe file types, ownership and permissions fail before setup execution.
    static func openFile(_ url: URL, scope: Scope) throws -> Int32? {
        guard let directory = try parent(url, scope: scope) else { return nil }
        defer { close(directory) }
        return try openFile(url, directory: directory, scope: scope)
    }

    private static func openFile(_ url: URL, directory: Int32, scope: Scope) throws -> Int32? {
        let descriptor = openat(directory, url.lastPathComponent, O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
        if descriptor < 0 {
            if errno == ENOENT { return nil }
            throw unsafe(url)
        }
        var value = stat()
        guard fstat(descriptor, &value) == 0, value.st_mode & S_IFMT == S_IFREG, value.st_nlink == 1,
              value.st_mode & 0o022 == 0, value.st_uid == getuid() || (scope.bundled && value.st_uid == 0) else {
            close(descriptor)
            throw unsafe(url)
        }
        return descriptor
    }

    static func validateFile(_ url: URL, scope: Scope) throws {
        if let descriptor = try openFile(url, scope: scope) { close(descriptor) }
    }

    static func safeExecutable(_ url: URL, scope: Scope) throws -> Bool {
        guard let directory = try parent(url, scope: scope) else { return false }
        defer { close(directory) }
        guard let descriptor = try openFile(url, directory: directory, scope: scope) else { return false }
        defer { close(descriptor) }
        // Respect the account's effective owner/group permissions rather than
        // accepting an execute bit that applies only to another class of user.
        return faccessat(directory, url.lastPathComponent, X_OK, 0) == 0
    }
}

private struct QueueBootstrapIdentity: Decodable, Equatable {
    let schemaVersion: Int
    let repository: String
    let version: String
    let commit: String?
    let build: Int?
}

private struct QueueBootstrapPackaging: Decodable {
    let embeddedPayload: Bool?
    enum CodingKeys: String, CodingKey { case embeddedPayload = "AgentQueueEmbeddedPayload" }
}

private struct QueueBootstrapBundle: Decodable {
    let identifier: String
    let version: String
    let build: String
    let embeddedPayload: Bool?
    enum CodingKeys: String, CodingKey {
        case identifier = "CFBundleIdentifier", version = "CFBundleShortVersionString", build = "CFBundleVersion"
        case embeddedPayload = "AgentQueueEmbeddedPayload"
    }
}

private struct QueueBootstrapLaunch: Decodable {
    let label: String
    let arguments: [String]
    let program: String?
    let runAtLoad: Bool?
    enum CodingKeys: String, CodingKey {
        case label = "Label", arguments = "ProgramArguments", program = "Program", runAtLoad = "RunAtLoad"
    }
}

struct QueueBootstrapRequest: Equatable, Sendable {
    let executable: URL
    let arguments: [String]
}

enum QueueBootstrapDecision: Equatable, Sendable {
    case legacy, ready
    case install(QueueBootstrapRequest, String)
}

@MainActor
final class QueueBootstrapModel: ObservableObject {
    typealias Executor = @Sendable (QueueBootstrapRequest, @escaping @Sendable (Data) -> Void) async throws -> Data
    enum Phase { case idle, checking, installing, failed, complete }
    @Published private(set) var phase = Phase.idle
    @Published private(set) var message = "Checking setup…"
    @Published private(set) var output = ""
    var isRunning: Bool { phase == .checking || phase == .installing }
    var onSetupRequired: (() -> Void)?
    private let inspect: @Sendable () throws -> QueueBootstrapDecision
    private let execute: Executor
    private let completed: @MainActor () -> Void
    private var attempt = 0

    init(environment: QueueBootstrapEnvironment,
         execute: @escaping Executor = QueueBootstrapModel.runInstaller,
         completed: @escaping @MainActor () -> Void) {
        inspect = { try environment.inspect() }
        self.execute = execute
        self.completed = completed
    }

    nonisolated static func runInstaller(_ request: QueueBootstrapRequest,
                                        output: @escaping @Sendable (Data) -> Void) async throws -> Data {
        try await QueueClient.run(request.arguments, budget: .setup,
            transport: .init(executable: request.executable, timeout: 330), onOutput: output)
    }

    @discardableResult func start() -> Task<Void, Never>? {
        guard !isRunning, phase != .complete else { return nil }
        phase = .checking
        message = "Checking setup…"
        output = ""
        attempt += 1
        let currentAttempt = attempt
        return Task { [self] in
            do {
                let decision = try await Task.detached(priority: .utility, operation: inspect).value
                if case let .install(request, reason) = decision {
                    message = reason
                    phase = .installing
                    onSetupRequired?()
                    let result = try await execute(request) { [weak self] data in
                        Task { @MainActor [weak self] in
                            guard let self, self.attempt == currentAttempt, self.phase == .installing else { return }
                            self.output = String(decoding: data.suffix(8_000), as: UTF8.self)
                        }
                    }
                    output = String(decoding: result.suffix(8_000), as: UTF8.self)
                    let verified = try await Task.detached(priority: .utility, operation: inspect).value
                    guard verified == .ready else {
                        throw ClientError.message("Setup exited before the installation was complete. Review the output and retry.")
                    }
                }
                phase = .complete
                completed()
            } catch {
                phase = .failed
                message = "Agent Queue setup could not finish."
                output = String(error.localizedDescription.suffix(8_000))
                onSetupRequired?()
            }
        }
    }
}

struct QueueBootstrapView: View {
    @ObservedObject var model: QueueBootstrapModel
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Label("Set up Agent Queue", systemImage: QueueBrand.symbolName).font(.title2.bold())
            Text("Setup is automatic for your macOS account. Manage optional phone access in settings.")
                .font(.callout).foregroundStyle(.secondary)
            HStack {
                if model.isRunning { ProgressView().controlSize(.small) }
                Text(model.message).font(.headline)
            }
            ScrollView {
                Text(model.output.isEmpty ? "Installing the queue backend and background services…" : model.output)
                    .font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading).padding(12)
            }.background(.quaternary.opacity(0.35)).clipShape(RoundedRectangle(cornerRadius: 8))
            HStack {
                Spacer()
                Button("Quit") { NSApp.terminate(nil) }.disabled(model.isRunning)
                if model.phase == .failed {
                    Button("Retry") { model.start() }.buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
                }
            }
        }.padding(24).frame(minWidth: 560, minHeight: 410)
    }
}
