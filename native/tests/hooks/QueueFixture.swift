// Test-only: the stand-in backend of a `--fixture` run. Reads come from the fixture file and
// canned data, changes are only recorded, and nothing starts a process or leaves the app.
import Foundation

struct RecordedQueueAction: Codable, Equatable {
    let action: String
    let jobID: Int
}

@MainActor
final class QueueFixture {
    /// The fixture and coordinator of this launch; nil unless the app was launched with `--fixture`.
    private(set) static var launched: (fixture: QueueFixture, coordinator: WindowCoordinator)?

    /// What the status read answers. Checks replace it to move the queue under the store.
    var snapshot: QueueSnapshot?
    private(set) var loadError: String?
    private(set) var reads: [QueueClient.Command] = []
    private(set) var changes: [QueueClient.Command] = []
    private(set) var logPaths: [String] = []
    private(set) var desktopRequests: [QueueDesktopRequest] = []
    private let remoteStopFailure: Bool
    private var readWaiters: [(QueueClient.Command, CheckedContinuation<Void, Never>)] = []

    /// Job actions among the changes, as the operator named them.
    var actions: [RecordedQueueAction] {
        changes.compactMap { command in
            guard command.arguments.count == 2, let id = Int(command.arguments[1]),
                  command == .jobAction(command.arguments[0], id: id), command.arguments[0] != "limit" else { return nil }
            return RecordedQueueAction(action: command.arguments[0], jobID: id)
        }
    }

    init(arguments: [String]) {
        remoteStopFailure = arguments.contains("--snapshot-remote-stop-failure")
            || arguments.contains("--verify-remote-stop-state")
        do {
            guard let index = arguments.firstIndex(of: "--fixture"), arguments.indices.contains(index + 1) else {
                throw ClientError.message("--fixture requires a JSONFILE argument.")
            }
            snapshot = try JSONDecoder().decode(QueueSnapshot.self,
                from: Data(contentsOf: URL(fileURLWithPath: arguments[index + 1])))
        } catch { loadError = "Fixture could not be loaded: \(error.localizedDescription)" }
    }

    /// A launch naming a fixture in any form gets this backend, even when the file cannot be loaded.
    static func launch(arguments: [String]) -> WindowCoordinator? {
        guard arguments.contains(where: { $0.hasPrefix("--fixture") }) else { return nil }
        let fixture = QueueFixture(arguments: arguments)
        let coordinator = fixture.coordinator()
        launched = (fixture, coordinator)
        return coordinator
    }

    var backend: QueueBackend {
        QueueBackend(command: { try await self.respond($0) }, logTail: { await self.logTail($0) },
            desktop: { self.desktopRequests.append($0) },
            runningVersion: RunningQueueVersion(version: "1.0.0", bundleBuild: "100", publishedBuild: 100))
    }

    /// Each store loads once and never polls on a timer, so nothing changes under a check.
    func coordinator() -> WindowCoordinator {
        let coordinator = WindowCoordinator(backend: backend, pollSleep: { _ in throw CancellationError() })
        // The queue is on screen from the first frame, before any read completes.
        coordinator.store.snapshot = snapshot
        coordinator.store.lastRefreshed = snapshot == nil ? nil : Date()
        coordinator.store.error = loadError
        return coordinator
    }

    /// Returns once this read has been asked for, so a check never has to time a view's loader.
    func waitForRead(_ command: QueueClient.Command) async {
        if reads.contains(command) { return }
        await withCheckedContinuation { readWaiters.append((command, $0)) }
    }

    private func logTail(_ path: String) -> String {
        logPaths.append(path)
        return "Sample output. A fixture run reads no log file."
    }

