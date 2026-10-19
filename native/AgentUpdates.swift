import AppKit
import Foundation
import SwiftUI

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
            let metadata = try JSONDecoder().decode(RunningBuildMetadata.self, from: data)
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
    let schemaVersion: Int
    let repository: String
    let version: String
    let commit: String?
    let build: Int?
}

struct QueueRelease: Codable, Equatable, Sendable {
    let version: String
    let build: Int
    let commit: String
    let tag: String
    let url: String
    var releaseURL: URL? {
        guard let components = URLComponents(string: url), components.scheme == "https",
              components.host == "github.com", components.user == nil, components.password == nil,
              components.port == nil, components.path.hasPrefix("/Dunky13/agent-job-queue/releases/") else { return nil }
        return components.url
    }
}

enum QueueUpdateComparison: String, Codable, Sendable {
    case available, current, newer, untracked, unavailable
}

struct QueueUpdateCheck: Codable, Sendable {
    let installed: InstalledQueueVersion
    let latest: QueueRelease?
    let comparison: QueueUpdateComparison
    let canInstall: Bool
    let message: String
}

struct QueueInstallerResult: Codable, Sendable {
    let installerOpened: Bool
    let build: Int
    let message: String
}

@MainActor
final class QueueUpdateStore: ObservableObject {
    enum Operation { case version, checking, downloading }
    @Published private(set) var installed: InstalledQueueVersion?
    @Published private(set) var check: QueueUpdateCheck?
    @Published private(set) var operation: Operation?
    @Published private(set) var error: String?
    @Published private(set) var notice: String?
    @Published private(set) var installerOpened = false
    let running: RunningQueueVersion
    let fixtureMode: Bool
    private let command: @Sendable ([String]) async throws -> Data

    init(fixtureMode: Bool = false, running: RunningQueueVersion = .current(),
         command: @escaping @Sendable ([String]) async throws -> Data = {
             try await QueueClient.run($0, budget: $0.first == "updates" ? .updates : .normal)
         }) {
        self.fixtureMode = fixtureMode
        self.running = fixtureMode ? RunningQueueVersion(version: "1.0.0", bundleBuild: "100", publishedBuild: 100) : running
        self.command = command
        if fixtureMode {
            let preview = InstalledQueueVersion(version: "1.0.0", build: 100, bundleBuild: "100", commit: nil,
                tracked: true, displayVersion: "1.0.0 (build 100)")
            installed = preview
            check = QueueUpdateCheck(installed: preview,
                latest: QueueRelease(version: "1.0.1", build: 101, commit: String(repeating: "a", count: 40),
                    tag: "build-101", url: "https://github.com/Dunky13/agent-job-queue/releases/tag/build-101"),
                comparison: .available, canInstall: true, message: "Version 1.0.1 (build 101) is available.")
        }
    }

    var isBusy: Bool { operation != nil }
    var canInstall: Bool {
        guard !fixtureMode, !isBusy, !installerOpened, let check, check.canInstall,
              check.comparison == .available || check.comparison == .untracked,
              running.metadataError == nil,
              let release = check.latest, release.releaseURL != nil else { return false }
        if let runningBuild = running.publishedBuild, release.build <= runningBuild { return false }
        if check.installed.tracked {
            guard let installedBuild = check.installed.build, release.build > installedBuild else { return false }
        }
        return true
    }
    var isReplacement: Bool { check?.installed.tracked == false || running.publishedBuild == nil }
    var installButtonLabel: String {
        if installerOpened { return "Installer opened" }
        return isReplacement ? "Download published installer…" : "Download and open installer"
    }
    var compatibilityMessage: String? {
        if let error = running.metadataError { return error }
        guard let release = check?.latest, check?.canInstall == true else { return nil }
        guard let runningBuild = running.publishedBuild else {
            return check?.installed.tracked == false ? nil : "The running app has no published build number. Choose a published release to replace it."
        }
        if runningBuild > release.build { return "The running app is newer than the published release. Downgrades are disabled." }
        if runningBuild == release.build { return "The running app already has this published build." }
        return nil
    }
    var progressLabel: String {
        switch operation {
        case .version: "Reading installed version…"
        case .checking: "Checking for updates…"
        case .downloading: "Downloading and verifying installer…"
        case nil: ""
        }
    }

    // This runs locally when the window opens. Network access requires the Check button.
    func refreshInstalled() async {
        guard !fixtureMode, !isBusy else { return }
        operation = .version
        defer { operation = nil }
        do {
            let latestInstalled = try JSONDecoder().decode(InstalledQueueVersion.self,
                from: try await command(["version", "--json"]))
            if let check, check.installed != latestInstalled { self.check = nil }
            installed = latestInstalled
            error = nil
        } catch { self.error = "Could not read the installed controller version. \(error.localizedDescription)" }
    }

