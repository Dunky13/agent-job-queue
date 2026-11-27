import AppKit
import Combine
import Foundation
// Sparkle's delegate protocols are not annotated for Sendable, and it calls them on the main thread, where this code runs.
@preconcurrency import Sparkle
import SwiftUI
@preconcurrency import UserNotifications

struct InstalledQueueVersion: Codable, Equatable, Sendable {
    let version: String?
    let build: Int?
    let bundleBuild: String?
    let commit: String?
    let tracked: Bool
    let displayVersion: String
}

struct RunningQueueVersion: Equatable, Sendable {
    let version: String?
    let bundleBuild: String?
    var publishedBuild: Int? = nil
    var metadataError: String? = nil
    var build: Int? { bundleBuild.flatMap(Int.init).flatMap { $0 > 0 ? $0 : nil } }
    var displayVersion: String {
        let label = version.flatMap { $0.isEmpty ? nil : $0 } ?? "Unknown version"
        guard let bundleBuild, !bundleBuild.isEmpty else { return "\(label) · untracked build" }
        return "\(label) · build \(bundleBuild)\(publishedBuild == nil ? " (untracked)" : "")"
    }
    static func current(bundle: Bundle = .main) -> RunningQueueVersion {
        var result = RunningQueueVersion(version: bundle.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String,
            bundleBuild: bundle.object(forInfoDictionaryKey: "CFBundleVersion") as? String)
        guard let metadataURL = bundle.url(forResource: "build-info", withExtension: "json") else { return result }
        do {
            let input = try FileHandle(forReadingFrom: metadataURL)
            defer { try? input.close() }
            let data = try input.read(upToCount: 16_385) ?? Data()
            guard data.count <= 16_384 else { throw ClientError.message("Running app build metadata is too large.") }
            let metadata = try StrictJSON.decode(RunningBuildMetadata.self, from: data, allowedKeys: RunningBuildMetadata.keys)
            guard metadata.schemaVersion == 1, metadata.repository == "Dunky13/agent-job-queue",
                  metadata.version == result.version,
                  String(metadata.build ?? 0) == result.bundleBuild,
                  metadata.commit == nil || metadata.commit?.range(of: "^[0-9a-f]{40}$", options: .regularExpression) != nil,
                  metadata.build == nil || ((1...999_999_999).contains(metadata.build ?? 0) && metadata.commit != nil) else {
                throw ClientError.message("Running app build metadata does not match its bundle.")
            }
            result.publishedBuild = metadata.build
        } catch { result.metadataError = "Could not verify the running app's build history. \(error.localizedDescription)" }
        return result
    }
}

private struct RunningBuildMetadata: Decodable {
    static let keys: Set<String> = ["schemaVersion", "repository", "version", "commit", "build"]
    let schemaVersion: Int
    let repository: String
    let version: String
    let commit: String?
    let build: Int?
}

/// Decides when Sparkle may act, apart from Sparkle itself.
enum QueueUpdatePolicy {
    static let releasesPage = URL(string: "https://github.com/Dunky13/agent-job-queue/releases")!

    /// nil means this running app may check for and install updates. Only published
    /// builds update: a local, pull-request or test-hook build never does.
    static func unavailableReason(running: RunningQueueVersion, testHooks: Bool) -> String? {
        if testHooks { return "This is a test build, which never checks for updates." }
        if let error = running.metadataError { return error }
        if running.publishedBuild == nil { return "This is an untracked build. Only published builds check for updates." }
        return nil
    }

    /// An incomplete installation must finish setup before any update starts.
    static func requireCompleteInstallation(incomplete: Bool) throws {
        if incomplete {
            throw NSError(domain: "io.went.agent-queue.updates", code: 1, userInfo: [NSLocalizedDescriptionKey:
                "Agent Queue is finishing an incomplete installation. Updates resume once setup completes."])
        }
    }

    static var isTestHookBuild: Bool {
        #if AGENT_QUEUE_TEST_HOOKS
        true
        #else
        false
        #endif
    }
}

/// What the update flow recorded about the drain it started.
struct QueueUpdateDrainRecord: Codable, Equatable, Sendable {
    /// The live drain's exact expiry, as the controller reported it.
    let drainUntil: String
    /// The queue was already paused before the flow drained, so releasing must not resume it.
    let wasPaused: Bool
}

