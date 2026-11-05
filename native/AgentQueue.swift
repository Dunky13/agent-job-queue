import AppKit
import SwiftUI
import Foundation
import Darwin
import Combine

struct QueueSnapshot: Codable, Sendable {
    let updatedAt: String
    let queue: QueueSummary
    let jobs: [QueueJob]
    var scheduler: QueueScheduler? = nil
    // Daemons that predate plugin lanes omit these; treat that as the default lane alone.
    var lanes: [QueueLane]? = nil
    var plugins: [QueuePlugin]? = nil

    func hasSameContent(as other: QueueSnapshot) -> Bool {
        queue == other.queue && jobs == other.jobs && scheduler == other.scheduler && lanes == other.lanes && plugins == other.plugins
    }
}

struct QueueLane: Codable, Sendable, Equatable {
    let name: String
    let label: String
    let plugin: String?
    let concurrency: Int
    let priority: Int
    let sharesLimit: Bool
    let running: Int
    let held: Int
    let waiting: Int
    var removed: Bool? = nil
}

struct QueuePlugin: Codable, Sendable, Equatable {
    let name: String
    let loaded: Bool
    let error: String?
    var warning: String? = nil
    let lanes: [String]
}

struct QueueSummary: Codable, Sendable, Equatable {
    let status: String
    let limit: Int
    let running: Int
    let waiting: Int
    let legacyOccupied: Bool
}

enum SchedulingMode: String, Codable, CaseIterable, Sendable {
    case fixed, shadow, adaptive
    var label: String {
        switch self { case .fixed: "Fixed"; case .shadow: "Learn only"; case .adaptive: "Adaptive" }
    }
}

struct QueueScheduler: Codable, Sendable, Equatable {
    let mode: SchedulingMode
    let available: Bool
    let reason: String
    let updatedAt: String?
    let host: HostResources?
    var summary: String { "\(mode.label) · Resource monitoring \(available ? "active" : "unavailable")" }
    static func == (lhs: QueueScheduler, rhs: QueueScheduler) -> Bool {
        // Acquisition timestamps alone do not change the rendered dashboard.
        lhs.mode == rhs.mode && lhs.available == rhs.available && lhs.reason == rhs.reason && lhs.host == rhs.host
    }
}

struct HostResources: Codable, Sendable, Equatable {
    let sampledAt: Double
    let totalMemoryBytes: Int64
    let availableMemoryBytes: Int64
    let cpuCores: Int
    let cpuBusyCores: Double
    let memoryPressure: String
    let swapUsedBytes: Int64
    let swapInBytesPerSecond: Double
    let swapOutBytesPerSecond: Double
    let diskFreeBytes: Int64
    let machine: String
    var complete: Bool? = nil
    static func == (lhs: HostResources, rhs: HostResources) -> Bool {
        lhs.totalMemoryBytes == rhs.totalMemoryBytes && lhs.availableMemoryBytes == rhs.availableMemoryBytes &&
        lhs.cpuCores == rhs.cpuCores && lhs.cpuBusyCores == rhs.cpuBusyCores && lhs.memoryPressure == rhs.memoryPressure &&
        lhs.swapUsedBytes == rhs.swapUsedBytes && lhs.swapInBytesPerSecond == rhs.swapInBytesPerSecond &&
        lhs.swapOutBytesPerSecond == rhs.swapOutBytesPerSecond && lhs.diskFreeBytes == rhs.diskFreeBytes &&
        lhs.machine == rhs.machine && lhs.complete == rhs.complete
    }
    var summary: String {
        "Memory pressure \(memoryPressure) · \(resourceBytes(availableMemoryBytes)) available · CPU \(cpuBusyCores.formatted(.number.precision(.fractionLength(1))))/\(cpuCores) cores\(complete == false ? " · Collecting rates" : "")"
    }
}

struct ResourcePrediction: Codable, Sendable, Equatable {
    let memoryBytes: Int64
    let cpuCores: Double
    let durationSeconds: Double?
    let samples: Int
    let confidence: String
    let source: String
    var summary: String {
        "Estimate \(resourceBytes(memoryBytes)) · \(resourceCores(cpuCores)) · \(confidence) confidence (\(samples) runs, \(source))"
    }
}

struct ResourceObservation: Codable, Sendable, Equatable {
    let memoryBytes: Int64
    let peakMemoryBytes: Int64
    let cpuSeconds: Double
    let cpuCores: Double
    let durationSeconds: Double
    let samples: Int
    let complete: Bool
    var summary: String {
        "Memory \(resourceBytes(memoryBytes)) · peak \(resourceBytes(peakMemoryBytes)) · \(resourceCores(cpuCores))\(complete ? "" : samples > 0 ? " · partial sample coverage" : " · collecting")"
    }
}

struct ResourceDecision: Codable, Sendable, Equatable { let allowed: Bool; let reason: String }
struct JobResources: Codable, Sendable, Equatable {
    let prediction: ResourcePrediction
    let observed: ResourceObservation?
    let decision: ResourceDecision?
    var learning: JobLearningReference? = nil
}

func resourceBytes(_ value: Int64) -> String {
    "\((Double(value) / 1_073_741_824).formatted(.number.precision(.fractionLength(0...2)))) GiB"
}
func resourceCores(_ value: Double) -> String {
    "\(value.formatted(.number.precision(.fractionLength(0...1)))) CPU cores"
}

struct QueueJob: Codable, Identifiable, Sendable, Equatable {
    let id: Int
    let state: String
    let title: String
    let project: String
    let branch: String
    let directory: String
    let executable: String
    let priority: Int
    let queuePosition: Int?
    let submittedAt: String?
    let startedAt: String?
    let finishedAt: String?
    let exitCode: Int?
    let logPath: String
    let canReorder: Bool
    let canCancel: Bool
    let phase: String
    var canStart: Bool? = nil
    var resources: JobResources? = nil
    var lane: String? = nil
    var kind: String? = nil
    // Lease fields; absent for ordinary commands.
    var label: String? = nil
    var meta: [String: String]? = nil
    var ownerPid: Int? = nil
    var ownerAlive: Bool? = nil
    var position: Int? = nil
    var queuedAt: String? = nil
    var grantedAt: String? = nil
    var renewedAt: String? = nil
    var expiresAt: String? = nil
    var reason: String? = nil

    var isLease: Bool { kind == "lease" }
    var name: String { title.isEmpty ? executable : title }
    var isWaiting: Bool { state == "queued" || state == "waiting" }
    var isRunning: Bool { state == "running" || state == "starting" || state == "cancelling" }
    var permitsStart: Bool { isWaiting && (canStart ?? canReorder) }
    var distinctPhase: String? {
        let display = phase.replacingOccurrences(of: "_", with: " ")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        let normalizedState = state.replacingOccurrences(of: "_", with: " ")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        guard !display.isEmpty, display.caseInsensitiveCompare(normalizedState) != .orderedSame else { return nil }
        return display.capitalized
    }
    var context: String { [project, branch].filter { !$0.isEmpty }.joined(separator: " / ") }
    var stateLabel: String {
        if let code = exitCode, code != 0 { return "Failed (\(code))" }
        return state.capitalized
    }
    var color: Color {
        if state == "failed" || (exitCode ?? 0) != 0 { return .red }
        if state == "cancelled" || state == "canceled" { return .secondary }
        if isRunning { return .green }
        if isWaiting { return .orange }
        return .secondary
    }
}

enum ClientError: LocalizedError {
    case message(String)
    var errorDescription: String? {
        switch self { case .message(let message): return message }
    }
}

// CLI I/O and deadline handling stay off the main actor. Drain the private pipe
// while the child runs so neither pipe capacity nor output size can block us.
private struct QueueControllerFailure: Decodable { let error: String }

private final class QueueReadCancellation: @unchecked Sendable {
    private let lock = NSLock()
    private var cancelled = false
    func cancel() { lock.withLock { cancelled = true } }
    var isCancelled: Bool { lock.withLock { cancelled } }
    func check() throws { if lock.withLock({ cancelled }) { throw CancellationError() } }
}

enum QueueClient {
    enum Controller: String, Sendable { case heavy = "agent-heavy", remote = "agent-remote" }
    // Private executable injection keeps transport tests off installed state and
    // lets setup run its selected installer. A timeout overrides the command's own.
    struct Transport: Sendable {
        let executable: URL
        var timeout: TimeInterval? = nil
    }

    /// One controller invocation and its whole policy. Call sites name a factory
    /// below; nothing downstream reads argv to decide how the command behaves.
    struct Command: Equatable, Sendable {
        enum Cancellation: Sendable {
            /// An accepted change finishes and returns its original response.
            case retain
            /// A known read ends its private child and throws CancellationError.
            case discard
            /// Automatic setup asks the backend to stop; only a reported wait ends as cancelled.
            case stopWhileWaiting
        }
        /// Output bounds and the wording of deadline and failure errors.
        enum Reporting: Sendable { case queue, updates, setup }

        var controller = Controller.heavy
        var arguments: [String]
        var input: Data? = nil
        var timeout: TimeInterval = 25
        var cancellation = Cancellation.retain
        var reporting = Reporting.queue
        /// Decode `{"error": ...}` from a failed controller instead of showing its raw output.
        var decodesStructuredErrors = false
    }

    /// `install` target. Only an absolute path earns a discardable or unbounded setup policy.
    struct InstallTarget: Equatable, Sendable {
        enum Option: String, Sendable { case appBundle = "--app-bundle", payload = "--payload" }
        let option: Option
        let path: String
        fileprivate var arguments: [String] { ["install", option.rawValue, path] }
        fileprivate var isExactPath: Bool { path.hasPrefix("/") && !path.contains("\0") }
    }