    /// Answers one controller command. Anything unknown fails: nothing is ever passed on.
    func respond(_ command: QueueClient.Command) throws -> Data {
        let arguments = command.arguments
        switch (command.controller, arguments.first) {
        case (.remote, "status") where command == .remoteStatus:
            return try read(command, remoteStatus)
        case (.remote, "enable"), (.remote, "disable"), (.remote, "configure"):
            return try change(command, remoteStatus)
        case (.heavy, "status") where command == .status:
            guard let snapshot else { throw ClientError.message(loadError ?? "The fixture has no queue snapshot.") }
            return try read(command, snapshot)
        case (.heavy, "version") where command == .version:
            return try read(command, installed)
        case (.heavy, "next"), (.heavy, "last"), (.heavy, "start"), (.heavy, "cancel"),
             (.heavy, "resume"), (.heavy, "limit"), (.heavy, "scheduling"):
            return try change(command, [String: String]())
        case (.heavy, "agents") where command == .agentsStatus:
            return try read(command, integration)
        case (.heavy, "agents") where arguments.count == 5 && ["install", "remove"].contains(arguments[1]):
            return try change(command, integration)
        case (.heavy, "updates") where command == .updatesCheck:
            return try read(command, QueueUpdateCheck(installed: installed,
                latest: QueueRelease(version: "1.0.1", build: 101, commit: String(repeating: "a", count: 40),
                    tag: "build-101", url: "https://github.com/Dunky13/agent-job-queue/releases/tag/build-101"),
                comparison: .available, canDownload: true, message: "Version 1.0.1 (build 101) is available."))
        case (.heavy, "updates") where arguments.count == 5 && arguments[1] == "download":
            guard let build = Int(arguments[3]) else { break }
            return try change(command, QueueUpdateDownload(downloaded: true, build: build,
                app: "/example/Downloads/Agent Queue.app", message: "Sample download. Nothing was saved."))
        case (.heavy, "profiles") where arguments.count >= 4:
            return try profiles(command)
        default: break
        }
        throw ClientError.message("The fixture has no answer for \(command.controller.rawValue) \(arguments.joined(separator: " ")).")
    }

    private func read(_ command: QueueClient.Command, _ answer: some Encodable) throws -> Data {
        reads.append(command)
        for waiter in readWaiters where waiter.0 == command { waiter.1.resume() }
        readWaiters.removeAll { $0.0 == command }
        return try JSONEncoder().encode(answer)
    }

    private func change(_ command: QueueClient.Command, _ answer: some Encodable) throws -> Data {
        changes.append(command)
        return try JSONEncoder().encode(answer)
    }

    private var remoteStatus: RemoteStatus {
        RemoteStatus(enabled: false, configured: remoteStopFailure, running: false,
            url: "https://192.0.2.10:8743/", port: 8743, username: "agent", certificatePath: "",
            error: remoteStopFailure ? "Remote stop incomplete" : nil)
    }

    private var installed: InstalledQueueVersion {
        InstalledQueueVersion(version: "1.0.0", build: 100, bundleBuild: "100", commit: nil,
            tracked: true, displayVersion: "1.0.0 (build 100)")
    }

    private var integration: AgentIntegrationSnapshot {
        AgentIntegrationSnapshot(runners: [
            AgentRunnerConfiguration(id: "codex", name: "Codex", path: "~/.codex/AGENTS.md", configured: false, error: nil),
            AgentRunnerConfiguration(id: "claude", name: "Claude Code", path: "~/.claude/CLAUDE.md", configured: false, error: nil)
        ], instruction: "Run heavy builds and broad tests through agent-heavy -- <command>.\nKeep lightweight checks outside the queue.")
    }

    private func profiles(_ command: QueueClient.Command) throws -> Data {
        let arguments = command.arguments
        let sample = Self.previewDetail()
        func value(_ option: String) -> String? {
            guard let index = arguments.firstIndex(of: option), arguments.indices.contains(index + 1) else { return nil }
            return arguments[index + 1]
        }
        switch arguments[1] {
        case "list" where arguments.contains("--hierarchy"):
            return try read(command, Self.previewHierarchy())
        case "list":
            let offset = value("--offset").flatMap(Int.init) ?? 0
            return try read(command, LearningCatalog(revision: sample.revision, profiles: offset == 0 ? [sample.profile] : [],
                total: 1, offset: offset, limit: 50,
                controllerAppliedRevision: sample.revision, controllerStatus: "applied"))
        case "show":
            if let family = value("--family") {
                let members = sample.members.filter { $0.family == family }
                return try read(command, detail(members: members, family: family))
            }
            if let query = value("--members-query"), let offset = value("--members-offset").flatMap(Int.init) {
                let matches = sample.members.filter { query.isEmpty || $0.label.localizedCaseInsensitiveContains(query) }
                var page = detail(members: Array(matches.dropFirst(offset)))
                page.memberPage = LearningMemberPage(total: matches.count, offset: offset, limit: 50, query: query)
                return try read(command, page)
            }
            return try read(command, sample)
        case "edit", "reset", "combine":
            return try change(command, detail(id: arguments[2]))
        case "split":
            return try change(command, detail(id: String(repeating: "f", count: 64)))
        case "merge":
            struct Target: Decodable { let targetId: String }
            return try change(command, detail(id: JSONDecoder().decode(Target.self, from: command.input ?? Data()).targetId))
        default:
            throw ClientError.message("The fixture has no answer for profiles \(arguments[1]).")
        }
    }

