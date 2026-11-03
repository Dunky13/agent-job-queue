@testable import AgentQueueCore
import Foundation
import AppKit

actor IntegrationFixture {
    var configured: Set<String> = []
    var calls: [[String]] = []
    var failClaude = false
    private var held: CheckedContinuation<Void, Never>?
    private var started: CheckedContinuation<Void, Never>?
    private var shouldHold = false

    func holdNextMutation() { shouldHold = true }
    func waitForMutation() async {
        if held != nil { return }
        await withCheckedContinuation { started = $0 }
    }
    func releaseMutation() { held?.resume(); held = nil }
    func failSecondRunner() { failClaude = true }
    func load(_ arguments: [String]) async throws -> Data {
        calls.append(arguments)
        if arguments[1] != "status" {
            if shouldHold {
                shouldHold = false
                await withCheckedContinuation {
                    held = $0
                    started?.resume()
                    started = nil
                }
            }
            let id = arguments[3]
            if id == "claude" && failClaude { throw ClientError.message("Instruction file is read-only") }
            if arguments[1] == "install" { configured.insert(id) } else { configured.remove(id) }
        }
        return try JSONEncoder().encode(AgentIntegrationSnapshot(runners: [
            AgentRunnerConfiguration(id: "codex", name: "Codex", path: "~/.codex/AGENTS.md", configured: configured.contains("codex"), error: nil),
            AgentRunnerConfiguration(id: "claude", name: "Claude Code", path: "~/.claude/CLAUDE.md", configured: configured.contains("claude"), error: nil)
        ], instruction: "Shared queue instructions"))
    }
}

actor RemoteFixture {
    var commands: [QueueClient.Command] = []
    var failure: String?
    func fail(_ message: String?) { failure = message }
    func load(_ command: QueueClient.Command) throws -> Data {
        commands.append(command)
        if let failure { throw ClientError.message(failure) }
        return try JSONEncoder().encode(RemoteStatus(enabled: command.arguments != ["disable"], configured: true, running: false,
            url: "https://192.0.2.10:8743/", port: 8743, username: "agent", certificatePath: "", error: nil))
    }
}