    static func run(_ command: Command, transport: Transport? = nil,
                    onOutput: (@Sendable (Data) -> Void)? = nil) async throws -> Data {
        let cancellation = command.cancellation == .retain ? nil : QueueReadCancellation()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { continuation in
                DispatchQueue.global(qos: .utility).async {
                    do { continuation.resume(returning: try runBlocking(command, cancellation: cancellation,
                        transport: transport, onOutput: onOutput)) }
                    catch { continuation.resume(throwing: error) }
                }
            }
        } onCancel: {
            cancellation?.cancel()
        }
    }

    /// Applies one command's policy to the byte-level process runner beneath it.
    private static func runBlocking(_ command: Command, cancellation: QueueReadCancellation?, transport: Transport?,
                                    onOutput: (@Sendable (Data) -> Void)?) throws -> Data {
        try cancellation?.check()
        guard (command.input?.count ?? 0) <= 16_384 else { throw ClientError.message("Controller input exceeds the private-pipe limit.") }
        let executable = try transport?.executable ?? installedExecutable(command.controller)
        let timeout = transport?.timeout ?? command.timeout
        let result: (status: Int32, output: Data)
        do {
            result = try runProcess(executable: executable, arguments: command.arguments, input: command.input, timeout: timeout,
                capture: command.reporting == .setup ? .tail(32_768) : .rejectingOverflow(2_097_152),
                cancellation: cancellation,
                gracefulStop: command.cancellation != .stopWhileWaiting ? nil : GracefulStop(signal: SIGUSR1) {
                    QueueSetupProgress.parse(String(decoding: $0, as: UTF8.self)).isWaiting
                }, onOutput: onOutput)
        } catch ProcessRunnerFailure.deadline(let partial) {
            throw command.deadlineError(timeout: timeout, partial: partial)
        } catch ProcessRunnerFailure.overflow {
            throw ClientError.message("The queue response exceeds the 2 MB display limit.")
        } catch ProcessRunnerFailure.stopSignal {
            throw ClientError.message("Could not ask setup to stop waiting. Its installation evidence is preserved.")
        }
        guard result.status == 0 else { throw command.failure(output: result.output, status: result.status) }
        return result.output
    }

    private static func installedExecutable(_ controller: Controller) throws -> URL {
        guard Bundle.main.object(forInfoDictionaryKey: "AgentQueueServiceManagement") as? Bool == true else {
            return FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".local/bin/\(controller.rawValue)")
        }
        let app = Bundle.main.bundleURL
        let executable = app.appendingPathComponent("Contents/MacOS/\(controller.rawValue)")
        guard try QueueBootstrapFiles.safeExecutable(executable, scope: .init(root: app, bundled: true)) else {
            throw ClientError.message("The bundled controller is missing or unsafe. Download Agent Queue again and drag it into Applications.")
        }
        return executable
    }

    private enum Capture {
        case rejectingOverflow(Int)
        case tail(Int)
    }
    private enum ProcessRunnerFailure: Error {
        case deadline(partial: Data), overflow, stopSignal
    }
    /// Keeps a cancelled child running until it exits; cancellation then applies
    /// only if the output read so far says stopping was safe.
    private struct GracefulStop {
        let signal: Int32
        let isStoppable: @Sendable (Data) -> Bool
    }

    private static func runProcess(executable: URL, arguments: [String], input: Data?, timeout: TimeInterval, capture: Capture,
                                   cancellation: QueueReadCancellation?, gracefulStop: GracefulStop?,
                                   onOutput: (@Sendable (Data) -> Void)?) throws -> (status: Int32, output: Data) {
        let output = Pipe()
        defer {
            try? output.fileHandleForReading.close()
            try? output.fileHandleForWriting.close()
        }
        guard fcntl(output.fileHandleForReading.fileDescriptor, F_SETFL, O_NONBLOCK) != -1 else {
            throw ClientError.message("Could not prepare the private controller output pipe.")
        }
        let process = Process()
        process.executableURL = executable
        process.arguments = arguments
        let inputPipe = input == nil ? nil : Pipe()
        if let inputPipe {
            guard fcntl(inputPipe.fileHandleForWriting.fileDescriptor, F_SETNOSIGPIPE, 1) != -1,
                  fcntl(inputPipe.fileHandleForWriting.fileDescriptor, F_SETFL, O_NONBLOCK) != -1 else {
                throw ClientError.message("Could not prepare the private controller input pipe.")
            }
            process.standardInput = inputPipe.fileHandleForReading
        } else { process.standardInput = FileHandle.nullDevice }
        defer {
            try? inputPipe?.fileHandleForReading.close()
            try? inputPipe?.fileHandleForWriting.close()
        }
        process.standardOutput = output.fileHandleForWriting
        process.standardError = output.fileHandleForWriting
        try cancellation?.check()
        try process.run()
        // Every exit path tears down and reaps only this private controller child.
        // Queue daemon jobs have their own ownership and are never signalled here.
        defer {
            if process.isRunning {
                process.terminate()
                for _ in 0..<25 {
                    if !process.isRunning { break }
                    Thread.sleep(forTimeInterval: 0.01)
                }
                if process.isRunning { kill(process.processIdentifier, SIGKILL) }
            }
            process.waitUntilExit()
        }
        try output.fileHandleForWriting.close()
        try inputPipe?.fileHandleForReading.close()
        var inputOffset = 0
        var inputClosed = inputPipe == nil
        let deadline = ProcessInfo.processInfo.systemUptime + timeout
        var response = Data()
        var lastOutputDelivery = 0.0
        var pendingOutputDelivery = false
        var buffer = [UInt8](repeating: 0, count: 16_384)
        var observedChildExit = false
        var stoppable = false
        var requestedStop = false
        while true {
            // Progress can lag a mutation. The backend, rather than this cached
            // display state, decides when a stop request is safe to honor.
            if let gracefulStop {
                if !requestedStop, cancellation?.isCancelled == true, process.isRunning {
                    guard kill(process.processIdentifier, gracefulStop.signal) == 0 || errno == ESRCH else {
                        throw ProcessRunnerFailure.stopSignal
                    }
                    requestedStop = true
                }
            } else { try cancellation?.check() }
            if ProcessInfo.processInfo.systemUptime >= deadline { throw ProcessRunnerFailure.deadline(partial: response) }
            // Bound each read, including the one that detects overflow. No output
            // is written to disk and retained response bytes never exceed the cap.
            let readLimit: Int
            switch capture {
            case .tail: readLimit = buffer.count
            case .rejectingOverflow(let limit): readLimit = min(buffer.count, limit - response.count + 1)
            }
            let count = read(output.fileHandleForReading.fileDescriptor, &buffer, readLimit)
            let readError = count < 0 ? errno : 0
            if count > 0 {
                if case .rejectingOverflow(let limit) = capture, count > limit - response.count {
                    throw ProcessRunnerFailure.overflow
                }
                response.append(contentsOf: buffer.prefix(count))
                if let gracefulStop { stoppable = gracefulStop.isStoppable(response) }
                if case .tail(let limit) = capture, response.count > limit { response.removeFirst(response.count - limit) }
                pendingOutputDelivery = true
            } else if count < 0 && readError != EAGAIN && readError != EINTR {
                throw ClientError.message("Could not read the private controller response.")
            }
            // Flush the last throttled update even when the child then waits
            // silently for jobs or approval and emits no additional bytes.
            if pendingOutputDelivery, let onOutput, ProcessInfo.processInfo.systemUptime - lastOutputDelivery >= 0.1 {
                onOutput(response)
                lastOutputDelivery = ProcessInfo.processInfo.systemUptime
                pendingOutputDelivery = false
            }
            if !inputClosed, let input, let inputPipe {
                let written = input.withUnsafeBytes { bytes in
                    write(inputPipe.fileHandleForWriting.fileDescriptor,
                          bytes.baseAddress?.advanced(by: inputOffset), input.count - inputOffset)
                }
                if written > 0 { inputOffset += written }
                else if written < 0 && errno != EAGAIN && errno != EINTR {
                    throw ClientError.message("Could not send private configuration to the controller.")
                }
                if inputOffset == input.count {
                    try inputPipe.fileHandleForWriting.close()
                    inputClosed = true
                }
            }
            // Drain remaining buffered bytes after exit without waiting on a pipe
            // descriptor inherited by an independently running daemon.
            if count < 0 && readError == EINTR { continue }
            if count <= 0 && !process.isRunning {
                if observedChildExit { break }
                // The child may have written its final bytes between our empty
                // read and the exit observation. Drain once more after that fence.
                observedChildExit = true
                continue
            }
            if count < 0 && readError == EAGAIN {
                // Wake as soon as the child writes again. Fixed sleeps can be
                // coalesced at utility QoS and repeatedly stall a blocked writer.
                // The bounded wait still checks cancellation and the deadline.
                var descriptor = pollfd(fd: output.fileHandleForReading.fileDescriptor,
                                        events: Int16(POLLIN), revents: 0)
                if poll(&descriptor, 1, 10) < 0 && errno != EINTR {
                    throw ClientError.message("Could not wait for the private controller response.")
                }
            } else if count == 0 {
                // EOF may precede child exit. Avoid spinning on persistent HUP.
                Thread.sleep(forTimeInterval: 0.01)
            }
        }
        if gracefulStop == nil || stoppable { try cancellation?.check() }
        onOutput?(response)
        return (process.terminationStatus, response)
    }

    static func logTail(_ path: String) async throws -> String {
        try await Task.detached(priority: .utility) {
            let handle = try FileHandle(forReadingFrom: URL(fileURLWithPath: path))
            defer { try? handle.close() }
            let length = try handle.seekToEnd()
            let start = length > 65_536 ? length - 65_536 : 0
            try handle.seek(toOffset: start)
            let bytes = try handle.read(upToCount: 65_536) ?? Data()
            let text = String(decoding: bytes, as: UTF8.self)
            if start > 0 { return "[Showing the latest 64 KB]\n\n" + text }
            return text.isEmpty ? "No output yet." : text
        }.value
    }
}

extension QueueClient.Command {
    fileprivate func deadlineError(timeout: TimeInterval, partial: Data) -> ClientError {
        switch reporting {
        case .setup:
            .message("Setup did not finish within \(Int(timeout)) seconds. Quit and reopen Agent Queue, or retry setup.\n" + String(decoding: partial.suffix(8_000), as: UTF8.self))
        case .updates:
            .message("The update operation did not finish within \(Int(timeout)) seconds. Check your Downloads folder, then check for updates before retrying.")
        case .queue:
            .message("The queue did not respond within \(Int(timeout)) seconds. Jobs continue independently. Refresh to check whether the change was applied.")
        }
    }

    fileprivate func failure(output: Data, status: Int32) -> ClientError {
        if decodesStructuredErrors, let failure = try? JSONDecoder().decode(QueueControllerFailure.self, from: output) {
            return .message(failure.error)
        }
        let message = String(decoding: reporting == .setup ? Data(output.suffix(8_000)) : Data(output.prefix(1_000)), as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return .message(message.isEmpty ? "Queue command failed (\(status))." : message)
    }

    // Queue. A snapshot is a discardable read; accepted changes keep the 25-second default.
    static let status = Self(arguments: ["status", "--json"], timeout: 8, cancellation: .discard)
    static func jobAction(_ action: String, id: Int) -> Self { .init(arguments: [action, String(id)]) }
    static let resume = Self(arguments: ["resume"])
    static func limit(_ limit: Int) -> Self { .init(arguments: ["limit", String(limit)]) }
    static func scheduling(_ mode: SchedulingMode) -> Self { .init(arguments: ["scheduling", mode.rawValue]) }
    static let version = Self(arguments: ["version", "--json"])

    // Agent instructions. Their status read is not discardable.
    static let agentsStatus = Self(arguments: ["agents", "status", "--json"])
    static func agents(_ action: String, runner: String) -> Self {
        .init(arguments: ["agents", action, "--runner", runner, "--json"])
    }

    // Learning. Reads are discardable; every profiles command reports structured errors.
    private static func profilesRead(_ arguments: [String]) -> Self {
        .init(arguments: ["profiles"] + arguments + ["--json"], cancellation: .discard, decodesStructuredErrors: true)
    }
    static func profilesList(offset: Int, limit: Int, query: String, project: String? = nil,
                             operation: String? = nil, otherProjects: Bool = false) -> Self {
        var arguments = ["list", "--offset", String(offset), "--limit", String(limit), "--query", query]
        if let project { arguments += ["--project", project] }
        if let operation { arguments += ["--operation", operation] }
        if otherProjects { arguments.append("--other-projects") }
        return profilesRead(arguments)
    }
    static func profilesHierarchy(limit: Int) -> Self { profilesRead(["list", "--hierarchy", "--limit", String(limit)]) }
    static func profilesShow(_ id: String) -> Self { profilesRead(["show", id]) }
    static func profilesShow(_ id: String, family: String) -> Self { profilesRead(["show", id, "--family", family]) }
    static func profilesShow(_ id: String, membersQuery: String, membersOffset: Int) -> Self {
        profilesRead(["show", id, "--members-query", membersQuery, "--members-offset", String(membersOffset)])
    }
    static func profilesChange(_ operation: String, _ id: String, payload: Data) -> Self {
        .init(arguments: ["profiles", operation, id, "--json"], input: payload, decodesStructuredErrors: true)
    }

    // A verified update includes a bounded download and signature checks. The
    // backend has a 120-second aggregate deadline; normal queue I/O is unchanged.
    static let updatesCheck = Self(arguments: ["updates", "check", "--json"], timeout: 130, reporting: .updates)
    static func updatesDownload(build: Int) -> Self {
        .init(arguments: ["updates", "download", "--build", String(build), "--json"], timeout: 130, reporting: .updates)
    }

    // Phone lifecycle owns up to 120 seconds for native stop/start and
    // readiness. Its caller must allow startup and result delivery too.
    static let remoteStatus = Self(controller: .remote, arguments: ["status", "--json"], timeout: 8, cancellation: .discard)
    static func remoteEnabled(_ enabled: Bool) -> Self {
        .init(controller: .remote, arguments: [enabled ? "enable" : "disable"], timeout: 125)
    }
    static func remoteConfigure(_ input: Data) -> Self {
        .init(controller: .remote, arguments: ["configure"], input: input, timeout: 125)
    }

    // Setup. The backend bounds each inspection and mutation independently.
    /// Any setup argv without a named read or wait policy stays finite and retained.
    static func setup(_ arguments: [String], timeout: TimeInterval = 330, cancellation: Cancellation = .retain) -> Self {
        .init(arguments: arguments, timeout: timeout, cancellation: cancellation, reporting: .setup)
    }
    static func install(_ target: QueueClient.InstallTarget) -> Self { setup(target.arguments) }
    /// Only the complete read-only form is discardable. Additional recovery
    /// options can mutate state and must retain their accepted response.
    static func installPreflight(_ target: QueueClient.InstallTarget) -> Self {
        setup(target.arguments + ["--preflight", "--json"], cancellation: target.isExactPath ? .discard : .retain)
    }
    /// Waiting for existing work to drain has no aggregate deadline. It can stop
    /// only while its controller reports a wait; pending identity and custody survive.
    static func installRepairAndContinue(_ target: QueueClient.InstallTarget) -> Self {
        let arguments = target.arguments + ["--repair-and-continue"]
        return target.isExactPath ? setup(arguments, timeout: .infinity, cancellation: .stopWhileWaiting) : setup(arguments)
    }
    static func installDisablingPhoneAccess(_ target: QueueClient.InstallTarget) -> Self {
        setup(target.arguments + ["--disable-phone-access"])
    }
    static func installRecovery(_ target: QueueClient.InstallTarget, action: String, jobID: Int? = nil,
                                expectedRevision: String? = nil) -> Self {
        setup(target.arguments + ["--recover", action] + (jobID.map { ["--job-id", String($0)] } ?? [])
            + (expectedRevision.map { ["--expected-revision", $0] } ?? []))
    }
    static func serviceStatus(_ service: QueueServiceManagement.Service) -> Self {
        setup(["--service-management", "status", service.rawValue], timeout: 5, cancellation: .discard)
    }
}

struct PreparedQueueSnapshot: Sendable {
    let snapshot: QueueSnapshot
    let running: [QueueJob]
    let waiting: [QueueJob]
    let recent: [QueueJob]