/// Decodes JSON that may hold only the listed top-level keys, each once. Codable and
/// JSONSerialization both drop a repeated key silently, so the keys are read from the text.
enum StrictJSON {
    static func decode<T: Decodable>(_ type: T.Type, from data: Data, allowedKeys: Set<String>) throws -> T {
        guard let keys = topLevelKeys(data), Set(keys).count == keys.count, Set(keys).isSubset(of: allowedKeys) else {
            throw ClientError.message("JSON has unknown or duplicate keys.")
        }
        return try JSONDecoder().decode(type, from: data)
    }

    private static func topLevelKeys(_ data: Data) -> [String]? {
        let bytes = [UInt8](data)
        var keys: [String] = [], depth = 0, index = 0
        while index < bytes.count {
            switch bytes[index] {
            case UInt8(ascii: "{"), UInt8(ascii: "["): depth += 1
            case UInt8(ascii: "}"), UInt8(ascii: "]"): depth -= 1
            case UInt8(ascii: "\""):
                var end = index + 1
                while end < bytes.count, bytes[end] != UInt8(ascii: "\"") { end += bytes[end] == UInt8(ascii: "\\") ? 2 : 1 }
                guard end < bytes.count else { return nil }
                var after = end + 1
                while after < bytes.count, [0x20, 0x09, 0x0a, 0x0d].contains(bytes[after]) { after += 1 }
                if depth == 1, after < bytes.count, bytes[after] == UInt8(ascii: ":") {
                    guard let key = try? JSONDecoder().decode(String.self, from: Data(bytes[index...end])) else { return nil }
                    keys.append(key)
                }
                index = end
            default: break
            }
            index += 1
        }
        return depth == 0 ? keys : nil
    }
}

/// Closes queue intake for a bounded time before an update restarts the app, so running
/// callers survive. The marker file binds a drain to this flow: it holds the drain's exact
/// expiry, and a drain whose expiry differs (an operator's own, or one they extended) is
/// never released. The marker is private to the account and lives only as long as the drain.
struct QueueUpdateDrain: Sendable {
    private struct Probe: Decodable {
        struct Queue: Decodable { let drainUntil: String?; let status: String }
        let queue: Queue
    }

    let marker: URL
    let command: @Sendable (QueueClient.Command) async throws -> Data
    var now: @Sendable () -> Date = { Date() }

    /// The recorded drain while it can still be live. A missing, unreadable, foreign or lapsed marker is none.
    var record: QueueUpdateDrainRecord? {
        guard let record = Self.read(marker), let expiry = parseDate(record.drainUntil), expiry > now() else { return nil }
        return record
    }

    var hasMarker: Bool { record != nil }

    /// Removes a marker that can no longer be released: lapsed, unreadable or not the account's own.
    func discardIfStale() { if record == nil { try? FileManager.default.removeItem(at: marker) } }

    /// A failed drain is logged and the update proceeds: the drain only protects callers.
    func begin() async {
        do {
            let before = try await probe()
            if let live = before.drainUntil {
                // Our own drain from an earlier call keeps its marker; anything else is not ours.
                if record?.drainUntil != live { try? FileManager.default.removeItem(at: marker) }
                return
            }
            try? FileManager.default.removeItem(at: marker)
            _ = try await command(.drain)
            guard let until = try await probe().drainUntil else { throw ClientError.message("The drain did not take effect.") }
            try Self.write(QueueUpdateDrainRecord(drainUntil: until, wasPaused: before.status != "running"), to: marker)
        } catch {
            NSLog("Agent Queue update drain failed, continuing: %@", error.localizedDescription)
        }
    }

    /// One attempt per relaunch: the marker goes either way, and an unreleased drain expires on its own.
    func release() async {
        let ours = record
        try? FileManager.default.removeItem(at: marker)
        guard let ours else { return }
        do {
            guard try await probe().drainUntil == ours.drainUntil else { return }
            _ = try await command(ours.wasPaused ? .undrainKeepPaused : .undrain)
        } catch { NSLog("Agent Queue could not release its update drain; it expires on its own: %@", error.localizedDescription) }
    }

