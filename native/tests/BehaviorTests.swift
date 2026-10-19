import Foundation

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

@main
struct BehaviorTests {
    @MainActor static func main() async throws {
        func check(_ value: Bool, _ name: String) throws {
            FileHandle.standardOutput.write(Data("\(value ? "PASS" : "FAIL") \(name)\n".utf8))
            if !value { throw ClientError.message(name) }
        }
        let store = QueueStore()
        guard store.fixtureMode, let recorder = store.fixtureRecorder,
              let queued = store.waiting.first, let original = store.snapshot else {
            throw ClientError.message("Native behavior tests require a recorded fixture.")
        }
        let navigation = QueueNavigation()
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

        let suite = "agent-queue-onboarding-test-\(UUID().uuidString)"
        guard let defaults = UserDefaults(suiteName: suite) else { throw ClientError.message("No isolated defaults") }
        defer { defaults.removePersistentDomain(forName: suite) }
        try check(AgentOnboarding.shouldOffer(defaults: defaults, fixtureMode: false), "New users receive integration setup")
        try check(!AgentOnboarding.shouldOffer(defaults: defaults, fixtureMode: true), "Fixtures never trigger real onboarding")
        defaults.set(true, forKey: AgentOnboarding.offeredKey)
        try check(!AgentOnboarding.shouldOffer(defaults: defaults, fixtureMode: false), "Dismissed onboarding stays dismissed")

        let integrationFixture = IntegrationFixture()
        let integration = AgentIntegrationStore(command: { try await integrationFixture.load($0) })
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
}