    init(_ snapshot: QueueSnapshot) {
        self.snapshot = snapshot
        running = snapshot.jobs.filter(\.isRunning)
        waiting = snapshot.jobs.filter(\.isWaiting).sorted {
            ($0.queuePosition ?? Int.max, $0.id) < ($1.queuePosition ?? Int.max, $1.id)
        }
        recent = Array(snapshot.jobs.lazy.filter { !$0.isRunning && !$0.isWaiting }.prefix(12))
    }

    static func decode(_ data: Data, comparedTo previous: QueueSnapshot?) async throws -> Self? {
        let preparation = Task<Self?, Error>.detached(priority: .utility) {
            assert(!Thread.isMainThread, "Queue snapshot decoding must stay off the main thread.")
            try Task.checkCancellation()
            let next = try JSONDecoder().decode(QueueSnapshot.self, from: data)
            guard previous?.hasSameContent(as: next) != true else { return nil }
            try Task.checkCancellation()
            return Self(next)
        }
        return try await withTaskCancellationHandler {
            let next = try await preparation.value
            try Task.checkCancellation()
            return next
        } onCancel: { preparation.cancel() }
    }
}

@MainActor
final class QueueStore: ObservableObject {
    @Published private var prepared: PreparedQueueSnapshot?
    // Synchronous assignment is used only by isolated fixture navigation checks.
    // Live refresh publishes one complete, precomputed value from the worker.
    var snapshot: QueueSnapshot? {
        get { prepared?.snapshot }
        set { prepared = newValue.map(PreparedQueueSnapshot.init) }
    }
    var snapshotPublisher: AnyPublisher<QueueSnapshot?, Never> {
        $prepared.map { $0?.snapshot }.eraseToAnyPublisher()
    }
    @Published var error: String?
    @Published var lastRefreshed: Date?
    @Published var isRefreshing = false
    @Published var isMutating = false
    @Published private(set) var isResuming = false
    @Published var notice: String?
    private(set) var visibleSurfaces = 0
    private var pollTask: Task<Void, Never>?
    private let statusLoader: @Sendable () async throws -> Data
    private let command: @Sendable (QueueClient.Command) async throws -> Data
    private let snapshotPreparer: @Sendable (Data, QueueSnapshot?) async throws -> PreparedQueueSnapshot?
    private var refreshInFlight = false
    private var refreshRequested = false
    private var mutationRevision = 0
    let fixtureMode: Bool
    #if AGENT_QUEUE_TEST_HOOKS
    private(set) var fixtureRecorder: FixtureDispatchRecorder?
    #endif

    init(statusLoader: @escaping @Sendable () async throws -> Data = {
        try await QueueClient.run(.status)
    }, command: @escaping @Sendable (QueueClient.Command) async throws -> Data = { try await QueueClient.run($0) },
         snapshotPreparer: @escaping @Sendable (Data, QueueSnapshot?) async throws -> PreparedQueueSnapshot? = {
             try await PreparedQueueSnapshot.decode($0, comparedTo: $1)
         }, arguments: [String] = CommandLine.arguments) {
        self.statusLoader = statusLoader
        self.command = command
        self.snapshotPreparer = snapshotPreparer
        #if AGENT_QUEUE_TEST_HOOKS
        let args = arguments
        if let index = args.firstIndex(of: "--fixture"), args.indices.contains(index + 1) {
            fixtureMode = true
            if args.contains("--verify-confirmation-navigation") { fixtureRecorder = FixtureDispatchRecorder() }
            do {
                let data = try Data(contentsOf: URL(fileURLWithPath: args[index + 1]))
                snapshot = try JSONDecoder().decode(QueueSnapshot.self, from: data)
                lastRefreshed = Date()
            } catch { self.error = "Fixture could not be loaded: \(error.localizedDescription)" }
        } else { fixtureMode = false }
        #else
        fixtureMode = false
        #endif
    }

    var running: [QueueJob] { prepared?.running ?? [] }
    var waiting: [QueueJob] { prepared?.waiting ?? [] }
    var recent: [QueueJob] { prepared?.recent ?? [] }
    var isPaused: Bool { snapshot?.queue.status.lowercased() == "paused" }
    var actionsDisabled: Bool {
        #if AGENT_QUEUE_TEST_HOOKS
        if fixtureMode && fixtureRecorder == nil { return true }
        #endif
        return isRefreshing || isMutating || error != nil
    }
    func start() {
        guard !fixtureMode, pollTask == nil else { return }
        pollTask = Task { [weak self] in
            await self?.refresh()
            var ticks = 0
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(2)) } catch { return }
                guard let self else { return }
                ticks += 1
                if visibleSurfaces > 0 || AppRuntime.queueWindow?.isVisible == true || ticks % 5 == 0 { await refresh() }
            }
        }
    }

    func appear(fullWindow: Bool) {
        if !fullWindow { visibleSurfaces += 1 }
        Task { await refresh() }
    }
    func disappear(fullWindow: Bool) {
        if !fullWindow { visibleSurfaces = max(0, visibleSurfaces - 1) }
    }

    func refresh(manual: Bool = false) async {
        guard !fixtureMode, !isMutating else { return }
        guard !refreshInFlight else { refreshRequested = true; return }
        refreshInFlight = true
        let revision = mutationRevision
        let showProgress = manual || snapshot == nil
        if showProgress { isRefreshing = true }
        defer {
            refreshInFlight = false
            if showProgress { isRefreshing = false }
            if refreshRequested {
                refreshRequested = false
                Task { await refresh() }
            }
        }
        do {
            let data = try await statusLoader()
            guard revision == mutationRevision, !isMutating else { return }
            let next = try await snapshotPreparer(data, snapshot)
            try Task.checkCancellation()
            guard revision == mutationRevision, !isMutating else { return }
            if let next {
                prepared = next
                lastRefreshed = Date()
            }
            if error != nil { error = nil }
        } catch is CancellationError {
            // A discarded refresh is not a controller failure.
        } catch {
            guard revision == mutationRevision, !isMutating else { return }
            if self.error != error.localizedDescription { self.error = error.localizedDescription }
        }
    }

    @discardableResult
    func dispatch(_ action: String, job: QueueJob) -> Task<Void, Never>? {
        guard !actionsDisabled, ["next", "last", "cancel", "start"].contains(action) else { return nil }
        guard action == "cancel" ? job.canCancel : action == "start" ? job.permitsStart : job.isWaiting && job.canReorder else { return nil }
        // Reserve before scheduling asynchronous I/O so polling and other windows
        // cannot silently consume the user's confirmed action between these steps.
        isMutating = true
        mutationRevision += 1
        notice = nil
        return Task { await completeAction(action, job: job) }
    }

    private func completeAction(_ action: String, job: QueueJob) async {
        #if AGENT_QUEUE_TEST_HOOKS
        if fixtureMode {
            // Fixture actions can only reach this recorder, never the live CLI.
            fixtureRecorder?.record(action: action, jobID: job.id)
            isMutating = false
            return
        }
        #endif
        do {
            _ = try await command(.jobAction(action, id: job.id))
            switch action {
            case "next": notice = "Job #\(job.id) will run next."
            case "last": notice = "Job #\(job.id) moved to the end."
            case "start": notice = "Job #\(job.id) requested to start outside the parallel limit."
            default: notice = "Cancellation requested for job #\(job.id)."
            }
            error = nil
        } catch {
            self.error = error.localizedDescription
            isMutating = false
            return
        }
        isMutating = false
        await refresh()
    }

    @discardableResult
    func dispatchResume() -> Task<Void, Never>? {
        guard !fixtureMode, !actionsDisabled, isPaused else { return nil }
        isMutating = true
        isResuming = true
        mutationRevision += 1
        notice = nil
        return Task {
            do {
                _ = try await command(.resume)
                notice = "Queue resumed."
                error = nil
            } catch {
                self.error = error.localizedDescription
                isResuming = false
                isMutating = false
                return
            }
            isResuming = false
            isMutating = false
            await refresh()
        }
    }

    @discardableResult
    func dispatchLimit(_ limit: Int) -> Task<Void, Never>? {
        guard !fixtureMode, !actionsDisabled, (1...8).contains(limit) else { return nil }
        isMutating = true
        mutationRevision += 1
        notice = nil
        return Task { await completeLimit(limit) }
    }

    private func completeLimit(_ limit: Int) async {
        do {
            _ = try await command(.limit(limit))
            notice = "Parallel job limit changed to \(limit)."
            error = nil
        } catch {
            self.error = error.localizedDescription
            isMutating = false
            return
        }
        isMutating = false
        await refresh()
    }

    @discardableResult
    func dispatchScheduling(_ mode: SchedulingMode) -> Task<Void, Never>? {
        guard !fixtureMode, !actionsDisabled, let scheduler = snapshot?.scheduler, scheduler.mode != mode else { return nil }
        isMutating = true
        mutationRevision += 1
        notice = nil
        return Task {
            do {
                _ = try await command(.scheduling(mode))
                notice = "Scheduling changed to \(mode.label)."
                error = nil
            } catch {
                self.error = error.localizedDescription
                isMutating = false
                return
            }
            isMutating = false
            await refresh()
        }
    }
}

struct RemoteStatus: Codable, Sendable {
    let enabled: Bool
    let configured: Bool
    let running: Bool
    let url: String
    let port: Int
    let username: String
    let certificatePath: String
    let error: String?
}

private struct RemoteConfiguration: Encodable { let password: String }

@MainActor
final class RemoteAccessStore: ObservableObject {
    @Published var status: RemoteStatus?
    @Published var error: String?
    @Published var notice: String?
    @Published var isBusy = false
    @Published var isRefreshing = false
    private var pollTask: Task<Void, Never>?
    let fixtureMode: Bool
    /// Runs `agent-remote serve` as a child of the app. Nil for fixtures and unmanaged launches.
    var supervisor: QueueHelperSupervisor?
    private let command: @Sendable (QueueClient.Command) async throws -> Data

