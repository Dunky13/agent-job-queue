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
    var distinctPhase: String? {
        let display = phase.replacingOccurrences(of: "_", with: " ")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        let normalizedState = state.replacingOccurrences(of: "_", with: " ")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        guard !display.isEmpty, display.caseInsensitiveCompare(normalizedState) != .orderedSame else { return nil }
        return display.capitalized
    }
    var context: String { [project, branch].filter { !$0.isEmpty }.joined(separator: " / ") }
    /// One row line combining phase, resource use and the waiting reason; `details` holds the full figures.
    var resourceLine: (text: String, details: String)? {
        var parts: [String] = []
        if let phase = distinctPhase { parts.append(phase) }
        var details: [String] = []
        if let resources {
            let prediction = resources.prediction
            if let observed = resources.observed, isRunning {
                parts.append("\(resourceBytes(observed.memoryBytes)) now · peak \(resourceBytes(observed.peakMemoryBytes)) · est. \(resourceBytes(prediction.memoryBytes))")
            } else {
                parts.append("Est. \(resourceBytes(prediction.memoryBytes)) · \(resourceCores(prediction.cpuCores))")
            }
            details.append(prediction.summary)
            if let observed = resources.observed { details.append(observed.summary) }
            if isWaiting, let reason = resources.decision?.reason, !reason.isEmpty {
                details.append(reason)
                if reason.caseInsensitiveCompare(distinctPhase ?? "") != .orderedSame { parts.append(reason) }
            }
        }
        return parts.isEmpty ? nil : (parts.joined(separator: " · "), details.joined(separator: "\n"))
    }
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
    /// The setup window passes keepingCustomCallback so the backend never replaces a callback for it.
    static func installRepairAndContinue(_ target: QueueClient.InstallTarget, keepingCustomCallback: Bool = false) -> Self {
        let arguments = target.arguments + ["--repair-and-continue"] + (keepingCustomCallback ? ["--keep-custom-callback"] : [])
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
    var surfaces = QueueSurfaces()
    private var pollTask: Task<Void, Never>?
    private let pollSleep: PollSleep
    private let statusLoader: @Sendable () async throws -> Data
    private let command: @Sendable (QueueClient.Command) async throws -> Data
    private let snapshotPreparer: @Sendable (Data, QueueSnapshot?) async throws -> PreparedQueueSnapshot?
    private var refreshInFlight = false
    private var refreshRequested = false
    private var mutationRevision = 0
    init(statusLoader: @escaping @Sendable () async throws -> Data,
         command: @escaping @Sendable (QueueClient.Command) async throws -> Data,
         snapshotPreparer: @escaping @Sendable (Data, QueueSnapshot?) async throws -> PreparedQueueSnapshot? = {
             try await PreparedQueueSnapshot.decode($0, comparedTo: $1)
         },
         pollSleep: @escaping PollSleep = { try await Task.sleep(for: $0, clock: .continuous) }) {
        self.pollSleep = pollSleep
        self.statusLoader = statusLoader
        self.command = command
        self.snapshotPreparer = snapshotPreparer
    }

    var running: [QueueJob] { prepared?.running ?? [] }
    var waiting: [QueueJob] { prepared?.waiting ?? [] }
    var recent: [QueueJob] { prepared?.recent ?? [] }
    var isPaused: Bool { QueueAction.resume.isAllowed(in: snapshot) }
    var actionsDisabled: Bool { isRefreshing || isMutating || error != nil }
    func start() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self, pollSleep] in
            await self?.refresh()
            var ticks = 0
            while !Task.isCancelled {
                do { try await pollSleep(.seconds(2)) } catch { return }
                guard let self else { return }
                ticks += 1
                if surfaces.visibleCount > 0 || ticks % 5 == 0 { await refresh() }
            }
        }
    }

    /// A surface just came on screen: refresh without waiting for the next poll.
    @discardableResult
    func appear() -> Task<Void, Never> { Task { await refresh() } }

    func refresh(manual: Bool = false) async {
        guard !isMutating else { return }
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
    func dispatch(_ action: QueueAction) -> Task<Void, Never>? {
        guard !actionsDisabled, action.isAllowed(in: snapshot) else { return nil }
        // Reserve before scheduling asynchronous I/O so polling and other windows
        // cannot silently consume the user's confirmed action between these steps.
        isMutating = true
        if action == .resume { isResuming = true }
        mutationRevision += 1
        notice = nil
        return Task { await complete(action) }
    }

    private func complete(_ action: QueueAction) async {
        do {
            _ = try await command(action.command)
            notice = action.notice
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
    var surfaces = QueueSurfaces()
    private var pollTask: Task<Void, Never>?
    private let pollSleep: PollSleep
    /// Runs `agent-remote serve` as a child of the app. Nil for unmanaged launches.
    var supervisor: QueueHelperSupervisor?
    private let command: @Sendable (QueueClient.Command) async throws -> Data

    init(command: @escaping @Sendable (QueueClient.Command) async throws -> Data,
         pollSleep: @escaping PollSleep = { try await Task.sleep(for: $0, clock: .continuous) }) {
        self.pollSleep = pollSleep
        self.command = command
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

    /// Polls while a surface is visible. Returns the new loop, or nil when none was started.
    @discardableResult
    func startPolling() -> Task<Void, Never>? {
        guard pollTask == nil else { return nil }
        pollTask = Task { [weak self, pollSleep] in
            while !Task.isCancelled {
                guard let self else { return }
                guard surfaces.visibleCount > 0 else { pollTask = nil; return }
                await refresh()
                do { try await pollSleep(.seconds(5)) } catch { return }
            }
        }
        return pollTask
    }

    func refresh() async {
        guard !isBusy, !isRefreshing else { return }
        isRefreshing = true
        defer { isRefreshing = false }
        do {
            status = try JSONDecoder().decode(RemoteStatus.self,
                from: await command(.remoteStatus))
            error = status?.error
        } catch { self.error = error.localizedDescription }
    }

    func setEnabled(_ enabled: Bool) {
        guard !isBusy, !isRefreshing, let status,
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
        guard !isBusy, !isRefreshing, Self.acceptsPassword(password) else { return false }
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

// Owns only startup presentation. It never creates the window coordinator or exposes
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
final class QueueMenuBarController: NSObject, NSPopoverDelegate, QueueSurface {
    let statusItem: NSStatusItem
    let popover = NSPopover()
    let menu = NSMenu()
    private var observation: AnyCancellable?
    private var pendingTitle: String?
    private var menuTrackingObservations: [AnyCancellable] = []
    private var menuTrackingDepth = 0
    private var eventMonitor: Any?
    private var globalMouseMonitor: Any?
    var isMonitoringEvents: Bool { eventMonitor != nil || globalMouseMonitor != nil }
    private var isTornDown = false
    private let store: QueueStore
    /// True from `popoverWillShow` until the popover is closed, ahead of its close animation.
    private(set) var isVisible = false
    private let app: QueueAppRoutes

    init(store: QueueStore, statusItem: NSStatusItem? = nil, app: QueueAppRoutes) {
        self.statusItem = statusItem ?? NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        self.store = store
        self.app = app
        super.init()
        store.surfaces.add(self)
        // Transient popovers consume Control-clicks on the status button while
        // shown. Own dismissal so every status-button gesture remains available.
        popover.behavior = .applicationDefined
        popover.delegate = self
        popover.contentSize = NSSize(width: 500, height: 650)
        popover.contentViewController = NSHostingController(rootView: QueuePanel(store: store, app: app, refreshesOnAppear: false))
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
            guard let self else { return }
            let presentation = QueueStatusPresentation(snapshot: snapshot, error: error)
            // A wider or narrower title moves the anchored popover sideways, so hold it while shown.
            // The accessibility label and tooltip stay current; the open popover shows the same state.
            if self.popover.isShown { self.pendingTitle = presentation.title } else { self.statusItem.button?.title = presentation.title }
            self.statusItem.button?.setAccessibilityLabel(presentation.accessibilityLabel)
            self.statusItem.button?.toolTip = presentation.accessibilityLabel
        }
        // The popover deliberately survives another app taking focus (agent jobs launch browsers
        // and simulators). Outside clicks, Escape and the status item still dismiss it.
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
        app.perform(action)
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
        guard !isVisible else { return }
        isVisible = true
        startMonitoringEvents()
        store.appear()
    }

    func popoverDidClose(_ notification: Notification) {
        if !popover.isShown { releaseVisibleSurface() }
    }

    private func releaseVisibleSurface() {
        stopMonitoringEvents()
        if let pendingTitle { statusItem.button?.title = pendingTitle; self.pendingTitle = nil }
        isVisible = false
    }

    func tearDown() {
        guard !isTornDown else { return }
        isTornDown = true
        closePopover()
        observation = nil
        menuTrackingObservations = []
        NSStatusBar.system.removeStatusItem(statusItem)
    }
}

struct QueueConfirmation {
    let kind: QueueConfirmationKind
    let job: QueueJob
}

@MainActor
final class QueueNavigation: ObservableObject {
    @Published var selected: QueueJob?
    @Published var confirmation: QueueConfirmation?
    func show(_ job: QueueJob) { selected = job }
    func back() { selected = nil }
    func request(_ kind: QueueConfirmationKind, job: QueueJob) {
        selected = nil
        confirmation = QueueConfirmation(kind: kind, job: job)
    }
    func decline() { confirmation = nil }

    /// The one entry point for surfaces: asks first when the action needs confirmation.
    @discardableResult
    func perform(_ action: QueueAction, in store: QueueStore) -> Task<Void, Never>? {
        guard let kind = action.confirmation else { return store.dispatch(action) }
        if let job = action.job { request(kind, job: job) }
        return nil
    }

    func unavailability(in store: QueueStore) -> String? {
        guard let confirmation else { return "No action is awaiting confirmation." }
        guard let current = store.snapshot?.jobs.first(where: { $0.id == confirmation.job.id }) else {
            return "This job is no longer in the current queue. Return to the queue to see its latest status."
        }
        if !confirmation.kind.action(for: current).isAllowed(in: store.snapshot) { return confirmation.kind.unavailableMessage }
        return nil
    }

    @discardableResult
    func confirm(in store: QueueStore) -> Task<Void, Never>? {
        guard !store.actionsDisabled, unavailability(in: store) == nil,
              let confirmation,
              let current = store.snapshot?.jobs.first(where: { $0.id == confirmation.job.id }) else { return nil }
        guard let dispatch = store.dispatch(confirmation.kind.action(for: current)) else { return nil }
        // Reservation and consumption both finish before yielding the main actor.
        self.confirmation = nil
        return dispatch
    }
}

struct QueuePanel: View {
    @ObservedObject var store: QueueStore
    let app: QueueAppRoutes
    var fullWindow = false
    var refreshesOnAppear = true
    @StateObject private var navigation: QueueNavigation
    // The requested value shows until the queue reports it, so a menu never snaps back mid-change.
    @State private var pendingLimit: Int?
    @State private var pendingMode: SchedulingMode?

    init(store: QueueStore, app: QueueAppRoutes, fullWindow: Bool = false, refreshesOnAppear: Bool = true,
         navigation: QueueNavigation = QueueNavigation()) {
        self.store = store
        self.app = app
        self.fullWindow = fullWindow
        self.refreshesOnAppear = refreshesOnAppear
        _navigation = StateObject(wrappedValue: navigation)
    }

    var body: some View {
        let overlaid = navigation.confirmation != nil || navigation.selected != nil
        ZStack {
            // The queue stays alive under details and confirmations so returning keeps its scroll position.
            VStack(spacing: 0) {
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
            .opacity(overlaid ? 0 : 1).disabled(overlaid).accessibilityHidden(overlaid)
            if let confirmation = navigation.confirmation {
                let currentJob = store.snapshot?.jobs.first(where: { $0.id == confirmation.job.id }) ?? confirmation.job
                JobConfirmationPanel(kind: confirmation.kind, job: currentJob,
                    unavailable: navigation.unavailability(in: store), error: store.error,
                    disabled: store.actionsDisabled, onDecline: navigation.decline,
                    onConfirm: { navigation.confirm(in: store) })
                    .background(Color(nsColor: .windowBackgroundColor))
            } else if let job = navigation.selected {
                let currentJob = store.snapshot?.jobs.first(where: { $0.id == job.id }) ?? job
                JobDetail(job: currentJob, logTail: app.logTail, desktop: app.desktop,
                          openLearning: app.openLearning, onClose: navigation.back)
                    .background(Color(nsColor: .windowBackgroundColor))
            }
        }
        .frame(minWidth: 470, minHeight: 440)
        .frame(width: fullWindow ? nil : 500, height: fullWindow ? nil : 650)
        .background(Color(nsColor: .windowBackgroundColor))
        .onAppear { if refreshesOnAppear { store.appear() } }
        .onChange(of: store.snapshot?.queue.limit) { pendingLimit = nil }
        .onChange(of: store.snapshot?.scheduler?.mode) { pendingMode = nil }
        .onChange(of: store.error) { pendingLimit = nil; pendingMode = nil }
    }

    // Menus apply on selection. They are disabled while an action runs, so no choice is silently refused.
    private var limitSelection: Binding<Int> {
        Binding(get: { pendingLimit ?? store.snapshot?.queue.limit ?? 1 },
                set: { value in
                    guard store.dispatch(.limit(value)) != nil else { return }
                    pendingLimit = value
                })
    }
    private var modeSelection: Binding<SchedulingMode> {
        Binding(get: { pendingMode ?? store.snapshot?.scheduler?.mode ?? .fixed },
                set: { value in if store.dispatch(.scheduling(value)) != nil { pendingMode = value } })
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("Agent Queue", systemImage: QueueBrand.symbolName).font(.system(size: 15, weight: .semibold))
                Spacer()
                Button { Task { await store.refresh(manual: true) } } label: {
                    Image(systemName: "arrow.clockwise").frame(width: 20, height: 20)
                }.buttonStyle(.borderless).help("Refresh queue (Command-R)")
                    .accessibilityLabel("Refresh queue").keyboardShortcut("r")
                    .disabled(store.isRefreshing || store.isMutating)
            }
            HStack(spacing: 14) {
                Label("\(store.snapshot?.queue.running ?? 0) running", systemImage: "play.fill").foregroundStyle(.green)
                Label("\(store.snapshot?.queue.waiting ?? 0) waiting", systemImage: "clock").foregroundStyle(.secondary)
            }.font(.system(size: 12, weight: .medium))
            HStack(spacing: 14) {
                Picker("Parallel", selection: limitSelection) {
                    ForEach(QueueAction.limits, id: \.self) { Text(String($0)).tag($0) }
                }
                .fixedSize()
                .help("How many jobs run at a time, 1 to 8. Lowering the limit lets active jobs finish.")
                .accessibilityLabel("Parallel job limit")
                if store.snapshot?.scheduler != nil {
                    Picker("Scheduling", selection: modeSelection) {
                        ForEach(SchedulingMode.allCases, id: \.self) { mode in Text(mode.label).tag(mode) }
                    }
                    .fixedSize()
                    .help("Adaptive fits jobs within your parallel limit. Learn only records predictions. Running jobs are never stopped automatically.")
                }
                Spacer(minLength: 0)
            }
            .pickerStyle(.menu).font(.caption)
            .disabled(store.actionsDisabled || store.snapshot == nil)
            if let scheduler = store.snapshot?.scheduler {
                hostLine(scheduler)
            } else {
                Text("Resource monitoring unavailable. Update the queue controller to enable scheduling controls.")
                    .font(.caption2).foregroundStyle(.secondary)
            }
        }.padding(.horizontal, 16).padding(.vertical, 12)
    }

    private func hostLine(_ scheduler: QueueScheduler) -> some View {
        let details = [scheduler.host?.summary, scheduler.summary, scheduler.reason.isEmpty ? nil : scheduler.reason]
            .compactMap { $0 }.joined(separator: "\n")
        return VStack(alignment: .leading, spacing: 3) {
            if let host = scheduler.host {
                HStack(spacing: 6) {
                    Text("Memory")
                    meter(1 - Double(host.availableMemoryBytes) / Double(max(host.totalMemoryBytes, 1)),
                          tint: ["normal": .green, "warning": .orange, "critical": .red][host.memoryPressure] ?? .secondary)
                    Text("\(resourceBytes(host.availableMemoryBytes)) free")
                    Text("·")
                    Text("CPU")
                    meter(host.cpuBusyCores / Double(max(host.cpuCores, 1)), tint: .accentColor)
                    Text("\(host.cpuBusyCores.formatted(.number.precision(.fractionLength(1))))/\(host.cpuCores)")
                }
                .lineLimit(1)
                .accessibilityElement(children: .ignore)
                .accessibilityLabel([host.summary, scheduler.reason].filter { !$0.isEmpty }.joined(separator: ". "))
            } else {
                Text("Host measurements unavailable.")
            }
            if !scheduler.available {
                Text(scheduler.reason.isEmpty ? "Resource monitoring unavailable." : scheduler.reason).foregroundStyle(.orange)
            }
        }
        .font(.caption2.monospacedDigit()).foregroundStyle(.secondary)
        .help(details)
    }

    private func meter(_ fraction: Double, tint: Color) -> some View {
        // Drawn rather than a ProgressView, which ignores the tint in inactive popovers.
        Capsule().fill(.quaternary).frame(width: 44, height: 4)
            .overlay(alignment: .leading) { Capsule().fill(tint).frame(width: 44 * min(max(fraction, 0), 1)) }
    }

    private var queueList: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 0) {
                if store.isPaused {
                    HStack(spacing: 12) {
                        Label("Queue paused. Waiting jobs will not start automatically.", systemImage: "pause.circle")
                            .foregroundStyle(.orange)
                        Spacer(minLength: 0)
                        Button(store.isResuming ? "Resuming…" : "Resume", systemImage: "play.fill") {
                            store.dispatch(.resume)
                        }
                        .buttonStyle(.bordered)
                        .fixedSize()
                        .disabled(store.actionsDisabled)
                        .accessibilityLabel("Resume queue")
                        .accessibilityIdentifier("queue.resume")
                        .help("Resume automatic starts for waiting jobs.")
                    }.font(.callout).padding(.top, 10)
                }
                if store.snapshot?.queue.legacyOccupied == true {
                    Label("A legacy validation batch holds the shared slot.", systemImage: "hourglass")
                        .font(.callout).foregroundStyle(.secondary).padding(.top, 10)
                }
                sectionHeader("Running", count: store.running.count)
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
                    }.padding(.vertical, 8)
                }
                ForEach(store.running) { job in row(job) }
                sectionHeader("Up next", count: store.waiting.count)
                if store.waiting.isEmpty {
                    Text("No waiting jobs").font(.callout).foregroundStyle(.secondary).padding(.vertical, 8)
                }
                ForEach(store.waiting) { job in row(job) }
                if !store.recent.isEmpty {
                    sectionHeader("Recent", count: store.recent.count)
                    ForEach(store.recent) { job in row(job) }
                }
            }.padding(.horizontal, 16).padding(.bottom, 10)
        }
    }

    private func sectionHeader(_ title: String, count: Int) -> some View {
        VStack(spacing: 4) {
            HStack { Text(title.uppercased()); Spacer(); Text(String(count)).monospacedDigit() }
                .font(.caption2.weight(.semibold)).foregroundStyle(.secondary)
            Divider()
        }.padding(.top, 12).accessibilityElement(children: .combine).accessibilityAddTraits(.isHeader)
    }

    private func row(_ job: QueueJob) -> some View {
        VStack(spacing: 0) {
            HStack(alignment: .top, spacing: 10) {
                Group {
                    if let position = job.queuePosition, job.isWaiting {
                        Text(String(position)).font(.system(size: 12, weight: .semibold, design: .rounded)).foregroundStyle(.secondary)
                    } else {
                        Image(systemName: job.isRunning ? "play.fill" : (job.state == "failed" || (job.exitCode ?? 0) != 0 ? "exclamationmark.circle" : "checkmark.circle"))
                            .font(.system(size: 12)).foregroundStyle(job.color)
                    }
                }.frame(width: 18, height: 18)
                VStack(alignment: .leading, spacing: 3) {
                    Button { navigation.show(job) } label: {
                        Text(job.name).font(.system(size: 13, weight: .semibold)).foregroundStyle(.primary)
                            .multilineTextAlignment(.leading).lineLimit(2).frame(maxWidth: .infinity, alignment: .leading)
                    }.buttonStyle(.plain).help("View details and output for job #\(job.id)")
                    HStack(spacing: 5) {
                        if !job.context.isEmpty {
                            Text(job.context).lineLimit(1).truncationMode(.middle)
                            Text("·")
                        }
                        Text("#\(job.id)").fixedSize()
                        Text("·")
                        Text(job.stateLabel).foregroundStyle(job.color).fixedSize()
                        if job.isRunning, let startedAt = job.startedAt, let date = parseDate(startedAt) {
                            Text("·")
                            Text(date, style: .relative).fixedSize()
                        }
                    }.font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                    if let line = job.resourceLine {
                        Text(line.text).font(.caption2).foregroundStyle(.secondary).lineLimit(1).help(line.details)
                    }
                }
                HStack(spacing: 2) {
                    // Icon buttons keep every row the same height; titles are in the tooltips and the ⋯ menu.
                    if QueueAction.next(job).isAllowed(in: store.snapshot) {
                        iconAction("Run job \(job.id) next", symbol: "arrow.up.to.line", help: "Run job #\(job.id) after the current job finishes") {
                            navigation.perform(.next(job), in: store)
                        }
                        iconAction("Move job \(job.id) to the end", symbol: "arrow.down.to.line", help: "Move job #\(job.id) to the end of the queue") {
                            navigation.perform(.last(job), in: store)
                        }
                    }
                    if QueueAction.start(job).isAllowed(in: store.snapshot) {
                        iconAction("Start job \(job.id) now", symbol: "play.fill", help: "Start job #\(job.id) now, bypassing the parallel limit and adaptive resource checks") {
                            navigation.perform(.start(job), in: store)
                        }
                    }
                    Menu {
                        Button("View details and output", systemImage: "doc.text") { navigation.show(job) }
                        if let learning = job.resources?.learning {
                            Button("View learned workload", systemImage: "chart.bar.doc.horizontal") {
                                app.openLearning(learning.id)
                            }
                        }
                        if QueueAction.next(job).isAllowed(in: store.snapshot) || QueueAction.start(job).isAllowed(in: store.snapshot) {
                            Divider()
                            if QueueAction.next(job).isAllowed(in: store.snapshot) {
                                Button("Run next", systemImage: "arrow.up.to.line") { navigation.perform(.next(job), in: store) }.disabled(store.actionsDisabled)
                                Button("Move to end", systemImage: "arrow.down.to.line") { navigation.perform(.last(job), in: store) }.disabled(store.actionsDisabled)
                            }
                            if QueueAction.start(job).isAllowed(in: store.snapshot) {
                                Button("Start now…", systemImage: "play.fill") { navigation.perform(.start(job), in: store) }.disabled(store.actionsDisabled)
                            }
                        }
                        if QueueAction.cancel(job).isAllowed(in: store.snapshot) {
                            Divider()
                            Button("Cancel job…", systemImage: "stop.circle", role: .destructive) { navigation.perform(.cancel(job), in: store) }.disabled(store.actionsDisabled)
                        }
                    } label: { Image(systemName: "ellipsis").frame(width: 22, height: 22) }
                        .menuStyle(.borderlessButton).menuIndicator(.hidden).fixedSize().help("Actions for job #\(job.id)")
                        .accessibilityLabel("Actions for job \(job.id)")
                }
            }.padding(.vertical, 8)
            Divider().padding(.leading, 28)
        }
    }

    private func iconAction(_ title: String, symbol: String, help: String, action: @escaping () -> Void) -> some View {
        Button(action: action) { Image(systemName: symbol).frame(width: 22, height: 22) }
            .buttonStyle(.borderless).foregroundStyle(.secondary)
            .help(help).accessibilityLabel(title)
            .disabled(store.actionsDisabled)
    }

    private var footer: some View {
        HStack(spacing: 8) {
            // A notice briefly takes the status line's place so the list never changes height.
            if let notice = store.notice {
                Label(notice, systemImage: "checkmark.circle").foregroundStyle(.primary).lineLimit(1).truncationMode(.tail)
            } else {
                Circle().fill(store.error != nil ? Color.orange : Color.green).frame(width: 6, height: 6)
                if store.isMutating { Text("Updating queue…") }
                else if let updated = store.lastRefreshed { Text("Updated \(updated.formatted(date: .omitted, time: .shortened))") }
                else { Text("Connecting…") }
            }
            Spacer(minLength: 8)
            if !fullWindow { Button(QueueAppAction.openWindow.title, systemImage: QueueAppAction.openWindow.symbol) { app.perform(.openWindow) }.buttonStyle(.borderless) }
            Menu {
                ForEach(QueueAppAction.settings) { action in
                    Button(action.title, systemImage: action.symbol) { app.perform(action) }
                }
                Divider()
                Text(QueueAppAction.quitExplanation)
                Divider()
                Button(QueueAppAction.quit.title, systemImage: QueueAppAction.quit.symbol) { app.perform(.quit) }.keyboardShortcut("q")
            } label: { Image(systemName: "gearshape") }.menuStyle(.borderlessButton).allowsWindowActivationEvents(true).fixedSize().help("Application options").accessibilityLabel("Application options")
        }
        .font(.caption).foregroundStyle(.secondary)
        .padding(.horizontal, 16).padding(.vertical, 10)
        .task(id: store.notice) {
            guard let shown = store.notice, (try? await Task.sleep(for: .seconds(5))) != nil, store.notice == shown else { return }
            store.notice = nil
        }
    }
}

