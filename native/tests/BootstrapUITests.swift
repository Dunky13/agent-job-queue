import AppKit
import Foundation

// A distinct process with a disposable HOME drives the real delegate and window.
// The installer is an injected closure; normal startup uses --fixture throughout.
@MainActor
final class BootstrapUIFixtureDelegate: NSObject, NSApplicationDelegate {
    private var production: AppDelegate?
    private var checks: [String: Bool] = [:]
    private var pictures: [(String, Data)] = []
    private let output: URL
    private let temporary: URL

    init(output: URL, temporary: URL) { self.output = output; self.temporary = temporary }

    func applicationDidFinishLaunching(_ notification: Notification) {
        let watchdog = Timer(timeInterval: 45, target: self, selector: #selector(timedOut(_:)), userInfo: nil, repeats: false)
        RunLoop.main.add(watchdog, forMode: .common)
        Task {
            do {
                try await verify(notification)
                try writeReport()
                NSApp.terminate(nil)
            } catch {
                try? writeReport(error: error.localizedDescription)
                FileHandle.standardError.write(Data("Bootstrap UI fixture failed: \(error.localizedDescription)\n".utf8))
                production?.applicationWillTerminate(notification)
                exit(EXIT_FAILURE)
            }
        }
    }

    @objc private func timedOut(_ timer: Timer) {
        try? writeReport(error: "Bootstrap UI fixture exceeded its 45-second deadline.")
        production?.applicationWillTerminate(Notification(name: NSApplication.willTerminateNotification))
        exit(EXIT_FAILURE)
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        production?.applicationShouldTerminate(sender) ?? .terminateNow
    }

    func applicationWillTerminate(_ notification: Notification) { production?.applicationWillTerminate(notification) }

    private func check(_ condition: Bool, _ name: String) throws {
        checks[name] = condition
        print("\(condition ? "PASS" : "FAIL") \(name)")
        if !condition { throw ClientError.message(name) }
    }

    private func waitUntil(_ predicate: () async -> Bool) async throws {
        let deadline = ProcessInfo.processInfo.systemUptime + 8
        while !(await predicate()) {
            guard ProcessInfo.processInfo.systemUptime < deadline else {
                throw ClientError.message("Bootstrap UI fixture did not reach the expected state. Phase: \(String(describing: production?.bootstrap?.phase)); output: \(production?.bootstrap?.output ?? "no model")")
            }
            try await Task.sleep(for: .milliseconds(10))
        }
    }

    private func verify(_ notification: Notification) async throws {
        guard CommandLine.arguments.contains("--fixture"),
              let privateHome = ProcessInfo.processInfo.environment["HOME"],
              URL(fileURLWithPath: privateHome).deletingLastPathComponent().standardizedFileURL == temporary.standardizedFileURL,
              ProcessInfo.processInfo.environment["CFFIXED_USER_HOME"] == privateHome else {
            throw ClientError.message("UI fixture requires --fixture, a temporary HOME and CFFIXED_USER_HOME from its runner.")
        }
        let fixture = try BootstrapFixture(parent: temporary)
        let phoneAccessBlocker = !CommandLine.arguments.contains("--generic-retry")
        let executor = BootstrapExecutorFixture(fixture, phoneAccessBlocker: phoneAccessBlocker)
        let delegate = AppDelegate(environment: fixture.environment) { request, output in
            try await executor.execute(request, output: output)
        }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        try await waitUntil { await executor.isHeld && delegate.bootstrap?.output.contains("Preparing") == true }
        guard let setup = delegate.setupWindow, let model = delegate.bootstrap else {
            throw ClientError.message("Production setup window was not presented")
        }
        try check(setup.isVisible && model.phase == .installing && AppRuntime.menuBarController == nil && AppRuntime.queueWindow == nil,
            "Progress window appears before normal menu and queue window")
        try check(delegate.applicationShouldTerminate(NSApp) == .terminateCancel,
            "Quit is guarded while the installer owns setup")
        try snapshot(setup, name: "01-progress")
        await executor.release()
        try await waitUntil { model.phase == .failed }
        let expectedFailure = phoneAccessBlocker ? model.canDisablePhoneAccess && model.message.contains("Phone access") :
            !model.canDisablePhoneAccess && model.output.contains("Fixture service could not start")
        try check(delegate.setupWindow === setup && setup.isVisible && expectedFailure &&
            !model.output.contains("AGENT_QUEUE_SETUP_RECOVERY") &&
            AppRuntime.menuBarController == nil && delegate.applicationShouldTerminate(NSApp) == .terminateNow,
            "Failure keeps the same window, shows the diagnostic and permits quit")
        try snapshot(setup, name: phoneAccessBlocker ? "02-disable-phone-access" : "02-failure-retry")

        // Exercise SwiftUI's real recovery default action through its hosting window.
        setup.makeKeyAndOrderFront(nil)
        guard let enter = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: [],
            timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: setup.windowNumber, context: nil,
            characters: "\r", charactersIgnoringModifiers: "\r", isARepeat: false, keyCode: 36) else {
            throw ClientError.message("Could not create the recovery fixture key event")
        }
        if !setup.performKeyEquivalent(with: enter) { setup.sendEvent(enter) }
        try await waitUntil { await executor.isHeld }
        try check(model.phase == .installing && delegate.setupWindow === setup && delegate.applicationShouldTerminate(NSApp) == .terminateCancel,
            "\(phoneAccessBlocker ? "Disable phone access and continue" : "Retry") starts a private attempt and restores termination guard")
        await executor.release()
        try await waitUntil { model.phase == .complete && AppRuntime.queueWindow?.isVisible == true }
        guard let controller = AppRuntime.menuBarController, let queue = AppRuntime.queueWindow else {
            throw ClientError.message("Normal fixture startup did not create its menu and queue window")
        }
        let requests = await executor.requests
        try check(delegate.setupWindow == nil && !setup.isVisible && AppRuntime.store.fixtureMode &&
            requests.count == 2 && requests[0].arguments == ["install", "--app-bundle", fixture.environment.app.path] &&
            requests[1].arguments == requests[0].arguments + (phoneAccessBlocker ? ["--disable-phone-access"] : []),
            "Successful recovery verifies readback, closes setup and continues real fixture startup")
        try check(model.start() == nil && AppRuntime.menuBarController === controller &&
            delegate.applicationShouldTerminate(NSApp) == .terminateNow,
            "Completed startup keeps exactly one menu controller and permits normal quit")
        try snapshot(queue, name: "03-normal-fixture-startup")
    }

