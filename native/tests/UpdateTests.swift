@testable import AgentQueueCore
import Foundation
import Sparkle

/// Records controller calls and answers the status, drain and undrain commands.
actor DrainFixture {
    var calls: [[String]] = []
    var drainUntil: String?
    var paused = false
    var failDrain = false
    var failUndrain = false
    var status: String?

    func reset() { calls = []; drainUntil = nil; paused = false; failDrain = false; failUndrain = false; status = nil }
    func setStatus(_ value: String) { status = value }
    func setOperatorDrain(_ until: String = "2099-01-01T00:00:00Z") { drainUntil = until }
    func setPaused() { paused = true }
    func setFailures(drain: Bool = false, undrain: Bool = false) { failDrain = drain; failUndrain = undrain }
    func load(_ arguments: [String]) throws -> Data {
        calls.append(arguments)
        switch arguments {
        case ["status", "--json"]:
            let until = drainUntil.map { "\"\($0)\"" } ?? "null"
            return Data("{\"queue\":{\"drainUntil\":\(until),\"status\":\"\(status ?? (paused ? "paused" : "running"))\"}}".utf8)
        case ["drain", "--for", "4h"]:
            if failDrain { throw ClientError.message("Queue daemon is not running") }
            drainUntil = ISO8601DateFormatter().string(from: Date().addingTimeInterval(4 * 3600))
            return Data()
        case ["undrain"]:
            if failUndrain { throw ClientError.message("Queue daemon is not running") }
            drainUntil = nil
            paused = false
            return Data()
        case ["undrain", "--keep-paused"]:
            if failUndrain { throw ClientError.message("Queue daemon is not running") }
            drainUntil = nil
            return Data()
        case ["version", "--json"]:
            return try JSONEncoder().encode(InstalledQueueVersion(version: "1.0.0", build: 100, bundleBuild: "100",
                commit: nil, tracked: true, displayVersion: "1.0.0 (build 100)"))
        default:
            throw ClientError.message("Unexpected update command")
        }
    }
}


