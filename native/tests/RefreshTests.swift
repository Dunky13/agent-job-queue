@testable import AgentQueueCore
import Foundation
import Combine

actor StatusFixture {
    var data: Data
    var fails = false
    var commands: [[String]] = []
    private var shouldHoldNextRead = false
    private var heldRead: CheckedContinuation<Void, Never>?
    private var readWaiter: CheckedContinuation<Void, Never>?
    private(set) var loads = 0
    init(_ data: Data) { self.data = data }
    func set(_ data: Data, fails: Bool = false) { self.data = data; self.fails = fails }
    func holdNextRead() {
        precondition(!shouldHoldNextRead && heldRead == nil && readWaiter == nil)
        shouldHoldNextRead = true
    }
    func waitUntilReadHeld() async {
        if heldRead != nil { return }
        precondition(shouldHoldNextRead && readWaiter == nil)
        await withCheckedContinuation { readWaiter = $0 }
    }
    func releaseRead() {
        guard let continuation = heldRead else { preconditionFailure("No read is held") }
        heldRead = nil
        continuation.resume()
    }
    func command(_ arguments: [String]) throws -> Data {
        commands.append(arguments)
        var snapshot = try JSONDecoder().decode(QueueSnapshot.self, from: data)
        guard arguments.count == 2, arguments[0] == "scheduling", let mode = SchedulingMode(rawValue: arguments[1]),
              let scheduler = snapshot.scheduler else { throw ClientError.message("Invalid fixture command") }
        if mode == .adaptive && !scheduler.available { throw ClientError.message("Resource sampler unavailable") }
        snapshot.scheduler = QueueScheduler(mode: mode, available: scheduler.available,
            reason: scheduler.reason, updatedAt: scheduler.updatedAt, host: scheduler.host)
        data = try JSONEncoder().encode(snapshot)
        return Data("{}".utf8)
    }
    func load() async throws -> Data {
        loads += 1
        let captured = data
        let failed = fails
        if shouldHoldNextRead {
            shouldHoldNextRead = false
            await withCheckedContinuation {
                heldRead = $0
                readWaiter?.resume()
                readWaiter = nil
            }
        }
        if failed { throw ClientError.message("Fixture disconnected") }
        return captured
    }
}

private actor PreparationFixture {
    private let gate = StatusFixture(Data())
    func hold() async { await gate.holdNextRead() }
    func waitUntilHeld() async { await gate.waitUntilReadHeld() }
    func release() async { await gate.releaseRead() }
    func prepare(_ data: Data, previous: QueueSnapshot?) async throws -> PreparedQueueSnapshot? {
        let result = try await PreparedQueueSnapshot.decode(data, comparedTo: previous)
        _ = try await gate.load()
        return result
    }
}

private actor ResumeFixture {
    let status: StatusFixture
    let commandGate: StatusFixture
    private(set) var commands: [[String]] = []
    var fails = false
    init(status: StatusFixture, commandGate: StatusFixture) { self.status = status; self.commandGate = commandGate }
    func setFailure(_ value: Bool) { fails = value }
    func command(_ arguments: [String]) async throws -> Data {
        commands.append(arguments)
        _ = try await commandGate.load()
        guard arguments == ["resume"], !fails else { throw ClientError.message("Resume rejected by fixture") }
        let previous = try JSONDecoder().decode(QueueSnapshot.self, from: await status.load())
        let queue = previous.queue
        let next = QueueSnapshot(updatedAt: previous.updatedAt,
            queue: QueueSummary(status: "running", limit: queue.limit, running: queue.running,
                                waiting: queue.waiting, legacyOccupied: queue.legacyOccupied), jobs: previous.jobs,
            scheduler: previous.scheduler, lanes: previous.lanes, plugins: previous.plugins)
        await status.set(try JSONEncoder().encode(next))
        return Data("{}".utf8)
    }
}

/// Stands in for the continuous clock: a poll loop parks here until the test moves time.
@MainActor
private final class ManualPollClock {
    private var elapsed = Duration.zero
    private var sleeper: (deadline: Duration, wake: CheckedContinuation<Void, Never>)?
    private var observer: CheckedContinuation<Void, Never>?
    /// Set false when the loop must end instead of sleeping again, so a regression traps instead of hanging.
    var allowsSleep = true
    var pollSleep: PollSleep { { await self.sleep($0) } }

