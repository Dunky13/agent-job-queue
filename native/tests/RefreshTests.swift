import Foundation
import Combine

actor StatusFixture {
    var data: Data
    var fails = false
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
    }
}