@main
struct BehaviorTests {
    @MainActor static func main() async throws {
        func check(_ value: Bool, _ name: String) throws {
            FileHandle.standardOutput.write(Data("\(value ? "PASS" : "FAIL") \(name)\n".utf8))
            if !value { throw ClientError.message(name) }
        }
        try checkMenuBarBehavior(check)
        try checkLaneStatusDecoding(check)
        let store = QueueStore()
        guard store.fixtureMode, let recorder = store.fixtureRecorder,
              let queued = store.waiting.first, let original = store.snapshot else {
            throw ClientError.message("Native behavior tests require a recorded fixture.")
        }
        let navigation = QueueNavigation()
        store.snapshot = QueueSnapshot(updatedAt: original.updatedAt,
            queue: QueueSummary(status: "paused", limit: original.queue.limit, running: 0,
                                waiting: original.queue.waiting, legacyOccupied: false), jobs: original.jobs)
        let beforeResume = recorder.actions.count
        try check(store.dispatchResume() == nil && recorder.actions.count == beforeResume,
                  "Read-only fixtures cannot dispatch Resume even with an action recorder")
        store.snapshot = original
        navigation.show(queued)
        navigation.back()
        try check(navigation.selected == nil, "Done returns to queue navigation")
        for action in [ConfirmationAction.start, .cancel] {
            let before = recorder.actions.count
            navigation.request(action, job: queued)
            try check(recorder.actions.count == before, "Opening confirmation sends no action")
            navigation.decline()
            try check(recorder.actions.count == before && navigation.confirmation == nil, "Declining confirmation sends no action")
            navigation.request(action, job: queued)
            let task = navigation.confirm(in: store)
            try check(task != nil && store.isMutating, "Confirm reserves action before I/O")
            try check(navigation.confirm(in: store) == nil, "Duplicate confirmation is rejected")
            await task?.value
            try check(recorder.actions.count == before + 1, "Confirmed action dispatches exactly once")
        }
        navigation.request(.start, job: queued)
        store.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue, jobs: [])
        try check(navigation.unavailability(in: store) != nil && navigation.confirm(in: store) == nil, "A stale job cannot be confirmed")
        store.snapshot = original
        var measured = queued
        measured.canStart = false
        measured.resources = JobResources(prediction: ResourcePrediction(memoryBytes: 2_147_483_648,
            cpuCores: 2, durationSeconds: nil, samples: 3, confidence: "low", source: "family"),
            observed: ResourceObservation(memoryBytes: 1_073_741_824, peakMemoryBytes: 1_610_612_736,
                cpuSeconds: 20, cpuCores: 1.5, durationSeconds: 15, samples: 5, complete: false),
            decision: ResourceDecision(allowed: false, reason: "Waiting for memory headroom"))
        let decoded = try JSONDecoder().decode(QueueJob.self, from: JSONEncoder().encode(measured))
        try check(decoded.resources == measured.resources && decoded.resources?.prediction.summary.contains("low confidence") == true,
                  "Native resource predictions retain worktree family confidence")
        let expectedPeak = "peak 1\(Locale.current.decimalSeparator ?? ".")5 GiB"
        try check(decoded.resources?.observed?.summary.contains("Memory 1 GiB") == true &&
                  decoded.resources?.observed?.summary.contains(expectedPeak) == true,
                  "Native observations show current and peak memory with the current locale")
        try check(decoded.canStart == false && !decoded.permitsStart,
                  "Native decoding preserves explicit start denial")
        store.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue, jobs: [measured])
        navigation.request(.start, job: queued)
        try check(navigation.unavailability(in: store) != nil && navigation.confirm(in: store) == nil,
                  "Start confirmation rechecks explicit permission")
        store.snapshot = original
        navigation.decline()
        let passwords: [(String, Bool)] = [("", false), ("abc", false), ("word", true), ("2468", true),
            ("simple", true), (String(repeating: "a", count: 72), true),
            (String(repeating: "a", count: 73), false), (String(repeating: "é", count: 36), true),
            (String(repeating: "é", count: 37), false), ("word\n", false), ("word\t", false)]
        for (value, expected) in passwords {
            try check(RemoteAccessStore.acceptsPassword(value) == expected, "Remote password boundary")
        }
        let remote = RemoteAccessStore(fixtureMode: true)
        try check(remote.fixtureMode && !remote.configure(password: "word", enable: true), "Fixture cannot change actual remote credentials")

        let remoteFixture = RemoteFixture()
        let liveRemote = RemoteAccessStore(fixtureMode: false, command: { try await remoteFixture.load($0) })
        func settleRemote() async throws {
            let deadline = ProcessInfo.processInfo.systemUptime + 3
            while liveRemote.isBusy {
                guard ProcessInfo.processInfo.systemUptime < deadline else { throw ClientError.message("Remote store did not settle") }
                try await Task.sleep(for: .milliseconds(5))
            }
        }
        await liveRemote.refresh()
        var remoteCommands = await remoteFixture.commands
        try check(remoteCommands == [.remoteStatus] && liveRemote.status?.enabled == true, "Remote refresh names its discardable status read")
        liveRemote.setEnabled(false)
        try await settleRemote()
        liveRemote.setEnabled(true)
        try await settleRemote()
        remoteCommands = await remoteFixture.commands
        try check(remoteCommands == [.remoteStatus, .remoteEnabled(false), .remoteEnabled(true)] &&
            liveRemote.status?.enabled == true && liveRemote.notice == "Remote access enabled.",
            "Remote toggles name their long-running lifecycle commands")
        try check(liveRemote.configure(password: "word", enable: true), "Remote configuration starts through the injected command")
        try await settleRemote()
        remoteCommands = await remoteFixture.commands
        let secret = remoteCommands.dropFirst(3).first
        try check(remoteCommands.count == 5 && remoteCommands.last == .remoteEnabled(true) &&
            secret?.arguments == ["configure"] && secret?.input.map { String(decoding: $0, as: UTF8.self) } == #"{"password":"word"}"#,
            "Remote configuration sends the password only as private input, then enables")
        await remoteFixture.fail("echoed word failure")
        try check(liveRemote.configure(password: "word", enable: false), "Failed remote configuration still starts")
        try await settleRemote()
        try check(liveRemote.error == "echoed [redacted] failure", "Remote failures never reflect the configured password")

        let integrationFixture = IntegrationFixture()
        let integration = AgentIntegrationStore(command: { try await integrationFixture.load($0.arguments) })
        let initialIntegrationCalls = await integrationFixture.calls
        try check(initialIntegrationCalls.isEmpty && !integration.canSave && QueueAppAction.settings.contains(.integration),
            "Agent integration remains opt-in through settings and performs no implicit I/O")
        await integration.refresh()
        try check(integration.selected.isEmpty && !integration.canSave, "Integration starts with no runners selected")
        integration.select("codex", enabled: true)
        await integrationFixture.holdNextMutation()
        let saving = integration.save()
        await integrationFixture.waitForMutation()
        try check(integration.isBusy && integration.save() == nil, "Duplicate saves are rejected before I/O")
        integration.select("claude", enabled: true)
        try check(!integration.selected.contains("claude"), "Selection is stable while saving")
        await integrationFixture.releaseMutation()
        await saving?.value
        let calls = await integrationFixture.calls
        try check(calls == [["agents", "status", "--json"], ["agents", "install", "--runner", "codex", "--json"]], "Only the selected runner is configured")
        try check(integration.snapshot?.runners.first?.configured == true && !integration.hasChanges && integration.notice != nil, "Saved configuration is reflected")
        integration.select("codex", enabled: false)
        await integration.save()?.value
        try check(integration.snapshot?.runners.first?.configured == false, "Deselecting a runner removes its managed section")

        integration.select("codex", enabled: true)
        integration.select("claude", enabled: true)
        await integrationFixture.failSecondRunner()
        await integration.save()?.value
        try check(integration.error != nil && integration.notice == nil && !integration.isBusy, "Partial failure is explained without a success notice")
        try check(integration.snapshot?.runners.first?.configured == true && integration.snapshot?.runners.last?.configured == false, "Partial failure reconciles actual per-runner state")
        let preview = AgentIntegrationStore(fixtureMode: true, command: { _ in
            throw ClientError.message("Preview must not dispatch")
        })
        preview.select("codex", enabled: true)
        await preview.refresh()
        try check(preview.save() == nil && preview.error == nil, "Preview never dispatches instruction changes")
    }

    @MainActor private static func checkMenuBarBehavior(_ check: (Bool, String) throws -> Void) throws {
        func mouse(_ type: NSEvent.EventType, count: Int = 1,
                   modifiers: NSEvent.ModifierFlags = []) throws -> NSEvent {
            guard let event = NSEvent.mouseEvent(with: type, location: .zero, modifierFlags: modifiers,
                timestamp: 0, windowNumber: 0, context: nil, eventNumber: 0, clickCount: count, pressure: 0) else {
                throw ClientError.message("Could not create fixture mouse event")
            }
            return event
        }
        try check(QueueStatusActivation.from(try mouse(.leftMouseUp)) == .primaryClick,
                  "Primary mouse activation keeps the queue popover")
        try check(QueueStatusActivation.from(try mouse(.leftMouseUp, count: 2)) == .doubleClick,
                  "Primary double-click opens the queue window")
        try check(QueueStatusActivation.from(try mouse(.rightMouseUp)) == .contextMenu,
                  "Secondary click opens the context menu")
        try check(QueueStatusActivation.from(try mouse(.leftMouseUp, count: 2, modifiers: .control)) == .contextMenu,
                  "Control-click takes precedence over double-click")
        let key = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: [], timestamp: 0,
            windowNumber: 0, context: nil, characters: " ", charactersIgnoringModifiers: " ", isARepeat: false, keyCode: 49)
        try check(key != nil && QueueStatusActivation.from(key) == .accessible && QueueStatusActivation.from(nil) == .accessible,
                  "Keyboard and accessibility activation never inspect mouse clickCount")

        try check(QueueAppAction.allCases == [.openWindow] + QueueAppAction.settings + [.quit],
                  "Context menu and footer share all application actions in order")
        try check(QueueAppAction.settings.map(\.title) == ["Learned workloads…", "About and updates…", "Agent integration…", "Remote access settings…"]
            && QueueAppAction.quitExplanation == "Quitting stops the queue. Running jobs continue; the queue reopens paused if any were active.",
                  "Application actions retain existing settings and the quit explanation")
        let summary = QueueSummary(status: "running", limit: 1, running: 2, waiting: 3, legacyOccupied: false)
        let snapshot = QueueSnapshot(updatedAt: "fixture", queue: summary, jobs: [])
        let presentation = QueueStatusPresentation(snapshot: snapshot, error: nil)
        try check(presentation.title == "2 · 3" && presentation.accessibilityLabel == "Agent Queue, 2 running, 3 waiting",
                  "Status item preserves running and waiting counts and accessibility")
        try check(QueueStatusPresentation(snapshot: snapshot, error: "Fixture error").title == "!"
            && QueueStatusPresentation(snapshot: nil, error: nil).title.isEmpty,
                  "Status item preserves the error and idle badge states")
    }
}

