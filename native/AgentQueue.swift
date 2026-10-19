import AppKit
import SwiftUI
import Foundation
import Darwin

struct QueueSnapshot: Codable, Sendable {
    let updatedAt: String
    let queue: QueueSummary
    let jobs: [QueueJob]

    func hasSameContent(as other: QueueSnapshot) -> Bool {
        queue == other.queue && jobs == other.jobs
    }
}

struct QueueSummary: Codable, Sendable, Equatable {
    let status: String
    let limit: Int
    let running: Int
    let waiting: Int
    let legacyOccupied: Bool
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

// CLI I/O and deadline handling stay off the main actor. A file avoids pipe-buffer
// deadlocks with large status responses; every response is bounded before loading.
enum QueueClient {
    enum Controller: String, Sendable { case heavy = "agent-heavy", remote = "agent-remote" }

    static func run(_ arguments: [String], controller: Controller = .heavy, input: Data? = nil) async throws -> Data {
        try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .utility).async {
                do { continuation.resume(returning: try runBlocking(arguments, controller: controller, input: input)) }
                catch { continuation.resume(throwing: error) }
            }
        }
    }

    private static func runBlocking(_ arguments: [String], controller: Controller, input: Data?) throws -> Data {
        guard (input?.count ?? 0) <= 16_384 else { throw ClientError.message("Controller input exceeds the private-pipe limit.") }
        let temporary = FileManager.default.temporaryDirectory
            .appendingPathComponent("agent-queue-client-\(UUID().uuidString)")
        guard FileManager.default.createFile(atPath: temporary.path, contents: nil,
                                             attributes: [.posixPermissions: 0o600]) else {
            throw ClientError.message("Could not create a temporary response file.")
        }
        defer { try? FileManager.default.removeItem(at: temporary) }
        let output = try FileHandle(forWritingTo: temporary)
        defer { try? output.close() }
        let process = Process()
        process.executableURL = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".local/bin/\(controller.rawValue)")
        process.arguments = arguments
        let inputPipe = input == nil ? nil : Pipe()
        if let inputPipe {
            guard fcntl(inputPipe.fileHandleForWriting.fileDescriptor, F_SETNOSIGPIPE, 1) != -1 else {
                throw ClientError.message("Could not prepare the private controller input pipe.")
            }
            process.standardInput = inputPipe.fileHandleForReading
        } else { process.standardInput = FileHandle.nullDevice }
        defer {
            try? inputPipe?.fileHandleForReading.close()
            try? inputPipe?.fileHandleForWriting.close()
        }
        process.standardOutput = output
        process.standardError = output
        try process.run()
        if let input, let inputPipe {
            do {
                try inputPipe.fileHandleForReading.close()
                try inputPipe.fileHandleForWriting.write(contentsOf: input)
                try inputPipe.fileHandleForWriting.close()
            } catch {
                if process.isRunning { process.terminate() }
                throw ClientError.message("Could not send private configuration to the controller.")
            }
        }
        let timeout: TimeInterval = arguments.first == "status" ? 8 : (controller == .remote ? 35 : 25)
        let deadline = Date().addingTimeInterval(timeout)
        while process.isRunning && Date() < deadline { Thread.sleep(forTimeInterval: 0.04) }
        if process.isRunning {
            process.terminate()
            for _ in 0..<25 {
                if !process.isRunning { break }
                Thread.sleep(forTimeInterval: 0.02)
            }
            if process.isRunning { kill(process.processIdentifier, SIGKILL) }
            throw ClientError.message("The queue did not respond within \(Int(timeout)) seconds. Jobs continue independently. Refresh to check whether the change was applied.")
        }
        let input = try FileHandle(forReadingFrom: temporary)
        defer { try? input.close() }
        let response = try input.read(upToCount: 2_097_153) ?? Data()
        guard response.count <= 2_097_152 else {
            throw ClientError.message("The queue response exceeds the 2 MB display limit.")
        }
        guard process.terminationStatus == 0 else {
            let message = String(decoding: response.prefix(1_000), as: UTF8.self)
                .trimmingCharacters(in: .whitespacesAndNewlines)
            throw ClientError.message(message.isEmpty ? "Queue command failed (\(process.terminationStatus))." : message)
        }
        return response
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