@main
struct UpdateTests {
    @MainActor static func main() async throws {
        func check(_ value: Bool, _ name: String) throws {
            FileHandle.standardOutput.write(Data("\(value ? "PASS" : "FAIL") \(name)\n".utf8))
            if !value { throw ClientError.message(name) }
        }
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

        // The store decides once, from the build, whether an updater exists.
        let tracked = RunningQueueVersion(version: "1.0.0", bundleBuild: "100", publishedBuild: 100)
        let untracked = RunningQueueVersion(version: "1.0.0", bundleBuild: "0")
        let broken = RunningQueueVersion(version: "1.0.0", bundleBuild: "100", metadataError: "Running metadata is invalid")
        try check(QueueUpdatePolicy.unavailableReason(running: tracked, testHooks: false) == nil,
            "A tracked, non-test build may update")
        try check(QueueUpdatePolicy.unavailableReason(running: untracked, testHooks: false) != nil,
            "An untracked build never updates")
        try check(QueueUpdatePolicy.unavailableReason(running: broken, testHooks: false) == "Running metadata is invalid",
            "Unverifiable build metadata never updates and says why")
        try check(QueueUpdatePolicy.unavailableReason(running: tracked, testHooks: true) != nil,
            "A test-hook build never updates")
        try check(QueueUpdatePolicy.isTestHookBuild, "The test library carries the test-hook flag that disables updates")
        let scratch = FileManager.default.temporaryDirectory.appendingPathComponent("queue-update-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: scratch, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: scratch) }
        let environment = QueueBootstrapEnvironment(app: scratch.appendingPathComponent("Agent Queue.app"),
            resources: scratch.appendingPathComponent("Resources"), home: scratch)
        let drainFixture = DrainFixture()
        let command: @Sendable (QueueClient.Command) async throws -> Data = { try await drainFixture.load($0.arguments) }
        for (name, running, hooks) in [("untracked", untracked, false), ("test-hook", tracked, true)] {
            let store = QueueUpdateStore(running: running, testHooks: hooks,
                environment: { environment }, command: command)
            store.start()
            store.checkForUpdates()
            try check(!store.updaterCreated && !store.canCheckForUpdates && store.unavailableReason != nil,
                "The \(name) build creates no updater and cannot check")
        }
        let released = QueueUpdateStore(running: tracked, testHooks: false, environment: { environment }, command: command)
        try check(released.updaterCreated && released.unavailableReason == nil, "A tracked build creates the updater")

        // Setup's incomplete-installation marker refuses checks and installs.
        let root = scratch.appendingPathComponent(".local/share/agent-heavy")
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        try check(!environment.installIncomplete, "A complete installation has no incomplete marker")
        try Data("{}".utf8).write(to: root.appendingPathComponent("install-in-progress.json"))
        try check(environment.installIncomplete, "Setup's install-in-progress marker is detected")
        var refused = false
        do { try QueueUpdatePolicy.requireCompleteInstallation(incomplete: environment.installIncomplete) } catch { refused = true }
        try check(refused, "Updates are refused while the incomplete-installation marker exists")
        try QueueUpdatePolicy.requireCompleteInstallation(incomplete: false)
        let sparkleHost = SPUStandardUpdaterController(startingUpdater: false, updaterDelegate: nil, userDriverDelegate: nil)
        let drain = QueueUpdateDrain(marker: environment.updateDrainMarker, command: command)
        let delegate = QueueUpdaterDelegate(drain: drain, installationIncomplete: { environment.installIncomplete })
        var delegateRefused = false
        do { try delegate.updater(sparkleHost.updater, mayPerform: .updates) } catch { delegateRefused = true }
        try check(delegateRefused, "The updater delegate refuses a check while the marker exists")
        try FileManager.default.removeItem(at: root.appendingPathComponent("install-in-progress.json"))
        try delegate.updater(sparkleHost.updater, mayPerform: .updates)
        try check(true, "The updater delegate allows a check once setup completes")

        // A menu-bar app announces a scheduled update found in the background instead of a window behind other apps.
        let announcer = QueueUpdateAnnouncer()
        nonisolated(unsafe) var announced: [String?] = []
        announcer.changed = { announced.append($0) }
        try check(announcer.supportsGentleScheduledUpdateReminders
            && !announcer.standardUserDriverShouldHandleShowingScheduledUpdate(.empty(), andInImmediateFocus: false)
            && announcer.standardUserDriverShouldHandleShowingScheduledUpdate(.empty(), andInImmediateFocus: true),
            "Sparkle shows a scheduled update's window only while Agent Queue is in front")
        announcer.standardUserDriverDidReceiveUserAttention(forUpdate: .empty())
        announcer.standardUserDriverWillFinishUpdateSession()
        try check(announced == [nil, nil], "Attention or the end of the session clears the announcement")
        let announcing = QueueUpdateStore(running: tracked, environment: { environment }, command: command)
        announcing.announce("2.0.0")
        try check(announcing.availableUpdate == "2.0.0", "An announced update is offered in the menu and panel")
        announcing.announce(nil)
        try check(announcing.availableUpdate == nil, "A cleared announcement removes the offer")

        // The drain runs before the install block on a user-initiated relaunch.
        try? FileManager.default.removeItem(at: environment.updateDrainMarker)
        await drainFixture.reset()
        nonisolated(unsafe) var installed = false
        let postponed = delegate.updater(sparkleHost.updater, shouldPostponeRelaunchForUpdate: .empty(), untilInvokingBlock: { installed = true })
        try check(postponed, "The relaunch path holds the install until the drain finishes")
        while !installed { try await Task.sleep(for: .milliseconds(10)) }
        try check(await drainFixture.calls == [["status", "--json"], ["drain", "--for", "4h"], ["status", "--json"]] && drain.hasMarker,
            "The relaunch path drains once, then installs, and records that this flow drained")
        // Installing on quit never restarts the app mid-session and never drains.
        await drainFixture.reset()
        nonisolated(unsafe) var quitBlockRan = false
        let quitHandled = delegate.updater(sparkleHost.updater, willInstallUpdateOnQuit: .empty(), immediateInstallationBlock: { quitBlockRan = true })
        try await Task.sleep(for: .milliseconds(100))
        let quitCalls = await drainFixture.calls
        try check(!quitHandled && !quitBlockRan && quitCalls.isEmpty,
            "The install-on-quit path returns false, never runs the immediate block and never drains")

        // Only a drain this flow started is ever released.
        func fresh() async { try? FileManager.default.removeItem(at: environment.updateDrainMarker); await drainFixture.reset() }
        await fresh()
        await drain.release()
        try check(await drainFixture.calls.isEmpty, "Undrain does not run without the flow's marker")
        await drainFixture.setOperatorDrain()
        await drain.begin()
        try check(await drainFixture.calls == [["status", "--json"]] && !drain.hasMarker,
            "An operator's drain is left alone and never marked")
        await drain.release()
        try check(await drainFixture.calls == [["status", "--json"]], "An operator's drain is never undrained")

        await fresh()
        await drain.begin()
        let recorded = drain.record
        try check(recorded != nil && recorded?.wasPaused == false, "The flow's own drain is marked with its exact expiry")
        var stat1 = stat()
        var dirStat = stat()
        try check(lstat(environment.updateDrainMarker.path, &stat1) == 0 && stat1.st_mode & 0o777 == 0o600 &&
            lstat(environment.updateDrainMarker.deletingLastPathComponent().path, &dirStat) == 0 && dirStat.st_mode & 0o777 == 0o700,
            "The marker is private: 0600 in a 0700 directory")
        await drain.begin()
        try check(drain.record == recorded, "A repeated call keeps the flow's marker and drains no second time")
        await drain.release()
        let tail = await drainFixture.calls.suffix(2)
        try check(Array(tail) == [["status", "--json"], ["undrain"]] && !drain.hasMarker,
            "Normal release undrains once and consumes the marker")
        await drainFixture.reset()
        await drain.release()
        try check(await drainFixture.calls.isEmpty, "A consumed marker never undrains again")

        // The operator paused before the update: the drain goes, the pause stays.
        await fresh()
        await drainFixture.setPaused()
        await drain.begin()
        try check(drain.record?.wasPaused == true, "A queue paused before the flow is recorded as paused")
        await fresh()
        await drainFixture.setStatus("unknown")
        await drain.begin()
        try check(drain.record?.wasPaused == true, "An unknown queue status is kept paused on release, never resumed")
        await fresh()
        await drainFixture.setPaused()
        await drain.begin()
        await drain.release()
        let stillPaused = await drainFixture.paused
        let lastCall = await drainFixture.calls.last
        try check(lastCall == ["undrain", "--keep-paused"] && stillPaused,
            "Release ends only the drain and never resumes an operator's pause")

        // The operator extended our drain, or replaced it after ours lapsed: not ours any more.
        await fresh()
        await drain.begin()
        await drainFixture.setOperatorDrain("2099-01-01T00:00:00Z")
        await drain.release()
        let extendedLast = await drainFixture.calls.last
        try check(extendedLast == ["status", "--json"] && !drain.hasMarker,
            "An operator who extended the drain keeps it, and the marker is dropped")
        await fresh()
        await drain.begin()
        let lapsed = QueueUpdateDrain(marker: environment.updateDrainMarker, command: command,
            now: { Date().addingTimeInterval(5 * 3600) })
        try check(!lapsed.hasMarker, "A marker whose drain lapsed is ignored")
        drain.discardIfStale()
        try check(drain.hasMarker, "A live marker survives the stale-marker sweep")
        lapsed.discardIfStale()
        try check(!FileManager.default.fileExists(atPath: environment.updateDrainMarker.path),
            "A lapsed marker is removed at launch even though nothing is released")
        await drainFixture.setOperatorDrain()
        let calls = await drainFixture.calls.count
        await lapsed.release()
        let afterLapsed = await drainFixture.calls.count
        try check(afterLapsed == calls && !FileManager.default.fileExists(atPath: environment.updateDrainMarker.path),
            "A lapsed marker is removed without undraining an operator's later drain")
        await fresh()
        await drain.begin()
        await drainFixture.setOperatorDrain()
        await drain.begin()
        try check(!drain.hasMarker, "A mismatched marker is dropped when an operator drain is live")
        await fresh()
        await drain.begin()
        await drainFixture.setOperatorDrain("2000-01-01T00:00:00Z")
        let staleBegin = QueueUpdateDrain(marker: environment.updateDrainMarker, command: command,
            now: { Date().addingTimeInterval(5 * 3600) })
        await drainFixture.reset()
        await staleBegin.begin()
        try check(await drainFixture.calls.contains(["drain", "--for", "4h"]), "A stale marker does not stop the flow from draining")

        // Failures and hostile markers.
        await fresh()
        await drainFixture.setFailures(drain: true)
        await drain.begin()
        try check(!drain.hasMarker, "A failed drain is logged, leaves no marker and does not stop the update")
        await fresh()
        await drain.begin()
        await drainFixture.setFailures(undrain: true)
        await drain.release()
        try check(!drain.hasMarker, "A failed undrain still consumes the marker so a later launch cannot release an operator's drain")
        await fresh()
        await drain.begin()
        let good = try Data(contentsOf: environment.updateDrainMarker)
        let until = drain.record?.drainUntil ?? ""
        for (name, text) in [("an unknown key", "{\"drainUntil\":\"\(until)\",\"wasPaused\":false,\"extra\":1}"),
                             ("a duplicate key", "{\"drainUntil\":\"\(until)\",\"wasPaused\":false,\"wasPaused\":true}"),
                             ("garbage", "not json")] {
            try Data(text.utf8).write(to: environment.updateDrainMarker)
            chmod(environment.updateDrainMarker.path, 0o600)
            try check(!drain.hasMarker, "A marker with \(name) is rejected")
        }
        try good.write(to: environment.updateDrainMarker)
        chmod(environment.updateDrainMarker.path, 0o644)
        try check(!drain.hasMarker, "A marker readable by others is rejected")
        chmod(environment.updateDrainMarker.path, 0o600)
        try check(drain.hasMarker, "The same marker with private permissions is accepted")
        let markerDirectory = environment.updateDrainMarker.deletingLastPathComponent().path
        chmod(markerDirectory, 0o755)
        try check(!drain.hasMarker, "A marker in a directory others can read is rejected")
        chmod(markerDirectory, 0o700)
        let target = scratch.appendingPathComponent("elsewhere.json")
        try good.write(to: target)
        try FileManager.default.removeItem(at: environment.updateDrainMarker)
        try FileManager.default.createSymbolicLink(at: environment.updateDrainMarker, withDestinationURL: target)
        try check(!drain.hasMarker, "A marker that is a symbolic link is never followed")
        await fresh()
        await drain.begin()
        var linkStat = stat()
        try check(drain.hasMarker && lstat(environment.updateDrainMarker.path, &linkStat) == 0 && linkStat.st_mode & S_IFMT == S_IFREG,
            "Writing replaces a planted link instead of following it")
        try? FileManager.default.removeItem(at: environment.updateDrainMarker)
        try Data(metadata.replacingOccurrences(of: "\"build\":205", with: "\"extra\":1,\"build\":205").utf8)
            .write(to: publishedBundleURL.appendingPathComponent("Contents/Resources/build-info.json"))
        try check(RunningQueueVersion.current(bundle: publishedBundle).metadataError != nil,
            "Running build metadata with an unknown key is rejected")
        try Data(metadata.replacingOccurrences(of: "\"build\":205", with: "\"build\":205,\"build\":205").utf8)
            .write(to: publishedBundleURL.appendingPathComponent("Contents/Resources/build-info.json"))
        try check(RunningQueueVersion.current(bundle: publishedBundle).metadataError != nil,
            "Running build metadata with a duplicate key is rejected")

        // The window model reads the installed version locally and never starts an update by itself.
        await drainFixture.reset()
        let model = QueueUpdateStore(running: tracked, testHooks: true, environment: { environment }, command: command)
        try check(await drainFixture.calls.isEmpty && model.installed == nil, "Opening the update model performs no controller I/O")
        await model.refreshInstalled()
        try check(await drainFixture.calls == [["version", "--json"]] && model.installed?.build == 100,
            "Refreshing reads only the installed version")
        // A fixture launch builds its update store from the fixture: test hooks rule out Sparkle, and reads end there.
        let recorder = QueueFixture(arguments: [])
        let fixtureUpdates = recorder.coordinator().updateStore
        await fixtureUpdates.refreshInstalled()
        try check(!fixtureUpdates.updaterCreated && fixtureUpdates.unavailableReason != nil
            && recorder.reads == [.version] && recorder.changes.isEmpty && fixtureUpdates.installed != nil,
            "A fixture update store creates no updater and reads only from the fixture")
        for service in QueueServiceManagement.Service.allCases {
            try check(QueueClient.Command.serviceStatus(service).timeout == 5,
                "Exact native status for \(service.rawValue) has a five-second read budget")
        }
        try check([["--service-management", "register", "queue"], ["--service-management", "status", "queue", "extra"],
                   ["--service-management", "status", "unknown"]].allSatisfy {
                let command = QueueClient.Command.setup($0)
                return command.timeout == 330 && command.cancellation == .retain
            }, "Native mutation and malformed arguments cannot inherit discardable read budgets")
        try check(QueueClient.Command.status.timeout == 8 &&
            QueueClient.Command.jobAction("cancel", id: 1).timeout == 25 &&
            QueueClient.Command.remoteStatus.timeout == 8 &&
            QueueClient.Command.remoteConfigure(Data()).timeout == 125 &&
            QueueClient.Command.drain.arguments == ["drain", "--for", "4h"] && QueueClient.Command.undrain.arguments == ["undrain"] &&
            QueueClient.Command.version.timeout == 25,
            "The update drain commands leave normal queue and remote deadlines unchanged")
    }
}
