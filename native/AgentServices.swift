import AppKit
import CoreFoundation
import Darwin
import Foundation
import ServiceManagement

// A display hint from the verified installer. It cannot authorize registration
// or establish readiness; those still require native status and install readback.
enum QueueSetupProgress {
    static let approvalMarker = "AGENT_QUEUE_SETUP_STATE:requires-approval"

    static func parse(_ text: String) -> (output: String, requiresServiceApproval: Bool) {
        let lines = text.components(separatedBy: .newlines)
        let waiting = lines.contains(approvalMarker)
        let visible = lines.filter { line in
            line != approvalMarker && !(line.hasPrefix("AGENT_QUEUE_") && approvalMarker.hasPrefix(line))
        }
        return (visible.joined(separator: "\n"), waiting)
    }
}

/// Native CLI arguments exclude argv[0]. Call before starting NSApplication.
/// A nil result means this is an ordinary app launch. Print result.json to stdout
/// and exit with result.exitCode. On a thrown error, print to stderr and exit 1.
/// This entry point never opens System Settings or changes legacy launchd jobs.
enum QueueServiceManagement {
    enum Action: String, CaseIterable, Sendable {
        case status, register, unregister
    }

    enum Service: String, Codable, CaseIterable, Sendable {
        case queue, resources, remote, login

        var identity: Identity {
            switch self {
            case .queue: .agent(plistName: "io.went.agent-heavy.plist")
            case .resources: .agent(plistName: "io.went.agent-resources.plist")
            case .remote: .agent(plistName: "io.went.agent-remote.plist")
            case .login: .mainApp
            }
        }
    }

    enum Identity: Equatable, Sendable {
        case agent(plistName: String)
        case mainApp
    }

    enum Status: String, Codable, CaseIterable, Sendable {
        case notRegistered, enabled, requiresApproval, notFound
    }

    struct Command: Equatable, Sendable {
        let action: Action
        let service: Service
    }

    /// Tests supply all three operations. No test needs an SMAppService instance.
    struct Adapter {
        let status: () throws -> Status
        let register: () throws -> Void
        let unregister: () throws -> Void
    }

    struct Record: Encodable, Equatable, Sendable {
        let schemaVersion = 1
        let service: Service
        let status: Status
    }

    struct CLIResult: Equatable, Sendable {
        let json: String
        /// status queries succeed for every known state. Mutations succeed only
        /// when their requested end state is observed. Approval pending is code 2.
        let exitCode: Int32
    }

    enum Failure: Error, LocalizedError, Equatable {
        case invalidArguments
        case invalidOwningBundle
        case unknownStatus(Int)
        case unregisterTimedOut

        var errorDescription: String? {
            switch self {
            case .invalidArguments:
                "Usage: --service-management status|register|unregister queue|resources|remote|login"
            case .invalidOwningBundle:
                "Service management requires the packaged io.went.agent-queue app with AgentQueueServiceManagement set to boolean true."
            case .unknownStatus(let value):
                "Service management returned an unsupported status: \(value)."
            case .unregisterTimedOut:
                "Service shutdown did not complete within 30 seconds. Keep the installed app in place and review its service status."
            }
        }
    }

    static func parse(arguments: [String]) throws -> Command? {
        guard arguments.contains(where: { $0.hasPrefix("--service-management") }) else { return nil }
        guard arguments.count == 3, arguments[0] == "--service-management",
              let action = Action(rawValue: arguments[1]),
              let service = Service(rawValue: arguments[2]) else {
            throw Failure.invalidArguments
        }
        return Command(action: action, service: service)
    }

    /// Production entry point. The bundle gate runs before constructing any real
    /// SMAppService, including for a read-only status request.
    static func handle(arguments: [String]) throws -> CLIResult? {
        try handle(arguments: arguments, owningBundle: .main, makeAdapter: systemAdapter)
    }

    /// Injectable CLI boundary for private fixture bundles and inert adapters.
    static func handle(arguments: [String], owningBundle: Bundle,
                       makeAdapter: (Identity) throws -> Adapter) throws -> CLIResult? {
        guard let command = try parse(arguments: arguments) else { return nil }
        try validateOwningBundle(owningBundle)
        let record = try run(command, adapter: makeAdapter(command.service.identity))
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let json = String(decoding: try encoder.encode(record), as: UTF8.self)
        let exitCode: Int32
        switch command.action {
        case .status: exitCode = 0
        case .register:
            exitCode = record.status == .enabled ? 0 : (record.status == .requiresApproval ? 2 : 1)
        case .unregister:
            exitCode = record.status == .notRegistered ? 0 : (record.status == .requiresApproval ? 2 : 1)
        }
        return CLIResult(json: json, exitCode: exitCode)
    }

