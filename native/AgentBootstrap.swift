import AppKit
import Darwin
import Foundation
import ServiceManagement
import SwiftUI

// This is a read-only startup decision. The backend remains the authority for
// installation, migration, ownership checks and preserving existing settings.
struct QueueBootstrapEnvironment: Sendable {
    let app: URL
    let resources: URL
    let home: URL
    var arguments: [String] = []
    var systemApplications = URL(fileURLWithPath: "/Applications", isDirectory: true)
    /// A drag-and-drop install has no separate installer: an ordinary launch
    /// from Applications runs setup. Only private test fixtures disable it.
    var setupAllowed = true

    static func current(bundle: Bundle = .main) -> Self {
        Self(app: bundle.bundleURL, resources: bundle.resourceURL ?? bundle.bundleURL.appendingPathComponent("Contents/Resources"),
             home: FileManager.default.homeDirectoryForCurrentUser, arguments: CommandLine.arguments)
    }

    var root: URL { home.appendingPathComponent(".local/share/agent-heavy") }
    var source: URL { resources.appendingPathComponent("payload/source") }
    var allowsInstallation: Bool { setupAllowed && !arguments.contains("--fixture") }

    func validateDiagnostics() throws {
        if arguments.filter({ $0 == "--setup" }).count > 1 || arguments.contains(where: { $0.hasPrefix("--setup=") }) {
            throw ClientError.message("Use --setup once to open the explicit installation workflow.")
        }
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
        let packaging = try? Self.readPlist(QueueBootstrapPackaging.self, at: infoURL, scope: bundleScope)
        let managed = packaging?.serviceManagement == true
        let marked = packaging?.embeddedPayload == true || managed
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
                let binary = managed ? app.appendingPathComponent("Contents/MacOS/\(name)") : source.appendingPathComponent("bin/\(name)")
                guard try QueueBootstrapFiles.safeExecutable(binary, scope: bundleScope) else {
                    throw ClientError.message("The bundled \(name) executable is missing or unsafe.")
                }
            }
        } catch {
            throw ClientError.message("Agent Queue's embedded setup payload is incomplete or invalid. Download the app again. \(error.localizedDescription)")
        }
        let request = QueueBootstrapRequest(executable: managed ? app.appendingPathComponent("Contents/MacOS/agent-heavy") : source.appendingPathComponent("bin/agent-heavy"),
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
        for name in ["build-info.json", "install-in-progress.json", "installation.json"] + (managed ? [] : ["bin/agent-heavy", "bin/agent-remote"]) {
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
        if managed {
            guard let installation = try? JSONDecoder().decode(QueueBootstrapInstallation.self,
                from: Self.read(root.appendingPathComponent("installation.json"), scope: privateScope)),
                  installation.schemaVersion == 1, installation.serviceManagement,
                  installation.app == app.path else {
                return .install(request, "The installed application location needs setup.")
            }
            for name in ["agent-heavy", "agent-remote"] {
                guard try QueueBootstrapFiles.safeExecutable(cli.appendingPathComponent(name), scope: cliScope) else {
                    return .install(request, "The command entry points need setup.")
                }
            }
            for (label, helper, command) in [("io.went.agent-heavy", "agent-heavy", "daemon"),
                                             ("io.went.agent-resources", "agent-heavy", "scheduler"),
                                             ("io.went.agent-remote", "agent-remote", "serve")] {
                let definition = app.appendingPathComponent("Contents/Library/LaunchAgents/\(label).plist")
                guard let service = try? Self.readPlist(QueueBootstrapManagedLaunch.self, at: definition, scope: bundleScope),
                      service.label == label, service.bundleProgram == "Contents/MacOS/\(helper)",
                      service.program == nil,
                      service.arguments == [helper, command] else {
                    throw ClientError.message("A bundled background service definition is missing or invalid. Download a complete installer.")
                }
            }
            return .ready
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

    func serviceStatusRequests() throws -> [QueueBootstrapRequest] {
        guard !arguments.contains("--fixture"), Self.exists(app.appendingPathComponent("Contents/Info.plist")) else { return [] }
        let scope = QueueBootstrapFiles.Scope(root: app, bundled: true)
        let packaging = try Self.readPlist(QueueBootstrapPackaging.self,
            at: app.appendingPathComponent("Contents/Info.plist"), scope: scope)
        guard packaging.serviceManagement == true else { return [] }
        let executable = app.appendingPathComponent("Contents/MacOS/AgentQueue")
        guard try QueueBootstrapFiles.safeExecutable(executable, scope: scope) else {
            throw ClientError.message("The app launcher is missing or unsafe. Download a complete installer.")
        }
        return ["queue", "resources"].map {
            QueueBootstrapRequest(executable: executable, arguments: ["--service-management", "status", $0])
        }
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
        return faccessat(directory, url.lastPathComponent, X_OK, 0) == 0 &&
            QueueExecutableNamespace.allows(executable: url, root: scope.root)
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
    let serviceManagement: Bool?
    enum CodingKeys: String, CodingKey {
        case embeddedPayload = "AgentQueueEmbeddedPayload"
        case serviceManagement = "AgentQueueServiceManagement"
    }
}

private struct QueueBootstrapInstallation: Decodable {
    let schemaVersion: Int
    let app: String
    let serviceManagement: Bool
}

private struct QueueBootstrapManagedLaunch: Decodable {
    let label: String
    let bundleProgram: String
    let arguments: [String]
    let program: String?
    enum CodingKeys: String, CodingKey {
        case label = "Label", bundleProgram = "BundleProgram", arguments = "ProgramArguments", program = "Program"
    }
}

private struct QueueBootstrapServiceState: Decodable {
    let schemaVersion: Int
    let service: String
    let status: String
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

struct QueueBootstrapAction: Codable, Equatable, Sendable, Identifiable {
    let id: String
    let title: String
    let destructive: Bool
    static let executable = Set(["cancel-job", "cleanup-job", "repair-service", "restore-service-definitions", "disable-phone-access", "reset-callback", "finish-pending-installation"])
    static let navigation = Set(["wait", "review-jobs", "review-configuration", "reveal", "download-release", "move-app"])
    var needsConfirmation: Bool { destructive || ["cancel-job", "reset-callback", "restore-service-definitions", "finish-pending-installation"].contains(id) }
}

struct QueueBootstrapJob: Codable, Equatable, Sendable, Identifiable {
    let id: Int
    let label: String
    let status: String
}

struct QueueBootstrapIssue: Codable, Equatable, Sendable, Identifiable {
    let id: String
    let title: String
    let detail: String
    var path: String? = nil
    let actions: [QueueBootstrapAction]
    var jobs: [QueueBootstrapJob]? = nil
}

struct QueueBootstrapReport: Codable, Equatable, Sendable {
    let schemaVersion: Int
    let issues: [QueueBootstrapIssue]
    var revision: String? = nil

    static let clear = Self(schemaVersion: 1, issues: [])

    static func decode(_ data: Data) throws -> Self {
        guard data.count <= 32_768 else { throw ClientError.message("Setup preflight exceeded its response limit.") }
        let report = try JSONDecoder().decode(Self.self, from: data)
        guard report.schemaVersion == 1, report.issues.count <= 24,
              Set(report.issues.map(\.id)).count == report.issues.count,
              report.revision == nil || report.revision?.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil else {
            throw ClientError.message("Setup preflight returned an unsupported or invalid report.")
        }
        for issue in report.issues {
            guard issue.id.range(of: "^[a-z][a-z0-9-]{0,79}$", options: .regularExpression) != nil,
                  Self.validText(issue.title, limit: 240), Self.validText(issue.detail, limit: 8_000, multiline: true),
                  issue.actions.count <= 12, Set(issue.actions.map(\.id)).count == issue.actions.count,
                  (issue.jobs?.count ?? 0) <= 25 else {
                throw ClientError.message("Setup preflight returned an invalid issue.")
            }
            if let path = issue.path {
                guard path.hasPrefix("/"), Self.validText(path, limit: Int(PATH_MAX)),
                      !path.split(separator: "/").contains(".."), !path.split(separator: "/").contains(".") else {
                    throw ClientError.message("Setup preflight returned an invalid path.")
                }
            }
            for action in issue.actions {
                guard QueueBootstrapAction.executable.union(QueueBootstrapAction.navigation).contains(action.id),
                      Self.validText(action.title, limit: 160) else {
                    throw ClientError.message("Setup preflight returned an unsupported action.")
                }
            }
            if let jobs = issue.jobs {
                guard Set(jobs.map(\.id)).count == jobs.count,
                      jobs.allSatisfy({ $0.id >= 0 && Self.validText($0.label, limit: 512) && Self.validText($0.status, limit: 80) }) else {
                    throw ClientError.message("Setup preflight returned invalid jobs.")
                }
            }
        }
        return report
    }

    private static func validText(_ value: String, limit: Int, multiline: Bool = false) -> Bool {
        !value.isEmpty && value.utf8.count <= limit && value.unicodeScalars.allSatisfy {
            !CharacterSet.controlCharacters.contains($0) || (multiline && ($0 == "\n" || $0 == "\t"))
        }
    }
}

@MainActor
final class QueueBootstrapModel: ObservableObject {
    typealias Executor = @Sendable (QueueBootstrapRequest, @escaping @Sendable (Data) -> Void) async throws -> Data
    enum Phase { case idle, checking, setupRequired, reviewing, waiting, recovering, installing, failed, complete }
    @Published private(set) var phase = Phase.idle
    @Published private(set) var message = "Checking setup…"
    @Published private(set) var output = ""
    @Published private(set) var issues: [QueueBootstrapIssue] = []
    @Published private(set) var reviewedIssueID: String?
    @Published private(set) var canDisablePhoneAccess = false
    @Published private(set) var requiresServiceApproval = false
    private static let phoneAccessRecovery = "AGENT_QUEUE_SETUP_RECOVERY:disable-phone-access"
    var isRunning: Bool { [.checking, .waiting, .recovering, .installing].contains(phase) }
    var canStopWaiting: Bool { phase == .waiting || phase == .checking }
    var allowsInstallation: Bool { environment.allowsInstallation }
    var onSetupRequired: (() -> Void)?
    private let environment: QueueBootstrapEnvironment
    private let inspect: @Sendable () throws -> QueueBootstrapDecision
    private let execute: Executor
    private let completed: @MainActor () -> Void
    private let waitInterval: Duration
    private let waitLimit: Int
    private var attempt = 0
    private var operation: Task<Void, Never>?
    private var request: QueueBootstrapRequest?
    private var serviceSetupRequest: QueueBootstrapRequest?
    private var revision: String?
    private var legacyPhoneAccess = false

    private enum ApprovedRepairStep {
        case recovery(String, Int?)
        case wait
    }

    var canRepairAndContinue: Bool {
        allowsInstallation && !isRunning && phase != .complete && !issues.isEmpty && issues.allSatisfy { Self.approvedRepairStep($0) != nil }
    }

    private static func approvedRepairStep(_ issue: QueueBootstrapIssue) -> ApprovedRepairStep? {
        func has(_ id: String) -> Bool { issue.actions.contains { $0.id == id } }
        switch issue.id {
        case "pending-installation":
            return has("finish-pending-installation") ? .recovery("finish-pending-installation", nil) : nil
        case "service-definitions-missing":
            return has("restore-service-definitions") ? .recovery("restore-service-definitions", nil) : nil
        case "legacy-phone-access", "remote-migration":
            return has("disable-phone-access") ? .recovery("disable-phone-access", nil) : nil
        case "service-not-ready":
            if has("repair-service") { return .recovery("repair-service", nil) }
            return has("wait") ? .wait : nil
        case "pending-cleanup":
            if has("cleanup-job"), let job = issue.jobs?.first,
               ["Cleanup pending", "Done"].contains(job.status) {
                return .recovery("cleanup-job", job.id)
            }
            return has("wait") ? .wait : nil
        case "legacy-jobs":
            guard let jobs = issue.jobs, !jobs.isEmpty,
                  jobs.allSatisfy({ ["Running", "Queued"].contains($0.status) }) else { return nil }
            return has("wait") ? .wait : nil
        case "installer-busy", "scheduling-busy", "remote-busy", "controller-stopping":
            return has("wait") ? .wait : nil
        default:
            return nil
        }
    }

    init(environment: QueueBootstrapEnvironment,
         execute: @escaping Executor = QueueBootstrapModel.runInstaller,
         waitInterval: Duration = .seconds(2), waitLimit: Int = 60,
         completed: @escaping @MainActor () -> Void) {
        self.environment = environment
        inspect = { try environment.inspect() }
        self.execute = execute
        self.completed = completed
        self.waitInterval = waitInterval
        self.waitLimit = max(1, min(waitLimit, 300))
    }

    nonisolated static func runInstaller(_ request: QueueBootstrapRequest,
                                        output: @escaping @Sendable (Data) -> Void) async throws -> Data {
        try await QueueClient.run(request.arguments, budget: .setup,
            transport: .init(executable: request.executable, timeout: QueueClient.timeout(for: request.arguments, budget: .setup)), onOutput: output)
    }

    @discardableResult func start(disablePhoneAccess: Bool = false) -> Task<Void, Never>? {
        guard !isRunning, phase != .complete else { return nil }
        guard !disablePhoneAccess || allowsInstallation else { return nil }
        if disablePhoneAccess && !legacyPhoneAccess {
            guard let issue = issues.first(where: { $0.actions.contains(where: { $0.id == "disable-phone-access" }) }) else { return nil }
            return perform(actionID: "disable-phone-access", issueID: issue.id, confirmed: true)
        }
        guard !disablePhoneAccess || canDisablePhoneAccess else { return nil }
        let current = begin(.checking, message: "Checking installation requirements…")
        issues = []
        request = nil
        requiresServiceApproval = false
        let task = Task { [self] in
            do {
                let decision = try await inspectDecision(current)
                if case let .install(request, reason) = decision {
                    guard allowsInstallation else {
                        phase = .setupRequired
                        message = "Setup must finish before Agent Queue can open"
                        output = "Setup is disabled for this launch. Open Agent Queue from Applications to complete installation or upgrade."
                        onSetupRequired?()
                        return
                    }
                    // A clean drag-and-drop install runs silently. The window
                    // opens only for a review, a failure or macOS approval.
                    self.request = request
                    if disablePhoneAccess {
                        legacyPhoneAccess = false
                        try await install(QueueBootstrapRequest(executable: request.executable,
                            arguments: request.arguments + ["--disable-phone-access"]), reason: reason, current: current)
                    } else if try await preflight(request, current: current) {
                        try await install(request, reason: reason, current: current)
                    }
                } else {
                    for statusRequest in try environment.serviceStatusRequests() {
                        let status = try await serviceStatus(statusRequest)
                        guard status == "enabled" else {
                            requiresServiceApproval = status == "requiresApproval"
                            if requiresServiceApproval && !environment.allowsInstallation {
                                _ = try await continueAfterServiceApproval(current: current, alreadyObserved: true)
                                return
                            }
                            if !requiresServiceApproval && allowsInstallation {
                                let setup = QueueBootstrapRequest(executable: environment.app.appendingPathComponent("Contents/MacOS/agent-heavy"),
                                    arguments: ["install", "--app-bundle", environment.app.path])
                                request = setup
                                serviceSetupRequest = setup
                            }
                            phase = .setupRequired
                            message = requiresServiceApproval ? "Allow background services in System Settings" : "Setup must finish background service setup"
                            output = requiresServiceApproval
                                ? "Open Login Items settings, allow Agent Queue's background services, then check again."
                                : (allowsInstallation ? "Check again to register this app's background services." : "Setup is disabled for this launch, so background services were not registered or repaired.")
                            onSetupRequired?()
                            return
                        }
                    }
                    finish(current)
                }
            } catch { await fail(error, current: current) }
        }
        operation = task
        return task
    }

    @discardableResult func perform(actionID: String, issueID: String, jobID: Int? = nil, confirmed: Bool = false) -> Task<Void, Never>? {
        guard allowsInstallation, !isRunning, phase != .complete, let issue = issues.first(where: { $0.id == issueID }),
              let action = issue.actions.first(where: { $0.id == actionID }), QueueBootstrapAction.executable.contains(actionID),
              !action.needsConfirmation || confirmed else { return nil }
        let needsJob = actionID == "cancel-job" || actionID == "cleanup-job"
        guard !needsJob || (jobID != nil && issue.jobs?.contains(where: { $0.id == jobID }) == true),
              needsJob || jobID == nil else { return nil }
        let approvedRevision = revision
        if actionID == "reset-callback" && (reviewedIssueID != issueID || approvedRevision == nil) { return nil }
        let current = begin(.recovering, message: action.title + "…")
        let task = Task { [self] in
            do {
                let decision = try await inspectDecision(current)
                guard case let .install(request, reason) = decision else { finish(current); return }
                self.request = request
                var arguments = request.arguments + ["--recover", actionID]
                if let jobID { arguments += ["--job-id", String(jobID)] }
                if actionID == "reset-callback", let approvedRevision { arguments += ["--expected-revision", approvedRevision] }
                let data = try await execute(.init(executable: request.executable, arguments: arguments)) { _ in }
                _ = try QueueBootstrapReport.decode(data)
                guard attempt == current else { return }
                if try await preflight(request, current: current) {
                    try await install(request, reason: reason, current: current)
                }
            } catch { await fail(error, current: current) }
        }
        operation = task
        return task
    }

    @discardableResult func repairAndContinue(confirmed: Bool = false) -> Task<Void, Never>? {
        guard confirmed, canRepairAndContinue else { return nil }
        let current = begin(.checking, message: "Checking the approved setup repairs…")
        let task = Task { [self] in
            var performed = Set<String>()
            do {
                while true {
                    try Task.checkCancellation()
                    let decision = try await inspectDecision(current)
                    guard case let .install(request, reason) = decision else { finish(current); return }
                    self.request = request
                    if try await preflight(request, current: current, waiting: true) {
                        try await install(request, reason: reason, current: current)
                        return
                    }
                    guard issues.allSatisfy({ Self.approvedRepairStep($0) != nil }) else {
                        phase = .reviewing
                        message = "A requirement needs review. The approved automatic repair has stopped."
                        return
                    }
                    let steps = issues.compactMap(Self.approvedRepairStep)
                    let coordinationBusy = issues.contains { ["installer-busy", "scheduling-busy", "remote-busy", "controller-stopping"].contains($0.id) }
                    var next: (String, Int?)?
                    if !coordinationBusy {
                        for priority in ["finish-pending-installation", "restore-service-definitions", "repair-service", "cleanup-job", "disable-phone-access"] {
                            for case let .recovery(actionID, jobID) in steps where actionID == priority {
                                next = (actionID, jobID)
                                break
                            }
                            if next != nil { break }
                        }
                    }
                    if let (actionID, jobID) = next {
                        let key = actionID + ":" + (jobID.map(String.init) ?? "")
                        guard performed.insert(key).inserted else {
                            throw ClientError.message("The approved repair did not clear its setup requirement. No repair was repeated. Review the current requirements before approving another attempt.")
                        }
                        guard performed.count <= 128 else {
                            throw ClientError.message("Setup reached its verified repair limit. Review the remaining requirements before approving another attempt.")
                        }
                        phase = .recovering
                        message = issues.flatMap(\.actions).first(where: { $0.id == actionID })?.title.appending("…") ?? "Repairing setup…"
                        var arguments = request.arguments + ["--recover", actionID]
                        if let jobID { arguments += ["--job-id", String(jobID)] }
                        let data = try await execute(.init(executable: request.executable, arguments: arguments)) { _ in }
                        _ = try QueueBootstrapReport.decode(data)
                        guard attempt == current else { return }
                        phase = .checking
                        message = "Checking the repaired installation…"
                    } else {
                        phase = .waiting
                        message = "Waiting for queue work and setup requirements to finish. Setup will continue automatically."
                        // Approval remains scoped to this task while normal work
                        // finishes. Stop waiting cancels the read-only polling and
                        // fences its eventual responses without stopping jobs.
                        try await Task.sleep(for: waitInterval)
                        guard attempt == current else { return }
                        phase = .checking
                    }
                }
            } catch { await fail(error, current: current) }
        }
        operation = task
        return task
    }

    @discardableResult func waitAndContinue() -> Task<Void, Never>? {
        guard allowsInstallation, !isRunning, phase != .complete, issues.contains(where: { $0.actions.contains(where: { $0.id == "wait" }) }) else { return nil }
        let current = begin(.waiting, message: "Waiting for the installation requirements to clear…")
        let task = Task { [self] in
            do {
                for index in 0..<waitLimit {
                    if index > 0 { try await Task.sleep(for: waitInterval) }
                    let decision = try await inspectDecision(current)
                    guard case let .install(request, reason) = decision else { finish(current); return }
                    self.request = request
                    if try await preflight(request, current: current, waiting: true) {
                        try await install(request, reason: reason, current: current)
                        return
                    }
                    guard issues.allSatisfy({ $0.actions.contains(where: { $0.id == "wait" }) }) else {
                        phase = .reviewing
                        message = "Some requirements need your attention before setup can continue."
                        return
                    }
                }
                guard attempt == current else { return }
                phase = .reviewing
                message = "Setup is still waiting. Review the requirements or wait again."
            } catch { await fail(error, current: current) }
        }
        operation = task
        return task
    }

    func cancelWaiting() {
        guard canStopWaiting else { return }
        attempt += 1
        operation?.cancel()
        operation = nil
        phase = issues.isEmpty ? .failed : .reviewing
        message = "Setup check stopped. You can continue when you are ready."
    }

    func review(_ issueID: String) {
        guard !isRunning, issues.contains(where: { $0.id == issueID }) else { return }
        reviewedIssueID = issueID
    }

    func canPerform(_ action: QueueBootstrapAction, issue: QueueBootstrapIssue) -> Bool {
        !isRunning && (allowsInstallation || QueueBootstrapAction.navigation.contains(action.id)) &&
            (action.id != "reset-callback" || (reviewedIssueID == issue.id && revision != nil))
    }

    func showLoginItems() { SMAppService.openSystemSettingsLoginItems() }

    func reveal(_ issue: QueueBootstrapIssue) {
        guard !isRunning, issues.contains(issue), let path = issue.path else { return }
        let url = URL(fileURLWithPath: path).standardizedFileURL
        let permitted = url.path == environment.home.path || url.path.hasPrefix(environment.home.path + "/") ||
            url.path == environment.app.path || url.path.hasPrefix(environment.app.path + "/")
        guard permitted else { return }
        NSWorkspace.shared.activateFileViewerSelecting([url])
    }

    func showApplications() {
        NSWorkspace.shared.open(environment.systemApplications)
    }

    func downloadRelease() {
        if let url = URL(string: "https://github.com/Dunky13/agent-job-queue/releases") { NSWorkspace.shared.open(url) }
    }

    private func begin(_ next: Phase, message: String) -> Int {
        attempt += 1
        phase = next
        self.message = message
        output = ""
        reviewedIssueID = nil
        canDisablePhoneAccess = false
        return attempt
    }

    private func inspectDecision(_ current: Int) async throws -> QueueBootstrapDecision {
        let result = try await Task.detached(priority: .utility, operation: inspect).value
        guard attempt == current else { throw CancellationError() }
        if phase == .checking || phase == .waiting { try Task.checkCancellation() }
        if result == .ready, allowsInstallation, let serviceSetupRequest {
            return .install(serviceSetupRequest, "Setting up background services…")
        }
        return result
    }

    private func preflight(_ request: QueueBootstrapRequest, current: Int, waiting: Bool = false) async throws -> Bool {
        let data = try await execute(.init(executable: request.executable,
            arguments: request.arguments + ["--preflight", "--json"])) { _ in }
        guard attempt == current else { throw CancellationError() }
        try Task.checkCancellation()
        let report = try QueueBootstrapReport.decode(data)
        issues = report.issues
        revision = report.revision
        canDisablePhoneAccess = issues.contains(where: { $0.actions.contains(where: { $0.id == "disable-phone-access" }) })
        if !issues.isEmpty {
            if !waiting { phase = .reviewing }
            message = waiting ? "Waiting for the listed requirements to clear…" : "Review these requirements before continuing."
            onSetupRequired?()
            return false
        }
        return true
    }

    private func install(_ request: QueueBootstrapRequest, reason: String, current: Int) async throws {
        guard attempt == current else { throw CancellationError() }
        phase = .installing
        message = reason
        canDisablePhoneAccess = false
        let result = try await execute(request) { [weak self] data in
            Task { @MainActor [weak self] in
                guard let self, self.attempt == current, self.phase == .installing else { return }
                let progress = QueueSetupProgress.parse(String(decoding: data, as: UTF8.self))
                self.requiresServiceApproval = self.requiresServiceApproval || progress.requiresServiceApproval
                self.output = String(decoding: progress.output.utf8.suffix(8_000), as: UTF8.self)
                if self.requiresServiceApproval { self.onSetupRequired?() }
            }
        }
        guard attempt == current else { return }
        output = String(decoding: QueueSetupProgress.parse(String(decoding: result, as: UTF8.self)).output.utf8.suffix(8_000), as: UTF8.self)
        let verified = try await Task.detached(priority: .utility, operation: inspect).value
        guard attempt == current else { return }
        guard verified == .ready else { throw ClientError.message("Setup exited before the installation was complete. Review the output and retry.") }
        finish(current)
    }

    private func serviceStatus(_ request: QueueBootstrapRequest) async throws -> String {
        let data = try await execute(request) { _ in }
        guard data.count <= 16_384 else { throw ClientError.message("Background service status exceeded its response limit.") }
        let state = try JSONDecoder().decode(QueueBootstrapServiceState.self, from: data)
        guard state.schemaVersion == 1, state.service == request.arguments.last,
              ["notRegistered", "enabled", "requiresApproval", "notFound"].contains(state.status) else {
            throw ClientError.message("Background service status is invalid.")
        }
        return state.status
    }

    private func continueAfterServiceApproval(current: Int, alreadyObserved: Bool = false) async throws -> Bool {
        let requests = try environment.serviceStatusRequests()
        guard !requests.isEmpty else { return false }
        var statuses: [String] = []
        for request in requests { statuses.append(try await serviceStatus(request)) }
        guard alreadyObserved || statuses.contains("requiresApproval") else { return false }
        requiresServiceApproval = true
        onSetupRequired?()
        while statuses.contains("requiresApproval") {
            guard attempt == current else { throw CancellationError() }
            try Task.checkCancellation()
            phase = .waiting
            message = "Allow background services in System Settings"
            output = "macOS requires your approval. Allow Agent Queue in Login Items settings; setup will continue automatically."
            try await Task.sleep(for: waitInterval)
            statuses = []
            for request in requests { statuses.append(try await serviceStatus(request)) }
        }
        guard attempt == current else { throw CancellationError() }
        try Task.checkCancellation()
        requiresServiceApproval = false
        phase = .checking
        let decision = try await Task.detached(priority: .utility, operation: inspect).value
        guard attempt == current else { throw CancellationError() }
        if decision == .ready && statuses.allSatisfy({ $0 == "enabled" }) {
            finish(current)
        } else {
            phase = .setupRequired
            message = "Setup must finish before Agent Queue can open"
            output = "Setup is disabled for this launch, so background services were not installed, repaired or registered."
        }
        return true
    }

    private func finish(_ current: Int) {
        guard attempt == current, phase != .complete else { return }
        phase = .complete
        issues = []
        canDisablePhoneAccess = false
        requiresServiceApproval = false
        completed()
    }

    private func fail(_ error: Error, current: Int) async {
        guard attempt == current else { return }
        let wasMutation = phase == .installing || phase == .recovering
        let progress = QueueSetupProgress.parse(error.localizedDescription)
        requiresServiceApproval = requiresServiceApproval || progress.requiresServiceApproval
        let lines = progress.output.components(separatedBy: .newlines)
        legacyPhoneAccess = lines.contains(Self.phoneAccessRecovery)
        if wasMutation && !legacyPhoneAccess && !Task.isCancelled, let request {
            issues = []
            revision = nil
            do {
                let data = try await execute(.init(executable: request.executable,
                    arguments: request.arguments + ["--preflight", "--json"])) { _ in }
                guard attempt == current else { return }
                try Task.checkCancellation()
                let report = try QueueBootstrapReport.decode(data)
                issues = report.issues
                revision = report.revision
            } catch {
                guard attempt == current else { return }
            }
        }
        guard attempt == current else { return }
        phase = .failed
        canDisablePhoneAccess = legacyPhoneAccess || issues.contains(where: { $0.actions.contains(where: { $0.id == "disable-phone-access" }) })
        message = legacyPhoneAccess ? "Phone access must be disabled to continue setup." : "Agent Queue setup could not finish."
        output = String(lines.filter { $0 != Self.phoneAccessRecovery }.joined(separator: "\n").suffix(8_000))
        if !legacyPhoneAccess && request == nil {
            if output.contains("Move Agent Queue to Applications") {
                issues = [.init(id: "app-location", title: "Move Agent Queue to Applications", detail: "Quit this copy, copy Agent Queue.app into Applications using Finder, then open that copy. Existing apps will not be replaced automatically.",
                    path: environment.app.path, actions: [.init(id: "move-app", title: "Open Applications", destructive: false)])]
            } else if output.contains("payload") || output.contains("package metadata") {
                issues = [.init(id: "payload-invalid", title: "Download a complete copy", detail: "Use the official release download, move the app to Applications, and open that copy.",
                    actions: [.init(id: "download-release", title: "Download the correct release", destructive: false)])]
            } else {
                issues = [.init(id: "setup-path", title: "Review the setup diagnostic", detail: output,
                    path: environment.home.path, actions: [.init(id: "reveal", title: "Show in Finder", destructive: false)])]
            }
        }
        onSetupRequired?()
    }
}

private enum QueueBootstrapPendingAction: Identifiable {
    case individual(QueueBootstrapIssue, QueueBootstrapAction, Int?)
    case repairInstallation

    var id: String {
        switch self {
        case let .individual(issue, action, jobID): return issue.id + ":" + action.id + ":" + String(jobID ?? -1)
        case .repairInstallation: return "repair-installation"
        }
    }

    var title: String {
        switch self {
        case let .individual(_, action, _): return action.title
        case .repairInstallation: return "Repair and continue"
        }
    }
}

struct QueueBootstrapView: View {
    @ObservedObject var model: QueueBootstrapModel
    @State private var pending: QueueBootstrapPendingAction?
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Label(model.allowsInstallation ? "Set up Agent Queue" : "Agent Queue", systemImage: QueueBrand.symbolName).font(.title2.bold())
            Text(model.allowsInstallation ? "Existing jobs, history and credentials are preserved."
                 : "Setup is disabled for this launch.")
                .font(.callout).foregroundStyle(.secondary)
            HStack {
                if model.isRunning { ProgressView().controlSize(.small) }
                Text(model.message).font(.headline)
            }
            if model.canDisablePhoneAccess && model.issues.isEmpty {
                Text("Your credentials and configuration will be preserved. You can enable phone access again in settings after setup.")
                    .font(.callout).foregroundStyle(.secondary)
            }
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    ForEach(model.issues) { issue in
                        VStack(alignment: .leading, spacing: 8) {
                            Text(issue.title).font(.headline)
                            Text(issue.detail).font(.callout).textSelection(.enabled)
                            if let path = issue.path { Text(path).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled) }
                            if let jobs = issue.jobs {
                                ForEach(jobs) { job in
                                    HStack(alignment: .top) {
                                        VStack(alignment: .leading) {
                                            Text("Job \(job.id): \(job.label)")
                                            Text(job.status).font(.caption).foregroundStyle(.secondary)
                                        }
                                        Spacer()
                                        ForEach(issue.actions.filter { ["cancel-job", "cleanup-job"].contains($0.id) }) { action in
                                            actionButton(action, issue: issue, jobID: job.id)
                                        }
                                    }
                                }
                            }
                            ViewThatFits(in: .horizontal) {
                                HStack {
                                    ForEach(issue.actions.filter { !["cancel-job", "cleanup-job"].contains($0.id) }) { action in
                                        actionButton(action, issue: issue)
                                    }
                                }.fixedSize(horizontal: true, vertical: false)
                                VStack(alignment: .leading, spacing: 8) {
                                    ForEach(issue.actions.filter { !["cancel-job", "cleanup-job"].contains($0.id) }) { action in
                                        actionButton(action, issue: issue)
                                    }
                                }
                            }
                            if model.reviewedIssueID == issue.id && issue.actions.contains(where: { $0.id == "reset-callback" }) {
                                Text("Review the current file before applying a change. Replacing the callback removes its custom behavior; setup saves the original before writing the standard cleanup callback.")
                                    .font(.callout).foregroundStyle(.secondary)
                            }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                    }
                    if !model.output.isEmpty || model.issues.isEmpty {
                        Text(model.output.isEmpty ? "Checking the installation and background services…" : model.output)
                            .font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                }.padding(12)
            }.background(.quaternary.opacity(0.35)).clipShape(RoundedRectangle(cornerRadius: 8))
            HStack {
                if model.canStopWaiting { Button("Stop waiting") { model.cancelWaiting() } }
                Spacer()
                if model.requiresServiceApproval { Button("Login Items settings") { model.showLoginItems() } }
                Button("Quit") { NSApp.terminate(nil) }.disabled(model.isRunning)
                if !model.isRunning && model.phase != .complete {
                    if model.requiresServiceApproval {
                        Button("Check again") { model.start() }
                            .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
                    } else if !model.allowsInstallation {
                        Button("Download installer") { model.downloadRelease() }
                        Button("Check again") { model.start() }
                            .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
                    } else if model.canRepairAndContinue {
                        Button("Check again") { model.start() }
                        Button("Repair and continue") { pending = .repairInstallation }
                            .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
                    } else if model.canDisablePhoneAccess && model.issues.isEmpty {
                        Button("Disable phone access and continue") { model.start(disablePhoneAccess: true) }
                            .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
                    } else {
                        Button(model.issues.isEmpty ? "Retry" : "Check again") { model.start() }
                            .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
                    }
                }
            }
        }.padding(24).frame(minWidth: 560, minHeight: 410)
        .alert(item: $pending) { selected in
            Alert(title: Text(selected.title),
                  message: Text(confirmation(selected)),
                  primaryButton: .destructive(Text(selected.title)) {
                    switch selected {
                    case let .individual(issue, action, jobID):
                        model.perform(actionID: action.id, issueID: issue.id, jobID: jobID, confirmed: true)
                    case .repairInstallation:
                        model.repairAndContinue(confirmed: true)
                    }
                  }, secondaryButton: .cancel())
        }
    }

    private func actionButton(_ action: QueueBootstrapAction, issue: QueueBootstrapIssue, jobID: Int? = nil) -> some View {
        Button(action.title) {
            switch action.id {
            case "wait": model.waitAndContinue()
            case "review-jobs": model.review(issue.id)
            case "review-configuration":
                model.review(issue.id)
                model.reveal(issue)
            case "reveal": model.reveal(issue)
            case "move-app": model.showApplications()
            case "download-release": model.downloadRelease()
            default:
                if action.needsConfirmation { pending = .individual(issue, action, jobID) }
                else { model.perform(actionID: action.id, issueID: issue.id, jobID: jobID) }
            }
        }.disabled(!model.canPerform(action, issue: issue))
    }

    private func confirmation(_ selected: QueueBootstrapPendingAction) -> String {
        if case .repairInstallation = selected {
            return "Approve the verified repairs required for this installation? Setup can finish the exact retained pending installation before upgrading, restore missing standard service files, run verified cleanup for completed jobs, start a verified stopped service and disable phone access when migration requires it. Lost custom startup settings cannot be recovered. Credentials and trusted certificates will be preserved; phone access can be enabled again in settings. Active jobs will finish normally, and setup will wait and then continue automatically."
        }
        guard case let .individual(issue, action, jobID) = selected else { return "" }
        switch action.id {
        case "cancel-job": return "Cancel job \(jobID.map(String.init) ?? "")? Running work will be stopped and cannot be restored by setup."
        case "reset-callback": return "Back up and replace the current callback with Agent Queue's standard cleanup callback? Its custom behavior will no longer run. The configuration revision will be checked again before changing it."
        case "restore-service-definitions": return "Restore missing service files using verified loaded commands and standard startup settings? Any lost custom startup settings cannot be recovered. Running services and jobs will stay active. Setup will check all requirements again before continuing."
        case "disable-phone-access": return "Disable phone access and continue setup? Credentials, configuration, and trusted certificates will be preserved. You can enable access again after setup."
        default: return issue.detail
        }
    }
}