@MainActor
final class QueueStore: ObservableObject {
    @Published var snapshot: QueueSnapshot?
    @Published var error: String?
    @Published var lastRefreshed: Date?
    @Published var isRefreshing = false
    @Published var isMutating = false
    @Published var notice: String?
    private var visibleSurfaces = 0
    private var pollTask: Task<Void, Never>?
    private let statusLoader: @Sendable () async throws -> Data
    private var refreshInFlight = false
    private var refreshRequested = false
    private var mutationRevision = 0
    let fixtureMode: Bool
    private(set) var fixtureRecorder: FixtureDispatchRecorder?

    init(statusLoader: @escaping @Sendable () async throws -> Data = {
        try await QueueClient.run(["status", "--json"])
    }) {
        self.statusLoader = statusLoader
        let args = CommandLine.arguments
        if let index = args.firstIndex(of: "--fixture"), args.indices.contains(index + 1) {
            fixtureMode = true
            if args.contains("--verify-confirmation-navigation") { fixtureRecorder = FixtureDispatchRecorder() }
            do {
                let data = try Data(contentsOf: URL(fileURLWithPath: args[index + 1]))
                snapshot = try JSONDecoder().decode(QueueSnapshot.self, from: data)
                lastRefreshed = Date()
            } catch { self.error = "Fixture could not be loaded: \(error.localizedDescription)" }
        } else { fixtureMode = false }
    }