    /// Lifecycle policy shared by production and tests. Framework errors propagate
    /// unchanged; a denied registration is never converted into a successful call.
    static func run(_ command: Command, adapter: Adapter) throws -> Record {
        let status: Status
        switch command.action {
        case .status:
            status = try adapter.status()
        case .register:
            let current = try adapter.status()
            switch current {
            case .enabled, .requiresApproval:
                // Revoked consent requires the user's action in System Settings.
                status = current
            case .notRegistered, .notFound:
                // A lookup miss is not a filesystem inspection. Let the API
                // validate the bundled definition and preserve its exact error.
                try adapter.register()
                status = try adapter.status()
            }
        case .unregister:
            // Do not suppress errors, even kSMErrorJobNotFound. A successful API
            // call is followed by inspection, not an assumed notRegistered state.
            // The production adapter waits for Apple's completion callback before
            // allowing the installer to replace code or re-register the service.
            try adapter.unregister()
            status = try adapter.status()
        }
        return Record(service: command.service, status: status)
    }

    static func validateOwningBundle(_ bundle: Bundle) throws {
        let root = bundle.bundleURL.resolvingSymlinksInPath().standardizedFileURL
        guard root.pathExtension == "app",
              bundle.bundleIdentifier == "io.went.agent-queue",
              bundle.object(forInfoDictionaryKey: "CFBundlePackageType") as? String == "APPL",
              let marker = bundle.object(forInfoDictionaryKey: "AgentQueueServiceManagement") as? NSNumber,
              CFGetTypeID(marker) == CFBooleanGetTypeID(), marker.boolValue,
              let name = bundle.object(forInfoDictionaryKey: "CFBundleExecutable") as? String,
              !name.isEmpty, name != ".", name != "..", !name.contains("/"),
              let executable = bundle.executableURL?.resolvingSymlinksInPath().standardizedFileURL,
              executable == root.appendingPathComponent("Contents/MacOS").appendingPathComponent(name),
              FileManager.default.isExecutableFile(atPath: executable.path),
              QueueExecutableNamespace.allows(executable: bundle.executableURL ?? executable, root: bundle.bundleURL) else {
            throw Failure.invalidOwningBundle
        }
    }

    private static func systemAdapter(_ identity: Identity) -> Adapter {
        let service: SMAppService
        switch identity {
        case .agent(let plistName): service = SMAppService.agent(plistName: plistName)
        case .mainApp: service = SMAppService.mainApp
        }
        return Adapter(status: { try mapStatus(service.status) },
                       register: { try service.register() },
                       unregister: {
                           try waitForUnregistration { completion in
                               service.unregister(completionHandler: completion)
                           }
                       })
    }

    // Apple's SDK documents this callback as the safe re-registration point.
    // It runs on libdispatch's default queue, so the CLI can wait before AppKit
    // starts without relying on the main run loop or a guessed sleep interval.
    static func waitForUnregistration(
        start: (@escaping @Sendable ((any Error)?) -> Void) -> Void
    ) throws {
        let completion = UnregistrationCompletion()
        start { completion.finish($0) }
        try completion.wait()
    }

    private final class UnregistrationCompletion: @unchecked Sendable {
        private let semaphore = DispatchSemaphore(value: 0)
        private let lock = NSLock()
        private var error: (any Error)?

        func finish(_ error: (any Error)?) {
            lock.lock()
            self.error = error
            lock.unlock()
            semaphore.signal()
        }

        func wait() throws {
            guard semaphore.wait(timeout: .now() + 30) == .success else {
                throw Failure.unregisterTimedOut
            }
            lock.lock()
            let failure = error
            lock.unlock()
            if let failure { throw failure }
        }
    }

    static func mapStatus(_ status: SMAppService.Status) throws -> Status {
        switch status {
        case .notRegistered: .notRegistered
        case .enabled: .enabled
        case .requiresApproval: .requiresApproval
        case .notFound: .notFound
        @unknown default: throw Failure.unknownStatus(status.rawValue)
        }
    }
}

