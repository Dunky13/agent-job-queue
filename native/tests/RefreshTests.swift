import Foundation
import Combine

actor StatusFixture {
    var data: Data
    var fails = false
    var commands: [[String]] = []
    private var shouldHoldNextRead = false
    private var heldRead: CheckedContinuation<Void, Never>?
    private var readWaiter: CheckedContinuation<Void, Never>?
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

@main
struct RefreshTests {
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

        var telemetry = try JSONDecoder().decode(QueueSnapshot.self, from: snapshot("2026-10-19T10:00:00Z"))
        try check(telemetry.scheduler == nil, "Older status without telemetry decodes as unavailable")
        telemetry.scheduler = QueueScheduler(mode: .fixed, available: true, reason: "Learning", updatedAt: nil,
            host: HostResources(sampledAt: 1792404000, totalMemoryBytes: 17_179_869_184,
                availableMemoryBytes: 4_294_967_296, cpuCores: 8, cpuBusyCores: 2.5,
                memoryPressure: "warning", swapUsedBytes: 1_073_741_824, swapInBytesPerSecond: 0,
                swapOutBytesPerSecond: 0, diskFreeBytes: 21_474_836_480, machine: "arm64", complete: false))
        let schedulingFixture = StatusFixture(try JSONEncoder().encode(telemetry))
        let scheduling = QueueStore(statusLoader: { try await schedulingFixture.load() },
            command: { try await schedulingFixture.command($0) })
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
        let change = scheduling.dispatchScheduling(.adaptive)
        try check(change != nil && scheduling.isMutating && scheduling.dispatchScheduling(.shadow) == nil,
                  "Scheduling mutation reserves before I/O and rejects duplicate changes")
        await change?.value
        let commands = await schedulingFixture.commands
        try check(commands == [["scheduling", "adaptive"]] && scheduling.snapshot?.scheduler?.mode == .adaptive,
                  "Scheduling dispatch uses allowlisted mode and refreshes actual status")
        telemetry.scheduler = QueueScheduler(mode: .fixed, available: false, reason: "Sampling failed", updatedAt: nil, host: nil)
        await schedulingFixture.set(try JSONEncoder().encode(telemetry))
        await scheduling.refresh()
        await scheduling.dispatchScheduling(.shadow)?.value
        try check(scheduling.snapshot?.scheduler?.mode == .shadow, "Learning mode remains available without sampler")
        await scheduling.dispatchScheduling(.adaptive)?.value
        try check(scheduling.error?.contains("Resource sampler unavailable") == true && scheduling.snapshot?.scheduler?.mode == .shadow,
                  "Unavailable adaptive mode reports failure and preserves confirmed mode")
        let olderData = try snapshot("2026-10-19T10:00:01Z")
        let unsupported = QueueStore(statusLoader: { olderData }, command: { _ in
            throw ClientError.message("Unsupported controller must never mutate")
        })
        await unsupported.refresh()
        try check(unsupported.dispatchScheduling(.adaptive) == nil, "Older controller cannot receive unsupported scheduling mutation")
    }
}