    // Only a queue known to be running is resumed on release; a paused or unknown status stays paused.
    private func probe() async throws -> (drainUntil: String?, status: String) {
        let queue = try JSONDecoder().decode(Probe.self, from: try await command(.status)).queue
        return (queue.drainUntil, queue.status)
    }

    /// The marker is the account's own regular file, readable by nobody else, inside the account's own directory.
    private static func privateDirectory(_ directory: URL, create: Bool) -> Bool {
        if create { try? FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700]) }
        var value = stat()
        return lstat(directory.path, &value) == 0 && value.st_mode & S_IFMT == S_IFDIR && value.st_uid == getuid() && value.st_mode & 0o077 == 0
    }

    static func write(_ record: QueueUpdateDrainRecord, to marker: URL) throws {
        guard privateDirectory(marker.deletingLastPathComponent(), create: true) else {
            throw ClientError.message("The update drain directory is not private to this account.")
        }
        unlink(marker.path)
        let descriptor = open(marker.path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0o600)
        guard descriptor >= 0 else { throw ClientError.message("Could not create the update drain marker.") }
        defer { close(descriptor) }
        let data = try JSONEncoder().encode(record)
        guard data.withUnsafeBytes({ Darwin.write(descriptor, $0.baseAddress, data.count) }) == data.count else {
            throw ClientError.message("Could not write the update drain marker.")
        }
    }

    static func read(_ marker: URL) -> QueueUpdateDrainRecord? {
        guard privateDirectory(marker.deletingLastPathComponent(), create: false) else { return nil }
        let descriptor = open(marker.path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard descriptor >= 0 else { return nil }
        defer { close(descriptor) }
        var value = stat()
        guard fstat(descriptor, &value) == 0, value.st_mode & S_IFMT == S_IFREG, value.st_uid == getuid(),
              value.st_mode & 0o077 == 0, value.st_size > 0, value.st_size <= 4096 else { return nil }
        let data = FileHandle(fileDescriptor: descriptor, closeOnDealloc: false).readData(ofLength: 4096)
        return try? StrictJSON.decode(QueueUpdateDrainRecord.self, from: data, allowedKeys: ["drainUntil", "wasPaused"])
    }
}

@MainActor
final class QueueUpdaterDelegate: NSObject, SPUUpdaterDelegate {
    private let drain: QueueUpdateDrain
    private let installationIncomplete: @MainActor () -> Bool

    init(drain: QueueUpdateDrain, installationIncomplete: @escaping @MainActor () -> Bool) {
        self.drain = drain
        self.installationIncomplete = installationIncomplete
    }

    func updater(_ updater: SPUUpdater, mayPerform updateCheck: SPUUpdateCheck) throws {
        try QueueUpdatePolicy.requireCompleteInstallation(incomplete: installationIncomplete())
    }

    func updater(_ updater: SPUUpdater, shouldPostponeRelaunchForUpdate item: SUAppcastItem,
                 untilInvokingBlock installHandler: @escaping () -> Void) -> Bool {
        drainThenInstall(installHandler)
    }

    /// Declining leaves Sparkle to install on a real quit. The block would restart the app mid-session, so it is never called.
    func updater(_ updater: SPUUpdater, willInstallUpdateOnQuit item: SUAppcastItem,
                 immediateInstallationBlock immediateInstallHandler: @escaping () -> Void) -> Bool {
        false
    }

    private func drainThenInstall(_ install: @escaping () -> Void) -> Bool {
        let drain = drain
        Task { @MainActor in
            await drain.begin()
            install()
        }
        return true
    }
}

/// Agent Queue is a menu-bar app, so Sparkle would open a scheduled update's window where it can sit unseen behind
/// other apps. Sparkle shows its window itself only in immediate focus (the app launched recently or the Mac was idle);
/// otherwise this reports the waiting version, and nil once the user has seen it or the update session ends.
@MainActor
final class QueueUpdateAnnouncer: NSObject, @preconcurrency SPUStandardUserDriverDelegate {
    var changed: @MainActor (String?) -> Void = { _ in }

    var supportsGentleScheduledUpdateReminders: Bool { true }

    func standardUserDriverShouldHandleShowingScheduledUpdate(_ update: SUAppcastItem, andInImmediateFocus immediateFocus: Bool) -> Bool {
        immediateFocus
    }