    private func sleep(_ duration: Duration) async {
        await withCheckedContinuation {
            precondition(allowsSleep && sleeper == nil, "Unexpected poll sleep")
            sleeper = (elapsed + duration, $0)
            observer?.resume()
            observer = nil
        }
    }
    /// Returns once the loop sleeps, so everything it did after its last wake has finished.
    func waitUntilAsleep() async {
        if sleeper == nil { await withCheckedContinuation { observer = $0 } }
    }
    /// Moves time forward and wakes the loop if its deadline passed.
    func advance(_ duration: Duration) {
        elapsed += duration
        guard let sleeper, sleeper.deadline <= elapsed else { return }
        self.sleeper = nil
        sleeper.wake.resume()
    }
}

@MainActor
private final class FakeSurface: QueueSurface {
    var isVisible = false
}

private actor CommandLog {
    private(set) var commands: [QueueClient.Command] = []
    func record(_ command: QueueClient.Command) { commands.append(command) }
}

@main
struct RefreshTests {
    /// Queue polling: every 2 s while any surface is visible, otherwise every fifth tick.
    @MainActor private static func checkQueueCadence(_ data: Data, _ check: (Bool, String) throws -> Void) async throws {
        let clock = ManualPollClock()
        let fixture = StatusFixture(data)
        let coordinator = WindowCoordinator(command: { _ in try await fixture.load() }, pollSleep: clock.pollSleep,
                                            arguments: [])
        let store = coordinator.store
        let window = FakeSurface()
        let popover = FakeSurface()
        store.surfaces.add(window)
        store.surfaces.add(popover)
        store.start()
        await clock.waitUntilAsleep()
        var seen = await fixture.loads
        try check(seen == 1, "Polling loads the status once at start")

        /// Loads caused by each of the next ticks; nothing may load a millisecond before a tick is due.
        func ticks(_ count: Int) async throws -> [Int] {
            var result: [Int] = []
            for _ in 0..<count {
                clock.advance(.milliseconds(1999))
                await clock.waitUntilAsleep()
                let early = await fixture.loads
                if early != seen { throw ClientError.message("A status load came before the 2 s tick") }
                clock.advance(.milliseconds(1))
                await clock.waitUntilAsleep()
                let loads = await fixture.loads
                result.append(loads - seen)
                seen = loads
            }
            return result
        }
        var loaded = try await ticks(10)
        try check(loaded == [0, 0, 0, 0, 1, 0, 0, 0, 0, 1], "A hidden queue loads the status every 10 s")
        window.isVisible = true
        loaded = try await ticks(3)
        try check(loaded == [1, 1, 1], "A visible queue window loads the status every 2 s without announcing itself")
        popover.isVisible = true
        loaded = try await ticks(2)
        try check(loaded == [1, 1], "A visible popover and window load once per 2 s tick")
        window.isVisible = false
        loaded = try await ticks(2)
        try check(loaded == [1, 1], "The popover alone keeps the 2 s cadence")
        popover.isVisible = false
        // 17 ticks have passed: the hidden cadence counts from the start, so ticks 20 and 25 load.
        loaded = try await ticks(8)
        try check(loaded == [0, 0, 1, 0, 0, 0, 0, 1], "Hiding every surface returns to the 10 s cadence counted from the start")
        await store.appear().value
        let appeared = await fixture.loads
        try check(appeared == seen + 1, "An appearing surface loads the status at once")
    }

    /// Remote access polls every 5 s while its window is visible, stops once it is not and restarts when shown.
    @MainActor private static func checkRemoteCadence(_ check: (Bool, String) throws -> Void) async throws {
        let clock = ManualPollClock()
        let log = CommandLog()
        let status = try JSONEncoder().encode(RemoteStatus(enabled: false, configured: false, running: false,
            url: "https://192.0.2.10:8743/", port: 8743, username: "agent", certificatePath: "", error: nil))
        let coordinator = WindowCoordinator(command: { await log.record($0); return status }, pollSleep: clock.pollSleep,
                                            arguments: [])
        let store = coordinator.remoteStore
        let window = FakeSurface()
        window.isVisible = true
        store.surfaces.add(window)
        let polling = store.startPolling()
        await clock.waitUntilAsleep()
        var commands = await log.commands
        try check(polling != nil && commands == [.remoteStatus], "A shown remote window loads the status at once")
        try check(store.startPolling() == nil, "Showing a polling remote window again starts no second loop")
        for expected in 2...3 {
            clock.advance(.milliseconds(4999))
            await clock.waitUntilAsleep()
            let early = await log.commands.count
            clock.advance(.milliseconds(1))
            await clock.waitUntilAsleep()
            commands = await log.commands
            try check(early == expected - 1 && commands.count == expected, "A visible remote window loads status \(expected) after 5 s")
        }
        window.isVisible = false
        clock.allowsSleep = false
        clock.advance(.seconds(5))
        await polling?.value
        commands = await log.commands
        try check(commands.count == 3, "A hidden remote window ends polling without another load")
        window.isVisible = true
        clock.allowsSleep = true
        let restarted = store.startPolling()
        await clock.waitUntilAsleep()
        commands = await log.commands
        try check(restarted != nil && commands.count == 4, "Showing the remote window restarts polling")
        clock.advance(.seconds(5))
        await clock.waitUntilAsleep()
        commands = await log.commands
        try check(commands == Array(repeating: .remoteStatus, count: 5), "Restarted polling keeps the 5 s cadence")
    }

