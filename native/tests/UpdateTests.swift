import Foundation

actor UpdateFixture {
    var installed = InstalledQueueVersion(version: "1.0.0", build: 100, bundleBuild: "100", commit: nil,
        tracked: true, displayVersion: "1.0.0 (build 100)")
    var latest = QueueRelease(version: "1.0.1", build: 101, commit: String(repeating: "a", count: 40),
        tag: "build-101", url: "https://github.com/Dunky13/agent-job-queue/releases/tag/build-101")
    var comparison = QueueUpdateComparison.available
    var canDownload = true
    var calls: [[String]] = []
    var failCheck = false
    var failDownload = false
    var downloadedBuild = 101
    private var shouldHold = false
    private var held: CheckedContinuation<Void, Never>?
    private var waiter: CheckedContinuation<Void, Never>?

    func holdNextCall() { shouldHold = true }
    func waitUntilHeld() async {
        if held != nil { return }
        await withCheckedContinuation { waiter = $0 }
    }
    func release() { held?.resume(); held = nil }
    func setCheckFailure(_ value: Bool) { failCheck = value }
    func setDownloadFailure(_ value: Bool) { failDownload = value }
    func setDownloadedBuild(_ value: Int) { downloadedBuild = value }
    func setComparison(_ value: QueueUpdateComparison, installable: Bool) {
        comparison = value
        canDownload = installable
    }
    func setUnknownInstalled() {
        installed = InstalledQueueVersion(version: "1.0.0", build: nil, bundleBuild: "0", commit: nil,
            tracked: false, displayVersion: "1.0.0 (untracked build)")
    }
    func setInvalidRelease() {
        latest = QueueRelease(version: "1.0.1", build: 101, commit: String(repeating: "a", count: 40),
            tag: "build-101", url: "https://example.test/release")
    }
    func load(_ arguments: [String]) async throws -> Data {
        calls.append(arguments)
        if shouldHold {
            shouldHold = false
            await withCheckedContinuation {
                held = $0
                waiter?.resume()
                waiter = nil
            }
        }
        let encoder = JSONEncoder()
        if arguments == ["version", "--json"] { return try encoder.encode(installed) }
        if arguments == ["updates", "check", "--json"] {
            if failCheck { throw ClientError.message("Network unavailable") }
            return try encoder.encode(QueueUpdateCheck(installed: installed, latest: latest,
                comparison: comparison, canDownload: canDownload, message: "Release status confirmed."))
        }
        if arguments == ["updates", "download", "--build", "101", "--json"] {
            if failDownload { throw ClientError.message("Checksum mismatch") }
            return try encoder.encode(QueueUpdateDownload(downloaded: true, build: downloadedBuild,
                app: "/private/tmp/fixture/Downloads/Agent Queue 1.0.1 (build 101)/Agent Queue.app",
                message: "Quit Agent Queue, then drag the new copy into Applications and choose Replace. Opening it finishes the update automatically."))
        }
        throw ClientError.message("Unexpected update command")
    }
}