    func standardUserDriverWillHandleShowingUpdate(_ handleShowingUpdate: Bool, forUpdate update: SUAppcastItem, state: SPUUserUpdateState) {
        if !handleShowingUpdate { changed(update.displayVersionString) }
    }

    func standardUserDriverDidReceiveUserAttention(forUpdate update: SUAppcastItem) { changed(nil) }

    func standardUserDriverWillFinishUpdateSession() { changed(nil) }
}

/// Posts the one update notification. Clicking it opens Sparkle's update window.
@MainActor
final class QueueUpdateNotifier: NSObject, @preconcurrency UNUserNotificationCenterDelegate {
    nonisolated static let identifier = "agent-queue-update"
    private let open: @MainActor () -> Void
    private var posted = false

    init(open: @escaping @MainActor () -> Void) { self.open = open }

    /// Called at launch of a build that updates, so a click on a notification left from an earlier run still opens
    /// Sparkle, and a notification for an update that may be gone is removed.
    func adopt() {
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.removeDeliveredNotifications(withIdentifiers: [Self.identifier])
    }

    func post(version: String) {
        UNUserNotificationCenter.current().delegate = self
        posted = true
        Task {
            let granted = (try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert])) ?? false
            if granted { deliver(version: version) }
        }
    }

    // The permission prompt can still be open when the update is withdrawn; post only if it is still wanted.
    private func deliver(version: String) {
        guard posted else { return }
        let center = UNUserNotificationCenter.current()
        let content = UNMutableNotificationContent()
        content.title = "Agent Queue \(version) is available"
        content.body = "Click to install it, or choose Install Update in the menu bar."
        center.add(UNNotificationRequest(identifier: Self.identifier, content: content, trigger: nil))
    }

    func withdraw() {
        guard posted else { return }
        posted = false
        let center = UNUserNotificationCenter.current()
        center.removePendingNotificationRequests(withIdentifiers: [Self.identifier])
        center.removeDeliveredNotifications(withIdentifiers: [Self.identifier])
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        guard response.notification.request.identifier == Self.identifier else { return }
        open()
    }
}

@MainActor
final class QueueUpdateStore: ObservableObject {
    @Published private(set) var installed: InstalledQueueVersion?
    @Published private(set) var isReadingVersion = false
    @Published private(set) var error: String?
    @Published private(set) var canCheckForUpdates = false
    /// A version a scheduled check found while Agent Queue was not in front, until the user turns to it.
    @Published private(set) var availableUpdate: String?
    let running: RunningQueueVersion
    /// Why this build never updates, or nil when it does.
    let unavailableReason: String?
    private let command: @Sendable (QueueClient.Command) async throws -> Data
    private let drain: QueueUpdateDrain
    private let controller: SPUStandardUpdaterController?
    private let updaterDelegate: QueueUpdaterDelegate?
    private let announcer: QueueUpdateAnnouncer?
    private lazy var notifier = QueueUpdateNotifier { [weak self] in self?.checkForUpdates() }

    init(running: RunningQueueVersion,
         testHooks: Bool = QueueUpdatePolicy.isTestHookBuild,
         environment: @escaping @Sendable () -> QueueBootstrapEnvironment,
         command: @escaping @Sendable (QueueClient.Command) async throws -> Data) {
        self.running = running
        self.command = command
        let drain = QueueUpdateDrain(marker: environment().updateDrainMarker, command: command)
        self.drain = drain
        unavailableReason = QueueUpdatePolicy.unavailableReason(running: running, testHooks: testHooks)
        if unavailableReason == nil {
            let delegate = QueueUpdaterDelegate(drain: drain, installationIncomplete: { environment().installIncomplete })
            let announcer = QueueUpdateAnnouncer()
            let controller = SPUStandardUpdaterController(startingUpdater: false, updaterDelegate: delegate, userDriverDelegate: announcer)
            updaterDelegate = delegate
            self.announcer = announcer
            self.controller = controller
        } else {
            updaterDelegate = nil
            announcer = nil
            controller = nil
        }
        announcer?.changed = { [weak self] version in self?.announce(version) }
        controller?.updater.publisher(for: \.canCheckForUpdates).receive(on: DispatchQueue.main).assign(to: &$canCheckForUpdates)
    }