    /// Every store the coordinator builds, and the merge-target picker, sends its commands through the injected adapter.
    @MainActor private static func checkCoordinatorAdapters(_ check: (Bool, String) throws -> Void) async throws {
        let log = CommandLog()
        let coordinator = WindowCoordinator(command: { await log.record($0); throw ClientError.message("Recorded") },
                                            arguments: [])
        var count = 0
        func sent(_ name: String) async throws {
            let commands = await log.commands
            try check(commands.count > count, "The coordinator's adapter serves \(name)")
            count = commands.count
        }
        await coordinator.store.refresh()
        try await sent("the queue store")
        await coordinator.remoteStore.refresh()
        try await sent("the remote access store")
        await coordinator.integrationStore.refresh()
        try await sent("the integration store")
        await coordinator.updateStore.refreshInstalled()
        try await sent("the update store")
        await coordinator.learningStore.loadPage()?.value
        try await sent("the learning store")
        await coordinator.learningStore.targetPickerStore().loadPage()?.value
        try await sent("the merge-target picker store")
    }

    @MainActor static func main() async throws {
        func snapshot(_ stamp: String, limit: Int = 1) throws -> Data {
            try JSONEncoder().encode(QueueSnapshot(updatedAt: stamp,
                queue: QueueSummary(status: "running", limit: limit, running: 0, waiting: 0, legacyOccupied: false), jobs: []))
        }
        func check(_ value: Bool, _ name: String) throws {
            FileHandle.standardOutput.write(Data("\(value ? "PASS" : "FAIL") \(name)\n".utf8))
            if !value { throw ClientError.message(name) }
        }
        let fixture = StatusFixture(try snapshot("2026-10-18T10:00:00Z"))
        let store = QueueStore(statusLoader: { try await fixture.load() })
        await store.refresh()
        var publishes = 0
        let subscription = store.objectWillChange.sink { publishes += 1 }
        await fixture.set(try snapshot("2026-10-18T10:00:02Z"))
        await fixture.holdNextRead()
        let quiet = Task { await store.refresh() }
        await fixture.waitUntilReadHeld()
        // Capture every failure before throwing so the pre-fix run explains both causes.
        let controlsStable = !store.actionsDisabled && !store.isRefreshing
        await fixture.releaseRead()
        await quiet.value
        let unchangedWasSilent = publishes == 0
        FileHandle.standardOutput.write(Data("\(controlsStable ? "PASS" : "FAIL") background polling leaves controls enabled\n".utf8))
        FileHandle.standardOutput.write(Data("\(unchangedWasSilent ? "PASS" : "FAIL") unchanged poll emits zero observable updates (got \(publishes))\n".utf8))
        try check(controlsStable && unchangedWasSilent, "unchanged refresh is silent")
        await fixture.set(try snapshot("2026-10-18T10:00:04Z", limit: 2))
        await store.refresh()
        try check(store.snapshot?.queue.limit == 2 && publishes > 0, "real queue changes are published")
        await fixture.set(try snapshot("2026-10-18T10:00:06Z", limit: 2), fails: true)
        await store.refresh()
        try check(store.error != nil && store.actionsDisabled && store.snapshot?.queue.limit == 2, "failure keeps data and disables actions")
        await fixture.set(try snapshot("2026-10-18T10:00:08Z", limit: 2))
        await store.refresh()
        try check(store.error == nil && !store.actionsDisabled, "unchanged recovery re-enables actions")
        // A slow poll that began before a mutation must not overwrite its newer state.
        await fixture.set(try snapshot("2026-10-18T10:00:10Z", limit: 1))
        await fixture.holdNextRead()
        let oldRead = Task { await store.refresh() }
        await fixture.waitUntilReadHeld()
        store.isMutating = true
        await fixture.releaseRead()
        await oldRead.value
        try check(store.snapshot?.queue.limit == 2, "in-flight read cannot overwrite a mutation")
        store.isMutating = false
        await store.refresh()
        try check(store.snapshot?.queue.limit == 1, "refresh resumes after mutation")
        subscription.cancel()

        let pausedData = try JSONEncoder().encode(QueueSnapshot(updatedAt: "resume-fixture",
            queue: QueueSummary(status: "Paused", limit: 3, running: 0, waiting: 0, legacyOccupied: false), jobs: []))
        let resumeStatus = StatusFixture(pausedData)
        let commandGate = StatusFixture(Data())
        let resumeFixture = ResumeFixture(status: resumeStatus, commandGate: commandGate)
        let resumable = QueueStore(statusLoader: { try await resumeStatus.load() },
                                   command: { try await resumeFixture.command($0.arguments) })
        try check(resumable.dispatch(.resume) == nil, "An unknown queue cannot be resumed")
        await resumable.refresh()
        await commandGate.holdNextRead()
        let resuming = resumable.dispatch(.resume)
        try check(resuming != nil && resumable.isMutating && resumable.isResuming
                  && resumable.dispatch(.resume) == nil && resumable.dispatch(.limit(2)) == nil,
                  "Resume reserves the shared mutation guard and rejects duplicate actions")
        await commandGate.waitUntilReadHeld()
        await commandGate.releaseRead()
        await resuming?.value
        let resumedCommands = await resumeFixture.commands
        try check(resumedCommands == [["resume"]] && !resumable.isPaused && !resumable.isMutating
                  && !resumable.isResuming && resumable.notice == "Queue resumed." && resumable.error == nil,
                  "Resume sends the exact CLI action and refreshes confirmed queue state")
        try check(resumable.dispatch(.resume) == nil, "An already running queue does not dispatch Resume")
        await resumeStatus.set(pausedData)
        await resumable.refresh()
        await resumeFixture.setFailure(true)
        await resumable.dispatch(.resume)?.value
        try check(resumable.isPaused && !resumable.isMutating && !resumable.isResuming && resumable.notice == nil
                  && resumable.error?.contains("Resume rejected") == true && resumable.dispatch(.resume) == nil,
                  "Failed Resume preserves paused state, displays the error and releases busy state")
        await resumeFixture.setFailure(false)
        await resumable.refresh()
        await resumable.dispatch(.resume)?.value
        try check(!resumable.isPaused && resumable.error == nil, "Resume can retry after connection recovery")

        var telemetry = try JSONDecoder().decode(QueueSnapshot.self, from: snapshot("2026-10-19T10:00:00Z"))
        try check(telemetry.scheduler == nil, "Older status without telemetry decodes as unavailable")
        telemetry.scheduler = QueueScheduler(mode: .fixed, available: true, reason: "Learning", updatedAt: nil,
            host: HostResources(sampledAt: 1792404000, totalMemoryBytes: 17_179_869_184,
                availableMemoryBytes: 4_294_967_296, cpuCores: 8, cpuBusyCores: 2.5,
                memoryPressure: "warning", swapUsedBytes: 1_073_741_824, swapInBytesPerSecond: 0,
                swapOutBytesPerSecond: 0, diskFreeBytes: 21_474_836_480, machine: "arm64", complete: false))
        let schedulingFixture = StatusFixture(try JSONEncoder().encode(telemetry))
        let scheduling = QueueStore(statusLoader: { try await schedulingFixture.load() },
            command: { try await schedulingFixture.command($0.arguments) })
        await scheduling.refresh()
        try check(scheduling.snapshot?.scheduler?.host?.summary.contains("Collecting rates") == true,
                  "Cold telemetry explicitly collects rates")
        var telemetryPublishes = 0
        let telemetrySubscription = scheduling.objectWillChange.sink { telemetryPublishes += 1 }
        telemetry.scheduler = QueueScheduler(mode: .fixed, available: true, reason: "Learning",
            updatedAt: "2026-10-19T10:00:01Z", host: telemetry.scheduler?.host)
        await schedulingFixture.set(try JSONEncoder().encode(telemetry))
        await scheduling.refresh()
        try check(telemetryPublishes == 0, "Scheduler acquisition timestamp alone does not republish unchanged data")
        telemetrySubscription.cancel()
        let change = scheduling.dispatch(.scheduling(.adaptive))
        try check(change != nil && scheduling.isMutating && scheduling.dispatch(.scheduling(.shadow)) == nil,
                  "Scheduling mutation reserves before I/O and rejects duplicate changes")
        await change?.value
        let commands = await schedulingFixture.commands
        try check(commands == [["scheduling", "adaptive"]] && scheduling.snapshot?.scheduler?.mode == .adaptive,
                  "Scheduling dispatch uses allowlisted mode and refreshes actual status")
        telemetry.scheduler = QueueScheduler(mode: .fixed, available: false, reason: "Sampling failed", updatedAt: nil, host: nil)
        await schedulingFixture.set(try JSONEncoder().encode(telemetry))
        await scheduling.refresh()
        await scheduling.dispatch(.scheduling(.shadow))?.value
        try check(scheduling.snapshot?.scheduler?.mode == .shadow, "Learning mode remains available without sampler")
        await scheduling.dispatch(.scheduling(.adaptive))?.value
        try check(scheduling.error?.contains("Resource sampler unavailable") == true && scheduling.snapshot?.scheduler?.mode == .shadow,
                  "Unavailable adaptive mode reports failure and preserves confirmed mode")
        let olderData = try snapshot("2026-10-19T10:00:01Z")
        let unsupported = QueueStore(statusLoader: { olderData }, command: { _ in
            throw ClientError.message("Unsupported controller must never mutate")
        })
        await unsupported.refresh()
        try check(unsupported.dispatch(.scheduling(.adaptive)) == nil, "Older controller cannot receive unsupported scheduling mutation")

        let preparation = PreparationFixture()
        let preparedFixture = StatusFixture(try snapshot("initial"))
        let preparedStore = QueueStore(statusLoader: { try await preparedFixture.load() },
            command: { _ in Data("{}".utf8) },
            snapshotPreparer: { try await preparation.prepare($0, previous: $1) })
        await preparedStore.refresh()
        await preparation.hold()
        await preparedFixture.set(try snapshot("obsolete", limit: 2))
        let preparedRead = Task { await preparedStore.refresh() }
        await preparation.waitUntilHeld()
        await preparedFixture.set(try snapshot("new mutation", limit: 3))
        await preparedStore.dispatch(.limit(3))?.value
        await preparation.release()
        await preparedRead.value
        // Mutation completion coalesces one fresh refresh behind the obsolete one.
        for _ in 0..<100 where preparedStore.snapshot?.queue.limit != 3 {
            try await Task.sleep(for: .milliseconds(5))
        }
        try check(preparedStore.snapshot?.queue.limit == 3,
                  "Prepared obsolete refresh cannot overwrite a completed mutation and coalesced fresh refresh")

        await preparation.hold()
        await preparedFixture.set(try snapshot("cancelled", limit: 4))
        let cancelledRead = Task { await preparedStore.refresh() }
        await preparation.waitUntilHeld()
        cancelledRead.cancel()
        await preparation.release()
        await cancelledRead.value
        try check(preparedStore.snapshot?.queue.limit == 3 && preparedStore.error == nil,
                  "Cancelled prepared refresh publishes neither stale data nor a controller error")

        let jobs = (0..<1000).map { id in
            QueueJob(id: id, state: id < 2 ? "running" : id < 970 ? "queued" : "done",
                title: "Fixture \(id)", project: "Fixture", branch: "test", directory: "/fixture",
                executable: "test", priority: 0, queuePosition: id % 3 == 0 ? nil : 1000 - id,
                submittedAt: nil, startedAt: nil, finishedAt: nil, exitCode: nil, logPath: "/fixture/log",
                canReorder: true, canCancel: true, phase: "fixture")
        }
        let large = QueueSnapshot(updatedAt: "large", queue: telemetry.queue, jobs: jobs)
        let largeData = try JSONEncoder().encode(large)
        let largeStore = QueueStore(statusLoader: { largeData })
        await largeStore.refresh()
        let expectedWaiting = jobs.filter(\.isWaiting).sorted {
            ($0.queuePosition ?? Int.max, $0.id) < ($1.queuePosition ?? Int.max, $1.id)
        }
        try check(largeStore.snapshot?.jobs == jobs && largeStore.running == jobs.filter(\.isRunning) &&
                  largeStore.waiting == expectedWaiting && largeStore.recent == Array(jobs.suffix(30).prefix(12)),
                  "Large prepared snapshot atomically preserves source, queue position, tie, and recent order")
        let unchangedPrepared = try await PreparedQueueSnapshot.decode(largeData, comparedTo: large)
        try check(unchangedPrepared == nil, "Off-main comparison suppresses unchanged prepared snapshots")

        try await checkQueueCadence(try snapshot("cadence"), check)
        try await checkRemoteCadence(check)
        try await checkCoordinatorAdapters(check)
    }
}