/// Executable names must not be replaceable by another account. The broader
/// bundle-data reader may inspect shared Applications; execution cannot inherit
/// that exception. Root-owned sticky temporary ancestors retain their normal
/// owner protection for downloaded and private fixture bundles.
enum QueueExecutableNamespace {
    static func allows(executable: URL, root: URL) -> Bool {
        guard executable.isFileURL, root.isFileURL else { return false }
        let path = physicalSystemPath(executable.path)
        let boundary = physicalSystemPath(root.path)
        guard path.hasPrefix(boundary + "/"), !path.contains("\0"),
              physicalSystemPath(executable.standardizedFileURL.path) == path,
              physicalSystemPath(root.standardizedFileURL.path) == boundary else { return false }
        let components = path.split(separator: "/").map(String.init)
        guard let name = components.last else { return false }
        var directory = open("/", O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard directory >= 0 else { return false }
        defer { close(directory) }
        var current = ""
        for component in components.dropLast() {
            let next = openat(directory, component, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
            guard next >= 0 else { return false }
            close(directory)
            directory = next
            current += "/" + component
            var info = stat()
            guard fstat(directory, &info) == 0 else { return false }
            let inside = current == boundary || current.hasPrefix(boundary + "/")
            let sticky = !inside && info.st_uid == 0 && info.st_mode & S_ISVTX != 0
            guard info.st_uid == getuid() || info.st_uid == 0,
                  info.st_mode & 0o022 == 0 || sticky else { return false }
        }
        let file = openat(directory, name, O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
        guard file >= 0 else { return false }
        defer { close(file) }
        var info = stat()
        return fstat(file, &info) == 0 && info.st_mode & S_IFMT == S_IFREG &&
            (info.st_uid == getuid() || info.st_uid == 0) && info.st_nlink == 1 &&
            info.st_mode & 0o7022 == 0 && faccessat(directory, name, X_OK, 0) == 0
    }

    private static func physicalSystemPath(_ path: String) -> String {
        if path.hasPrefix("/var/") || path.hasPrefix("/tmp/") { return "/private" + path }
        return path
    }
}

/// Explicit installer boundary, separate from login-item unregistration. Run on
/// the main thread before NSApplication startup so AppKit can refresh live state.
@MainActor
enum QueueAppReplacement {
    struct Metadata: Equatable {
        let processID: pid_t
        let bundleURL: URL?
        let bundleIdentifier: String?
        let userID: uid_t?
    }

    /// All lifecycle operations and time are injected in the inert test suite.
    /// Keep the NSRunningApplication instance, rather than sending signals by PID.
    struct RunningApp {
        let metadata: () throws -> Metadata
        let isTerminated: () throws -> Bool
        let terminate: () throws -> Bool
    }

    struct Adapter {
        let applications: () throws -> [RunningApp]
        let now: () -> TimeInterval
        let advanceRunLoop: (TimeInterval) throws -> Void
    }

    private struct Record: Encodable {
        let schemaVersion = 1
        let app: String
        let status = "stopped"
    }

    enum Failure: Error, LocalizedError, Equatable {
        case invalidArguments
        case invalidTarget
        case unverifiedOwnership
        case processChanged
        case terminationRefused
        case terminationTimedOut

        var errorDescription: String? {
            switch self {
            case .invalidArguments:
                "Usage: --prepare-app-replacement ABSOLUTE_APP_PATH"
            case .invalidTarget:
                "App replacement requires a canonical app directly in the current user's Applications directory or /Applications."
            case .unverifiedOwnership:
                "Cannot verify ownership of the running Agent Queue app. Keep the installed app in place."
            case .processChanged:
                "The running Agent Queue app changed during replacement preparation. Keep the installed app in place and retry."
            case .terminationRefused:
                "The running Agent Queue app refused the quit request. Keep the installed app in place."
            case .terminationTimedOut:
                "The running Agent Queue app did not exit within 30 seconds. Keep the installed app in place."
            }
        }
    }

    static func parse(arguments: [String]) throws -> String? {
        guard arguments.contains(where: { $0.hasPrefix("--prepare-app-replacement") }) else { return nil }
        guard arguments.count == 2, arguments[0] == "--prepare-app-replacement",
              arguments[1].hasPrefix("/") else { throw Failure.invalidArguments }
        return arguments[1]
    }

    static func handle(arguments: [String]) throws -> QueueServiceManagement.CLIResult? {
        try handle(arguments: arguments, owningBundle: .main,
                   home: FileManager.default.homeDirectoryForCurrentUser,
                   currentUserID: getuid(), currentProcessID: getpid(), makeAdapter: systemAdapter)
    }

    static func handle(arguments: [String], owningBundle: Bundle, home: URL,
                       currentUserID: uid_t, currentProcessID: pid_t,
                       makeAdapter: () throws -> Adapter) throws -> QueueServiceManagement.CLIResult? {
        guard let path = try parse(arguments: arguments) else { return nil }
        try QueueServiceManagement.validateOwningBundle(owningBundle)
        let target = try validateTarget(path, home: home)
        try stop(target: target, currentUserID: currentUserID,
                 currentProcessID: currentProcessID, adapter: makeAdapter())
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let json = String(decoding: try encoder.encode(Record(app: target.path)), as: UTF8.self)
        return .init(json: json, exitCode: 0)
    }

    static func validateTarget(_ path: String, home: URL) throws -> URL {
        let target = URL(fileURLWithPath: path)
        let userApplications = home.resolvingSymlinksInPath().standardizedFileURL
            .appendingPathComponent("Applications")
        guard path.hasPrefix("/"), target.path == path,
              target.standardizedFileURL.path == path,
              target.resolvingSymlinksInPath().standardizedFileURL.path == path,
              target.lastPathComponent.hasSuffix(".app"),
              target.deletingLastPathComponent().path == userApplications.path
                || target.deletingLastPathComponent().path == "/Applications" else {
            throw Failure.invalidTarget
        }
        return target
    }

    private static func matches(_ metadata: Metadata, target: URL,
                                currentUserID: uid_t, currentProcessID: pid_t) throws -> Bool {
        guard metadata.processID > 0, metadata.processID != currentProcessID,
              metadata.bundleIdentifier == "io.went.agent-queue",
              let url = metadata.bundleURL, url.isFileURL,
              url.resolvingSymlinksInPath().standardizedFileURL == target else { return false }
        guard let userID = metadata.userID else { throw Failure.unverifiedOwnership }
        return userID == currentUserID
    }

    private static func stop(target: URL, currentUserID: uid_t, currentProcessID: pid_t,
                             adapter: Adapter) throws {
        var matching: [(RunningApp, Metadata)] = []
        for application in try adapter.applications() {
            if try application.isTerminated() { continue }
            let metadata = try application.metadata()
            if try matches(metadata, target: target, currentUserID: currentUserID,
                           currentProcessID: currentProcessID) {
                matching.append((application, metadata))
            }
        }
        let deadline = adapter.now() + 30
        for (application, metadata) in matching {
            if try application.isTerminated() { continue }
            guard try application.metadata() == metadata,
                  try matches(metadata, target: target, currentUserID: currentUserID,
                          currentProcessID: currentProcessID) else { throw Failure.processChanged }
            guard try application.terminate() else { throw Failure.terminationRefused }
        }
        while true {
            var pending = false
            for (application, _) in matching where try !application.isTerminated() { pending = true }
            if !pending { break }
            let remaining = deadline - adapter.now()
            guard remaining > 0 else { throw Failure.terminationTimedOut }
            try adapter.advanceRunLoop(min(0.05, remaining))
        }
        // A replacement must also fail if another target instance appeared while
        // the original was quitting. Never chase or kill a newly launched copy.
        for application in try adapter.applications() {
            if try application.isTerminated() { continue }
            if try matches(application.metadata(), target: target, currentUserID: currentUserID,
                           currentProcessID: currentProcessID) {
                throw Failure.processChanged
            }
        }
    }

    private static func systemAdapter() -> Adapter {
        Adapter(applications: {
            NSRunningApplication.runningApplications(withBundleIdentifier: "io.went.agent-queue").map { app in
                RunningApp(metadata: {
                    Metadata(processID: app.processIdentifier, bundleURL: app.bundleURL,
                             bundleIdentifier: app.bundleIdentifier, userID: processOwner(app.processIdentifier))
                }, isTerminated: { app.isTerminated }, terminate: { app.terminate() })
            }
        }, now: { ProcessInfo.processInfo.systemUptime }, advanceRunLoop: { interval in
            // AppKit documents time-varying properties as cached until the next
            // main run-loop turn. Sleeping or semaphore polling leaves them stale.
            RunLoop.main.run(until: Date(timeIntervalSinceNow: interval))
        })
    }

    private static func processOwner(_ processID: pid_t) -> uid_t? {
        guard processID > 0 else { return nil }
        var info = proc_bsdinfo()
        let size = Int32(MemoryLayout<proc_bsdinfo>.size)
        guard proc_pidinfo(processID, PROC_PIDTBSDINFO, 0, &info, size) == size,
              info.pbi_pid == UInt32(processID), info.pbi_uid == info.pbi_ruid else { return nil }
        return info.pbi_uid
    }
}