    var running: [QueueJob] { snapshot?.jobs.filter(\.isRunning) ?? [] }
    var waiting: [QueueJob] {
        (snapshot?.jobs.filter(\.isWaiting) ?? []).sorted {
            ($0.queuePosition ?? Int.max, $0.id) < ($1.queuePosition ?? Int.max, $1.id)
        }
    }
    var recent: [QueueJob] { Array((snapshot?.jobs.filter { !$0.isRunning && !$0.isWaiting } ?? []).prefix(12)) }
    var actionsDisabled: Bool { (fixtureMode && fixtureRecorder == nil) || isRefreshing || isMutating || error != nil }
    var badge: String {
        guard let queue = snapshot?.queue else { return "" }
        if queue.running == 0 && queue.waiting == 0 { return "" }
        return "\(queue.running) · \(queue.waiting)"
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
            let next = try JSONDecoder().decode(QueueSnapshot.self, from: data)
            if snapshot?.hasSameContent(as: next) != true {
                snapshot = next
                lastRefreshed = Date()
            }
            if error != nil { error = nil }
        } catch {
            guard revision == mutationRevision, !isMutating else { return }
            if self.error != error.localizedDescription { self.error = error.localizedDescription }
        }
    }

    @discardableResult
    func dispatch(_ action: String, job: QueueJob) -> Task<Void, Never>? {
        guard !actionsDisabled, ["next", "last", "cancel", "start"].contains(action) else { return nil }
        guard action == "cancel" ? job.canCancel : job.canReorder else { return nil }
        // Reserve before scheduling asynchronous I/O so polling and other windows
        // cannot silently consume the user's confirmed action between these steps.
        isMutating = true
        mutationRevision += 1
        notice = nil
        return Task { await completeAction(action, job: job) }
    }

    private func completeAction(_ action: String, job: QueueJob) async {
        if fixtureMode {
            // Fixture actions can only reach this recorder, never the live CLI.
            fixtureRecorder?.record(action: action, jobID: job.id)
            isMutating = false
            return
        }
        do {
            _ = try await QueueClient.run([action, String(job.id)])
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
    func dispatchLimit(_ limit: Int) -> Task<Void, Never>? {
        guard !fixtureMode, !actionsDisabled, (1...8).contains(limit) else { return nil }
        isMutating = true
        mutationRevision += 1
        notice = nil
        return Task { await completeLimit(limit) }
    }

    private func completeLimit(_ limit: Int) async {
        do {
            _ = try await QueueClient.run(["limit", String(limit)])
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

    init(fixtureMode: Bool) {
        self.fixtureMode = fixtureMode
        if fixtureMode {
            let stopFailure = CommandLine.arguments.contains("--snapshot-remote-stop-failure")
                || CommandLine.arguments.contains("--verify-remote-stop-state")
            status = RemoteStatus(enabled: false, configured: stopFailure, running: false,
                url: "https://192.0.2.10:8743/", port: 8743, username: "agent", certificatePath: "",
                error: stopFailure ? "Remote stop incomplete" : nil)
        }
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
                from: await QueueClient.run(["status", "--json"], controller: .remote))
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
                let data = try await QueueClient.run([enabled ? "enable" : "disable"], controller: .remote)
                self.status = try JSONDecoder().decode(RemoteStatus.self, from: data)
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
                let data = try await QueueClient.run(["configure"], controller: .remote, input: input)
                status = try JSONDecoder().decode(RemoteStatus.self, from: data)
                if enable {
                    status = try JSONDecoder().decode(RemoteStatus.self,
                        from: await QueueClient.run(["enable"], controller: .remote))
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
    static var queueWindow: NSWindow?
    static var remoteWindow: NSWindow?
    static var integrationWindow: NSWindow?

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

@MainActor
final class FixtureDispatchRecorder {
    private(set) var actions: [RecordedQueueAction] = []
    func record(action: String, jobID: Int) { actions.append(RecordedQueueAction(action: action, jobID: jobID)) }
}

struct RecordedQueueAction: Codable, Equatable {
    let action: String
    let jobID: Int
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
            guard current.isWaiting && current.canReorder else {
                return "This job is no longer waiting. It cannot be started again."
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
    @StateObject private var navigation: QueueNavigation
    @State private var draftLimit = "1"

    init(store: QueueStore, fullWindow: Bool = false, navigation: QueueNavigation = QueueNavigation()) {
        self.store = store
        self.fullWindow = fullWindow
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
            store.appear(fullWindow: fullWindow)
        }
        .onChange(of: store.snapshot?.queue.limit) { _, value in
            if let value { draftLimit = String(value) }
        }
        .onDisappear { store.disappear(fullWindow: fullWindow) }
    }

    private func applyLimit() {
        guard let value = Int(draftLimit), (1...8).contains(value) else { return }
        store.dispatchLimit(value)
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 11) {
            HStack {
                Label("Agent Queue", systemImage: "square.stack.3d.up").font(.system(size: 20, weight: .semibold))
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
        }.padding(18)
    }

    private var queueList: some View {
        List {
            if store.snapshot?.queue.status.lowercased() == "paused" {
                Label("Queue paused. Waiting jobs will not start automatically.", systemImage: "pause.circle")
                    .font(.callout).foregroundStyle(.orange).padding(.vertical, 5)
            }
            if store.snapshot?.queue.legacyOccupied == true {
                Label("A legacy validation batch holds the shared slot.", systemImage: "hourglass")
                    .font(.callout).foregroundStyle(.secondary).padding(.vertical, 5)
            }
            Section {
                if store.running.isEmpty {
                    HStack(spacing: 10) {
                        Image(systemName: store.waiting.isEmpty ? "checkmark.circle" : "hourglass").foregroundStyle(.secondary)
                        VStack(alignment: .leading, spacing: 3) {
                            Text(store.waiting.isEmpty ? "Ready for the next command" : "Waiting for the shared slot").font(.callout)
                            Text(store.waiting.isEmpty ? "Heavy jobs appear here when agents submit them." : "Queued jobs start automatically when the slot is available.")
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
                if job.canReorder {
                    HStack(spacing: 8) {
                        Button("Run next", systemImage: "arrow.up.to.line") { store.dispatch("next", job: job) }
                            .help("Run job #\(job.id) after the current job finishes")
                        Button("Move to end", systemImage: "arrow.down.to.line") { store.dispatch("last", job: job) }
                        Button("Start now", systemImage: "play.fill") { navigation.request(.start, job: job) }
                            .help("Start this job outside the parallel limit")
                    }.font(.caption).buttonStyle(.borderless).disabled(store.actionsDisabled)
                }
            }
            Menu {
                Button("View details and output", systemImage: "doc.text") { navigation.show(job) }
                if job.canReorder {
                    Divider()
                    Button("Run next", systemImage: "arrow.up.to.line") { store.dispatch("next", job: job) }.disabled(store.actionsDisabled)
                    Button("Move to end", systemImage: "arrow.down.to.line") { store.dispatch("last", job: job) }.disabled(store.actionsDisabled)
                    Button("Start now…", systemImage: "play.fill") { navigation.request(.start, job: job) }.disabled(store.actionsDisabled)
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
                if !fullWindow { Button("Open window", systemImage: "arrow.up.left.and.arrow.down.right") { AppRuntime.showWindow() }.buttonStyle(.borderless) }
                Menu {
                    Button("Agent integration…", systemImage: "terminal") { AppRuntime.showIntegrationWindow() }
                    Button("Remote access settings…", systemImage: "network") { AppRuntime.showRemoteWindow() }
                    Divider()
                    Text("Quitting this app leaves jobs running.")
                    Divider()
                    Button("Quit Agent Queue", systemImage: "power") { NSApp.terminate(nil) }.keyboardShortcut("q")
                } label: { Image(systemName: "gearshape") }.menuStyle(.borderlessButton).fixedSize().help("Application options").accessibilityLabel("Application options")
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
            return "This starts the job outside the parallel limit and may increase CPU and memory pressure. Other queued jobs keep their order."
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
    func applicationDidFinishLaunching(_ notification: Notification) {
        AppRuntime.store.start()
        let arguments = CommandLine.arguments
        if let index = arguments.firstIndex(of: "--snapshot"), arguments.indices.contains(index + 1) {
            guard AppRuntime.store.fixtureMode else { diagnostic("--snapshot requires --fixture JSONFILE"); exit(EXIT_FAILURE) }
            AppRuntime.showWindow()
            let navigation = QueueNavigation()
            let verifyIndex = arguments.firstIndex(of: "--verify-detail-navigation")
            let verificationPath = verifyIndex.flatMap { arguments.indices.contains($0 + 1) ? arguments[$0 + 1] : nil }
            let confirmVerifyIndex = arguments.firstIndex(of: "--verify-confirmation-navigation")
            let confirmationVerificationPath = confirmVerifyIndex.flatMap { arguments.indices.contains($0 + 1) ? arguments[$0 + 1] : nil }
            let passwordVerifyIndex = arguments.firstIndex(of: "--verify-password-validation")
            let passwordVerificationPath = passwordVerifyIndex.flatMap { arguments.indices.contains($0 + 1) ? arguments[$0 + 1] : nil }
            let remoteStopVerifyIndex = arguments.firstIndex(of: "--verify-remote-stop-state")
            let remoteStopVerificationPath = remoteStopVerifyIndex.flatMap { arguments.indices.contains($0 + 1) ? arguments[$0 + 1] : nil }
            let confirmIndex = arguments.firstIndex(of: "--snapshot-confirmation")
            let requestedConfirmation = confirmIndex.flatMap {
                arguments.indices.contains($0 + 1) ? ConfirmationAction(rawValue: arguments[$0 + 1]) : nil
            }
            if let action = requestedConfirmation ?? (confirmationVerificationPath == nil ? nil : .start) {
                guard let job = AppRuntime.store.snapshot?.jobs.first(where: {
                    action == .start ? $0.isWaiting && $0.canReorder : $0.canCancel
                }) else {
                    diagnostic("Confirmation snapshot requires an eligible job in the fixture."); exit(EXIT_FAILURE)
                }
                navigation.request(action, job: job)
            } else if let detailIndex = arguments.firstIndex(of: "--snapshot-detail"), arguments.indices.contains(detailIndex + 1) {
                guard let jobID = Int(arguments[detailIndex + 1]),
                      let job = AppRuntime.store.snapshot?.jobs.first(where: { $0.id == jobID }) else {
                    diagnostic("--snapshot-detail requires a job ID present in the fixture."); exit(EXIT_FAILURE)
                }
                navigation.show(job)
            } else if verificationPath != nil {
                guard let job = AppRuntime.store.snapshot?.jobs.first else {
                    diagnostic("Navigation verification requires a fixture containing a job."); exit(EXIT_FAILURE)
                }
                navigation.show(job)
            }
            if let window = AppRuntime.queueWindow {
                let menuSize = arguments.contains("--snapshot-menu")
                window.contentView = NSHostingView(rootView: QueuePanel(store: AppRuntime.store, fullWindow: !menuSize, navigation: navigation))
                if menuSize {
                    window.minSize = NSSize(width: 500, height: 650)
                    window.setContentSize(NSSize(width: 500, height: 650))
                    window.center()
                }
                if arguments.contains("--snapshot-remote-settings") {
                    window.contentView = NSHostingView(rootView: RemoteAccessSettings(store: AppRuntime.remoteStore))
                    window.minSize = NSSize(width: 500, height: 570)
                    window.setContentSize(NSSize(width: 580, height: 780))
                    window.center()
                }
                if arguments.contains("--snapshot-agent-integration") {
                    window.contentView = NSHostingView(rootView: AgentIntegrationSettings(store: AppRuntime.integrationStore) {})
                    window.minSize = NSSize(width: 520, height: 540)
                    window.setContentSize(NSSize(width: 600, height: 720))
                    window.center()
                }
            }
            Task { @MainActor in
                try? await Task.sleep(for: .milliseconds(500))
                guard let window = AppRuntime.queueWindow, let view = window.contentView else {
                    self.diagnostic("Could not create snapshot view."); exit(EXIT_FAILURE)
                }
                do {
                    let snapshotPath = arguments[index + 1]
                    try self.writeSnapshot(view, to: snapshotPath)
                    if let remoteStopVerificationPath {
                        let remote = AppRuntime.remoteStore
                        remote.error = nil
                        await remote.refresh()
                        let report = RemoteStopStateVerification(label: remote.statusLabel,
                            enabled: remote.status?.enabled ?? true, error: remote.effectiveError,
                            warningIndicator: remote.needsAttention,
                            preservedAfterRefresh: remote.statusLabel == "Needs attention")
                        let encoder = JSONEncoder()
                        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                        try encoder.encode(report).write(to: URL(fileURLWithPath: remoteStopVerificationPath))
                        guard report.passed else { throw ClientError.message("Remote stop-state regression failed; inspect the verification report.") }
                    }
                    if let passwordVerificationPath {
                        let report = self.verifyPasswordValidation()
                        let encoder = JSONEncoder()
                        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                        try encoder.encode(report).write(to: URL(fileURLWithPath: passwordVerificationPath))
                        guard report.passed else { throw ClientError.message("Password validation regression failed; inspect the verification report.") }
                    }
                    if let confirmationVerificationPath {
                        let report = try await self.verifyConfirmations(navigation: navigation,
                            store: AppRuntime.store, window: window, view: view)
                        let encoder = JSONEncoder()
                        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                        try encoder.encode(report).write(to: URL(fileURLWithPath: confirmationVerificationPath))
                        guard report.passed else { throw ClientError.message("Confirmation navigation regression failed; inspect the verification report.") }
                        let returnedPath = URL(fileURLWithPath: snapshotPath).deletingPathExtension().path + "-returned.png"
                        try self.writeSnapshot(view, to: returnedPath)
                    }
                    if let verificationPath {
                        let openedJob = navigation.selected?.id
                        // Done calls this same local navigation handler. No sheet or
                        // environment dismiss action participates in returning.
                        navigation.back()
                        try? await Task.sleep(for: .milliseconds(150))
                        let report = NavigationVerification(openedJob: openedJob,
                            returnedToQueue: navigation.selected == nil,
                            windowStayedVisible: window.isVisible,
                            hostingViewUnchanged: window.contentView === view,
                            noAttachedSheet: window.attachedSheet == nil)
                        let encoder = JSONEncoder()
                        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                        try encoder.encode(report).write(to: URL(fileURLWithPath: verificationPath))
                        guard report.passed else { throw ClientError.message("Detail navigation regression failed; inspect the verification report.") }
                        let returnedPath = URL(fileURLWithPath: snapshotPath).deletingPathExtension().path + "-returned.png"
                        try self.writeSnapshot(view, to: returnedPath)
                    }
                } catch { self.diagnostic("Snapshot failed: \(error.localizedDescription)"); exit(EXIT_FAILURE) }
                NSApp.terminate(nil)
            }
        } else {
            if arguments.contains("--open-window") { AppRuntime.showWindow() }
            if AgentOnboarding.shouldOffer(defaults: .standard, fixtureMode: AppRuntime.store.fixtureMode) {
                AppRuntime.showIntegrationWindow()
                UserDefaults.standard.set(true, forKey: AgentOnboarding.offeredKey)
            }
        }
    }

    private func verifyPasswordValidation() -> PasswordValidationVerification {
        let candidates: [(String, String, Bool)] = [
            ("Empty rejected", "", false),
            ("ASCII below minimum", String(repeating: "a", count: 3), false),
            ("Simple word accepted", "word", true),
            ("Four-digit PIN accepted", "2468", true),
            ("Lowercase letters accepted", "simple", true),
            ("ASCII 72 bytes", String(repeating: "a", count: 72), true),
            ("ASCII 73 bytes", String(repeating: "a", count: 73), false),
            ("Two-byte characters at 72 bytes", String(repeating: "é", count: 36), true),
            ("Two-byte characters exceed 72 bytes", String(repeating: "é", count: 37), false),
            ("Three-byte characters at 72 bytes", String(repeating: "界", count: 24), true),
            ("Three-byte characters exceed 72 bytes", String(repeating: "界", count: 25), false),
            ("Printable ASCII spaces", "fixture password with spaces", true),
            ("Newline rejected", "fixture-password\n", false),
            ("Tab rejected", "fixture-password\t", false),
            ("Null rejected", "fixture-password\u{0}", false),
            ("Format character rejected", "fixture-password\u{200D}", false),
            ("Private-use character rejected", "fixture-password\u{E000}", false),
            ("Non-printable separator rejected", "fixture-password\u{00A0}", false)
        ]
        let cases = candidates.map { name, value, expected in
            PasswordValidationCase(name: name, expected: expected, accepted: RemoteAccessStore.acceptsPassword(value))
        }
        let remote = AppRuntime.remoteStore
        let enabledBefore = remote.status?.enabled
        let fixtureConfigurationRejected = !remote.configure(password: "fixture-only-password", enable: true)
        remote.setEnabled(true)
        return PasswordValidationVerification(cases: cases,
            fixtureConfigurationRejected: fixtureConfigurationRejected,
            fixtureEnableRejected: remote.status?.enabled == enabledBefore && !remote.isBusy)
    }

    private func verifyConfirmations(navigation: QueueNavigation, store: QueueStore,
                                     window: NSWindow, view: NSView) async throws -> ConfirmationVerification {
        guard store.fixtureMode, let recorder = store.fixtureRecorder, let original = store.snapshot,
              let queued = original.jobs.first(where: { $0.isWaiting && $0.canReorder && $0.canCancel }) else {
            throw ClientError.message("Confirmation verification requires a fixture with an eligible waiting job.")
        }
        let paused = QueueJob(id: -100, state: "paused", title: "Paused cancellation regression",
            project: "Fixture", branch: "", directory: "", executable: "", priority: 0, queuePosition: nil,
            submittedAt: nil, startedAt: nil, finishedAt: nil, exitCode: nil, logPath: "",
            canReorder: false, canCancel: true, phase: "Paused")
        store.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue, jobs: original.jobs + [paused])
        defer { navigation.decline(); store.snapshot = original; store.isMutating = false }

        func sameSurface() -> Bool {
            window.isVisible && window.contentView === view && window.attachedSheet == nil
        }
        var results: [ConfirmationCaseVerification] = []
        var cases: [(ConfirmationAction, QueueJob)] = [(.start, queued), (.cancel, queued), (.cancel, paused)]
        if let running = original.jobs.first(where: { $0.isRunning && $0.canCancel }) { cases.append((.cancel, running)) }
        for (action, job) in cases {
            let before = recorder.actions.count
            navigation.request(action, job: job)
            try await Task.sleep(for: .milliseconds(60))
            var surfaceStayedVisible = sameSurface()
            let openingDidNotDispatch = recorder.actions.count == before
            navigation.decline()
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let declineDidNotDispatch = recorder.actions.count == before && navigation.confirmation == nil

            navigation.request(action, job: job)
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let firstDispatch = navigation.confirm(in: store)
            let reservationHeldBeforeIO = store.isMutating
            let duplicateWasRejected = navigation.confirm(in: store) == nil
            let competingActionWasRejected = store.dispatch("next", job: queued) == nil
            // Exercise the old lost-confirmation seam: a refresh flag changes
            // after confirmation but before the reserved async task can execute.
            store.isRefreshing = true
            await firstDispatch?.value
            store.isRefreshing = false
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let expected = [RecordedQueueAction(action: action.rawValue, jobID: job.id)]
            let exactlyOneExpectedAction = Array(recorder.actions.dropFirst(before)) == expected

            navigation.request(action, job: job)
            store.isMutating = true
            let busyWasRejected = navigation.confirm(in: store) == nil
            store.isMutating = false
            navigation.decline()
            results.append(ConfirmationCaseVerification(action: action.rawValue, jobID: job.id,
                openingDidNotDispatch: openingDidNotDispatch, declineDidNotDispatch: declineDidNotDispatch,
                exactlyOneExpectedAction: exactlyOneExpectedAction, duplicateWasRejected: duplicateWasRejected,
                reservationHeldBeforeIO: reservationHeldBeforeIO, competingActionWasRejected: competingActionWasRejected,
                refreshDidNotLoseAcceptedAction: exactlyOneExpectedAction,
                busyWasRejected: busyWasRejected, returnedToQueue: navigation.confirmation == nil,
                surfaceStayedVisible: surfaceStayedVisible))
        }

        let beforeStale = recorder.actions.count
        navigation.request(.start, job: queued)
        // The same ID has become paused while the confirmation panel was open.
        let transitioned = QueueJob(id: queued.id, state: "paused", title: queued.title, project: queued.project,
            branch: queued.branch, directory: queued.directory, executable: queued.executable, priority: queued.priority,
            queuePosition: nil, submittedAt: queued.submittedAt, startedAt: queued.startedAt,
            finishedAt: nil, exitCode: nil, logPath: queued.logPath, canReorder: false, canCancel: true, phase: "Paused")
        store.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue,
            jobs: original.jobs.map { $0.id == queued.id ? transitioned : $0 })
        let staleJobExplained = navigation.unavailability(in: store) != nil
        let staleJobRejected = navigation.confirm(in: store) == nil && recorder.actions.count == beforeStale
        navigation.decline()
        // The recorder makes every fixture action inert, including accidental limit changes.
        store.dispatchLimit(2)
        let fixtureLimitDidNotDispatch = recorder.actions.count == beforeStale
        let restoredSurface = sameSurface()
        store.snapshot = original
        try await Task.sleep(for: .milliseconds(80))
        return ConfirmationVerification(cases: results, staleJobExplained: staleJobExplained,
            staleJobRejected: staleJobRejected, fixtureLimitDidNotDispatch: fixtureLimitDidNotDispatch,
            finalSurfaceStayedVisible: restoredSurface, recordedActions: recorder.actions)
    }

    private func writeSnapshot(_ view: NSView, to path: String) throws {
        view.layoutSubtreeIfNeeded()
        guard let representation = view.bitmapImageRepForCachingDisplay(in: view.bounds) else {
            throw ClientError.message("Could not create snapshot bitmap.")
        }
        view.cacheDisplay(in: view.bounds, to: representation)
        guard let png = representation.representation(using: .png, properties: [:]) else {
            throw ClientError.message("PNG encoding failed.")
        }
        try png.write(to: URL(fileURLWithPath: path))
    }

    private func diagnostic(_ text: String) { FileHandle.standardError.write(Data((text + "\n").utf8)) }
}

struct NavigationVerification: Encodable {
    let openedJob: Int?
    let returnedToQueue: Bool
    let windowStayedVisible: Bool
    let hostingViewUnchanged: Bool
    let noAttachedSheet: Bool
    var passed: Bool { openedJob != nil && returnedToQueue && windowStayedVisible && hostingViewUnchanged && noAttachedSheet }
}

struct RemoteStopStateVerification: Encodable {
    let label: String
    let enabled: Bool
    let error: String?
    let warningIndicator: Bool
    let preservedAfterRefresh: Bool
    var passed: Bool {
        label == "Needs attention" && !enabled && error == "Remote stop incomplete"
            && warningIndicator && preservedAfterRefresh
    }
}

struct PasswordValidationCase: Encodable {
    let name: String
    let expected: Bool
    let accepted: Bool
}

struct PasswordValidationVerification: Encodable {
    let cases: [PasswordValidationCase]
    let fixtureConfigurationRejected: Bool
    let fixtureEnableRejected: Bool
    var passed: Bool {
        !cases.isEmpty && cases.allSatisfy { $0.expected == $0.accepted }
            && fixtureConfigurationRejected && fixtureEnableRejected
    }
}

struct ConfirmationCaseVerification: Encodable {
    let action: String
    let jobID: Int
    let openingDidNotDispatch: Bool
    let declineDidNotDispatch: Bool
    let exactlyOneExpectedAction: Bool
    let duplicateWasRejected: Bool
    let reservationHeldBeforeIO: Bool
    let competingActionWasRejected: Bool
    let refreshDidNotLoseAcceptedAction: Bool
    let busyWasRejected: Bool
    let returnedToQueue: Bool
    let surfaceStayedVisible: Bool
    var passed: Bool {
        openingDidNotDispatch && declineDidNotDispatch && exactlyOneExpectedAction && duplicateWasRejected
            && reservationHeldBeforeIO && competingActionWasRejected && refreshDidNotLoseAcceptedAction
            && busyWasRejected && returnedToQueue && surfaceStayedVisible
    }
}

struct ConfirmationVerification: Encodable {
    let cases: [ConfirmationCaseVerification]
    let staleJobExplained: Bool
    let staleJobRejected: Bool
    let fixtureLimitDidNotDispatch: Bool
    let finalSurfaceStayedVisible: Bool
    let recordedActions: [RecordedQueueAction]
    var passed: Bool {
        !cases.isEmpty && cases.allSatisfy(\.passed) && staleJobExplained && staleJobRejected
            && fixtureLimitDidNotDispatch && finalSurfaceStayedVisible
    }
}

@main
struct AgentQueueApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @StateObject private var store = AppRuntime.store

    var body: some Scene {
        MenuBarExtra {
            QueuePanel(store: store)
        } label: {
            Label {
                if store.error != nil { Text("!") }
                else if !store.badge.isEmpty { Text(store.badge).monospacedDigit() }
            } icon: { Image(systemName: "square.stack.3d.up") }
            .accessibilityLabel("Agent Queue, \(store.snapshot?.queue.running ?? 0) running, \(store.snapshot?.queue.waiting ?? 0) waiting")
        }.menuBarExtraStyle(.window)
    }
}
