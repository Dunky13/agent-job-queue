import AppKit
import SwiftUI
import Foundation

private actor ResumeUIBackend {
    var snapshot: QueueSnapshot
    private(set) var commands: [[String]] = []
    init(_ snapshot: QueueSnapshot) { self.snapshot = snapshot }
    func load() throws -> Data { try JSONEncoder().encode(snapshot) }
    func command(_ arguments: [String]) throws -> Data {
        guard arguments == ["resume"] else { throw ClientError.message("Unexpected fixture command") }
        commands.append(arguments)
        let queue = snapshot.queue
        snapshot = QueueSnapshot(updatedAt: snapshot.updatedAt,
            queue: QueueSummary(status: "running", limit: queue.limit, running: queue.running,
                                waiting: queue.waiting, legacyOccupied: queue.legacyOccupied), jobs: snapshot.jobs,
            scheduler: snapshot.scheduler, lanes: snapshot.lanes, plugins: snapshot.plugins)
        return Data("{}".utf8)
    }
}

@MainActor
private final class ResumeUIDelegate: NSObject, NSApplicationDelegate {
    private var windows: [NSWindow] = []
    private var checks: [String: Bool] = [:]

    func applicationDidFinishLaunching(_ notification: Notification) {
        Task { @MainActor in
            do {
                try await verify()
                NSApp.terminate(nil)
            } catch {
                FileHandle.standardError.write(Data("Resume UI fixture: \(error.localizedDescription)\n".utf8))
                exit(EXIT_FAILURE)
            }
        }
    }

    private func option(_ name: String) throws -> String {
        let arguments = CommandLine.arguments
        guard let index = arguments.firstIndex(of: name), arguments.indices.contains(index + 1) else {
            throw ClientError.message("Missing fixture argument \(name)")
        }
        return arguments[index + 1]
    }

    private func check(_ value: Bool, _ name: String) throws {
        checks[name] = value
        guard value else { throw ClientError.message(name) }
        print("PASS \(name)")
    }

    private func table(in root: NSView) -> NSTableView? {
        if let table = root as? NSTableView { return table }
        for child in root.subviews { if let table = table(in: child) { return table } }
        return nil
    }

    private func pressResume(in table: NSTableView, window: NSWindow) throws {
        // The paused banner is the first list row. Click its trailing native button,
        // relative to the actual row, so header height and window size do not matter.
        let row = table.rect(ofRow: 0)
        let point = table.convert(NSPoint(x: row.maxX - 40, y: row.midY), to: nil)
        guard let down = NSEvent.mouseEvent(with: .leftMouseDown, location: point, modifierFlags: [],
            timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: window.windowNumber,
            context: nil, eventNumber: 0, clickCount: 1, pressure: 1),
              let up = NSEvent.mouseEvent(with: .leftMouseUp, location: point, modifierFlags: [],
            timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: window.windowNumber,
            context: nil, eventNumber: 1, clickCount: 1, pressure: 0) else {
            throw ClientError.message("Cannot create private fixture click")
        }
        NSApp.postEvent(up, atStart: true)
        window.sendEvent(down)
    }

    private func capture(_ view: NSView, to url: URL) throws {
        view.layoutSubtreeIfNeeded()
        guard let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) else {
            throw ClientError.message("Cannot prepare fixture screenshot")
        }
        view.cacheDisplay(in: view.bounds, to: bitmap)
        var background = NSColor.windowBackgroundColor.cgColor
        view.effectiveAppearance.performAsCurrentDrawingAppearance { background = NSColor.windowBackgroundColor.cgColor }
        try NativeSnapshotRenderer.png(from: bitmap, background: background).write(to: url)
    }

    private func verify() async throws {
        let output = URL(fileURLWithPath: try option("--output"), isDirectory: true)
        try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
        let original = try JSONDecoder().decode(QueueSnapshot.self,
            from: Data(contentsOf: URL(fileURLWithPath: try option("--input"))))
        guard let queued = original.jobs.first(where: \.isWaiting) else { throw ClientError.message("Waiting fixture job missing") }
        let captureOnly = CommandLine.arguments.contains("--capture-only")
        for fullWindow in [false, true] {
            let name = fullWindow ? "window-waiting" : "menu-empty"
            let jobs = fullWindow ? [queued] : []
            let snapshot = QueueSnapshot(updatedAt: original.updatedAt,
                queue: QueueSummary(status: "paused", limit: 3, running: 0, waiting: jobs.count, legacyOccupied: false),
                jobs: jobs, scheduler: original.scheduler)
            let backend = ResumeUIBackend(snapshot)
            // Only this store opts out of CLI fixtures, with both providers injected.
            // All global settings stores retain --fixture and cannot reach live controllers.
            let store = QueueStore(statusLoader: { try await backend.load() }, command: { try await backend.command($0) }, arguments: [])
            try check(AppRuntime.store.fixtureMode && AppRuntime.remoteStore.fixtureMode,
                      "\(name): global controllers remain inert")
            await store.refresh()
            let size = NSSize(width: fullWindow ? 800 : 500, height: fullWindow ? 700 : 650)
            let window = NSWindow(contentRect: NSRect(origin: .zero, size: size),
                                  styleMask: [.titled, .closable], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            window.appearance = NSAppearance(named: .darkAqua)
            let view = NSHostingView(rootView: QueuePanel(store: store, fullWindow: fullWindow,
                                                         tracksVisibility: false, navigation: QueueNavigation()))
            window.contentView = view
            window.setContentSize(size)
            window.orderFront(nil)
            windows.append(window)
            try await Task.sleep(for: .milliseconds(400))
            try check(store.snapshot?.queue.status == "paused", "\(name): paused fixture loaded")
            try capture(view, to: output.appendingPathComponent("paused-\(name).png"))
            if !captureOnly {
                guard let list = table(in: view), list.numberOfRows > 0 else {
                    throw ClientError.message("Paused fixture list missing")
                }
                let pausedRows = list.numberOfRows
                window.makeKeyAndOrderFront(nil)
                try pressResume(in: list, window: window)
                let deadline = ContinuousClock.now + .seconds(5)
                while store.snapshot?.queue.status != "running" && ContinuousClock.now < deadline {
                    try await Task.sleep(for: .milliseconds(10))
                }
                try check(store.snapshot?.queue.status == "running" && store.waiting.count == jobs.count,
                          "\(name): Resume refreshes state and preserves jobs")
                try check(await backend.commands == [["resume"]], "\(name): one exact resume command")
                let renderDeadline = ContinuousClock.now + .seconds(5)
                while list.numberOfRows == pausedRows && ContinuousClock.now < renderDeadline {
                    try await Task.sleep(for: .milliseconds(10))
                }
                try check(list.numberOfRows == pausedRows - 1, "\(name): paused control disappears after resume")
                try capture(view, to: output.appendingPathComponent("resumed-\(name).png"))
            }
            window.orderOut(nil)
            window.close()
        }
        let report = try JSONSerialization.data(withJSONObject: ["passed": checks.values.allSatisfy { $0 },
                                                                 "captureOnly": captureOnly, "checks": checks],
                                                options: [.prettyPrinted, .sortedKeys])
        try report.write(to: output.appendingPathComponent("verification.json"))
    }
}

@main
private struct ResumeUITests {
    @MainActor static func main() {
        let app = NSApplication.shared
        let delegate = ResumeUIDelegate()
        app.setActivationPolicy(.accessory)
        app.delegate = delegate
        app.run()
        withExtendedLifetime(delegate) {}
    }
}
