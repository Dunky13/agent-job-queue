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

    private func waitUntil(_ state: String = "the expected state", _ predicate: () async -> Bool) async throws {
        let deadline = ProcessInfo.processInfo.systemUptime + 8
        while !(await predicate()) {
            guard ProcessInfo.processInfo.systemUptime < deadline else {
                throw ClientError.message("Bootstrap UI fixture did not reach \(state). Phase: \(String(describing: production?.bootstrap?.phase)); output: \(production?.bootstrap?.output ?? "no model")")
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
        if CommandLine.arguments.contains("--quit-mutation") {
            try await verifyQuitMutation(notification)
            return
        }
        if CommandLine.arguments.contains("--unknown-requirement") {
            try await verifyUnknownRequirement(notification)
            return
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
        let startup = try checkStartup(delegate)
        let item = startup.statusItem
        try await waitUntil { await executor.isHeld && delegate.bootstrap?.output.contains("Preparing") == true }
        guard let model = delegate.bootstrap else { throw ClientError.message("Production startup did not start") }
        try check(model.phase == .installing && delegate.setupWindow == nil && AppRuntime.menuBarController == nil && AppRuntime.queueWindow == nil,
            "Fresh launch prepares behind the menu without starting an unverified queue")
        try check(!delegate.applicationShouldHandleReopen(NSApp, hasVisibleWindows: false) && delegate.setupWindow == nil,
            "Reopen during preparation stays in the menu bar")
        try snapshotStartup(startup, name: "01-preparing")
        await executor.release()
        try await waitUntil { model.phase == .failed }
        try await waitUntil { startup.menu.items.contains { $0.title == "Retry" && !$0.isHidden } }
        let expectedFailure = phoneAccessBlocker ? model.canDisablePhoneAccess && model.message.contains("Phone access") :
            !model.canDisablePhoneAccess && model.output.contains("Fixture service could not start")
        try check(delegate.setupWindow == nil && expectedFailure &&
            !model.output.contains("AGENT_QUEUE_SETUP_RECOVERY") && AppRuntime.menuBarController == nil &&
            delegate.applicationShouldTerminate(NSApp) == .terminateNow && startup.menu.items.contains { $0.title == "Retry" && !$0.isHidden },
            "Failure stays in the menu with its diagnostic, Retry and Quit")
        try snapshotStartup(startup, name: "02-failure-retry")
        // Invoke the production native menu action, without a setup confirmation.
        guard let retry = startup.menu.items.first(where: { $0.title == "Retry" }) else { throw ClientError.message("Retry missing") }
        startup.menu.performActionForItem(at: startup.menu.index(of: retry))
        try await waitUntil { await executor.isHeld }
        try check(model.phase == .installing && delegate.setupWindow == nil,
            "Menu Retry starts automatic recovery without a wizard or confirmation")
        await executor.release()
        try await waitUntil { model.phase == .complete }
        let requests = await executor.requests
        try check(requests.count == 2 && requests.allSatisfy { $0.arguments == ["install", "--app-bundle", fixture.environment.app.path, "--repair-and-continue"] },
            "Ordinary retry retains the backend automatic recovery contract")
        try await checkReady(delegate, startup: startup, item: item)
    }

    private func checkStartup(_ delegate: AppDelegate) throws -> QueueStartupMenuBarController {
        guard let startup = delegate.startupMenuBarController else { throw ClientError.message("Immediate startup menu missing") }
        try check(startup.statusItem.isVisible && startup.statusItem.button != nil && startup.statusItem.menu === startup.menu &&
            AppRuntime.menuBarController == nil && AppRuntime.queueWindow == nil && delegate.setupWindow == nil,
            "Ordinary launch presents its menu bar immediately, before background preparation")
        try check(startup.menu.items.contains { $0.title == "Quit Agent Queue" } &&
            !startup.menu.items.contains { $0.title == QueueAppAction.openWindow.title },
            "Startup menu offers Quit and no unready queue mutations")
        return startup
    }

    private func checkReady(_ delegate: AppDelegate, startup: QueueStartupMenuBarController, item: NSStatusItem) async throws {
        guard let controller = AppRuntime.menuBarController else { throw ClientError.message("Ready menu missing") }
        try check(controller.statusItem === item && startup.isTornDown && delegate.startupMenuBarController == nil &&
            delegate.setupWindow == nil && AppRuntime.queueWindow == nil && AppRuntime.store.fixtureMode &&
            delegate.applicationShouldTerminate(NSApp) == .terminateNow,
            "Readiness transfers the exact status item once and keeps default launch menu-bar-only")
        try check(delegate.bootstrap?.start() == nil, "Completed startup cannot create another controller")
        // AppKit can report a visible status window while its first layout still
        // has zero height. Wait for a usable on-screen target before posting the
        // actual mouse event path, without moving the real pointer.
        try await waitUntil("usable on-screen status-button geometry") {
            guard let button = controller.statusItem.button, let window = button.window,
                  window.isVisible, !button.bounds.isEmpty, !button.visibleRect.isEmpty,
                  !window.frame.isEmpty else { return false }
            return NSScreen.screens.contains { $0.frame.intersects(window.frame) }
        }
        guard let button = controller.statusItem.button, let window = button.window, window.isVisible else {
            throw ClientError.message("Transferred status button has no visible window")
        }
        let point = button.convert(NSPoint(x: button.bounds.midX, y: button.bounds.midY), to: nil)
        let now = ProcessInfo.processInfo.systemUptime
        for (index, type) in [NSEvent.EventType.leftMouseDown, .leftMouseUp].enumerated() {
            guard let event = NSEvent.mouseEvent(with: type, location: point, modifierFlags: [],
                timestamp: now + Double(index) * 0.001, windowNumber: window.windowNumber,
                context: nil, eventNumber: index, clickCount: 1, pressure: index == 0 ? 1 : 0) else {
                throw ClientError.message("Cannot create transferred status-button event")
            }
            NSApp.postEvent(event, atStart: false)
        }
        try await waitUntil("the transferred status item's queue popover") { controller.popover.isShown }
        try check(controller.popover.contentViewController != nil, "Retained menu item opens the normal usable queue popover")
        if let window = controller.popover.contentViewController?.view.window { try snapshot(window, name: "03-ready-popover") }
        controller.closePopover()
        try await waitUntil { !controller.popover.isShown }
        delegate.applicationWillTerminate(Notification(name: NSApplication.willTerminateNotification))
        startup.tearDown()
        controller.tearDown()
        try check(AppRuntime.menuBarController == nil && delegate.startupMenuBarController == nil && !controller.popover.isShown,
            "Repeated teardown leaves no startup or normal menu controller")
    }

    private func snapshotStartup(_ startup: QueueStartupMenuBarController, name: String) throws {
        guard let button = startup.statusItem.button else { throw ClientError.message("Startup status button missing") }
        try snapshotView(button, name: name + "-status-item")
        // NSMenu can host custom items in a separate representation. Identify
        // the newly visible window in this private fixture process instead of
        // assuming the original NSMenuItem.view is attached to that window.
        // cacheDisplay reads that owned AppKit surface, never the desktop.
        let previouslyVisible = Set(NSApp.windows.filter(\.isVisible).map(ObjectIdentifier.init))
        var captureError: Error?
        var captured = false
        let timer = Timer(timeInterval: 0.1, repeats: false) { [self] _ in
            MainActor.assumeIsolated {
                do {
                    guard let window = NSApp.windows.first(where: { $0.isVisible && !previouslyVisible.contains(ObjectIdentifier($0)) }) else {
                        throw ClientError.message("Private fixture menu window did not appear")
                    }
                    try snapshot(window, name: name + "-menu")
                    captured = true
                } catch { captureError = error }
                startup.menu.cancelTracking()
            }
        }
        RunLoop.main.add(timer, forMode: .eventTracking)
        startup.menu.popUp(positioning: nil, at: NSPoint(x: button.bounds.minX, y: button.bounds.minY), in: button)
        timer.invalidate()
        if let captureError { throw captureError }
        guard captured else { throw ClientError.message("Fixture menu capture did not run") }
    }

    private func snapshotView(_ view: NSView, name: String) throws {
        view.layoutSubtreeIfNeeded()
        guard let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { throw ClientError.message("Cannot cache fixture view") }
        view.cacheDisplay(in: view.bounds, to: bitmap)
        let png = try NativeSnapshotRenderer.png(from: bitmap, background: NSColor.windowBackgroundColor.cgColor)
        try png.write(to: output.appendingPathComponent("\(name).png"))
        pictures.append((name, png))
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
        let startup = try checkStartup(delegate)
        let item = startup.statusItem
        try await waitUntil { await executor.isHeld && delegate.bootstrap?.requiresServiceApproval == true }
        guard let model = delegate.bootstrap else { throw ClientError.message("Startup missing") }
        try await waitUntil { startup.menu.items.contains { $0.title == "Open Login Items Settings…" && !$0.isHidden } }
        try check(delegate.setupWindow == nil && model.phase == .waiting && model.canStopWaiting && !model.output.contains("AGENT_QUEUE_SETUP_STATE") &&
            AppRuntime.menuBarController == nil && startup.menu.items.contains { $0.title == "Open Login Items Settings…" && !$0.isHidden },
            "macOS approval is visible in the menu with Settings and no setup window")
        try check(!delegate.applicationShouldHandleReopen(NSApp, hasVisibleWindows: false) && delegate.setupWindow == nil,
            "Reopen during platform approval stays in the menu")
        try snapshotStartup(startup, name: "01-approval")
        await executor.release()
        try await waitUntil { model.phase == .complete }
        let installations = await executor.installations
        try check(installations == 1, "Approved startup continues without a second installation")
        try await checkReady(delegate, startup: startup, item: item)
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
        let startup = try checkStartup(delegate)
        try await waitUntil { delegate.bootstrap?.phase == .waiting }
        guard let model = delegate.bootstrap else { throw ClientError.message("Automatic wait did not start") }
        try check(model.canStopWaiting && delegate.setupWindow == nil && AppRuntime.menuBarController == nil,
            "Automatic drain waits stay in the menu without starting an unready runtime")
        try snapshotStartup(startup, name: "01-waiting")
        let directQuit = CommandLine.arguments.contains("--direct-quit")
        let stopping = directQuit ? nil : model.cancelWaiting()
        if !directQuit {
            try check(model.phase == .stopping && model.start() == nil,
                "Stop waiting blocks retry while its controller is still stopping")
        }
        let reply = delegate.applicationShouldTerminate(NSApp)
        try check(reply == .terminateLater && !terminationAcknowledged && delegate.setupWindow == nil,
            "Quit during an automatic wait defers termination without opening a window")
        try await waitUntil { terminationAcknowledged }
        await stopping?.value
        try check(!model.isRunning && model.phase == .failed && delegate.setupWindow == nil && AppRuntime.menuBarController == nil,
            "Stopped wait preserves the independent queue and shows no setup window")
        try snapshotStartup(startup, name: "02-stopped")
    }

    private func verifyQuitMutation(_ notification: Notification) async throws {
        let fixture = try BootstrapFixture(parent: temporary)
        let executor = BootstrapQuitMutationFixture(fixture)
        var replies = 0
        let delegate = AppDelegate(environment: fixture.environment, terminationReply: { replies += 1 }) { request, output in
            try await executor.execute(request, output: output)
        }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        let startup = try checkStartup(delegate)
        try await waitUntil { await executor.isHeld && delegate.bootstrap?.phase == .installing }
        guard let model = delegate.bootstrap else { throw ClientError.message("Automatic mutation missing") }
        try check(delegate.applicationShouldTerminate(NSApp) == .terminateLater && model.phase == .stopping &&
            model.start() == nil && replies == 0 && delegate.setupWindow == nil,
            "Quit during an accepted automatic mutation retains its operation and opens no window")
        try check(delegate.applicationShouldTerminate(NSApp) == .terminateLater && replies == 0,
            "Repeated Quit cannot create duplicate termination acknowledgements")
        try snapshotStartup(startup, name: "01-stopping-mutation")
        await executor.release()
        try await waitUntil { replies == 1 && !model.isRunning }
        let completed = await executor.completed
        try check(completed && (try fixture.environment.inspect()) == .ready && AppRuntime.menuBarController == nil &&
            delegate.setupWindow == nil && replies == 1,
            "Termination waits for the original accepted response and never drops or retries the mutation")
    }

    private func verifyUnknownRequirement(_ notification: Notification) async throws {
        let fixture = try BootstrapFixture(parent: temporary)
        let issue = QueueBootstrapIssue(id: "unknown-fixture-policy", title: "Platform policy needs attention",
            detail: "Private fixture: the required service was denied by platform policy.", actions: [])
        let executor = BootstrapPreflightExecutorFixture(fixture, report: .init(schemaVersion: 1, issues: [issue]))
        let delegate = AppDelegate(environment: fixture.environment) { request, output in
            try await executor.execute(request, output: output)
        }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        let startup = try checkStartup(delegate)
        try await waitUntil { delegate.bootstrap?.phase == .reviewing && startup.menu.items.contains { $0.title == "Download Agent Queue…" && !$0.isHidden } }
        let installations = await executor.installations
        try check(installations == 0 && delegate.setupWindow == nil && AppRuntime.menuBarController == nil &&
            delegate.bootstrap?.issues == [issue] && !delegate.applicationShouldHandleReopen(NSApp, hasVisibleWindows: false) &&
            delegate.applicationShouldTerminate(NSApp) == .terminateNow,
            "Unknown platform requirements remain visible, block installation, and permit reopen and quit without a setup window")
        try snapshotStartup(startup, name: "01-policy-failure")
        delegate.applicationWillTerminate(Notification(name: NSApplication.willTerminateNotification))
        startup.tearDown()
        try check(startup.isTornDown && startup.statusItem.menu == nil && delegate.startupMenuBarController == nil,
            "Quit before readiness tears down the startup item once")
    }

    private func verifySilentRecovery(_ notification: Notification) async throws {
        let fixture = try BootstrapFixture(parent: temporary, managed: true)
        let pending = QueueBootstrapIssue(id: "pending-installation", title: "Finish the previous installation",
            detail: "Finish the exact retained build before setting up this app at its new location.",
            actions: [.init(id: "finish-pending-installation", title: "Continue setup", destructive: true)])
        let executor = BootstrapAutomaticSetupFixture(fixture, report: .init(schemaVersion: 1, issues: [pending]))
        let delegate = AppDelegate(environment: fixture.environment) { request, _ in try await executor.execute(request) }
        production = delegate
        delegate.applicationDidFinishLaunching(notification)
        let startup = try checkStartup(delegate)
        let item = startup.statusItem
        guard let model = delegate.bootstrap else { throw ClientError.message("Automatic recovery did not start") }
        try await waitUntil { model.phase == .complete }
        let requests = await executor.requests
        try check(delegate.setupWindow == nil && requests.filter({ $0.arguments.contains("--repair-and-continue") }).count == 1,
            "Retained-build upgrade recovery completes behind the menu without an extra click")
        try await checkReady(delegate, startup: startup, item: item)
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
        let html = "<!doctype html><html lang='en'><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'><title>Bootstrap fixture verification</title><style>body{font:16px system-ui;margin:2rem;background:#f2f2f5;color:#202026}main{max-width:1000px;margin:auto}li{margin:.5rem 0}figure{margin:2rem 0}img{display:block;max-width:100%;height:auto;margin-top:1rem}figcaption{font-weight:600}</style><main><h1>Bootstrap fixture: \(passed ? "passed" : "failed")</h1><p>Production startup menu and explicit diagnostic fixtures, injected executor, disposable home and simulated queue.</p><p>\(escape(error ?? ""))</p><ul>\(list)</ul>\(images)</main></html>"
        let markdownChecks = checks.sorted { $0.key < $1.key }.map { "- \($0.value ? "PASS" : "FAIL") \($0.key)" }.joined(separator: "\n")
        let markdownPictures = pictures.map { "![\($0.0)](\($0.0).png)" }.joined(separator: "\n\n")
        let markdown = "# Menu-bar startup fixture: \(passed ? "passed" : "failed")\n\n\(error ?? "Injected executor, disposable home and simulated queue.")\n\n\(markdownChecks)\n\n\(markdownPictures)\n"
        try Data(markdown.utf8).write(to: output.appendingPathComponent("verification.md"))
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
