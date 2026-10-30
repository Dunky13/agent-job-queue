import AppKit
import Foundation
import SwiftUI

// A distinct process with a disposable HOME drives the real delegate and window.
// Setup is an injected closure; normal startup uses --fixture throughout.
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
                if let window = production?.setupWindow { try? snapshot(window, name: "failure") }
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
        if CommandLine.arguments.contains("--drag-and-drop") {
            try await verifyDragAndDropLaunch(notification)
            return
        }
        if CommandLine.arguments.contains("--cancellable-wait") {
            try await verifyCancellableWait(notification)
            return
        }
        if CommandLine.arguments.contains("--silent-recovery") {
            try await verifySilentRecovery(notification)
            return
        }
        if CommandLine.arguments.contains("--approved-repair") {
            try await verifyApprovedRepair(notification)
            return
        }
        if CommandLine.arguments.contains("--preflight-summary") {
            try await verifyPreflight(notification)
            return
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
        guard let model = delegate.bootstrap else { throw ClientError.message("Production setup did not start") }
        try check(delegate.setupWindow == nil && model.phase == .installing && AppRuntime.menuBarController == nil && AppRuntime.queueWindow == nil,
            "A clean first launch sets up silently before the normal menu and queue window")
        try check(delegate.applicationShouldTerminate(NSApp) == .terminateCancel,
            "Quit is guarded while setup runs")
        await executor.release()
        try await waitUntil { model.phase == .failed && delegate.setupWindow != nil }
        guard let setup = delegate.setupWindow else { throw ClientError.message("Setup failure did not present its window") }
        let expectedFailure = phoneAccessBlocker ? model.canDisablePhoneAccess && model.message.contains("Phone access") :
            !model.canDisablePhoneAccess && model.output.contains("Fixture service could not start")
        try check(setup.isVisible && expectedFailure &&
            !model.output.contains("AGENT_QUEUE_SETUP_RECOVERY") &&
            AppRuntime.menuBarController == nil && delegate.applicationShouldTerminate(NSApp) == .terminateNow,
            "Failure opens the setup window, shows the diagnostic and permits quit")
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
            requests.count == 2 && requests[0].arguments == ["install", "--app-bundle", fixture.environment.app.path, "--repair-and-continue"] &&
            requests[1].arguments == ["install", "--app-bundle", fixture.environment.app.path] + (phoneAccessBlocker ? ["--disable-phone-access"] : ["--repair-and-continue"]),
            "Successful recovery verifies readback, closes setup and continues real fixture startup")
        try check(model.start() == nil && AppRuntime.menuBarController === controller &&
            delegate.applicationShouldTerminate(NSApp) == .terminateNow,
            "Completed startup keeps exactly one menu controller and permits normal quit")
        try snapshot(queue, name: "03-normal-fixture-startup")
    }

    private func verifyPreflight(_ notification: Notification) async throws {
        let fixture = try BootstrapFixture(parent: temporary, arguments: ["--setup"])
        let waitingIssue = QueueBootstrapIssue(id: "installer-busy", title: "Another installation is running",
            detail: "Wait for the other installation to finish. Its lock and files will be preserved.",
            actions: [.init(id: "wait", title: "Wait and continue", destructive: false)])
        let phoneIssue = QueueBootstrapIssue(id: "legacy-phone-access", title: "Phone access needs to be disabled",
            detail: "Your credentials, configuration, and trusted certificates will be preserved. You can enable phone access again in settings after setup.",
            actions: [.init(id: "disable-phone-access", title: "Disable phone access and continue", destructive: false)])
        let missingIssue = QueueBootstrapIssue(id: "service-definitions-missing", title: "Restore missing background service files",
            detail: "The services are still running, but their startup files are missing. Setup can restore standard files after checking the loaded commands and installation paths. Lost custom startup settings cannot be recovered. Running services and jobs will stay active.",
            actions: [.init(id: "restore-service-definitions", title: "Restore verified service files", destructive: true)])
        let executor = BootstrapPreflightExecutorFixture(fixture,
            report: .init(schemaVersion: 1, issues: [missingIssue, waitingIssue, phoneIssue]),
            afterRecovery: .init(schemaVersion: 1, issues: [waitingIssue, phoneIssue]))
        let delegate = AppDelegate(environment: fixture.environment) { request, output in
            try await executor.execute(request, output: output)
        }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        try await waitUntil { delegate.bootstrap?.phase == .reviewing }
        guard let setup = delegate.setupWindow, let model = delegate.bootstrap else {
            throw ClientError.message("Preflight did not present its production setup window")
        }
        let initialInstalls = await executor.installations
        try check(setup.isVisible && model.issues.count == 3 && initialInstalls == 0 && AppRuntime.menuBarController == nil,
            "Production preflight displays multiple independent blockers before installation")
        try check(delegate.applicationShouldTerminate(NSApp) == .terminateNow,
            "A blocked preflight permits quit without initializing the queue")
        try snapshot(setup, name: "01-preflight-summary")

        await model.start()?.value
        try await waitUntil { await executor.checks >= 2 && model.phase == .reviewing }
        let checkedMutations = await executor.mutations
        try check(checkedMutations == 0 && model.issues.count == 3,
            "A read-only recheck refreshes every blocker without authorizing changes")

        try check(model.perform(actionID: "restore-service-definitions", issueID: missingIssue.id) == nil,
            "Missing service files cannot be restored without confirmation")
        await model.perform(actionID: "restore-service-definitions", issueID: missingIssue.id, confirmed: true)?.value
        let restoredInstalls = await executor.installations
        try check(model.phase == .reviewing && model.issues == [waitingIssue, phoneIssue] && restoredInstalls == 0,
            "Confirmed service file recovery preserves the setup window until other requirements are resolved")
        try snapshot(setup, name: "01-service-files-restored")
        await executor.updateAfterRecovery(.clear)

        await executor.update(.init(schemaVersion: 1, issues: [waitingIssue]))
        await model.start()?.value
        await executor.holdPreflight()
        let waiting = model.waitAndContinue()
        try await waitUntil { await executor.isHeld }
        try check(model.phase == .waiting && model.canStopWaiting,
            "Waiting shows progress and an explicit stop control while jobs continue independently")
        try snapshot(setup, name: "02-waiting")
        let stopping = model.cancelWaiting()
        await executor.update(.clear)
        await executor.releasePreflight()
        await waiting?.value
        await stopping?.value
        let cancelledInstalls = await executor.installations
        try check(model.phase == .reviewing && cancelledInstalls == 0 && delegate.applicationShouldTerminate(NSApp) == .terminateNow,
            "Stopping a pending preflight prevents its late result from installing")
        try snapshot(setup, name: "03-wait-stopped")

        await executor.update(.init(schemaVersion: 1, issues: [phoneIssue]))
        await model.start()?.value
        await model.perform(actionID: "disable-phone-access", issueID: phoneIssue.id)?.value
        try await waitUntil { model.phase == .complete && AppRuntime.queueWindow?.isVisible == true }
        let requests = await executor.requests
        let mutations = await executor.mutations
        try check(mutations == 2 && requests.contains(where: { $0.arguments.suffix(2) == ["--recover", "restore-service-definitions"] }) &&
            requests.contains(where: { $0.arguments.suffix(2) == ["--recover", "disable-phone-access"] }) &&
            delegate.setupWindow == nil && AppRuntime.store.fixtureMode,
            "Explicit preflight recovery refreshes requirements, verifies installation, and continues simulated queue startup")
        guard let queue = AppRuntime.queueWindow else { throw ClientError.message("Preflight recovery did not open its simulated queue") }
        try snapshot(queue, name: "04-normal-fixture-startup")
    }

    private func verifyDragAndDropLaunch(_ notification: Notification) async throws {
        let fixture = try BootstrapFixture(parent: temporary)
        let executor = BootstrapInstallerProgressFixture(fixture)
        let delegate = AppDelegate(environment: fixture.environment) { request, output in
            try await executor.execute(request, output: output)
        }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        try await waitUntil { await executor.isHeld && delegate.bootstrap?.requiresServiceApproval == true && delegate.setupWindow != nil }
        guard let setup = delegate.setupWindow, let model = delegate.bootstrap else {
            throw ClientError.message("macOS approval did not present the setup window")
        }
        try check(setup.isVisible && model.phase == .waiting && model.canStopWaiting && !model.output.contains("AGENT_QUEUE_SETUP_STATE") &&
            AppRuntime.menuBarController == nil,
            "A first launch opens the setup window only when macOS asks for background service approval")
        try snapshot(setup, name: "01-approval")
        await executor.release()
        try await waitUntil { model.phase == .complete && AppRuntime.queueWindow?.isVisible == true }
        let installations = await executor.installations
        try check(delegate.setupWindow == nil && installations == 1 && AppRuntime.store.fixtureMode,
            "Approved setup closes its window and opens the queue without another installation")
        guard let queue = AppRuntime.queueWindow else { throw ClientError.message("Drag-and-drop launch did not open the simulated queue") }
        try snapshot(queue, name: "02-installed-runtime")
    }

    private func verifyApprovedRepair(_ notification: Notification) async throws {
        let fixture = try BootstrapFixture(parent: temporary, arguments: ["--setup"])
        let busy = QueueBootstrapIssue(id: "installer-busy", title: "Another installation is running",
            detail: "Setup will wait without changing its files.", actions: [.init(id: "wait", title: "Wait and continue", destructive: false)])
        let missing = QueueBootstrapIssue(id: "service-definitions-missing", title: "Restore missing background service files",
            detail: "The existing services and jobs will stay active while their verified startup files are restored.",
            actions: [.init(id: "restore-service-definitions", title: "Restore verified service files", destructive: true)])
        let phone = QueueBootstrapIssue(id: "legacy-phone-access", title: "Phone access needs to be disabled",
            detail: "Credentials and trusted certificates are preserved. Phone access can be enabled again after setup.",
            actions: [.init(id: "disable-phone-access", title: "Disable phone access and continue", destructive: false)])
        let active = QueueBootstrapIssue(id: "legacy-jobs", title: "Waiting for existing work",
            detail: "Running jobs will finish normally. Setup will continue automatically.",
            actions: [.init(id: "wait", title: "Wait and continue", destructive: false)],
            jobs: [.init(id: 7, label: "Fixture validation", status: "Running")])
        let executor = BootstrapPreflightExecutorFixture(fixture,
            report: .init(schemaVersion: 1, issues: [busy, missing, phone]))
        await executor.queueRecoveryReports([.init(schemaVersion: 1, issues: [phone]), .init(schemaVersion: 1, issues: [active])])
        let delegate = AppDelegate(environment: fixture.environment) { request, output in
            try await executor.execute(request, output: output)
        }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        try await waitUntil { delegate.bootstrap?.phase == .reviewing }
        guard let setup = delegate.setupWindow, let model = delegate.bootstrap else {
            throw ClientError.message("Approved repair did not present its production setup window")
        }
        try check(model.canRepairAndContinue && model.repairAndContinue() == nil,
            "Known repairs are available but cannot begin without approval")
        // A model phase change precedes the rendered SwiftUI action. Capture
        // the ready controls before delivering their keyboard shortcut.
        try snapshot(setup, name: "00-repair-ready")
        try sendReturn(to: setup)
        try await waitUntil { self.alertButton("Repair and continue", for: setup) != nil }
        guard let alert = setup.attachedSheet ?? NSApp.modalWindow,
              let cancel = alertButton("Cancel", for: setup) else {
            throw ClientError.message("Repair approval dialog did not expose its Cancel button")
        }
        try snapshot(alert, name: "01-repair-approval")
        cancel.performClick(nil)
        try await waitUntil { setup.attachedSheet == nil && NSApp.modalWindow == nil }
        let declinedMutations = await executor.mutations
        try check(declinedMutations == 0 && model.phase == .reviewing,
            "Declining the actual approval dialog performs no repair")
        try sendReturn(to: setup)
        try await waitUntil { self.alertButton("Repair and continue", for: setup) != nil }
        guard let approve = alertButton("Repair and continue", for: setup) else {
            throw ClientError.message("Repair approval button disappeared")
        }
        approve.performClick(nil)
        try await waitUntil { model.phase == .waiting }
        let busyMutations = await executor.mutations
        try check(busyMutations == 0 && model.canStopWaiting,
            "One approval waits for an existing installer before changing anything")
        try snapshot(setup, name: "02-approved-waiting")
        await executor.update(.init(schemaVersion: 1, issues: [missing, phone]))
        try await waitUntil { await executor.mutations == 2 && model.phase == .waiting }
        try snapshot(setup, name: "03-repairs-complete-waiting")
        await executor.update(.clear)
        try await waitUntil { model.phase == .complete && AppRuntime.queueWindow?.isVisible == true }
        let requests = await executor.requests
        let installs = await executor.installations
        try check(installs == 1 && requests.filter { $0.arguments.contains("--recover") }.count == 2 &&
            requests.contains { $0.arguments.suffix(2) == ["--recover", "restore-service-definitions"] } &&
            requests.contains { $0.arguments.suffix(2) == ["--recover", "disable-phone-access"] } &&
            delegate.setupWindow == nil && AppRuntime.store.fixtureMode,
            "One accepted dialog repairs, waits and verifies normal startup without further intervention")
        guard let queue = AppRuntime.queueWindow else { throw ClientError.message("Approved repair did not start the simulated queue") }
        try snapshot(queue, name: "04-approved-startup")
    }

    private func verifyCancellableWait(_ notification: Notification) async throws {
        let fixture = try BootstrapFixture(parent: temporary)
        var terminationAcknowledged = false
        let delegate = AppDelegate(environment: fixture.environment, terminationReply: { terminationAcknowledged = true }) { request, output in
            if request.arguments.suffix(2) == ["--preflight", "--json"] {
                return try JSONEncoder().encode(QueueBootstrapReport.clear)
            }
            output(Data((QueueSetupProgress.waitingMarker + "\nWaiting for existing queue jobs. They will continue normally.").utf8))
            try await Task.sleep(for: .seconds(60))
            throw ClientError.message("Cancelled fixture setup continued")
        }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        try await waitUntil { delegate.bootstrap?.phase == .waiting && delegate.setupWindow?.isVisible == true }
        guard let model = delegate.bootstrap, let setup = delegate.setupWindow else {
            throw ClientError.message("Automatic waiting did not display its controls")
        }
        try check(model.canStopWaiting && AppRuntime.menuBarController == nil,
            "Automatic queue waiting presents its cancellable setup window without starting an unready runtime")
        try snapshot(setup, name: "01-waiting")
        let stopping = model.cancelWaiting()
        try check(model.phase == .stopping && model.start() == nil,
            "Stop waiting blocks retry while its controller is still stopping")
        let reply = delegate.applicationShouldTerminate(NSApp)
        try check(reply == .terminateLater && !terminationAcknowledged,
            "Quit and logout wait for the private controller to stop before acknowledging termination")
        try await waitUntil { terminationAcknowledged }
        await stopping?.value
        try check(!model.isRunning && model.phase == .failed && delegate.setupWindow === setup && AppRuntime.menuBarController == nil,
            "Stop waiting leaves retry available and preserves the independent queue")
        try snapshot(setup, name: "02-stopped")
    }

    private func verifySilentRecovery(_ notification: Notification) async throws {
        let fixture = try BootstrapFixture(parent: temporary, managed: true)
        // Replay the reveal-only report from the recording in this same private
        // environment, then exercise the recoverable report produced by the fix.
        let originalIssue = QueueBootstrapIssue(id: "pending-installation", title: "Finish the previous installation",
            detail: "Another candidate installation is pending. Finish that exact build before replacing it.",
            actions: [.init(id: "reveal", title: "Show affected file", destructive: false)])
        let originalExecutor = BootstrapAutomaticSetupFixture(fixture, report: .init(schemaVersion: 1, issues: [originalIssue]))
        let original = AppDelegate(environment: fixture.environment) { request, _ in
            try await originalExecutor.execute(request)
        }
        production = original
        original.applicationDidFinishLaunching(notification)
        try await waitUntil { original.bootstrap?.phase == .reviewing && original.setupWindow?.isVisible == true }
        guard let blocked = original.setupWindow else { throw ClientError.message("The recorded blocker was not reproduced") }
        let originalRequests = await originalExecutor.requests
        try check(originalRequests.allSatisfy({ $0.arguments.suffix(2) == ["--preflight", "--json"] }),
            "The original reveal-only report reproduces the blocked setup without installing")
        try snapshot(blocked, name: "00-before-recorded-blocker")
        blocked.close()
        let pending = QueueBootstrapIssue(id: "pending-installation", title: "Finish the previous installation",
            detail: "Finish the exact retained build before setting up this app at its new location.",
            actions: [.init(id: "finish-pending-installation", title: "Continue setup", destructive: true)])
        let executor = BootstrapAutomaticSetupFixture(fixture, report: .init(schemaVersion: 1, issues: [pending]))
        let delegate = AppDelegate(environment: fixture.environment) { request, _ in
            try await executor.execute(request)
        }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        guard let model = delegate.bootstrap else { throw ClientError.message("Silent recovery did not start") }
        var setupPresentations = 0
        let present = model.onSetupRequired
        model.onSetupRequired = { setupPresentations += 1; present?() }
        try await waitUntil { model.phase == .complete && AppRuntime.queueWindow?.isVisible == true }
        let requests = await executor.requests
        try check(setupPresentations == 0 && delegate.setupWindow == nil && AppRuntime.menuBarController != nil &&
            requests.filter({ $0.arguments.contains("--repair-and-continue") }).count == 1 && AppRuntime.store.fixtureMode,
            "Verified retained-build recovery opens a usable menu bar without a setup popup or extra click")
        guard let queue = AppRuntime.queueWindow else { throw ClientError.message("Silent recovery did not open the simulated queue") }
        try snapshot(queue, name: "01-recovered-runtime")
    }

    private func sendReturn(to window: NSWindow) throws {
        window.makeKeyAndOrderFront(nil)
        guard let enter = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: [],
            timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: window.windowNumber, context: nil,
            characters: "\r", charactersIgnoringModifiers: "\r", isARepeat: false, keyCode: 36) else {
            throw ClientError.message("Could not create the approval fixture event")
        }
        if !window.performKeyEquivalent(with: enter) { window.sendEvent(enter) }
    }

    private func alertButton(_ title: String, for window: NSWindow) -> NSButton? {
        func find(_ view: NSView) -> NSButton? {
            if let button = view as? NSButton, button.title == title { return button }
            for child in view.subviews { if let button = find(child) { return button } }
            return nil
        }
        guard let alert = window.attachedSheet ?? NSApp.modalWindow, let content = alert.contentView else { return nil }
        return find(content)
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