    init(fixtureMode: Bool, command: @escaping @Sendable (QueueClient.Command) async throws -> Data = { try await QueueClient.run($0) }) {
        self.fixtureMode = fixtureMode
        self.command = command
        #if AGENT_QUEUE_TEST_HOOKS
        if fixtureMode {
            let stopFailure = CommandLine.arguments.contains("--snapshot-remote-stop-failure")
                || CommandLine.arguments.contains("--verify-remote-stop-state")
            status = RemoteStatus(enabled: false, configured: stopFailure, running: false,
                url: "https://192.0.2.10:8743/", port: 8743, username: "agent", certificatePath: "",
                error: stopFailure ? "Remote stop incomplete" : nil)
        }
        #endif
    }

    var effectiveError: String? { status?.error ?? error }
    var needsAttention: Bool { effectiveError != nil }
    var statusLabel: String {
        if needsAttention { return "Needs attention" }
        if isBusy { return "Applying settings…" }
        guard let status else { return isRefreshing ? "Loading status…" : "Status unavailable" }
        if status.running { return "Available over HTTPS on your home LAN" }
        if status.enabled { return "Enabled, waiting for the HTTPS service" }
        return "Off"
    }

    func startPolling() {
        guard !fixtureMode, pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                guard AppRuntime.remoteWindow?.isVisible == true else { pollTask = nil; return }
                await refresh()
                do { try await Task.sleep(for: .seconds(5)) } catch { return }
            }
        }
    }

    func refresh() async {
        guard !fixtureMode, !isBusy, !isRefreshing else { return }
        isRefreshing = true
        defer { isRefreshing = false }
        do {
            status = try JSONDecoder().decode(RemoteStatus.self,
                from: await command(.remoteStatus))
            error = status?.error
        } catch { self.error = error.localizedDescription }
    }

    func setEnabled(_ enabled: Bool) {
        guard !fixtureMode, !isBusy, !isRefreshing, let status,
              !enabled || status.configured else { return }
        isBusy = true
        notice = nil
        Task {
            defer { isBusy = false }
            do {
                let data = try await command(.remoteEnabled(enabled))
                self.status = try JSONDecoder().decode(RemoteStatus.self, from: data)
                if enabled { supervisor?.start(.remote) } else { await supervisor?.stop(.remote) }
                error = self.status?.error
                notice = enabled ? "Remote access enabled." : "Remote access disabled. Local jobs continue running."
            } catch { self.error = error.localizedDescription }
        }
    }

    static func acceptsPassword(_ password: String) -> Bool {
        guard password.unicodeScalars.count >= 4, password.utf8.count <= 72 else { return false }
        let forbidden = CharacterSet.controlCharacters.union(.illegalCharacters).union(.newlines)
        return password.unicodeScalars.allSatisfy { scalar in
            guard !forbidden.contains(scalar) else { return false }
            switch scalar.properties.generalCategory {
            case .control, .format, .surrogate, .privateUse, .unassigned, .lineSeparator, .paragraphSeparator:
                return false
            case .spaceSeparator:
                return scalar.value == 0x20
            default:
                return true
            }
        }
    }

    @discardableResult
    func configure(password: String, enable: Bool) -> Bool {
        guard !fixtureMode, !isBusy, !isRefreshing, Self.acceptsPassword(password) else { return false }
        isBusy = true
        error = nil
        notice = nil
        Task {
            defer { isBusy = false }
            do {
                // The password is JSON-encoded directly into a private stdin pipe.
                // It is never an argument, environment variable, log, or disk file.
                let input = try JSONEncoder().encode(RemoteConfiguration(password: password))
                let data = try await command(.remoteConfigure(input))
                status = try JSONDecoder().decode(RemoteStatus.self, from: data)
                if enable {
                    status = try JSONDecoder().decode(RemoteStatus.self,
                        from: await command(.remoteEnabled(true)))
                    supervisor?.start(.remote)
                }
                error = status?.error
                notice = enable ? "Password saved and remote access enabled." : "Remote access password updated."
            } catch {
                // Never reflect an accidentally echoed password into the settings UI.
                self.error = error.localizedDescription.replacingOccurrences(of: password, with: "[redacted]")
            }
        }
        return true
    }
}

@MainActor enum AppRuntime {
    static let store = QueueStore()
    static let remoteStore = RemoteAccessStore(fixtureMode: store.fixtureMode)
    static let integrationStore = AgentIntegrationStore(fixtureMode: store.fixtureMode)
    static let updateStore = QueueUpdateStore(fixtureMode: store.fixtureMode)
    static let learningStore = LearningStore(fixtureMode: store.fixtureMode)
    static var queueWindow: NSWindow?
    static var remoteWindow: NSWindow?
    static var integrationWindow: NSWindow?
    static var updatesWindow: NSWindow?
    static var learningWindow: NSWindow?
    static var menuBarController: QueueMenuBarController?

    static func perform(_ action: QueueAppAction) {
        menuBarController?.closePopover()
        switch action {
        case .openWindow: showWindow()
        case .learnedWorkloads: showLearningWindow()
        case .updates: showUpdatesWindow()
        case .integration: showIntegrationWindow()
        case .remoteAccess: showRemoteWindow()
        case .quit: NSApp.terminate(nil)
        }
    }

    static func showLearningWindow(profileID: String? = nil) {
        if let learningWindow {
            learningWindow.makeKeyAndOrderFront(nil)
            NSApp.activate()
            if let profileID { learningStore.select(profileID) }
            return
        }
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1000, height: 780),
            styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "Learned Workloads"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 800, height: 550)
        window.contentView = NSHostingView(rootView: LearningWindow(store: learningStore) {
            AppRuntime.learningWindow?.close()
        })
        learningWindow = window
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate()
        if let profileID {
            Task {
                await learningStore.loadPage(preserveSelection: true)?.value
                await learningStore.select(profileID)?.value
            }
        }
    }

    static func showUpdatesWindow() {
        if let updatesWindow {
            updatesWindow.makeKeyAndOrderFront(nil)
            NSApp.activate()
            Task { await updateStore.refreshInstalled() }
            return
        }
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 620, height: 620),
            styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "About Agent Queue"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 590, height: 500)
        window.contentView = NSHostingView(rootView: QueueUpdatesSettings(store: updateStore) {
            AppRuntime.updatesWindow?.close()
        })
        updatesWindow = window
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate()
    }

    static func showIntegrationWindow() {
        if let integrationWindow {
            integrationWindow.makeKeyAndOrderFront(nil)
            NSApp.activate()
            Task { await integrationStore.refresh() }
            return
        }
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 600, height: 720),
            styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "Agent Integration"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 520, height: 540)
        window.contentView = NSHostingView(rootView: AgentIntegrationSettings(store: integrationStore) {
            AppRuntime.integrationWindow?.close()
        })
        integrationWindow = window
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate()
    }

    static func showWindow() {
        if let queueWindow { queueWindow.makeKeyAndOrderFront(nil); NSApp.activate(); return }
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 700, height: 760),
                              styleMask: [.titled, .closable, .miniaturizable, .resizable],
                              backing: .buffered, defer: false)
        window.title = "Agent Queue"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 490, height: 480)
        window.contentView = NSHostingView(rootView: QueuePanel(store: store, fullWindow: true))
        queueWindow = window
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate()
    }

    static func showRemoteWindow() {
        if let remoteWindow {
            remoteWindow.makeKeyAndOrderFront(nil)
            NSApp.activate()
            remoteStore.startPolling()
            Task { await remoteStore.refresh() }
            return
        }
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 580, height: 780),
            styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "Remote Access"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 500, height: 570)
        window.contentView = NSHostingView(rootView: RemoteAccessSettings(store: remoteStore))
        remoteWindow = window
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate()
        remoteStore.startPolling()
    }
}

enum QueueAppAction: Int, CaseIterable, Identifiable {
    case openWindow, learnedWorkloads, updates, integration, remoteAccess, quit
    var id: Int { rawValue }
    static let settings: [Self] = [.learnedWorkloads, .updates, .integration, .remoteAccess]
    static let quitExplanation = "Quitting stops the queue. Running jobs continue; the queue reopens paused if any were active."

    var title: String {
        switch self {
        case .openWindow: "Open window"
        case .learnedWorkloads: "Learned workloads…"
        case .updates: "About and updates…"
        case .integration: "Agent integration…"
        case .remoteAccess: "Remote access settings…"
        case .quit: "Quit Agent Queue"
        }
    }
    var symbol: String {
        switch self {
        case .openWindow: "arrow.up.left.and.arrow.down.right"
        case .learnedWorkloads: "chart.bar.doc.horizontal"
        case .updates: "info.circle"
        case .integration: "terminal"
        case .remoteAccess: "network"
        case .quit: "power"
        }
    }
}

enum QueueStatusActivation: Equatable {
    case primaryClick, doubleClick, contextMenu, accessible

    static func from(_ event: NSEvent?) -> Self {
        guard let event else { return .accessible }
        switch event.type {
        case .rightMouseDown, .rightMouseUp: return .contextMenu
        case .leftMouseDown, .leftMouseUp:
            if event.modifierFlags.contains(.control) { return .contextMenu }
            return event.clickCount > 1 ? .doubleClick : .primaryClick
        default:
            // clickCount raises an Objective-C exception for keyboard and other events.
            return .accessible
        }
    }
}

struct QueueStatusPresentation {
    let title: String
    let accessibilityLabel: String
    init(snapshot: QueueSnapshot?, error: String?) {
        let running = snapshot?.queue.running ?? 0
        let waiting = snapshot?.queue.waiting ?? 0
        title = error != nil ? "!" : (running == 0 && waiting == 0 ? "" : "\(running) · \(waiting)")
        accessibilityLabel = "Agent Queue, \(running) running, \(waiting) waiting" + (error == nil ? "" : ", queue unavailable")
    }
}