    /// Sparkle asks for consent to check automatically on its first scheduled run, then checks on its own.
    var updaterCreated: Bool { controller != nil }

    func start() {
        guard let controller else { return }
        notifier.adopt()
        controller.startUpdater()
    }

    /// Also resumes an update announced in the background: Sparkle shows it in front.
    func checkForUpdates() {
        guard let controller else { return }
        NSApp.activate()
        controller.checkForUpdates(nil)
    }

    func announce(_ version: String?) {
        availableUpdate = version
        // Only a build that updates announces anything; others never touch the notification centre.
        guard controller != nil else { return }
        if let version { notifier.post(version: version) } else { notifier.withdraw() }
    }

    /// Called once the relaunched queue is up. Releases only a drain the update flow started.
    func releaseDrainAfterRelaunch() async {
        await drain.release()
    }

    /// A marker for a drain that can still be live. Launches without helpers and a queue that never comes up leave it
    /// in place on purpose: the next launch releases it, or it lapses with the drain.
    var hasPendingDrain: Bool { drain.hasMarker }

    func discardStaleDrainMarker() {
        drain.discardIfStale()
    }

    // This runs locally when the window opens. It never contacts the network.
    func refreshInstalled() async {
        guard !isReadingVersion else { return }
        isReadingVersion = true
        defer { isReadingVersion = false }
        do {
            installed = try JSONDecoder().decode(InstalledQueueVersion.self, from: try await command(.version))
            error = nil
        } catch { self.error = "Could not read the installed controller version. \(error.localizedDescription)" }
    }
}

/// Shown in the queue panel while an update found in the background waits for the user.
struct QueueUpdateButton: View {
    @ObservedObject var store: QueueUpdateStore
    let install: @MainActor () -> Void

    var body: some View {
        if let version = store.availableUpdate {
            Button("Install Update \(version)…", systemImage: "arrow.down.circle", action: install)
                .buttonStyle(.borderless).foregroundStyle(Color.accentColor)
                .accessibilityIdentifier("agent-queue-install-update")
        }
    }
}

struct QueueUpdatesSettings: View {
    @ObservedObject var store: QueueUpdateStore
    let desktop: QueueDesktop
    var onDone: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 14) {
                Image(nsImage: QueueBrand.image(height: 40)).renderingMode(.template).foregroundStyle(.tint)
                VStack(alignment: .leading, spacing: 5) {
                    Text("Agent Queue").font(.title2.weight(.semibold))
                    Text("About and updates").font(.callout).foregroundStyle(.secondary)
                }
                Spacer()
            }.padding(22)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    versionRow("Running app", value: store.running.displayVersion)
                    VStack(alignment: .leading, spacing: 5) {
                        versionRow("Installed controller", value: store.installed?.displayVersion ?? "Not yet read")
                        if let installed = store.installed, !installed.tracked {
                            Text("This installation has no published build number.")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    if let reason = store.unavailableReason {
                        VStack(alignment: .leading, spacing: 8) {
                            Label(reason, systemImage: "info.circle").font(.callout)
                            Link("Open the releases page", destination: QueueUpdatePolicy.releasesPage)
                                .environment(\.openURL, OpenURLAction { desktop(.open($0)); return .handled })
                        }
                    } else {
                        Text("Agent Queue checks for updates automatically once you allow it. An update is verified, installed in place and the app relaunches. Jobs still running keep running, and new queued calls wait for a bounded time first.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    if let error = store.error {
                        Label(error, systemImage: "exclamationmark.triangle").font(.callout)
                            .foregroundStyle(.orange).textSelection(.enabled)
                    }
                }.frame(maxWidth: .infinity, alignment: .leading).padding(22)
            }
            Divider()
            HStack {
                Button("Done", action: onDone)
                Spacer()
                Button("Check for Updates…") { store.checkForUpdates() }
                    .buttonStyle(.borderedProminent).disabled(!store.canCheckForUpdates)
            }.padding(18)
        }.background(Color(nsColor: .windowBackgroundColor))
            .task { await store.refreshInstalled() }
    }

    private func versionRow(_ title: String, value: String) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            Text(title).font(.caption).foregroundStyle(.secondary)
            Text(value).font(.headline).textSelection(.enabled)
        }
    }
}