    /// The sample group, narrowed to some members or answering under another group's identity.
    private func detail(id: String? = nil, members: [LearningMember]? = nil, family: String? = nil) -> LearningDetail {
        let sample = Self.previewDetail()
        let old = sample.profile
        let profile = LearningProfile(id: id ?? old.id, label: old.label, memberCount: old.memberCount,
            variantCount: old.variantCount, successCount: old.successCount, incompleteCount: old.incompleteCount,
            peakMemoryBytes: old.peakMemoryBytes, peakCpuCores: old.peakCpuCores, lastRunAt: old.lastRunAt,
            memoryFloorBytes: old.memoryFloorBytes, cpuFloorCores: old.cpuFloorCores)
        return LearningDetail(revision: sample.revision, profile: profile, members: members ?? sample.members,
            variants: sample.variants.filter { family == nil || $0.family == family },
            runs: sample.runs.filter { family == nil || $0.family == family },
            prediction: sample.prediction, predictionMachine: sample.predictionMachine, truncated: sample.truncated,
            controllerAppliedRevision: sample.controllerAppliedRevision, controllerStatus: sample.controllerStatus,
            inspectedFamily: family)
    }

    nonisolated static func previewHierarchy() -> LearningHierarchy {
        func operations(_ rows: [(String, Int, Int, Int)]) -> [LearningOperationRow] {
            rows.map { LearningOperationRow(operation: $0.0, workloadCount: $0.1, groupCount: $0.2, runCount: $0.3) }
        }
        return LearningHierarchy(revision: 4, projects: [
            LearningProjectRow(project: "Sample project", collapsed: false, workloadCount: 9, groupCount: 3, runCount: 44,
                operations: operations([("tests", 1, 1, 20), ("build", 6, 1, 16), ("lint", 2, 1, 8)])),
            LearningProjectRow(project: "Sample service", collapsed: false, workloadCount: 5, groupCount: 2, runCount: 30,
                operations: operations([("tests", 4, 1, 22), ("race-tests", 1, 1, 8)])),
            LearningProjectRow(project: "Other projects", collapsed: true, projectCount: 3, workloadCount: 3, groupCount: 3, runCount: 9,
                operations: operations([("build", 2, 2, 6), ("other", 1, 1, 3)])),
        ], total: 2, limit: 100, controllerAppliedRevision: 4, controllerStatus: "applied")
    }

    nonisolated static func previewDetail() -> LearningDetail {
        let family = String(repeating: "b", count: 64)
        let variant = String(repeating: "c", count: 64)
        let profile = LearningProfile(id: String(repeating: "a", count: 64), label: "Web test suite", memberCount: 1,
            variantCount: 3, successCount: 18, incompleteCount: 2, peakMemoryBytes: 1_342_177_280,
            peakCpuCores: 1.6, lastRunAt: 1_790_000_000, memoryFloorBytes: 2_147_483_648, cpuFloorCores: nil)
        return LearningDetail(revision: 4, profile: profile,
            members: [LearningMember(family: family, label: "Web tests", project: "Sample project", executable: "npm",
                measurements: LearningMeasurements(variantCount: 3, successCount: 18, incompleteCount: 2,
                    peakMemoryBytes: 1_342_177_280, peakCpuCores: 1.6, lastRunAt: 1_790_000_000))],
            variants: [LearningVariant(id: variant, family: family, successCount: 18, incompleteCount: 2,
                lastRunAt: 1_790_000_000, peakMemoryBytes: 1_342_177_280, peakCpuCores: 1.6)],
            runs: [LearningRun(id: String(repeating: "d", count: 64), family: family, variant: variant, machine: "preview-machine", sampledAt: 1_790_000_000,
                memoryBytes: 1_342_177_280, cpuCores: 1.6, cpuSeconds: 42, durationSeconds: 30, success: true),
                LearningRun(id: String(repeating: "e", count: 64), family: family, variant: variant, machine: "preview-machine", sampledAt: 1_789_999_000,
                    memoryBytes: 1_073_741_824, cpuCores: 1.2, cpuSeconds: 20, durationSeconds: 24, success: false)],
            prediction: ResourcePrediction(memoryBytes: 2_147_483_648, cpuCores: 2.4, durationSeconds: 45,
                samples: 0, confidence: "low", source: "family"), predictionMachine: "preview-machine",
            truncated: LearningTruncation(members: false, variants: false, runs: false),
            controllerAppliedRevision: 4, controllerStatus: "applied")
    }
}