struct RemoteAccessSettings: View {
    @ObservedObject var store: RemoteAccessStore
    let desktop: QueueDesktop
    @State private var password = ""
    @State private var copied = false

    private var canSave: Bool {
        !store.isBusy && !store.isRefreshing && RemoteAccessStore.acceptsPassword(password)
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
                    Button { Task { await store.refresh() } } label: { Image(systemName: "arrow.clockwise") }
                        .buttonStyle(.borderless).help("Refresh remote access status").accessibilityLabel("Refresh remote access status")
                        .disabled(store.isBusy || store.isRefreshing)
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
                        .disabled(store.isBusy || store.isRefreshing
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
                        .disabled(store.isBusy)
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
                                desktop(.copy(remoteURL.absoluteString))
                                copied = true
                            }
                            Button("Open", systemImage: "arrow.up.forward.square") { desktop(.open(remoteURL)) }
                                .disabled(store.status?.running != true)
                        }
                    } else { Text("The phone address appears after setup.").font(.caption).foregroundStyle(.secondary) }
                    VStack(alignment: .leading, spacing: 14) {
                        instruction(1, "Transfer the certificate to Android", "Send public root.crt to Android over USB or your preferred file transfer. Use Share certificate or Show in Finder below.")
                        if let path = store.status?.certificatePath, !path.isEmpty {
                            HStack {
                                ShareLink(item: URL(fileURLWithPath: path)) { Label("Share certificate", systemImage: "square.and.arrow.up") }
                                Button("Show in Finder", systemImage: "folder") {
                                    desktop(.reveal(URL(fileURLWithPath: path)))
                                }
                            }
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
    let kind: QueueConfirmationKind
    let job: QueueJob
    let unavailable: String?
    let error: String?
    let disabled: Bool
    let onDecline: () -> Void
    let onConfirm: () -> Void

    private var starts: Bool { kind == .start }
    private var title: String { starts ? "Start this job now?" : "Cancel this job?" }
    private var confirmLabel: String { starts ? "Start now" : "Cancel job" }
    private var declineLabel: String { starts ? "Keep waiting" : "Keep job" }
    private var explanation: String {
        if starts {
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
                    Image(systemName: starts ? "play.circle" : "stop.circle")
                        .font(.system(size: 34, weight: .regular)).foregroundStyle(starts ? Color.orange : Color.red)
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
                    .tint(starts ? Color.accentColor : Color.red)
                    .disabled(disabled || unavailable != nil)
                    .accessibilityIdentifier("confirm-\(kind.rawValue)-job")
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
    let logTail: QueueLogTail
    let desktop: QueueDesktop
    let openLearning: @MainActor (String) -> Void
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
                                Button(learning.label) { openLearning(learning.id) }
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
                Button("Refresh", systemImage: "arrow.clockwise") { Task { await loadLog() } }.disabled(loading)
                Button("Open log", systemImage: "arrow.up.forward.square") {
                    desktop(.open(URL(fileURLWithPath: job.logPath)))
                }.disabled(job.logPath.isEmpty)
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
        guard !job.logPath.isEmpty else { output = "This job has no output log."; return }
        loading = true
        defer { loading = false }
        do { output = try await logTail(job.logPath) }
        catch { output = "Cannot read output: \(error.localizedDescription)" }
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private(set) var bootstrap: QueueBootstrapModel?
    private(set) var setupWindow: NSWindow?
    private(set) var startupMenuBarController: QueueStartupMenuBarController?
    private(set) var supervisor: QueueHelperSupervisor?
    /// Nil until setup is verified: nothing before that may reach the queue.
    private(set) var coordinator: WindowCoordinator?
    private var helperStartup: Task<Void, Never>?
    private var terminationPending = false
    private var normalLaunchStarted = false
    private var bootstrapEnvironment: QueueBootstrapEnvironment?
    private var bootstrapExecutor: QueueBootstrapModel.Executor = QueueBootstrapModel.runInstaller
    private var replyToTermination: @MainActor () -> Void = { NSApp.reply(toApplicationShouldTerminate: true) }
    private var makeSupervisor: @MainActor (QueueBootstrapEnvironment) -> QueueHelperSupervisor? = {
        $0.makeSupervisor()
    }

    override init() { super.init() }

    // The separate graphical fixture supplies a private bundle/home and an inert
    // executor. Normal launch still follows the production path.
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
        let window = QueueWindowSpec(title: "Set up Agent Queue", size: NSSize(width: 560, height: 410),
                                     style: [.titled, .miniaturizable])
            .makeWindow(content: NSHostingView(rootView: QueueBootstrapView(model: bootstrap)))
        setupWindow = window
        window.present(isNew: true)
    }

    private func finishLaunching() {
        guard !normalLaunchStarted else { return }
        normalLaunchStarted = true
        NSApp.mainMenu = makeMainMenu()
        let statusItem = startupMenuBarController?.transferStatusItem()
        startupMenuBarController = nil
        let environment = bootstrapEnvironment ?? .current()
        #if AGENT_QUEUE_TEST_HOOKS
        // `--fixture` in either argument list selects the stand-in backend, never the live one.
        let fixture = QueueFixture.launch(arguments: environment.arguments + CommandLine.arguments)
        let coordinator = fixture ?? WindowCoordinator(backend: .live)
        #else
        let coordinator = WindowCoordinator(backend: .live)
        #endif
        let helpers = makeSupervisor(environment)
        self.coordinator = coordinator
        coordinator.showMenuBar(statusItem: statusItem)
        if let helpers {
            startHelpers(helpers, for: coordinator)
        } else { coordinator.store.start() }
        #if AGENT_QUEUE_TEST_HOOKS
        if runSnapshotHooks(coordinator) { return }
        #endif
        if CommandLine.arguments.contains("--open-window"), supervisor == nil { coordinator.show(.queue) }
    }

    /// Bootstrap has completed, so setup is no longer running: nothing here can race the installer.
    private func startHelpers(_ helpers: QueueHelperSupervisor, for coordinator: WindowCoordinator) {
        supervisor = helpers
        coordinator.remoteStore.supervisor = helpers
        // The phone service is always started: it exits at once while phone access is off.
        helpers.startAll()
        helperStartup = Task { @MainActor in
            // Let the daemon come up first so the menu does not flash "Queue unavailable".
            _ = await helpers.waitUntilQueueReachable(timeout: .seconds(5))
            guard !Task.isCancelled else { return }
            coordinator.store.start()
            if CommandLine.arguments.contains("--open-window") { coordinator.show(.queue) }
        }
    }

    func applicationWillTerminate(_ notification: Notification) {
        helperStartup?.cancel()
        supervisor?.terminateRemaining()
        startupMenuBarController?.tearDown()
        startupMenuBarController = nil
        coordinator?.tearDown()
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
        guard let action = QueueAppAction(rawValue: sender.tag) else { preconditionFailure("Application menu item has no app action") }
        guard let coordinator else { preconditionFailure("Application menu used before the coordinator exists") }
        coordinator.perform(action)
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
