import AppKit
import SwiftUI
import Foundation
import ServiceManagement

@MainActor
enum InstallerActivity { static var running = false }

@MainActor
final class InstallerModel: ObservableObject {
    @Published var output = "Preparing Agent Queue…\n"
    @Published var running = false
    @Published var completed = false
    @Published private(set) var requiresServiceApproval = false
    private var process: Process?
    private var pipe: Pipe?
    private var progressOutput = ""
    private let bundle: Bundle
    private let installationCompleted: @MainActor () -> Void

    init(bundle: Bundle = .main,
         installationCompleted: @escaping @MainActor () -> Void = { NSApplication.shared.terminate(nil) }) {
        self.bundle = bundle
        self.installationCompleted = installationCompleted
    }

    func install() {
        guard !running, !completed else { return }
        guard let resources = bundle.resourceURL else {
            output += "\nInstaller resources are missing. Download the installer again.\n"
            return
        }
        let payload = resources.appendingPathComponent("payload")
        let controller = payload.appendingPathComponent("source/bin/agent-heavy")
        guard QueueExecutableNamespace.allows(executable: controller, root: bundle.bundleURL) else {
            output += "\nThe installer location permits unsafe executable replacement. Open it from a protected download or your account's Applications folder. No installation was started.\n"
            return
        }
        let task = Process()
        let stream = Pipe()
        task.executableURL = controller
        task.arguments = ["install", "--payload", payload.path, "--repair-and-continue"]
        task.standardOutput = stream
        task.standardError = stream
        task.standardInput = FileHandle.nullDevice
        task.environment = ProcessInfo.processInfo.environment.merging([
            "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        ]) { _, installer in installer }
        stream.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty else { return }
            let message = String(decoding: data, as: UTF8.self)
            Task { @MainActor [weak self] in self?.append(message) }
        }
        task.terminationHandler = { [weak self] finished in
            let status = finished.terminationStatus
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.running = false
                InstallerActivity.running = false
                self.completed = status == 0
                self.requiresServiceApproval = false
                self.append(status == 0 ? "\nInstallation complete. Agent Queue is in your menu bar.\n" : "\nInstallation failed (exit \(status)). Review the log above, correct the issue, then try again.\n")
                self.process = nil
                if self.completed { self.installationCompleted() }
            }
        }
        output = "Starting installation...\n"
        progressOutput = output
        requiresServiceApproval = false
        running = true
        InstallerActivity.running = true
        process = task
        pipe = stream
        do { try task.run() }
        catch {
            stream.fileHandleForReading.readabilityHandler = nil
            running = false
            InstallerActivity.running = false
            process = nil
            append("\nCould not start installer: \(error.localizedDescription)\n")
        }
    }

    private func append(_ message: String) {
        progressOutput += message
        if progressOutput.count > 200_000 { progressOutput = String(progressOutput.suffix(200_000)) }
        let progress = QueueSetupProgress.parse(progressOutput)
        if running { requiresServiceApproval = requiresServiceApproval || progress.requiresServiceApproval }
        output = progress.output
    }
}

struct InstallerWindowState: NSViewRepresentable {
    let installing: Bool
    func makeNSView(context: Context) -> NSView { NSView() }
    func updateNSView(_ view: NSView, context: Context) {
        DispatchQueue.main.async {
            view.window?.standardWindowButton(.closeButton)?.isEnabled = !installing
        }
    }
}

struct InstallerView: View {
    @StateObject private var model: InstallerModel

    init(model: InstallerModel = InstallerModel()) { _model = StateObject(wrappedValue: model) }
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Label("Agent Job Queue", systemImage: QueueBrand.symbolName)
                .font(.largeTitle.bold())
            Text("Keep heavy builds and tests under control.").foregroundStyle(.secondary)
            ScrollView {
                Text(model.output).font(.system(.body, design: .monospaced))
                    .textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                    .padding(12)
            }.background(.quaternary.opacity(0.35)).clipShape(RoundedRectangle(cornerRadius: 8))
            HStack {
                if model.running { ProgressView().controlSize(.small); Text("Installing...") }
                Spacer()
                if model.requiresServiceApproval {
                    Button("Login Items settings") { SMAppService.openSystemSettingsLoginItems() }
                }
                Button(model.completed ? "Done" : "Quit") { NSApplication.shared.terminate(nil) }
                    .disabled(model.running)
                if !model.completed {
                    Button("Retry") { model.install() }.buttonStyle(.borderedProminent)
                        .keyboardShortcut(.defaultAction).disabled(model.running)
                }
            }
        }.padding(24).frame(minWidth: 650, minHeight: 480)
            .background(InstallerWindowState(installing: model.running))
            .task { model.install() }
    }
}

final class InstallerDelegate: NSObject, NSApplicationDelegate {
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        !InstallerActivity.running
    }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        InstallerActivity.running ? .terminateCancel : .terminateNow
    }
}

@main
struct AgentQueueInstaller: App {
    @NSApplicationDelegateAdaptor(InstallerDelegate.self) private var delegate
    var body: some Scene {
        WindowGroup("Install Agent Job Queue") { InstallerView() }
            .defaultSize(width: 720, height: 540)
    }
}
