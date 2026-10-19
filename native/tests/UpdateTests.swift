import Foundation

actor UpdateFixture {
    var installed = InstalledQueueVersion(version: "1.0.0", build: 100, bundleBuild: "100", commit: nil,
        tracked: true, displayVersion: "1.0.0 (build 100)")
    var latest = QueueRelease(version: "1.0.1", build: 101, commit: String(repeating: "a", count: 40),
        tag: "build-101", url: "https://github.com/Dunky13/agent-job-queue/releases/tag/build-101")
    var comparison = QueueUpdateComparison.available
    var canInstall = true
    var calls: [[String]] = []
    var failCheck = false
    var failInstall = false
    var installerBuild = 101
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
    func setInstallFailure(_ value: Bool) { failInstall = value }
    func setInstallerBuild(_ value: Int) { installerBuild = value }
    func setComparison(_ value: QueueUpdateComparison, installable: Bool) {
        comparison = value
        canInstall = installable
    }
    func setUnknownInstalled() {
        installed = InstalledQueueVersion(version: "1.0.0", build: nil, bundleBuild: "0", commit: nil,
            tracked: false, displayVersion: "1.0.0 (untracked build)")
    }
    func setInvalidRelease() {
        latest = QueueRelease(version: "1.0.1", build: 101, commit: String(repeating: "a", count: 40),
            tag: "build-101", url: "https://example.test/installer")
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
                comparison: comparison, canInstall: canInstall, message: "Release status confirmed."))
        }
        if arguments == ["updates", "install", "--build", "101", "--json"] {
            if failInstall { throw ClientError.message("Checksum mismatch") }
            return try encoder.encode(QueueInstallerResult(installerOpened: true, build: installerBuild,
                message: "Installer opened. Choose Install to finish. Queued and running jobs are preserved"))
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
        try check(initialCalls.isEmpty && !store.canInstall, "Opening the update model performs no implicit network or download")
        await store.refreshInstalled()
        let offlineCalls = await fixture.calls
        try check(offlineCalls == [["version", "--json"]] && store.installed?.build == 100 && store.check == nil,
            "Reading installed version is an offline CLI operation")

        await fixture.holdNextCall()
        let checking = store.checkForUpdates()
        try check(checking != nil && store.isBusy && store.checkForUpdates() == nil && store.openInstaller() == nil,
            "Explicit checking reserves operation synchronously and prevents duplicate operations")
        await fixture.waitUntilHeld()
        await store.refreshInstalled()
        await fixture.release()
        await checking?.value
        try check(store.canInstall && store.check?.latest?.build == 101 && !store.isBusy,
            "Newer release enables explicit installer download after a successful check")
        let checkedCalls = await fixture.calls
        try check(checkedCalls.count == 2, "Version refresh while checking does not start overlapping I/O")

        await fixture.holdNextCall()
        let installation = store.openInstaller()
        try check(installation != nil && store.operation == .downloading && store.openInstaller() == nil && store.checkForUpdates() == nil,
            "Download reserves operation and prevents duplicate installation or release changes")
        await fixture.waitUntilHeld()
        await fixture.release()
        await installation?.value
        let installedCalls = await fixture.calls
        try check(installedCalls.last == ["updates", "install", "--build", "101", "--json"],
            "Installer operation pins the exact release build shown to the user")
        try check(store.installerOpened && !store.canInstall && store.notice?.contains("Choose Install") == true,
            "Opening installer requires explicit human Install and does not claim completion")

        await fixture.setCheckFailure(true)
        await store.checkForUpdates()?.value
        try check(store.error?.contains("Network unavailable") == true && store.check == nil && !store.canInstall,
            "Failed check clears prior install eligibility while preserving installed version")
        await fixture.setCheckFailure(false)
        await store.checkForUpdates()?.value
        await fixture.setInstallFailure(true)
        await store.openInstaller()?.value
        try check(store.error?.contains("Checksum mismatch") == true && !store.installerOpened && store.canInstall,
            "Verification failure is visible and does not report an opened installer")
        await fixture.setInstallFailure(false)
        await fixture.setInstallerBuild(102)
        await store.openInstaller()?.value
        try check(store.error?.contains("requested installer") == true && !store.installerOpened,
            "Mismatched installer response cannot claim success")

        await fixture.setComparison(.current, installable: false)
        await store.checkForUpdates()?.value
        try check(!store.canInstall && store.openInstaller() == nil, "Current build cannot start an updater download")
        await fixture.setComparison(.newer, installable: false)
        await store.checkForUpdates()?.value
        try check(!store.canInstall, "Newer installed source build cannot downgrade")
        await fixture.setComparison(.available, installable: true)
        let newerRunning = QueueUpdateStore(running: RunningQueueVersion(version: "1.1.0", bundleBuild: "105", publishedBuild: 105),
            command: { try await fixture.load($0) })
        await newerRunning.checkForUpdates()?.value
        try check(!newerRunning.canInstall && newerRunning.compatibilityMessage?.contains("newer") == true,
            "A newer running bundle independently prevents backend-triggered downgrade")
        let unknownRunning = QueueUpdateStore(running: RunningQueueVersion(version: "1.0.0", bundleBuild: "0"),
            command: { try await fixture.load($0) })
        await unknownRunning.checkForUpdates()?.value
        try check(unknownRunning.canInstall && unknownRunning.isReplacement &&
            unknownRunning.installButtonLabel == "Download published installer…" &&
            unknownRunning.compatibilityMessage?.contains("replace") == true,
            "Unknown running build offers an explicit published-installer replacement")
        let invalidRunning = QueueUpdateStore(running: RunningQueueVersion(version: "1.0.0", bundleBuild: "100",
            metadataError: "Running metadata is invalid"), command: { try await fixture.load($0) })
        await invalidRunning.checkForUpdates()?.value
        try check(!invalidRunning.canInstall && invalidRunning.compatibilityMessage == "Running metadata is invalid",
            "Invalid running provenance disables installation with a visible explanation")
        await fixture.setUnknownInstalled()
        await store.refreshInstalled()
        try check(store.check == nil && !store.canInstall, "Offline change of installed version invalidates earlier release comparison")
        await fixture.setComparison(.untracked, installable: true)
        await store.checkForUpdates()?.value
        try check(store.canInstall && store.installed?.tracked == false && store.isReplacement,
            "Legacy installation can explicitly download a published installer to establish release history")
        await newerRunning.checkForUpdates()?.value
        try check(!newerRunning.canInstall && newerRunning.compatibilityMessage?.contains("newer") == true,
            "Untracked controller cannot override a known newer published running app")
        await fixture.setInstallerBuild(101)
        await store.openInstaller()?.value
        try check(store.installerOpened && store.notice?.contains("Choose Install") == true,
            "Legacy replacement still only opens the pinned installer for explicit installation")
        await fixture.setInvalidRelease()
        await store.checkForUpdates()?.value
        try check(store.check == nil && store.error?.contains("invalid release") == true,
            "Untrusted release URL is rejected before it reaches links or install controls")

        let callsBeforePreview = await fixture.calls
        let preview = QueueUpdateStore(fixtureMode: true, command: { try await fixture.load($0) })
        await preview.refreshInstalled()
        try check(preview.checkForUpdates() == nil && preview.openInstaller() == nil && !preview.canInstall,
            "Fixture update controls cannot invoke live commands")
        let callsAfterPreview = await fixture.calls
        try check(callsAfterPreview == callsBeforePreview && preview.check?.latest?.build == 101,
            "Snapshot fixture has a simulated release and performs no external effects")
        try check(QueueClient.timeout(for: ["status"], budget: .updates) == 8 &&
            QueueClient.timeout(for: ["cancel", "1"]) == 25 &&
            QueueClient.timeout(for: ["status"], controller: .remote) == 8 &&
            QueueClient.timeout(for: ["configure"], controller: .remote) == 35 &&
            QueueClient.timeout(for: ["updates", "install"], budget: .updates) == 130 &&
            QueueClient.timeout(for: ["updates", "install"]) == 25,
            "Explicit update deadline leaves normal queue and remote deadlines unchanged")
    }
}
