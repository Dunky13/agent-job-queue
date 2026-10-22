import AppKit
import SwiftUI
import Foundation

@MainActor
enum InstallerActivity { static var running = false }

@MainActor
final class InstallerModel: ObservableObject {
    @Published var output = "Install Agent Queue for your macOS account.\n\nIncludes its queue and HTTPS runtime. macOS may need an internet connection for first-open verification. Finish or cancel all existing jobs before the first legacy queue migration. Later embedded queue upgrades preserve running jobs and settings. Phone access stays off until you enable it.\n"
    @Published var running = false
    @Published var completed = false
    private var process: Process?
    private var pipe: Pipe?

    func install() {
        guard !running, !completed else { return }
        guard let resources = Bundle.main.resourceURL else {
            output += "\nInstaller resources are missing. Download the installer again.\n"
            return
        }
        let payload = resources.appendingPathComponent("payload")
        let controller = payload.appendingPathComponent("source/bin/agent-heavy")
        let task = Process()
        let stream = Pipe()
        task.executableURL = controller
        task.arguments = ["install", "--payload", payload.path]
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
                self.output += status == 0 ? "\nInstallation complete. Agent Queue is in your menu bar.\n" : "\nInstallation failed (exit \(status)). Review the log above, correct the issue, then try again.\n"
                self.process = nil
            }
        }
        output = "Starting installation...\n"
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
            output += "\nCould not start installer: \(error.localizedDescription)\n"
        }
    }

    private func append(_ message: String) {
        output += message
        if output.count > 200_000 { output = String(output.suffix(200_000)) }
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
    @StateObject private var model = InstallerModel()
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Label("Agent Job Queue", systemImage: "square.stack.3d.up")
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
                Button(model.completed ? "Done" : "Quit") { NSApplication.shared.terminate(nil) }
                    .disabled(model.running)
                if !model.completed {
                    Button("Install") { model.install() }.buttonStyle(.borderedProminent)
                        .keyboardShortcut(.defaultAction).disabled(model.running)
                }
            }
        }.padding(24).frame(minWidth: 650, minHeight: 480)
            .background(InstallerWindowState(installing: model.running))
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