@main
struct UpdateTests {
    @MainActor static func main() async throws {
        func check(_ value: Bool, _ name: String) throws {
            FileHandle.standardOutput.write(Data("\(value ? "PASS" : "FAIL") \(name)\n".utf8))
            if !value { throw ClientError.message(name) }
        }
        let running = RunningQueueVersion(version: "1.0.0", bundleBuild: "100", publishedBuild: 100)
        let bundleURL = FileManager.default.temporaryDirectory.appendingPathComponent("queue-version-\(UUID().uuidString).bundle")
        try FileManager.default.createDirectory(at: bundleURL.appendingPathComponent("Contents"), withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: bundleURL) }
        let info = ["CFBundleShortVersionString": "2.0.0", "CFBundleVersion": "205", "CFBundleIdentifier": "io.went.queue.test"]
        let infoData = try PropertyListEncoder().encode(info)
        try infoData.write(to: bundleURL.appendingPathComponent("Contents/Info.plist"))
        guard let bundle = Bundle(url: bundleURL) else { throw ClientError.message("Could not create version bundle fixture") }
        try check(RunningQueueVersion.current(bundle: bundle) == RunningQueueVersion(version: "2.0.0", bundleBuild: "205"),
            "Running version reads the app bundle rather than installed controller state")
        try check(RunningQueueVersion.current(bundle: bundle).publishedBuild == nil,
            "A numeric bundle build alone is not proof of published release ordering")
        let publishedBundleURL = FileManager.default.temporaryDirectory.appendingPathComponent("queue-published-\(UUID().uuidString).bundle")
        try FileManager.default.createDirectory(at: publishedBundleURL.appendingPathComponent("Contents/Resources"), withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: publishedBundleURL) }
        try infoData.write(to: publishedBundleURL.appendingPathComponent("Contents/Info.plist"))
        let metadata = """
        {"schemaVersion":1,"repository":"Dunky13/agent-job-queue","version":"2.0.0","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","build":205}
        """
        try Data(metadata.utf8).write(to: publishedBundleURL.appendingPathComponent("Contents/Resources/build-info.json"))
        guard let publishedBundle = Bundle(url: publishedBundleURL) else { throw ClientError.message("Could not create published bundle fixture") }
        let publishedRunning = RunningQueueVersion.current(bundle: publishedBundle)
        try check(publishedRunning.publishedBuild == 205 && publishedRunning.metadataError == nil,
            "Matching release metadata establishes the running app's published build")
        try Data(metadata.replacingOccurrences(of: "\"build\":205", with: "\"build\":206").utf8)
            .write(to: publishedBundleURL.appendingPathComponent("Contents/Resources/build-info.json"))
        try check(RunningQueueVersion.current(bundle: publishedBundle).metadataError != nil,
            "Mismatched running metadata is reported instead of silently treated as legacy")
        let fixture = UpdateFixture()
        let store = QueueUpdateStore(running: running, command: { try await fixture.load($0) })
        try check(store.running.displayVersion == "1.0.0 · build 100" && store.installed == nil,
            "Running bundle version appears before controller I/O")
        let initialCalls = await fixture.calls
        try check(initialCalls.isEmpty && !store.canDownload, "Opening the update model performs no implicit network or download")
        await store.refreshInstalled()
        let offlineCalls = await fixture.calls
        try check(offlineCalls == [["version", "--json"]] && store.installed?.build == 100 && store.check == nil,
            "Reading installed version is an offline CLI operation")

        await fixture.holdNextCall()
        let checking = store.checkForUpdates()
        try check(checking != nil && store.isBusy && store.checkForUpdates() == nil && store.downloadUpdate() == nil,
            "Explicit checking reserves operation synchronously and prevents duplicate operations")
        await fixture.waitUntilHeld()
        await store.refreshInstalled()
        await fixture.release()
        await checking?.value
        try check(store.canDownload && store.check?.latest?.build == 101 && !store.isBusy,
            "Newer release enables explicit update download after a successful check")
        let checkedCalls = await fixture.calls
        try check(checkedCalls.count == 2, "Version refresh while checking does not start overlapping I/O")

        await fixture.holdNextCall()
        let installation = store.downloadUpdate()
        try check(installation != nil && store.operation == .downloading && store.downloadUpdate() == nil && store.checkForUpdates() == nil,
            "Download reserves operation and prevents duplicate downloads or release changes")
        await fixture.waitUntilHeld()
        await fixture.release()
        await installation?.value
        let installedCalls = await fixture.calls
        try check(installedCalls.last == ["updates", "download", "--build", "101", "--json"],
            "Download operation pins the exact release build shown to the user")
        try check(store.updateDownloaded && !store.canDownload && store.notice?.contains("drag the new copy into Applications") == true &&
            store.downloadButtonLabel == "Update downloaded",
            "Downloading reports the drag-and-drop step without claiming the update has been installed")

        await fixture.setCheckFailure(true)
        await store.checkForUpdates()?.value
        try check(store.error?.contains("Network unavailable") == true && store.check == nil && !store.canDownload,
            "Failed check clears prior download eligibility while preserving installed version")
        await fixture.setCheckFailure(false)
        await store.checkForUpdates()?.value
        await fixture.setDownloadFailure(true)
        await store.downloadUpdate()?.value
        try check(store.error?.contains("Checksum mismatch") == true && !store.updateDownloaded && store.canDownload,
            "Verification failure is visible and does not report a downloaded update")
        await fixture.setDownloadFailure(false)
        await fixture.setDownloadedBuild(102)
        await store.downloadUpdate()?.value
        try check(store.error?.contains("requested update was not downloaded") == true && !store.updateDownloaded,
            "Mismatched download response cannot claim success")

        await fixture.setComparison(.current, installable: false)
        await store.checkForUpdates()?.value
        try check(!store.canDownload && store.downloadUpdate() == nil, "Current build cannot start an updater download")
        await fixture.setComparison(.newer, installable: false)
        await store.checkForUpdates()?.value
        try check(!store.canDownload, "Newer installed source build cannot downgrade")
        await fixture.setComparison(.available, installable: true)
        let newerRunning = QueueUpdateStore(running: RunningQueueVersion(version: "1.1.0", bundleBuild: "105", publishedBuild: 105),
            command: { try await fixture.load($0) })
        await newerRunning.checkForUpdates()?.value
        try check(!newerRunning.canDownload && newerRunning.compatibilityMessage?.contains("newer") == true,
            "A newer running bundle independently prevents backend-triggered downgrade")
        let unknownRunning = QueueUpdateStore(running: RunningQueueVersion(version: "1.0.0", bundleBuild: "0"),
            command: { try await fixture.load($0) })
        await unknownRunning.checkForUpdates()?.value
        try check(unknownRunning.canDownload && unknownRunning.isReplacement &&
            unknownRunning.downloadButtonLabel == "Download published build…" &&
            unknownRunning.compatibilityMessage?.contains("replace") == true,
            "Unknown running build offers an explicit published-build replacement")
        let invalidRunning = QueueUpdateStore(running: RunningQueueVersion(version: "1.0.0", bundleBuild: "100",
            metadataError: "Running metadata is invalid"), command: { try await fixture.load($0) })
        await invalidRunning.checkForUpdates()?.value
        try check(!invalidRunning.canDownload && invalidRunning.compatibilityMessage == "Running metadata is invalid",
            "Invalid running provenance disables downloading with a visible explanation")
        await fixture.setUnknownInstalled()
        await store.refreshInstalled()
        try check(store.check == nil && !store.canDownload, "Offline change of installed version invalidates earlier release comparison")
        await fixture.setComparison(.untracked, installable: true)
        await store.checkForUpdates()?.value
        try check(store.canDownload && store.installed?.tracked == false && store.isReplacement,
            "Legacy installation can explicitly download a published build to establish release history")
        await newerRunning.checkForUpdates()?.value
        try check(!newerRunning.canDownload && newerRunning.compatibilityMessage?.contains("newer") == true,
            "Untracked controller cannot override a known newer published running app")
        await fixture.setDownloadedBuild(101)
        await store.downloadUpdate()?.value
        try check(store.updateDownloaded && store.notice?.contains("finishes the update automatically") == true,
            "Legacy replacement downloads the pinned build for drag-and-drop without claiming completion")
        await fixture.setInvalidRelease()
        await store.checkForUpdates()?.value
        try check(store.check == nil && store.error?.contains("invalid release") == true,
            "Untrusted release URL is rejected before it reaches links or download controls")

        let callsBeforePreview = await fixture.calls
        let preview = QueueUpdateStore(fixtureMode: true, command: { try await fixture.load($0) })
        await preview.refreshInstalled()
        try check(preview.checkForUpdates() == nil && preview.downloadUpdate() == nil && !preview.canDownload,
            "Fixture update controls cannot invoke live commands")
        let callsAfterPreview = await fixture.calls
        try check(callsAfterPreview == callsBeforePreview && preview.check?.latest?.build == 101,
            "Snapshot fixture has a simulated release and performs no external effects")
        for role in ["queue", "resources", "remote", "login"] {
            try check(QueueClient.timeout(for: ["--service-management", "status", role], budget: .setup) == 5,
                "Exact native status for \(role) has a five-second read budget")
        }
        try check(QueueClient.timeout(for: ["--service-management", "register", "queue"], budget: .setup) == 330 &&
            QueueClient.timeout(for: ["--service-management", "status", "queue", "extra"], budget: .setup) == 330 &&
            QueueClient.timeout(for: ["--service-management", "status", "unknown"], budget: .setup) == 330,
            "Native mutation and malformed arguments cannot inherit discardable read budgets")
        try check(QueueClient.timeout(for: ["status"], budget: .updates) == 8 &&
            QueueClient.timeout(for: ["cancel", "1"]) == 25 &&
            QueueClient.timeout(for: ["status"], controller: .remote) == 8 &&
            QueueClient.timeout(for: ["configure"], controller: .remote) == 125 &&
            QueueClient.timeout(for: ["updates", "download"], budget: .updates) == 130 &&
            QueueClient.timeout(for: ["updates", "download"]) == 25,
            "Explicit update deadline leaves normal queue and remote deadlines unchanged")
    }
}