    @discardableResult
    func checkForUpdates() -> Task<Void, Never>? {
        guard !fixtureMode, !isBusy else { return nil }
        operation = .checking
        error = nil
        notice = nil
        check = nil
        installerOpened = false
        return Task {
            defer { operation = nil }
            do {
                let result = try JSONDecoder().decode(QueueUpdateCheck.self,
                    from: try await command(["updates", "check", "--json"]))
                if let latest = result.latest {
                    guard latest.build > 0, latest.releaseURL != nil else {
                        throw ClientError.message("The update check returned an invalid release.")
                    }
                }
                installed = result.installed
                check = result
            } catch { self.error = "Could not check for updates. \(error.localizedDescription)" }
        }
    }

    @discardableResult
    func openInstaller() -> Task<Void, Never>? {
        guard canInstall, let release = check?.latest else { return nil }
        operation = .downloading
        error = nil
        notice = nil
        return Task {
            defer { operation = nil }
            do {
                let result = try JSONDecoder().decode(QueueInstallerResult.self,
                    from: try await command(["updates", "install", "--build", String(release.build), "--json"]))
                guard result.installerOpened, result.build == release.build else {
                    throw ClientError.message("The requested installer was not opened. Check for updates before retrying.")
                }
                installerOpened = true
                notice = result.message
            } catch { self.error = "Could not open the update installer. \(error.localizedDescription)" }
        }
    }
}

struct QueueUpdatesSettings: View {
    @ObservedObject var store: QueueUpdateStore
    var onDone: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 14) {
                Image(systemName: "tray.full.fill").font(.system(size: 34)).foregroundStyle(.tint)
                VStack(alignment: .leading, spacing: 5) {
                    Text("Agent Queue").font(.title2.weight(.semibold))
                    Text("About and updates").font(.callout).foregroundStyle(.secondary)
                }
                Spacer()
                if store.fixtureMode { Text("PREVIEW").font(.caption2).foregroundStyle(.secondary) }
            }.padding(22)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    versionRow("Running app", value: store.running.displayVersion)
                    VStack(alignment: .leading, spacing: 5) {
                        versionRow("Installed controller", value: store.installed?.displayVersion ?? "Not yet read")
                        if let installed = store.installed, !installed.tracked {
                            Text("This installation has no published build number. Choose a published release to replace it.")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    if let check = store.check {
                        VStack(alignment: .leading, spacing: 8) {
                            if let release = check.latest {
                                versionRow("Latest release", value: "\(release.version) · build \(release.build)")
                                if let url = release.releaseURL {
                                    Link("Release notes", destination: url).disabled(store.fixtureMode)
                                }
                            }
                            Label(check.message, systemImage: check.comparison == .available ? "arrow.down.circle" : "info.circle")
                                .font(.callout).textSelection(.enabled)
                        }
                    } else {
                        Text("Check for updates to contact GitHub for the latest published release.")
                            .font(.callout).foregroundStyle(.secondary)
                    }
                    if let message = store.compatibilityMessage {
                        Label(message, systemImage: store.running.metadataError == nil ? "info.circle" : "exclamationmark.triangle")
                            .font(.callout).foregroundStyle(store.running.metadataError == nil ? Color.secondary : Color.orange)
                    }
                    Text("Updates download a verified installer. You choose Install in that window. Jobs continue independently during installation.")
                        .font(.caption).foregroundStyle(.secondary)
                    if let error = store.error {
                        Label(error, systemImage: "exclamationmark.triangle").font(.callout)
                            .foregroundStyle(.orange).textSelection(.enabled)
                    }
                    if let notice = store.notice {
                        Label(notice, systemImage: "checkmark.circle").font(.callout)
                            .foregroundStyle(.green).textSelection(.enabled)
                    }
                }.frame(maxWidth: .infinity, alignment: .leading).padding(22)
            }
            Divider()
            VStack(alignment: .leading, spacing: 12) {
                if store.isBusy {
                    HStack { ProgressView().controlSize(.small); Text(store.progressLabel).font(.caption) }
                }
                HStack {
                    Button("Done", action: onDone)
                    Spacer()
                    Button("Check for updates") { store.checkForUpdates() }.disabled(store.fixtureMode || store.isBusy)
                    Button(store.installButtonLabel) { store.openInstaller() }
                        .buttonStyle(.borderedProminent).disabled(!store.canInstall)
                }
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
