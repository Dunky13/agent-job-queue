import Foundation

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
        let passwords: [(String, Bool)] = [("", false), ("abc", false), ("word", true), ("2468", true),
            ("simple", true), (String(repeating: "a", count: 72), true),
            (String(repeating: "a", count: 73), false), (String(repeating: "é", count: 36), true),
            (String(repeating: "é", count: 37), false), ("word\n", false), ("word\t", false)]
        for (value, expected) in passwords {
            try check(RemoteAccessStore.acceptsPassword(value) == expected, "Remote password boundary")
        }
        let remote = RemoteAccessStore(fixtureMode: true)
        try check(remote.fixtureMode && !remote.configure(password: "word", enable: true), "Fixture cannot change actual remote credentials")
    }
}