// Owns only startup presentation. It never initializes AppRuntime or exposes
// queue commands until the bootstrap controller has verified readiness.
@MainActor
final class QueueStartupMenuBarController: NSObject, NSMenuDelegate {
    let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let menu = NSMenu(title: "Agent Queue")
    private let model: QueueBootstrapModel
    private var observation: AnyCancellable?
    private var ownsStatusItem = true
    private(set) var isTornDown = false
    private let status = NSMenuItem(title: "Starting…", action: nil, keyEquivalent: "")
    private let detail = NSMenuItem()
    private var detailText: String?
    private let stopWaiting = NSMenuItem(title: "Stop waiting", action: #selector(stopWaitingForJobs), keyEquivalent: "")
    private let retry = NSMenuItem(title: "Retry", action: #selector(retryStartup), keyEquivalent: "")
    private let applications = NSMenuItem(title: "Open Applications", action: #selector(openApplications), keyEquivalent: "")
    private let download = NSMenuItem(title: "Download Agent Queue…", action: #selector(downloadRelease), keyEquivalent: "")

    init(model: QueueBootstrapModel) {
        self.model = model
        super.init()
        menu.autoenablesItems = false
        menu.delegate = self
        status.isEnabled = false
        menu.addItem(status)
        menu.addItem(detail)
        menu.addItem(.separator())
        for item in [stopWaiting, retry, applications, download] {
            item.target = self
            menu.addItem(item)
        }
        menu.addItem(.separator())
        let quit = NSMenuItem(title: "Quit Agent Queue", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        menu.addItem(quit)
        if let button = statusItem.button {
            button.image = NSImage(systemSymbolName: QueueBrand.symbolName, accessibilityDescription: nil)
            button.image?.isTemplate = true
            button.imagePosition = .imageLeading
            button.setAccessibilityIdentifier("agent-queue-status-item")
        }
        statusItem.menu = menu
        // @Published sends before changing the value. Render on the next main
        // actor turn so phase, output and the final approval hint agree.
        observation = model.objectWillChange.sink { [weak self] _ in
            Task { @MainActor [weak self] in self?.update() }
        }
        update()
    }

    func menuNeedsUpdate(_ menu: NSMenu) { update() }

    static let waitingLine = "Finishing running jobs before completing the update…"

    func update() {
        guard !isTornDown else { return }
        let blocked = !model.isRunning && model.phase != .complete && model.phase != .idle
        // A successful launch is one step for the user: setup wording and diagnostics
        // appear only once preparation has actually failed. The one exception is the
        // wait for running jobs, which names its reason in neutral words and can be stopped.
        let waiting = model.phase == .waiting
        status.title = blocked ? model.message : waiting ? Self.waitingLine : "Starting…"
        let summary = "Agent Queue, " + status.title
        statusItem.button?.title = blocked ? "!" : ""
        statusItem.button?.toolTip = summary
        statusItem.button?.setAccessibilityLabel(summary)
        // Keep the controller's bounded diagnostic readable from the menu,
        // including unknown failures. Scroll long output without opening a window.
        let text = blocked ? ([model.output] + model.issues.map { $0.title + "\n" + $0.detail })
            .filter { !$0.isEmpty }.joined(separator: "\n\n") : ""
        detail.isHidden = text.isEmpty
        if detailText != text {
            detailText = text
            let label = NSTextField(wrappingLabelWithString: text)
            label.font = .menuFont(ofSize: NSFont.smallSystemFontSize)
            label.textColor = .secondaryLabelColor
            label.frame.size.width = 340
            let height = max(24, label.sizeThatFits(NSSize(width: 340, height: CGFloat.greatestFiniteMagnitude)).height)
            label.frame = NSRect(x: 12, y: 8, width: 340, height: height)
            let content = NSView(frame: NSRect(x: 0, y: 0, width: 364, height: height + 16))
            content.addSubview(label)
            let scroll = NSScrollView(frame: NSRect(x: 0, y: 0, width: 364, height: min(240, height + 16)))
            scroll.drawsBackground = false
            scroll.hasVerticalScroller = height + 16 > 240
            scroll.documentView = content
            detail.view = scroll
        }
        stopWaiting.isHidden = !(waiting && model.canStopWaiting)
        stopWaiting.isEnabled = !stopWaiting.isHidden
        retry.isHidden = !blocked || !model.allowsInstallation
        applications.isHidden = !model.issues.contains { $0.actions.contains { $0.id == "move-app" } }
        download.isHidden = !blocked
        retry.isEnabled = !model.isRunning
    }

    @objc private func stopWaitingForJobs() { model.cancelWaiting() }
    @objc private func retryStartup() { model.start() }
    @objc private func openApplications() { model.showApplications() }
    @objc private func downloadRelease() { model.downloadRelease() }

    func transferStatusItem() -> NSStatusItem {
        ownsStatusItem = false
        tearDown()
        return statusItem
    }

    func tearDown() {
        guard !isTornDown else { return }
        isTornDown = true
        observation = nil
        menu.cancelTracking()
        menu.delegate = nil
        statusItem.menu = nil
        if ownsStatusItem { NSStatusBar.system.removeStatusItem(statusItem) }
    }
}

@MainActor
final class QueueMenuBarController: NSObject, NSPopoverDelegate {
    let statusItem: NSStatusItem
    let popover = NSPopover()
    let menu = NSMenu()
    private var observation: AnyCancellable?
    private var deactivationObservation: AnyCancellable?
    private var menuTrackingObservations: [AnyCancellable] = []
    private var menuTrackingDepth = 0
    private var eventMonitor: Any?
    private var globalMouseMonitor: Any?
    var isMonitoringEvents: Bool { eventMonitor != nil || globalMouseMonitor != nil }
    private var isTornDown = false
    private let store: QueueStore
    private var registeredVisibleSurface = false
    private let perform: @MainActor (QueueAppAction) -> Void

    init(store: QueueStore, statusItem: NSStatusItem? = nil, perform: @escaping @MainActor (QueueAppAction) -> Void = AppRuntime.perform) {
        self.statusItem = statusItem ?? NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        self.store = store
        self.perform = perform
        super.init()
        // Transient popovers consume Control-clicks on the status button while
        // shown. Own dismissal so every status-button gesture remains available.
        popover.behavior = .applicationDefined
        popover.delegate = self
        popover.contentSize = NSSize(width: 500, height: 650)
        popover.contentViewController = NSHostingController(rootView: QueuePanel(store: store, tracksVisibility: false))
        if let button = self.statusItem.button {
            button.image = NSImage(systemSymbolName: QueueBrand.symbolName, accessibilityDescription: nil)
            button.image?.isTemplate = true
            button.imagePosition = .imageLeading
            button.font = .monospacedDigitSystemFont(ofSize: NSFont.systemFontSize, weight: .regular)
            button.target = self
            button.action = #selector(activateStatusItem(_:))
            button.sendAction(on: [.leftMouseUp, .rightMouseUp])
            button.setAccessibilityIdentifier("agent-queue-status-item")
        }
        buildMenu()
        observation = store.snapshotPublisher.combineLatest(store.$error).sink { [weak self] snapshot, error in
            let presentation = QueueStatusPresentation(snapshot: snapshot, error: error)
            self?.statusItem.button?.title = presentation.title
            self?.statusItem.button?.setAccessibilityLabel(presentation.accessibilityLabel)
            self?.statusItem.button?.toolTip = presentation.accessibilityLabel
        }
        deactivationObservation = NotificationCenter.default.publisher(for: NSApplication.didResignActiveNotification)
            .sink { [weak self] _ in self?.closePopover() }
        menuTrackingObservations = [
            NotificationCenter.default.publisher(for: NSMenu.didBeginTrackingNotification)
                .sink { [weak self] _ in self?.menuTrackingDepth += 1 },
            NotificationCenter.default.publisher(for: NSMenu.didEndTrackingNotification)
                .sink { [weak self] _ in
                    guard let self else { return }
                    self.menuTrackingDepth = max(0, self.menuTrackingDepth - 1)
                }
        ]
    }

    private func buildMenu() {
        menu.autoenablesItems = false
        for action in QueueAppAction.allCases {
            if action == .learnedWorkloads { menu.addItem(.separator()) }
            if action == .quit {
                menu.addItem(.separator())
                let explanation = NSMenuItem(title: QueueAppAction.quitExplanation, action: nil, keyEquivalent: "")
                explanation.isEnabled = false
                menu.addItem(explanation)
                menu.addItem(.separator())
            }
            let item = NSMenuItem(title: action.title, action: #selector(selectMenuAction(_:)),
                                  keyEquivalent: action == .quit ? "q" : "")
            item.target = self
            item.tag = action.rawValue
            item.image = NSImage(systemSymbolName: action.symbol, accessibilityDescription: nil)
            menu.addItem(item)
        }
    }

    @objc private func activateStatusItem(_ sender: NSStatusBarButton) {
        activate(.from(NSApp.currentEvent))
    }

    func activate(_ activation: QueueStatusActivation) {
        guard !isTornDown else { return }
        switch activation {
        case .primaryClick, .accessible: togglePopover()
        case .doubleClick: performAction(.openWindow)
        case .contextMenu: showContextMenu()
        }
    }

    private func handleLocalEvent(type: NSEvent.EventType, keyCode: UInt16, windowNumber: Int) -> Bool {
        // Native and SwiftUI menus may track in separate windows. Let their
        // selection/dismissal finish without destroying the originating popover.
        guard menuTrackingDepth == 0 else { return false }
        if type == .keyDown {
            if keyCode == 53 && popover.isShown { closePopover(); return true }
        } else if windowNumber != statusItem.button?.window?.windowNumber {
            if popover.isShown && windowNumber != popover.contentViewController?.view.window?.windowNumber { closePopover() }
        }
        return false
    }

    private func startMonitoringEvents() {
        guard eventMonitor == nil else { return }
        eventMonitor = NSEvent.addLocalMonitorForEvents(matching: [.leftMouseDown, .rightMouseDown, .otherMouseDown, .keyDown]) { [weak self] event in
            let type = event.type
            let keyCode = type == .keyDown ? event.keyCode : 0
            let windowNumber = event.windowNumber
            let suppress = MainActor.assumeIsolated {
                self?.handleLocalEvent(type: type, keyCode: keyCode, windowNumber: windowNumber) ?? false
            }
            return suppress ? nil : event
        }
        // An accessory app can show its popover without becoming active. In
        // that state an outside click produces no local event or deactivation.
        globalMouseMonitor = NSEvent.addGlobalMonitorForEvents(matching: [.leftMouseDown, .rightMouseDown, .otherMouseDown]) { [weak self] _ in
            MainActor.assumeIsolated {
                guard let self, self.menuTrackingDepth == 0 else { return }
                self.closePopover()
            }
        }
    }

    private func stopMonitoringEvents() {
        if let eventMonitor { NSEvent.removeMonitor(eventMonitor); self.eventMonitor = nil }
        if let globalMouseMonitor { NSEvent.removeMonitor(globalMouseMonitor); self.globalMouseMonitor = nil }
    }

    @objc private func selectMenuAction(_ sender: NSMenuItem) {
        guard let action = QueueAppAction(rawValue: sender.tag) else { return }
        performAction(action)
    }

    private func performAction(_ action: QueueAppAction) {
        closePopover()
        perform(action)
    }

    private func togglePopover() {
        if popover.isShown { closePopover(); return }
        guard let button = statusItem.button else { return }
        NSApp.activate()
        popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
        popover.contentViewController?.view.window?.makeKey()
    }

    private func showContextMenu() {
        closePopover()
        guard let button = statusItem.button else { return }
        menu.popUp(positioning: nil, at: NSPoint(x: button.bounds.minX, y: button.bounds.minY), in: button)
    }

    func closePopover() {
        popover.performClose(nil)
        releaseVisibleSurface()
    }

    func popoverWillShow(_ notification: Notification) {
        guard !registeredVisibleSurface else { return }
        registeredVisibleSurface = true
        startMonitoringEvents()
        store.appear(fullWindow: false)
    }

    func popoverDidClose(_ notification: Notification) {
        if !popover.isShown { releaseVisibleSurface() }
    }

    private func releaseVisibleSurface() {
        stopMonitoringEvents()
        guard registeredVisibleSurface else { return }
        registeredVisibleSurface = false
        store.disappear(fullWindow: false)
    }

    func tearDown() {
        guard !isTornDown else { return }
        isTornDown = true
        closePopover()
        observation = nil
        deactivationObservation = nil
        menuTrackingObservations = []
        NSStatusBar.system.removeStatusItem(statusItem)
    }
}

enum ConfirmationAction: String, CaseIterable {
    case start, cancel
}

struct QueueConfirmation {
    let action: ConfirmationAction
    let job: QueueJob
}

@MainActor
final class QueueNavigation: ObservableObject {
    @Published var selected: QueueJob?
    @Published var confirmation: QueueConfirmation?
    func show(_ job: QueueJob) { selected = job }
    func back() { selected = nil }
    func request(_ action: ConfirmationAction, job: QueueJob) {
        selected = nil
        confirmation = QueueConfirmation(action: action, job: job)
    }
    func decline() { confirmation = nil }

    func unavailability(in store: QueueStore) -> String? {
        guard let confirmation else { return "No action is awaiting confirmation." }
        guard let current = store.snapshot?.jobs.first(where: { $0.id == confirmation.job.id }) else {
            return "This job is no longer in the current queue. Return to the queue to see its latest status."
        }
        switch confirmation.action {
        case .start:
            guard current.permitsStart else {
                return "This job cannot be started from its current state. Return to the queue to see its latest status."
            }
        case .cancel:
            guard current.canCancel else {
                return "This job is no longer available to cancel."
            }
        }
        return nil
    }

    @discardableResult
    func confirm(in store: QueueStore) -> Task<Void, Never>? {
        guard !store.actionsDisabled, unavailability(in: store) == nil,
              let confirmation,
              let current = store.snapshot?.jobs.first(where: { $0.id == confirmation.job.id }) else { return nil }
        guard let dispatch = store.dispatch(confirmation.action.rawValue, job: current) else { return nil }
        // Reservation and consumption both finish before yielding the main actor.
        self.confirmation = nil
        return dispatch
    }
}

struct QueuePanel: View {
    @ObservedObject var store: QueueStore
    var fullWindow = false
    var tracksVisibility = true
    @StateObject private var navigation: QueueNavigation
    @State private var draftLimit = "1"
    @State private var draftMode = SchedulingMode.fixed

    init(store: QueueStore, fullWindow: Bool = false, tracksVisibility: Bool = true, navigation: QueueNavigation = QueueNavigation()) {
        self.store = store
        self.fullWindow = fullWindow
        self.tracksVisibility = tracksVisibility
        _navigation = StateObject(wrappedValue: navigation)
    }

    var body: some View {
        VStack(spacing: 0) {
            if let confirmation = navigation.confirmation {
                let currentJob = store.snapshot?.jobs.first(where: { $0.id == confirmation.job.id }) ?? confirmation.job
                JobConfirmationPanel(action: confirmation.action, job: currentJob,
                    unavailable: navigation.unavailability(in: store), error: store.error,
                    disabled: store.actionsDisabled, onDecline: navigation.decline,
                    onConfirm: { navigation.confirm(in: store) })
            } else if let job = navigation.selected {
                let currentJob = store.snapshot?.jobs.first(where: { $0.id == job.id }) ?? job
                JobDetail(job: currentJob, fixtureMode: store.fixtureMode, onClose: navigation.back)
            } else {
                header
                Divider()
                if let error = store.error {
                    HStack(alignment: .top, spacing: 9) {
                        Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                        VStack(alignment: .leading, spacing: 3) {
                            Text(store.snapshot == nil ? "Queue unavailable" : "Connection interrupted").font(.headline)
                            Text(error).font(.caption).textSelection(.enabled)
                            if store.snapshot != nil { Text("Showing the last received queue. Actions are disabled.").font(.caption).foregroundStyle(.secondary) }
                        }
                        Spacer(minLength: 0)
                    }.padding(14).background(.orange.opacity(0.08))
                }
                if store.snapshot == nil {
                    Spacer()
                    ContentUnavailableView(store.isRefreshing ? "Connecting to queue" : "Cannot load the queue",
                        systemImage: store.isRefreshing ? "arrow.triangle.2.circlepath" : "network.slash",
                        description: Text("Use Refresh to reconnect to agent-heavy."))
                    Spacer()
                } else {
                    queueList
                }
                Divider()
                footer
            }
        }
        .frame(minWidth: 470, minHeight: 440)
        .frame(width: fullWindow ? nil : 500, height: fullWindow ? nil : 650)
        .background(Color(nsColor: .windowBackgroundColor))
        .onAppear {
            draftLimit = String(store.snapshot?.queue.limit ?? 1)
            draftMode = store.snapshot?.scheduler?.mode ?? .fixed
            if tracksVisibility { store.appear(fullWindow: fullWindow) }
        }
        .onChange(of: store.snapshot?.queue.limit) { _, value in
            if let value { draftLimit = String(value) }
        }
        .onChange(of: store.snapshot?.scheduler?.mode) { _, value in
            if let value { draftMode = value }
        }
        .onDisappear { if tracksVisibility { store.disappear(fullWindow: fullWindow) } }
    }

    private func applyLimit() {
        guard let value = Int(draftLimit), (1...8).contains(value) else { return }
        store.dispatchLimit(value)
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 11) {
            HStack {
                Label("Agent Queue", systemImage: QueueBrand.symbolName).font(.system(size: 20, weight: .semibold))
                Spacer()
                if store.fixtureMode { Text("PREVIEW").font(.caption2.weight(.semibold)).foregroundStyle(.secondary) }
                Button { Task { await store.refresh(manual: true) } } label: {
                    Image(systemName: "arrow.clockwise").frame(width: 20, height: 20)
                }.buttonStyle(.borderless).help("Refresh queue (Command-R)")
                    .accessibilityLabel("Refresh queue").keyboardShortcut("r")
                    .disabled(store.isRefreshing || store.isMutating || store.fixtureMode)
            }
            HStack(spacing: 16) {
                Label("\(store.snapshot?.queue.running ?? 0) running", systemImage: "play.fill").foregroundStyle(.green)
                Label("\(store.snapshot?.queue.waiting ?? 0) waiting", systemImage: "clock").foregroundStyle(.secondary)
                Spacer()
                Text("\(store.snapshot?.queue.limit ?? 1) at a time").foregroundStyle(.secondary)
            }.font(.system(size: 12, weight: .medium))
            HStack(spacing: 8) {
                Text("Parallel jobs").font(.caption.weight(.medium))
                TextField("Limit", text: $draftLimit)
                    .textFieldStyle(.roundedBorder).frame(width: 48)
                    .multilineTextAlignment(.center).font(.caption.monospacedDigit())
                    .accessibilityLabel("Parallel job limit").onSubmit { applyLimit() }
                Stepper("Parallel job limit", onIncrement: {
                    let current = min(8, max(1, Int(draftLimit) ?? store.snapshot?.queue.limit ?? 1))
                    draftLimit = String(min(8, current + 1))
                }, onDecrement: {
                    let current = min(8, max(1, Int(draftLimit) ?? store.snapshot?.queue.limit ?? 1))
                    draftLimit = String(current > 1 ? current - 1 : 1)
                }).labelsHidden().fixedSize()
                Button("Apply") { applyLimit() }
                    .disabled(store.actionsDisabled || !(1...8).contains(Int(draftLimit) ?? 0) || Int(draftLimit) == store.snapshot?.queue.limit)
                Spacer(minLength: 0)
            }.disabled(store.fixtureMode || store.isMutating || store.snapshot == nil || store.error != nil)
            Text("Choose 1 to 8 jobs. Lowering the limit lets active jobs finish.").font(.caption2).foregroundStyle(.secondary)
            if let scheduler = store.snapshot?.scheduler {
                HStack(spacing: 8) {
                    Picker("Scheduling", selection: $draftMode) {
                        ForEach(SchedulingMode.allCases, id: \.self) { mode in Text(mode.label).tag(mode) }
                    }.pickerStyle(.menu)
                    Button("Set mode") { store.dispatchScheduling(draftMode) }
                        .disabled(store.actionsDisabled || scheduler.mode == draftMode)
                }.disabled(store.fixtureMode || store.actionsDisabled)
                Text("Adaptive fits jobs within your parallel limit. Learn only records predictions. Running jobs are never stopped automatically.")
                    .font(.caption2).foregroundStyle(.secondary)
                Text(scheduler.summary).font(.caption2).foregroundStyle(.secondary)
                if !scheduler.reason.isEmpty { Text(scheduler.reason).font(.caption2).foregroundStyle(.secondary) }
                Text(scheduler.host?.summary ?? "Host measurements unavailable.")
                    .font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            } else {
                Text("Resource monitoring unavailable. Update the queue controller to enable scheduling controls.")
                    .font(.caption2).foregroundStyle(.secondary)
            }
        }.padding(18)
    }

    private var queueList: some View {
        List {
            if store.isPaused {
                HStack(spacing: 12) {
                    Label("Queue paused. Waiting jobs will not start automatically.", systemImage: "pause.circle")
                        .foregroundStyle(.orange)
                    Spacer(minLength: 0)
                    Button(store.isResuming ? "Resuming…" : "Resume", systemImage: "play.fill") {
                        store.dispatchResume()
                    }
                    .buttonStyle(.bordered)
                    .fixedSize()
                    .disabled(store.actionsDisabled)
                    .accessibilityLabel("Resume queue")
                    .accessibilityIdentifier("queue.resume")
                    .help("Resume automatic starts for waiting jobs.")
                }.font(.callout).padding(.vertical, 5)
            }
            if store.snapshot?.queue.legacyOccupied == true {
                Label("A legacy validation batch holds the shared slot.", systemImage: "hourglass")
                    .font(.callout).foregroundStyle(.secondary).padding(.vertical, 5)
            }
            Section {
                if store.running.isEmpty {
                    HStack(spacing: 10) {
                        Image(systemName: store.isPaused ? "pause.circle" : store.waiting.isEmpty ? "checkmark.circle" : "hourglass")
                            .foregroundStyle(.secondary)
                        VStack(alignment: .leading, spacing: 3) {
                            Text(store.isPaused ? "Queue paused" : store.waiting.isEmpty ? "Ready for the next command" : "Waiting for the shared slot").font(.callout)
                            Text(store.isPaused ? "Resume the queue to start jobs automatically."
                                 : store.waiting.isEmpty ? "Heavy jobs appear here when agents submit them." : "Queued jobs start automatically when the slot is available.")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                    }.padding(.vertical, 10)
                }
                ForEach(store.running) { job in row(job) }
            } header: { sectionHeader("Running", count: store.running.count) }
            Section {
                if store.waiting.isEmpty {
                    Text("No waiting jobs").font(.callout).foregroundStyle(.secondary).padding(.vertical, 8)
                }
                ForEach(store.waiting) { job in row(job) }
            } header: { sectionHeader("Up next", count: store.waiting.count) }
            if !store.recent.isEmpty {
                Section {
                    ForEach(store.recent) { job in row(job) }
                } header: { sectionHeader("Recent", count: store.recent.count) }
            }
        }.listStyle(.inset).scrollContentBackground(.hidden)
    }

    private func sectionHeader(_ title: String, count: Int) -> some View {
        HStack { Text(title); Spacer(); Text(String(count)).monospacedDigit() }.font(.caption.weight(.semibold))
    }

    private func row(_ job: QueueJob) -> some View {
        HStack(alignment: .top, spacing: 11) {
            Group {
                if let position = job.queuePosition, job.isWaiting {
                    Text(String(position)).font(.system(size: 13, weight: .semibold, design: .rounded)).foregroundStyle(.secondary)
                } else {
                    Image(systemName: job.isRunning ? "play.fill" : (job.state == "failed" || (job.exitCode ?? 0) != 0 ? "exclamationmark.circle" : "checkmark.circle"))
                        .font(.system(size: 14)).foregroundStyle(job.color)
                }
            }.frame(width: 22, height: 23)
            VStack(alignment: .leading, spacing: 5) {
                Button { navigation.show(job) } label: {
                    Text(job.name).font(.system(size: 13, weight: .semibold)).foregroundStyle(.primary)
                        .multilineTextAlignment(.leading).lineLimit(3).frame(maxWidth: .infinity, alignment: .leading)
                }.buttonStyle(.plain).help("View details and output for job #\(job.id)")
                if !job.context.isEmpty { Text(job.context).font(.caption).foregroundStyle(.secondary).lineLimit(2) }
                HStack(spacing: 7) {
                    Text("#\(job.id)").monospacedDigit()
                    Text("·")
                    Text(job.stateLabel).foregroundStyle(job.color)
                    if job.isRunning, let startedAt = job.startedAt, let date = parseDate(startedAt) {
                        Text("·")
                        Text(date, style: .relative).monospacedDigit()
                    }
                }.font(.caption2).foregroundStyle(.secondary)
                if let phase = job.distinctPhase {
                    Text(phase)
                        .font(.caption2).foregroundStyle(.secondary).lineLimit(2)
                }
                if let resources = job.resources {
                    Text(resources.prediction.summary).font(.caption2).foregroundStyle(.secondary)
                    if let observed = resources.observed { Text(observed.summary).font(.caption2).foregroundStyle(.secondary) }
                    if job.isWaiting, let decision = resources.decision {
                        Text(decision.reason).font(.caption2).foregroundStyle(.secondary)
                    }
                }
                if job.canReorder || job.permitsStart {
                    HStack(spacing: 8) {
                        if job.canReorder {
                        Button("Run next", systemImage: "arrow.up.to.line") { store.dispatch("next", job: job) }
                            .help("Run job #\(job.id) after the current job finishes")
                        Button("Move to end", systemImage: "arrow.down.to.line") { store.dispatch("last", job: job) }
                        }
                        if job.permitsStart {
                        Button("Start now", systemImage: "play.fill") { navigation.request(.start, job: job) }
                            .help("Bypass the parallel limit and adaptive resource checks")
                        }
                    }.font(.caption).buttonStyle(.borderless).disabled(store.actionsDisabled)
                }
            }
            Menu {
                Button("View details and output", systemImage: "doc.text") { navigation.show(job) }
                if let learning = job.resources?.learning {
                    Button("View learned workload", systemImage: "chart.bar.doc.horizontal") {
                        AppRuntime.showLearningWindow(profileID: learning.id)
                    }
                }
                if job.canReorder || job.permitsStart {
                    Divider()
                    if job.canReorder {
                    Button("Run next", systemImage: "arrow.up.to.line") { store.dispatch("next", job: job) }.disabled(store.actionsDisabled)
                    Button("Move to end", systemImage: "arrow.down.to.line") { store.dispatch("last", job: job) }.disabled(store.actionsDisabled)
                    }
                    if job.permitsStart {
                    Button("Start now…", systemImage: "play.fill") { navigation.request(.start, job: job) }.disabled(store.actionsDisabled)
                    }
                }
                if job.canCancel {
                    Divider()
                    Button("Cancel job…", systemImage: "stop.circle", role: .destructive) { navigation.request(.cancel, job: job) }.disabled(store.actionsDisabled)
                }
            } label: { Image(systemName: "ellipsis").frame(width: 22, height: 22) }
                .menuStyle(.borderlessButton).fixedSize().help("Actions for job #\(job.id)")
                .accessibilityLabel("Actions for job \(job.id)")
        }.padding(.vertical, 9)
    }

    private var footer: some View {
        VStack(alignment: .leading, spacing: 9) {
            if let notice = store.notice { Label(notice, systemImage: "checkmark.circle").font(.caption).foregroundStyle(.secondary) }
            HStack(spacing: 8) {
                Circle().fill(store.error != nil ? Color.orange : Color.green).frame(width: 6, height: 6)
                if store.isMutating { Text("Updating queue…") }
                else if let updated = store.lastRefreshed { Text("Updated \(updated.formatted(date: .omitted, time: .shortened))") }
                else { Text("Connecting…") }
                Spacer()
                if !fullWindow { Button(QueueAppAction.openWindow.title, systemImage: QueueAppAction.openWindow.symbol) { AppRuntime.perform(.openWindow) }.buttonStyle(.borderless) }
                Menu {
                    ForEach(QueueAppAction.settings) { action in
                        Button(action.title, systemImage: action.symbol) { AppRuntime.perform(action) }
                    }
                    Divider()
                    Text(QueueAppAction.quitExplanation)
                    Divider()
                    Button(QueueAppAction.quit.title, systemImage: QueueAppAction.quit.symbol) { AppRuntime.perform(.quit) }.keyboardShortcut("q")
                } label: { Image(systemName: "gearshape") }.menuStyle(.borderlessButton).allowsWindowActivationEvents(true).fixedSize().help("Application options").accessibilityLabel("Application options")
            }.font(.caption).foregroundStyle(.secondary)
        }.padding(.horizontal, 18).padding(.vertical, 12)
    }
}

struct RemoteAccessSettings: View {
    @ObservedObject var store: RemoteAccessStore
    @State private var password = ""
    @State private var copied = false

    private var canSave: Bool {
        !store.fixtureMode && !store.isBusy && !store.isRefreshing && RemoteAccessStore.acceptsPassword(password)
    }
    private var remoteURL: URL? {
        guard let value = store.status?.url, let url = URL(string: value), url.scheme == "https", url.host != nil else { return nil }
        return url
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    Label("Remote access", systemImage: "network").font(.title2.weight(.semibold))
                    Spacer()
                    if store.fixtureMode { Text("PREVIEW").font(.caption2.weight(.semibold)).foregroundStyle(.secondary) }
                    Button { Task { await store.refresh() } } label: { Image(systemName: "arrow.clockwise") }
                        .buttonStyle(.borderless).help("Refresh remote access status").accessibilityLabel("Refresh remote access status")
                        .disabled(store.fixtureMode || store.isBusy || store.isRefreshing)
                }
                Text("Open the queue from your phone over your home network or existing VPN.")
                    .font(.callout).foregroundStyle(.secondary)
            }.padding(22)
            Divider()
            Form {
                Section {
                    Toggle("Remote access", isOn: Binding(
                        get: { store.status?.enabled ?? false },
                        set: { store.setEnabled($0) }))
                        .disabled(store.fixtureMode || store.isBusy || store.isRefreshing
                            || (store.status?.configured != true && store.status?.enabled != true))
                    HStack(spacing: 7) {
                        Circle().fill(statusColor).frame(width: 7, height: 7)
                        Text(store.statusLabel).font(.caption).foregroundStyle(.secondary)
                    }
                    if store.status?.configured != true {
                        Text("Set a password below to enable access. Remote access starts turned off.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    if let error = store.effectiveError {
                        Label(error, systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.orange)
                            .textSelection(.enabled)
                    }
                    if let notice = store.notice, !store.needsAttention { Label(notice, systemImage: "checkmark.circle").font(.caption).foregroundStyle(.secondary) }
                }
                Section("Phone sign-in") {
                    LabeledContent("Username", value: store.status?.username ?? "agent")
                    SecureField("New password", text: $password).textContentType(.newPassword)
                        .disabled(store.fixtureMode || store.isBusy)
                    Text("Use at least 4 characters. No special characters required.")
                        .font(.caption).foregroundStyle(.secondary)
                    HStack {
                        Spacer()
                        Button(store.status?.enabled == true ? "Update password" : "Save password and enable") {
                            let submittedPassword = password
                            if store.configure(password: submittedPassword, enable: store.status?.enabled != true) {
                                password = ""
                            }
                        }.buttonStyle(.borderedProminent).disabled(!canSave)
                    }
                }
                Section("Connect from your phone") {
                    if let remoteURL {
                        Text(remoteURL.absoluteString).font(.system(.callout, design: .monospaced)).textSelection(.enabled)
                        HStack {
                            Button(copied ? "Copied" : "Copy address", systemImage: "doc.on.doc") {
                                NSPasteboard.general.clearContents()
                                NSPasteboard.general.setString(remoteURL.absoluteString, forType: .string)
                                copied = true
                            }.disabled(store.fixtureMode)
                            Button("Open", systemImage: "arrow.up.forward.square") { NSWorkspace.shared.open(remoteURL) }
                                .disabled(store.fixtureMode || store.status?.running != true)
                        }
                    } else { Text("The phone address appears after setup.").font(.caption).foregroundStyle(.secondary) }
                    VStack(alignment: .leading, spacing: 14) {
                        instruction(1, "Transfer the certificate to Android", "Send public root.crt to Android over USB or your preferred file transfer. Use Share certificate or Show in Finder below.")
                        if let path = store.status?.certificatePath, !path.isEmpty {
                            HStack {
                                ShareLink(item: URL(fileURLWithPath: path)) { Label("Share certificate", systemImage: "square.and.arrow.up") }
                                Button("Show in Finder", systemImage: "folder") {
                                    NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
                                }
                            }.disabled(store.fixtureMode)
                        }
                        instruction(2, "Install it as a CA certificate", "Go to Settings > Security & privacy > More security settings > Encryption & credentials > Install a certificate > CA certificate. Select root.crt and confirm your PIN. Labels vary by device; search Settings for CA certificate. Choose CA certificate for HTTPS.")
                        instruction(3, "Open the address and sign in", "Open the address above in Chrome or your browser. Sign in as agent with your saved password.")
                    }
                    Text("Your existing VPN must route to the home LAN. Use the same address while connected to that VPN.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }.formStyle(.grouped)
        }.frame(minWidth: 480, minHeight: 540)
            .background(Color(nsColor: .windowBackgroundColor))
            .onDisappear { password = "" }
    }

    private var statusColor: Color {
        if store.needsAttention { return .orange }
        if store.status?.running == true { return .green }
        return store.status?.enabled == true ? .orange : .secondary
    }
    private func instruction(_ number: Int, _ title: String, _ explanation: String) -> some View {
        HStack(alignment: .top, spacing: 10) {
            Text(String(number)).font(.caption.weight(.semibold)).foregroundStyle(.secondary).frame(width: 16)
            VStack(alignment: .leading, spacing: 4) {
                Text(title).font(.callout.weight(.medium))
                Text(explanation).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
        }
    }
}

struct JobConfirmationPanel: View {
    let action: ConfirmationAction
    let job: QueueJob
    let unavailable: String?
    let error: String?
    let disabled: Bool
    let onDecline: () -> Void
    let onConfirm: () -> Void

    private var title: String { action == .start ? "Start this job now?" : "Cancel this job?" }
    private var confirmLabel: String { action == .start ? "Start now" : "Cancel job" }
    private var declineLabel: String { action == .start ? "Keep waiting" : "Keep job" }
    private var explanation: String {
        if action == .start {
            return "This bypasses the parallel limit and adaptive resource checks and may increase CPU and memory pressure. Other queued jobs keep their order."
        }
        return job.isRunning || job.state == "paused"
            ? "This stops the running command and its managed child processes. Work already written to disk remains."
            : "This removes the waiting command before it starts."
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Button("Queue", systemImage: "chevron.left", action: onDecline).buttonStyle(.borderless)
                Spacer()
                Text("Review action").font(.caption).foregroundStyle(.secondary)
            }.padding(18)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    Image(systemName: action == .start ? "play.circle" : "stop.circle")
                        .font(.system(size: 34, weight: .regular)).foregroundStyle(action == .start ? Color.orange : Color.red)
                        .accessibilityHidden(true)
                    Text(title).font(.title2.weight(.semibold))
                    VStack(alignment: .leading, spacing: 7) {
                        Text(job.name).font(.headline).textSelection(.enabled)
                        if !job.context.isEmpty { Text(job.context).font(.callout).foregroundStyle(.secondary) }
                        Text("Job #\(job.id) · \(job.stateLabel)").font(.caption).foregroundStyle(job.color)
                    }
                    Text(explanation).font(.callout).fixedSize(horizontal: false, vertical: true)
                    if let unavailable {
                        Label(unavailable, systemImage: "info.circle").font(.callout).foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    if let error {
                        Label(error, systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.orange)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }.frame(maxWidth: .infinity, alignment: .leading).padding(24)
            }.frame(maxWidth: .infinity, maxHeight: .infinity)
            Divider()
            HStack(spacing: 10) {
                Spacer()
                Button(declineLabel, action: onDecline)
                Button(confirmLabel, action: onConfirm).buttonStyle(.borderedProminent)
                    .tint(action == .start ? Color.accentColor : Color.red)
                    .disabled(disabled || unavailable != nil)
                    .accessibilityIdentifier("confirm-\(action.rawValue)-job")
            }.padding(18)
        }.frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

func parseDate(_ value: String) -> Date? {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = formatter.date(from: value) { return date }
    formatter.formatOptions = [.withInternetDateTime]
    return formatter.date(from: value)
}

struct JobDetail: View {
    let job: QueueJob
    let fixtureMode: Bool
    let onClose: () -> Void
    @State private var output = "Loading output…"
    @State private var loading = false

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack(alignment: .top) {
                VStack(alignment: .leading, spacing: 5) {
                    Text(job.name).font(.title3.weight(.semibold)).textSelection(.enabled).lineLimit(3)
                    Text("Job #\(job.id) · \(job.stateLabel)").font(.caption).foregroundStyle(job.color)
                }
                Spacer()
                Button("Done", action: onClose).keyboardShortcut(.defaultAction)
                    .accessibilityIdentifier("job-detail-done")
            }
            ScrollView {
            Grid(alignment: .leading, horizontalSpacing: 14, verticalSpacing: 7) {
                detail("Project", job.project)
                detail("Branch", job.branch)
                detail("Directory", job.directory)
                detail("Executable", job.executable)
                if !job.phase.isEmpty { detail("Phase", job.phase) }
                if let value = job.submittedAt { detail("Submitted", formatted(value)) }
                if let value = job.startedAt { detail("Started", formatted(value)) }
                if let value = job.finishedAt { detail("Finished", formatted(value)) }
                if let resources = job.resources {
                    detail("Prediction", resources.prediction.summary)
                    if let duration = resources.prediction.durationSeconds { detail("Expected duration", "\(Int(duration.rounded())) seconds") }
                    detail("Measured usage", resources.observed?.summary ?? "No measurements available.")
                    if let observed = resources.observed {
                        detail("CPU cost", "\(observed.cpuSeconds.formatted(.number.precision(.fractionLength(1)))) CPU-seconds over \(observed.durationSeconds.formatted(.number.precision(.fractionLength(1)))) seconds (\(observed.samples) samples)")
                    }
                    if let decision = resources.decision { detail("Admission", decision.reason) }
                    if let learning = resources.learning {
                        GridRow {
                            Text("Learning").foregroundStyle(.secondary)
                            VStack(alignment: .leading, spacing: 5) {
                                Button(learning.label) { AppRuntime.showLearningWindow(profileID: learning.id) }
                                    .buttonStyle(.link)
                                Text(learning.reason).foregroundStyle(.secondary).textSelection(.enabled)
                            }.frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                } else { detail("Resources", "No measurements or predictions available.") }
            }.font(.caption).frame(maxWidth: .infinity, alignment: .leading)
            }.frame(maxHeight: 200)
            Divider()
            HStack {
                Text("Output").font(.headline)
                Text("Latest 64 KB").font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button("Refresh", systemImage: "arrow.clockwise") { Task { await loadLog() } }.disabled(loading || fixtureMode)
                Button("Open log", systemImage: "arrow.up.forward.square") {
                    NSWorkspace.shared.open(URL(fileURLWithPath: job.logPath))
                }.disabled(fixtureMode || job.logPath.isEmpty)
            }
            ScrollView([.horizontal, .vertical]) {
                Text(output).font(.system(size: 11, design: .monospaced)).textSelection(.enabled)
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading).padding(12)
            }.frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Color(nsColor: .textBackgroundColor)).clipShape(RoundedRectangle(cornerRadius: 6))
        }.padding(18).frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .task { await loadLog() }
    }

    private func detail(_ label: String, _ value: String) -> some View {
        GridRow {
            Text(label).foregroundStyle(.secondary)
            Text(value.isEmpty ? "Not provided" : value).textSelection(.enabled).lineLimit(3)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
    private func formatted(_ value: String) -> String { parseDate(value)?.formatted(date: .abbreviated, time: .standard) ?? value }
    private func loadLog() async {
        guard !fixtureMode else { output = "Log access is disabled in preview mode."; return }
        guard !job.logPath.isEmpty else { output = "This job has no output log."; return }
        loading = true
        defer { loading = false }
        do { output = try await QueueClient.logTail(job.logPath) }
        catch { output = "Cannot read output: \(error.localizedDescription)" }
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private(set) var bootstrap: QueueBootstrapModel?
    private(set) var setupWindow: NSWindow?
    private(set) var startupMenuBarController: QueueStartupMenuBarController?
    private(set) var supervisor: QueueHelperSupervisor?
    private var helperStartup: Task<Void, Never>?
    private var terminationPending = false
    private var normalLaunchStarted = false
    private var bootstrapEnvironment: QueueBootstrapEnvironment?
    private var bootstrapExecutor: QueueBootstrapModel.Executor = QueueBootstrapModel.runInstaller
    private var replyToTermination: @MainActor () -> Void = { NSApp.reply(toApplicationShouldTerminate: true) }
    private var makeSupervisor: @MainActor (QueueBootstrapEnvironment) -> QueueHelperSupervisor? = {
        $0.makeSupervisor(fixtureMode: AppRuntime.store.fixtureMode)
    }

    override init() { super.init() }

    // The separate graphical fixture supplies a private bundle/home and an inert
    // executor. Normal launch still follows the production path with --fixture.
    // Fixtures never supervise helpers unless a test injects a fake supervisor.
    init(environment: QueueBootstrapEnvironment,
         terminationReply: @escaping @MainActor () -> Void = { NSApp.reply(toApplicationShouldTerminate: true) },
         supervisor: QueueHelperSupervisor? = nil,
         executor: @escaping QueueBootstrapModel.Executor) {
        bootstrapEnvironment = environment
        bootstrapExecutor = executor
        replyToTermination = terminationReply
        makeSupervisor = { _ in supervisor }
        super.init()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        let environment = bootstrapEnvironment ?? .current()
        do { try environment.validateDiagnostics() }
        catch { diagnostic(error.localizedDescription); exit(EXIT_FAILURE) }
        let bootstrap = QueueBootstrapModel(environment: environment, execute: bootstrapExecutor) { [weak self] in
            guard let self else { return }
            self.setupWindow?.close()
            self.setupWindow = nil
            self.finishLaunching()
        }
        self.bootstrap = bootstrap
        // Present the menu before inspection starts, without touching queue state.
        startupMenuBarController = QueueStartupMenuBarController(model: bootstrap)
        let main = NSMenu()
        let application = NSMenuItem()
        let menu = NSMenu(title: "Agent Queue")
        menu.addItem(withTitle: "Quit Agent Queue", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        application.submenu = menu
        main.addItem(application)
        NSApp.mainMenu = main
        bootstrap.onSetupRequired = { [weak self] in
            if environment.arguments.contains("--setup") { self?.showSetupWindow() }
        }
        bootstrap.start()
    }

    private func showSetupWindow() {
        guard (bootstrapEnvironment ?? .current()).arguments.contains("--setup"),
              setupWindow == nil, let bootstrap else { return }
        let main = NSMenu()
        let application = NSMenuItem()
        let menu = NSMenu(title: "Agent Queue")
        menu.addItem(withTitle: "Quit Agent Queue", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        application.submenu = menu
        main.addItem(application)
        NSApp.mainMenu = main
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 560, height: 410),
            styleMask: [.titled, .miniaturizable], backing: .buffered, defer: false)
        window.title = "Set up Agent Queue"
        window.isReleasedWhenClosed = false
        window.contentView = NSHostingView(rootView: QueueBootstrapView(model: bootstrap))
        setupWindow = window
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate()
    }

    private func finishLaunching() {
        guard !normalLaunchStarted else { return }
        normalLaunchStarted = true
        NSApp.mainMenu = makeMainMenu()
        let statusItem = startupMenuBarController?.transferStatusItem()
        startupMenuBarController = nil
        AppRuntime.menuBarController = QueueMenuBarController(store: AppRuntime.store, statusItem: statusItem)
        if let helpers = makeSupervisor(bootstrapEnvironment ?? .current()) { startHelpers(helpers) }
        else { AppRuntime.store.start() }
        #if AGENT_QUEUE_TEST_HOOKS
        if runSnapshotHooks() { return }
        #endif
        if CommandLine.arguments.contains("--open-window"), supervisor == nil { AppRuntime.showWindow() }
    }

    /// Bootstrap has completed, so setup is no longer running: nothing here can race the installer.
    private func startHelpers(_ helpers: QueueHelperSupervisor) {
        supervisor = helpers
        AppRuntime.remoteStore.supervisor = helpers
        // The phone service is always started: it exits at once while phone access is off.
        helpers.startAll()
        helperStartup = Task { @MainActor in
            // Let the daemon come up first so the menu does not flash "Queue unavailable".
            _ = await helpers.waitUntilQueueReachable(timeout: .seconds(5))
            guard !Task.isCancelled else { return }
            AppRuntime.store.start()
            if CommandLine.arguments.contains("--open-window") { AppRuntime.showWindow() }
        }
    }

    func applicationWillTerminate(_ notification: Notification) {
        helperStartup?.cancel()
        supervisor?.terminateRemaining()
        startupMenuBarController?.tearDown()
        startupMenuBarController = nil
        AppRuntime.menuBarController?.tearDown()
        AppRuntime.menuBarController = nil
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        let setupRunning = bootstrap?.isRunning == true
        guard setupRunning || supervisor != nil else { return .terminateNow }
        if terminationPending { return .terminateLater }
        var setupStop: Task<Void, Never>?
        if setupRunning, let bootstrap {
            guard let stopped = bootstrap.stopForTermination() else {
                // Explicit diagnostic mutations retain their existing guard.
                showSetupWindow()
                return .terminateCancel
            }
            setupStop = stopped
        }
        terminationPending = true
        helperStartup?.cancel()
        let helpers = supervisor
        Task { @MainActor in
            await setupStop?.value
            await helpers?.stopAll()
            self.replyToTermination()
        }
        return .terminateLater
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        guard let bootstrap, bootstrap.phase != .complete else { return true }
        if (bootstrapEnvironment ?? .current()).arguments.contains("--setup") { showSetupWindow() }
        return false
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    @objc private func selectApplicationAction(_ sender: NSMenuItem) {
        guard let action = QueueAppAction(rawValue: sender.tag) else { return }
        AppRuntime.perform(action)
    }

    func makeMainMenu() -> NSMenu {
        let main = NSMenu()
        func submenu(_ title: String) -> NSMenu {
            let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
            let menu = NSMenu(title: title)
            item.submenu = menu
            main.addItem(item)
            return menu
        }
        func applicationItem(_ action: QueueAppAction, key: String = "") -> NSMenuItem {
            let item = NSMenuItem(title: action.title, action: #selector(selectApplicationAction(_:)), keyEquivalent: key)
            item.target = self
            item.tag = action.rawValue
            return item
        }
        let application = submenu("Agent Queue")
        application.addItem(applicationItem(.updates))
        application.addItem(.separator())
        let services = NSMenu(title: "Services")
        let servicesItem = NSMenuItem(title: "Services", action: nil, keyEquivalent: "")
        servicesItem.submenu = services
        application.addItem(servicesItem)
        NSApp.servicesMenu = services
        application.addItem(.separator())
        application.addItem(withTitle: "Hide Agent Queue", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        let hideOthers = application.addItem(withTitle: "Hide Others", action: #selector(NSApplication.hideOtherApplications(_:)), keyEquivalent: "h")
        hideOthers.keyEquivalentModifierMask = [.command, .option]
        application.addItem(withTitle: "Show All", action: #selector(NSApplication.unhideAllApplications(_:)), keyEquivalent: "")
        application.addItem(.separator())
        application.addItem(applicationItem(.quit, key: "q"))
        let file = submenu("File")
        file.addItem(applicationItem(.openWindow))
        file.addItem(withTitle: "Close Window", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        let edit = submenu("Edit")
        edit.addItem(withTitle: "Undo", action: NSSelectorFromString("undo:"), keyEquivalent: "z")
        let redo = edit.addItem(withTitle: "Redo", action: NSSelectorFromString("redo:"), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(.separator())
        edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        let window = submenu("Window")
        window.addItem(withTitle: "Minimize", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        window.addItem(withTitle: "Zoom", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
        window.addItem(.separator())
        window.addItem(withTitle: "Bring All to Front", action: #selector(NSApplication.arrangeInFront(_:)), keyEquivalent: "")
        NSApp.windowsMenu = window
        return main
    }

    func diagnostic(_ text: String) { FileHandle.standardError.write(Data((text + "\n").utf8)) }
}

public struct AgentQueueApp {
    @MainActor public static func main() {
        do {
            let arguments = Array(CommandLine.arguments.dropFirst())
            if let result = try QueueAppReplacement.handle(arguments: arguments)
                ?? QueueServiceManagement.handle(arguments: arguments) {
                FileHandle.standardOutput.write(Data((result.json + "\n").utf8))
                exit(result.exitCode)
            }
        } catch {
            FileHandle.standardError.write(Data((error.localizedDescription + "\n").utf8))
            exit(EXIT_FAILURE)
        }
        let application = NSApplication.shared
        application.setActivationPolicy(.accessory)
        let delegate = AppDelegate()
        application.delegate = delegate
        withExtendedLifetime(delegate) { application.run() }
    }
}
