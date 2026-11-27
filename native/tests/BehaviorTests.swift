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
        try checkQueueRowSummaries(check)
        let fixture = QueueFixture(arguments: CommandLine.arguments)
        let coordinator = fixture.coordinator()
        let store = coordinator.store
        guard fixture.loadError == nil, let queued = store.waiting.first, let original = store.snapshot else {
            throw ClientError.message("Native behavior tests require --fixture JSONFILE with a waiting job.")
        }
        let navigation = QueueNavigation()
        let paused = QueueSnapshot(updatedAt: original.updatedAt,
            queue: QueueSummary(status: "paused", limit: original.queue.limit, running: 0,
                                waiting: original.queue.waiting, legacyOccupied: false), jobs: original.jobs)
        fixture.snapshot = paused
        store.snapshot = paused
        await store.dispatch(.resume)?.value
        try check(fixture.changes == [.resume] && store.notice == "Queue resumed." && store.error == nil && !store.isMutating,
                  "Fixture Resume runs the live pipeline and ends at the recorder")
        fixture.snapshot = original
        store.snapshot = original
        navigation.show(queued)
        navigation.back()
        try check(navigation.selected == nil, "Done returns to queue navigation")
        for kind in [QueueConfirmationKind.start, .cancel] {
            let before = fixture.changes.count
            navigation.request(kind, job: queued)
            try check(fixture.changes.count == before, "Opening confirmation sends no action")
            navigation.decline()
            try check(fixture.changes.count == before && navigation.confirmation == nil, "Declining confirmation sends no action")
            navigation.request(kind, job: queued)
            let task = navigation.confirm(in: store)
            try check(task != nil && store.isMutating, "Confirm reserves action before I/O")
            try check(navigation.confirm(in: store) == nil, "Duplicate confirmation is rejected")
            await task?.value
            try check(Array(fixture.changes.dropFirst(before)) == [kind.action(for: queued).command],
                      "Confirmed action dispatches exactly once")
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
        try check(decoded.canStart == false && !QueueAction.start(decoded).isAllowed(in: nil),
                  "Native decoding preserves explicit start denial")
        store.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue, jobs: [measured])
        navigation.request(.start, job: queued)
        try check(navigation.unavailability(in: store) != nil && navigation.confirm(in: store) == nil,
                  "Start confirmation rechecks explicit permission")
        store.snapshot = original
        navigation.decline()
        func job(_ id: Int, state: String, canReorder: Bool, canCancel: Bool, canStart: Bool? = nil) -> QueueJob {
            var job = QueueJob(id: id, state: state, title: "Action rules", project: "Fixture", branch: "", directory: "",
                executable: "", priority: 0, queuePosition: nil, submittedAt: nil, startedAt: nil, finishedAt: nil,
                exitCode: nil, logPath: "", canReorder: canReorder, canCancel: canCancel, phase: state)
            job.canStart = canStart
            return job
        }
        let movable = job(7, state: "queued", canReorder: true, canCancel: true)
        let pinned = job(8, state: "waiting", canReorder: false, canCancel: true)
        let denied = job(9, state: "waiting", canReorder: true, canCancel: true, canStart: false)
        let granted = job(12, state: "waiting", canReorder: false, canCancel: true, canStart: true)
        let running = job(10, state: "running", canReorder: true, canCancel: true)
        let finished = job(11, state: "succeeded", canReorder: false, canCancel: false)
        func allowed(_ action: QueueAction, _ expected: Bool, _ name: String) throws {
            try check(action.isAllowed(in: nil) == expected, name)
        }
        try allowed(.next(movable), true, "Run next accepts a waiting reorderable job")
        try allowed(.next(pinned), false, "Run next refuses a job that cannot be reordered")
        try allowed(.next(running), false, "Run next refuses a running job")
        try allowed(.next(finished), false, "Run next refuses a finished job")
        try allowed(.last(movable), true, "Move to end accepts a waiting reorderable job")
        try allowed(.last(running), false, "Move to end refuses a running job")
        try allowed(.start(movable), true, "Start now falls back to reorderability when no start permission is reported")
        try allowed(.start(denied), false, "Start now refuses a waiting job whose start permission is false, even when reorderable")
        try allowed(.start(granted), true, "Start now accepts a waiting job whose start permission is true, even when not reorderable")
        try allowed(.start(running), false, "Start now refuses a running job")
        try allowed(.cancel(movable), true, "Cancel accepts a cancellable waiting job")
        try allowed(.cancel(running), true, "Cancel accepts a cancellable running job")
        try allowed(.cancel(finished), false, "Cancel refuses a job that cannot be cancelled")
        let pausedQueue = QueueSnapshot(updatedAt: original.updatedAt, queue: QueueSummary(status: "Paused", limit: 2,
            running: 0, waiting: 1, legacyOccupied: false), jobs: [movable])
        var activeQueue = QueueSnapshot(updatedAt: original.updatedAt, queue: QueueSummary(status: "running", limit: 2,
            running: 0, waiting: 1, legacyOccupied: false), jobs: [movable])
        activeQueue.scheduler = QueueScheduler(mode: .fixed, available: true, reason: "", updatedAt: nil, host: nil)
        try check(QueueAction.resume.isAllowed(in: pausedQueue), "Resume is allowed on a paused queue")
        try check(!QueueAction.resume.isAllowed(in: activeQueue), "Resume is refused on a running queue")
        try check(!QueueAction.resume.isAllowed(in: nil), "Resume is refused without a snapshot")
        for limit in QueueAction.limits {
            try check(QueueAction.limit(limit).isAllowed(in: nil) == true, "Limit \(limit) is in range")
        }
        try check(!QueueAction.limit(0).isAllowed(in: nil) && !QueueAction.limit(9).isAllowed(in: nil), "Limit outside one to eight is refused")
        try check(QueueAction.limit(3).isAllowed(in: activeQueue), "Limit accepts a different value")
        try check(!QueueAction.limit(2).isAllowed(in: activeQueue), "Limit refuses the current value")
        try check(QueueAction.scheduling(.adaptive).isAllowed(in: activeQueue), "Scheduling accepts a different mode")
        try check(!QueueAction.scheduling(.fixed).isAllowed(in: activeQueue), "Scheduling refuses the current mode")
        try check(!QueueAction.scheduling(.adaptive).isAllowed(in: pausedQueue), "Scheduling needs a reported scheduler")
        let sent: [(QueueAction, [String], String, Bool)] = [
            (.next(movable), ["next", "7"], "Job #7 will run next.", false),
            (.last(movable), ["last", "7"], "Job #7 moved to the end.", false),
            (.start(movable), ["start", "7"], "Job #7 requested to start outside the parallel limit.", true),
            (.cancel(movable), ["cancel", "7"], "Cancellation requested for job #7.", true),
            (.resume, ["resume"], "Queue resumed.", false),
            (.limit(3), ["limit", "3"], "Parallel job limit changed to 3.", false),
            (.scheduling(.shadow), ["scheduling", "shadow"], "Scheduling changed to Learn only.", false)]
        for (action, arguments, notice, confirms) in sent {
            try check(action.command.arguments == arguments, "\(action.name) sends its controller arguments")
            try check(action.notice == notice, "\(action.name) shows its notice")
            try check((action.confirmation != nil) == confirms, "\(action.name) confirmation need")
        }
        for kind in [QueueConfirmationKind.start, .cancel] {
            try check(kind.action(for: movable).confirmation == kind && kind.action(for: movable).name == kind.rawValue,
                      "\(kind.rawValue) confirmation builds the matching action")
        }
        // Any command reaching this store fails and sets its error.
        let rejecting = QueueStore(statusLoader: { Data() }, command: { _ in throw ClientError.message("A command was sent") })
        rejecting.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue, jobs: [running])
        try check(rejecting.dispatch(.next(running)) == nil && !rejecting.isMutating, "The store rejects a disallowed action")
        await rejecting.dispatch(.cancel(running))?.value
        try check(rejecting.error?.contains("A command was sent") == true, "The rejecting store sends an allowed action")
        // Surfaces go through navigation: confirming actions only ask, the others are sent.
        let surface = QueueNavigation()
        rejecting.error = nil
        try check(surface.perform(.cancel(running), in: rejecting) == nil, "A confirming action sends nothing yet")
        try check(surface.confirmation?.kind == .cancel && !rejecting.isMutating, "A confirming action asks before sending")
        surface.decline()
        rejecting.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue, jobs: [movable])
        let sentTask = surface.perform(.next(movable), in: rejecting)
        try check(sentTask != nil && surface.confirmation == nil && rejecting.isMutating, "A non-confirming action is sent without asking")
        await sentTask?.value
        try check(!rejecting.isMutating && rejecting.error?.contains("A command was sent") == true, "The sent action reaches the command")
        let passwords: [(String, Bool)] = [("", false), ("abc", false), ("word", true), ("2468", true),
            ("simple", true), (String(repeating: "a", count: 72), true),
            (String(repeating: "a", count: 73), false), (String(repeating: "é", count: 36), true),
            (String(repeating: "é", count: 37), false), ("word\n", false), ("word\t", false)]
        for (value, expected) in passwords {
            try check(RemoteAccessStore.acceptsPassword(value) == expected, "Remote password boundary")
        }
        let remote = coordinator.remoteStore
        await remote.refresh()
        let beforeCredentials = fixture.changes.count
        let configuring = remote.configure(password: "word", enable: false)
        for await busy in remote.$isBusy.values where !busy { break }
        try check(configuring && fixture.changes.dropFirst(beforeCredentials).map(\.controller) == [.remote]
                  && fixture.changes.last?.arguments == ["configure"],
                  "Fixture records remote credential changes instead of applying them")

        let remoteFixture = RemoteFixture()
        let liveRemote = RemoteAccessStore(command: { try await remoteFixture.load($0) })
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
        let preview = coordinator.integrationStore
        await preview.refresh()
        preview.select("codex", enabled: true)
        let beforeInstructions = fixture.changes.count
        await preview.save()?.value
        try check(Array(fixture.changes.dropFirst(beforeInstructions)) == [.agents("install", runner: "codex")]
                  && preview.error == nil, "Preview instruction changes end at the recorder")

        // A launch that names a fixture never falls back to the live backend.
        for arguments in [["app", "--fixture", "/nonexistent/queue.json"], ["app", "--fixture"], ["app", "--fixture=queue.json"]] {
            guard let broken = QueueFixture.launch(arguments: arguments), let launched = QueueFixture.launched,
                  launched.coordinator === broken else {
                throw ClientError.message("A fixture launch must build a fixture coordinator: \(arguments)")
            }
            await broken.store.refresh()
            try check(broken.store.error?.contains("Fixture could not be loaded") == true && broken.store.actionsDisabled
                      && broken.store.dispatch(.limit(2)) == nil && launched.fixture.changes.isEmpty,
                      "An unloadable fixture stays inert and says why: \(arguments.dropFirst().joined(separator: " "))")
        }
        try check(QueueFixture.launch(arguments: ["app", "--setup"]) == nil, "A launch without a fixture gets no stand-in backend")
        do {
            _ = try fixture.respond(.setup(["install", "--payload", "/example/payload"]))
            try check(false, "The fixture refuses commands it has no answer for")
        } catch {
            try check(error.localizedDescription.contains("no answer"), "The fixture refuses commands it has no answer for")
        }
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
        func mark(_ status: String, running: Int, drainUntil: String? = nil, error: String? = nil) -> QueueStatusPresentation {
            QueueStatusPresentation(snapshot: QueueSnapshot(updatedAt: "fixture", queue: QueueSummary(
                status: status, limit: 1, running: running, waiting: 3, legacyOccupied: false, drainUntil: drainUntil), jobs: []), error: error)
        }
        try check(presentation.mark == .running && mark("running", running: 0).mark == .idle
            && mark("running", running: 2, drainUntil: "2026-11-27T12:00:00Z").mark == .draining
            && mark("paused", running: 2, drainUntil: "2026-11-27T12:00:00Z").mark == .paused
            && mark("paused", running: 2, error: "Fixture error").mark == .attention,
                  "Status mark follows running, idle, draining, paused and unavailable, in that precedence")
        try check(mark("paused", running: 2).title == "2 · 3"
            && mark("paused", running: 2).accessibilityLabel == "Agent Queue, paused, 2 running, 3 waiting"
            && mark("running", running: 2, drainUntil: "2026-11-27T12:00:00Z").accessibilityLabel == "Agent Queue, draining, 2 running, 3 waiting",
                  "Paused and draining keep the counts and say so")
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