// Status from a daemon with plugin lanes decodes leases, lanes and plugins;
// status from an older daemon still decodes with those fields absent. The
// fixtures are generated by the Go producer; see docs/plugins.md.
func checkLaneStatusDecoding(_ check: (Bool, String) throws -> Void) throws {
    let fixtures = URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("fixtures")
    let snapshot = try JSONDecoder().decode(QueueSnapshot.self, from: Data(contentsOf: fixtures.appendingPathComponent("queue.json")))
    let variants = try JSONDecoder().decode([String: QueueSnapshot].self, from: Data(contentsOf: fixtures.appendingPathComponent("status-variants.json")))
    try check(variants["empty"]?.jobs.isEmpty == true && variants["empty"]?.lanes?.map(\.name) == ["default"] &&
              variants["leaseStates"]?.jobs.contains { $0.position == 1 } == true,
              "Every generated status variant decodes")
    guard let lease = snapshot.jobs.first(where: \.isLease) else {
        throw ClientError.message("Generated status has no lease")
    }
    try check(lease.isLease && lease.lane == "qa" && lease.meta?["ticket"] == "WM-123" && lease.ownerPid == 4242 && lease.ownerAlive == true,
              "Lease status fields decode")
    try check(snapshot.lanes?.map(\.name) == ["qa", "default"] && snapshot.lanes?.first?.plugin == "qa-plugin" && snapshot.lanes?.last?.plugin == nil,
              "Lanes decode in priority order")
    try check(snapshot.plugins?.first?.loaded == true && snapshot.plugins?.first?.lanes == ["qa"], "Plugins decode")
    var older = snapshot
    older.lanes = nil
    older.plugins = nil
    let reencoded = try JSONDecoder().decode(QueueSnapshot.self, from: JSONEncoder().encode(older))
    try check(reencoded.lanes == nil && !reencoded.hasSameContent(as: snapshot), "Lane changes are visible to refresh comparison")
}