    private func snapshot(_ window: NSWindow, name: String) throws {
        guard let view = window.contentView else { throw ClientError.message("Fixture window has no content view") }
        view.layoutSubtreeIfNeeded()
        window.displayIfNeeded()
        guard let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) else {
            throw ClientError.message("Could not render the bootstrap fixture window")
        }
        view.cacheDisplay(in: view.bounds, to: bitmap)
        let png = try NativeSnapshotRenderer.png(from: bitmap, background: window.backgroundColor.cgColor)
        try png.write(to: output.appendingPathComponent("\(name).png"))
        pictures.append((name, png))
    }

    private func writeReport(error: String? = nil) throws {
        struct Result: Encodable { let passed: Bool; let checks: [String: Bool]; let error: String? }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let passed = error == nil && !checks.isEmpty && checks.values.allSatisfy { $0 }
        try encoder.encode(Result(passed: passed, checks: checks, error: error)).write(to: output.appendingPathComponent("verification.json"))
        func escape(_ text: String) -> String {
            text.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "<", with: "&lt;")
                .replacingOccurrences(of: ">", with: "&gt;").replacingOccurrences(of: "\"", with: "&quot;")
        }
        let list = checks.sorted { $0.key < $1.key }.map { "<li>\($0.value ? "PASS" : "FAIL") \(escape($0.key))</li>" }.joined()
        let images = pictures.map { "<figure><figcaption>\(escape($0.0))</figcaption><img alt='\(escape($0.0))' src='data:image/png;base64,\($0.1.base64EncodedString())'></figure>" }.joined()
        let html = "<!doctype html><html lang='en'><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'><title>Bootstrap fixture verification</title><style>body{font:16px system-ui;margin:2rem;background:#f2f2f5;color:#202026}main{max-width:1000px;margin:auto}li{margin:.5rem 0}figure{margin:2rem 0}img{display:block;max-width:100%;height:auto;margin-top:1rem}figcaption{font-weight:600}</style><main><h1>Bootstrap fixture: \(passed ? "passed" : "failed")</h1><p>Production setup window, injected executor, disposable home and simulated queue.</p><p>\(escape(error ?? ""))</p><ul>\(list)</ul>\(images)</main></html>"
        try Data(html.utf8).write(to: output.appendingPathComponent("index.html"))
    }
}

@main
struct BootstrapUITests {
    @MainActor static func main() throws {
        let arguments = CommandLine.arguments
        guard let outputIndex = arguments.firstIndex(of: "--output"), arguments.indices.contains(outputIndex + 1),
              let tempIndex = arguments.firstIndex(of: "--temporary"), arguments.indices.contains(tempIndex + 1) else {
            throw ClientError.message("Run native/tests/run-bootstrap-ui-tests.sh OUTPUT_DIRECTORY")
        }
        let app = NSApplication.shared
        app.setActivationPolicy(.accessory)
        let delegate = BootstrapUIFixtureDelegate(output: URL(fileURLWithPath: arguments[outputIndex + 1]),
            temporary: URL(fileURLWithPath: arguments[tempIndex + 1]))
        app.delegate = delegate
        withExtendedLifetime(delegate) { app.run() }
    }
}