// Rows condense resources into one line; the waiting reason is not repeated when the phase already says it.
func checkQueueRowSummaries(_ check: (Bool, String) throws -> Void) throws {
    let fixtures = URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("fixtures")
    let snapshot = try JSONDecoder().decode(QueueSnapshot.self, from: Data(contentsOf: fixtures.appendingPathComponent("queue.json")))
    guard let running = snapshot.jobs.first(where: { $0.isRunning && $0.resources?.observed != nil }),
          let waiting = snapshot.jobs.first(where: { $0.isWaiting && $0.resources?.decision != nil }),
          let reason = waiting.resources?.decision?.reason else {
        throw ClientError.message("Generated status lacks running and waiting jobs with resources")
    }
    let runningLine = running.resourceLine
    try check(runningLine?.text.contains(" now · peak ") == true && runningLine?.text.contains("est. ") == true
              && runningLine?.details.contains(running.resources?.prediction.summary ?? "?") == true,
              "Running row shows observed memory against the estimate")
    let waitingLine = waiting.resourceLine
    try check(waitingLine?.text.hasPrefix("Waiting For Memory Headroom · Est. ") == true
              && waitingLine?.text.lowercased().components(separatedBy: reason.lowercased()).count == 2
              && waitingLine?.details.hasSuffix(reason) == true,
              "Waiting row states its reason once")
    var bare = running
    bare.resources = nil
    try check(bare.distinctPhase == nil && bare.resourceLine == nil, "Rows without phase or resources omit the resource line")
}
