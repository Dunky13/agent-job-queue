import AppKit
import Foundation

// A separate executable keeps graphical fixtures out of app startup and headless CI.
@MainActor
final class MenuBarFixtureDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private var controller: QueueMenuBarController?
    private var results: [String: Bool] = [:]
    private var skippedChecks: [String: String] = [:]
    private var actions: [QueueAppAction] = []
    private var menuOpenCount = 0
    private var menuSelection: QueueAppAction?
    private var trackedMenu: NSMenu?
    private var childSelections = 0
    private var outsideClicks = 0
    private let applicationMenuDelegate = AppDelegate()

    func applicationDidFinishLaunching(_ notification: Notification) {
        let watchdog = Timer(timeInterval: 60, target: self, selector: #selector(timedOut(_:)), userInfo: nil, repeats: false)
        RunLoop.main.add(watchdog, forMode: .common)
        Task { @MainActor in
            do {
                try await verify()
                try writeReport()
                NSApp.terminate(nil)
            } catch {
                try? writeReport(error: error.localizedDescription)
                FileHandle.standardError.write(Data("Menu-bar fixture failed: \(error.localizedDescription)\n".utf8))
                exit(EXIT_FAILURE)
            }
        }
    }

    @objc private func timedOut(_ timer: Timer) {
        try? writeReport(error: "Fixture verification did not complete within 60 seconds.")
        exit(EXIT_FAILURE)
    }

    func applicationWillTerminate(_ notification: Notification) { controller?.tearDown() }

    func menuWillOpen(_ menu: NSMenu) {
        if menu === controller?.menu { menuOpenCount += 1 }
        trackedMenu = menu
        // Menu tracking runs a nested event loop. A timer in common modes closes
        // the real menu and invokes its real target/action without input permissions.
        let timer = Timer(timeInterval: 0.08, target: self,
                          selector: #selector(finishMenuTracking(_:)), userInfo: nil, repeats: false)
        RunLoop.main.add(timer, forMode: .common)
    }

    @objc private func finishMenuTracking(_ timer: Timer) {
        guard let menu = trackedMenu else { return }
        trackedMenu = nil
        let selection = menuSelection
        menuSelection = nil
        menu.cancelTracking()
        if let selection, let item = menu.items.first(where: { $0.action != nil && $0.tag == selection.rawValue }) {
            menu.performActionForItem(at: menu.index(of: item))
        } else if menu !== controller?.menu {
            menu.performActionForItem(at: 0)
        }
    }

    @objc private func selectChildMenu(_ sender: NSMenuItem) { childSelections += 1 }
    @objc private func selectOutsideButton(_ sender: NSButton) { outsideClicks += 1 }

    private func check(_ passed: Bool, _ name: String) throws {
        results[name] = passed
        FileHandle.standardOutput.write(Data("\(passed ? "PASS" : "FAIL") \(name)\n".utf8))
        if !passed { throw ClientError.message(name) }
    }

    private func settle(_ interval: TimeInterval = 0.12) async throws {
        try await Task.sleep(for: .seconds(interval))
    }

    private func click(count: Int = 1, secondary: Bool = false,
                       modifiers: NSEvent.ModifierFlags = []) throws {
        guard let button = controller?.statusItem.button, let window = button.window else {
            throw ClientError.message("A logged-in graphical macOS session with a visible menu bar is required.")
        }
        let point = button.convert(NSPoint(x: button.bounds.midX, y: button.bounds.midY), to: nil)
        try postClick(in: window, at: point, count: count, secondary: secondary, modifiers: modifiers)
    }

    private func postClick(in window: NSWindow, at point: NSPoint, count: Int = 1,
                           secondary: Bool = false, modifiers: NSEvent.ModifierFlags = []) throws {
        let now = ProcessInfo.processInfo.systemUptime
        for (offset, type) in (secondary ? [NSEvent.EventType.rightMouseDown, .rightMouseUp] : [.leftMouseDown, .leftMouseUp]).enumerated() {
            guard let event = NSEvent.mouseEvent(with: type, location: point, modifierFlags: modifiers,
                timestamp: now + Double(offset) * 0.001, windowNumber: window.windowNumber,
                context: nil, eventNumber: offset, clickCount: count, pressure: offset == 0 ? 1 : 0) else {
                throw ClientError.message("Could not create fixture status-button event")
            }
            NSApp.postEvent(event, atStart: false)
        }
    }

    private func verify() async throws {
        guard AppRuntime.store.fixtureMode, AppRuntime.store.snapshot != nil else {
            throw ClientError.message("Menu-bar verification requires --fixture JSONFILE containing simulated jobs.")
        }
        let controller = QueueMenuBarController(store: AppRuntime.store) { [weak self] action in
            self?.actions.append(action)
            // The Quit item's real selector is exercised with an inert endpoint.
            // Every window action still uses the production AppRuntime handler.
            if action != .quit { AppRuntime.perform(action) }
        }
        self.controller = controller
        AppRuntime.menuBarController = controller
        NSApp.mainMenu = applicationMenuDelegate.makeMainMenu()
        controller.popover.animates = false
        controller.menu.delegate = self
        try await settle()
        try check(AppRuntime.store.visibleSurfaces == 0 && !controller.isMonitoringEvents,
                  "hiddenPopoverStartsWithoutVisibleQueueSurfaceOrInputMonitor")
        let interval = NSEvent.doubleClickInterval + 0.15
        try click()
        try await settle(0.03)
        try check(controller.popover.isShown, "singleClickShowsRealPopoverImmediately")
        try await settle()
        try check(AppRuntime.store.visibleSurfaces == 1 && controller.isMonitoringEvents,
                  "shownPopoverRegistersOneVisibleQueueSurfaceAndInputMonitor")
        try click()
        try await settle(interval)
        try check(!controller.popover.isShown, "singleClickClosesRealPopover")
        try check(AppRuntime.store.visibleSurfaces == 0 && !controller.isMonitoringEvents,
                  "closedPopoverReleasesVisibleQueueSurfaceAndInputMonitor")

        try click()
        try await settle(0.03)
        try click(count: 2)
        try await settle(interval)
        try check(AppRuntime.queueWindow?.isVisible == true && !controller.popover.isShown
            && actions == [.openWindow],
                  "doubleClickOpensMainWindowWithoutStalePopover")
        let queueWindow = AppRuntime.queueWindow
        queueWindow?.close()
        try click()
        try await settle(0.03)
        try click(count: 2)
        try await settle(interval)
        try check(AppRuntime.queueWindow === queueWindow && queueWindow?.isVisible == true,
                  "doubleClickReopensClosedSingletonWindow")
        queueWindow?.miniaturize(nil)
        try await settle(0.3)
        try check(queueWindow?.isMiniaturized == true, "mainWindowCanBeMiniaturized")
        if let item = controller.menu.items.first(where: { $0.action != nil && $0.tag == QueueAppAction.openWindow.rawValue }) {
            controller.menu.performActionForItem(at: controller.menu.index(of: item))
        }
        try await settle(0.3)
        try check(AppRuntime.queueWindow === queueWindow && queueWindow?.isVisible == true && queueWindow?.isMiniaturized == false,
                  "openWindowRestoresMiniaturizedSingleton")

        // Deliver a non-mouse event before performClick to exercise the real
        // button target/action through the keyboard/accessibility branch.
        guard let nonMouse = NSEvent.otherEvent(with: .applicationDefined, location: .zero,
            modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: 0,
            context: nil, subtype: 0, data1: 0, data2: 0) else {
            throw ClientError.message("Could not create non-mouse fixture activation")
        }
        NSApp.postEvent(nonMouse, atStart: false)
        try await settle()
        controller.statusItem.button?.performClick(nil)
        try await settle(interval)
        try check(controller.popover.isShown, "keyboardOrAccessibilityShowsPopoverImmediately")
        NSApp.postEvent(nonMouse, atStart: false)
        try await settle()
        controller.statusItem.button?.performClick(nil)
        try await settle()
        try check(!controller.popover.isShown, "keyboardOrAccessibilityClosesPopover")

        controller.activate(.accessible)
        try await settle()
        try click()
        try await settle(0.03)
        try click(count: 2)
        try await settle(interval)
        try check(AppRuntime.queueWindow === queueWindow && !controller.popover.isShown,
                  "doubleClickAlsoWorksWhenPopoverWasAlreadyOpen")

        try click()
        try await settle(0.03)
        menuSelection = .openWindow
        try click(secondary: true)
        try await settle(interval)
        try check(menuOpenCount == 1 && AppRuntime.queueWindow === queueWindow && queueWindow?.isVisible == true
            && !controller.popover.isShown,
                  "secondaryClickNativeMenuOpensExistingWindow")
        try click()
        try await settle(0.03)
        try click(modifiers: .control)
        try await settle(interval)
        try check(menuOpenCount == 2 && !controller.popover.isShown,
                  "controlClickOpensNativeMenuWithoutStalePopover")
        controller.activate(.accessible)
        try await settle()
        try click(secondary: true)
        try await settle(interval)
        try check(menuOpenCount == 3 && !controller.popover.isShown,
                  "secondaryClickOpensNativeMenuWhilePopoverIsShown")
        controller.activate(.accessible)
        try await settle()
        try click(modifiers: .control)
        try await settle(interval)
        try check(menuOpenCount == 4 && !controller.popover.isShown,
                  "controlClickOpensNativeMenuWhilePopoverIsShown")
        try await verifyPopoverDismissal(controller)

        let actionItems = controller.menu.items.filter { $0.action != nil }
        try check(actionItems.map(\.title) == QueueAppAction.allCases.map(\.title), "nativeMenuMatchesSharedFooterActions")
        try check(controller.menu.items.contains { $0.title == QueueAppAction.quitExplanation && !$0.isEnabled }
            && actionItems.last?.keyEquivalent == "q", "nativeMenuExplainsBackgroundJobsAndKeepsQuitShortcut")
        for action in QueueAppAction.settings + [.quit] {
            guard let item = actionItems.first(where: { $0.tag == action.rawValue }) else {
                throw ClientError.message("Missing native menu action")
            }
            controller.menu.performActionForItem(at: controller.menu.index(of: item))
            try await settle()
        }
        try check(AppRuntime.learningWindow?.isVisible == true && AppRuntime.updatesWindow?.isVisible == true
            && AppRuntime.integrationWindow?.isVisible == true && AppRuntime.remoteWindow?.isVisible == true,
                  "nativeSettingsSelectorsReachProductionWindowHandlers")
        try check(actions.suffix(5) == QueueAppAction.settings + [.quit], "allSettingsAndInertQuitDispatchExactlyOnce")
        try check(AppRuntime.remoteStore.fixtureMode && AppRuntime.integrationStore.fixtureMode
            && AppRuntime.updateStore.fixtureMode && AppRuntime.learningStore.fixtureMode,
                  "allOpenedSettingsRemainFixtureIsolated")
        try await verifyApplicationMenus()
        let original = AppRuntime.store.snapshot
        AppRuntime.store.error = "Simulated unavailable queue"
        try check(controller.statusItem.button?.title == "!", "errorBadgeUpdatesOnRealStatusButton")
        AppRuntime.store.error = nil
        let presentation = QueueStatusPresentation(snapshot: original, error: nil)
        try check(controller.statusItem.button?.title == presentation.title
            && controller.statusItem.button?.accessibilityLabel() == presentation.accessibilityLabel,
                  "countBadgeAndAccessibilityUpdateOnRealStatusButton")
        controller.activate(.primaryClick)
        controller.tearDown()
        try await settle(interval)
        try check(!controller.popover.isShown && !controller.isMonitoringEvents,
                  "teardownClosesPopoverAndRemovesEventMonitor")
        AppRuntime.menuBarController = nil
    }

    private func verifyPopoverDismissal(_ controller: QueueMenuBarController) async throws {
        controller.activate(.accessible)
        try await settle()
        guard let view = controller.popover.contentViewController?.view else {
            throw ClientError.message("Popover has no content view")
        }
        let child = NSMenu(title: "Fixture child menu")
        child.autoenablesItems = false
        child.delegate = self
        let item = NSMenuItem(title: "Fixture child action", action: #selector(selectChildMenu(_:)), keyEquivalent: "")
        item.target = self
        child.addItem(item)
        child.popUp(positioning: nil, at: NSPoint(x: 20, y: 20), in: view)
        try await settle()
        try check(childSelections == 1 && controller.popover.isShown,
                  "childNativeMenuSelectionPreservesOriginatingPopover")
        guard let escape = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: [],
            timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: view.window?.windowNumber ?? 0,
            context: nil, characters: "\u{1b}", charactersIgnoringModifiers: "\u{1b}", isARepeat: false, keyCode: 53) else {
            throw ClientError.message("Could not create Escape fixture event")
        }
        NSApp.postEvent(escape, atStart: false)
        try await settle()
        try check(!controller.popover.isShown, "escapeDismissesPopoverAfterChildMenuFinishes")
        let outside = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 150, height: 80),
            styleMask: [.titled, .closable], backing: .buffered, defer: false)
        outside.isReleasedWhenClosed = false
        let button = NSButton(title: "Fixture outside click", target: self, action: #selector(selectOutsideButton(_:)))
        button.frame = NSRect(x: 0, y: 0, width: 150, height: 80)
        outside.contentView = button
        outside.orderFront(nil)
        controller.activate(.accessible)
        try await settle()
        try postClick(in: outside, at: NSPoint(x: 75, y: 40))
        try await settle()
        try check(!controller.popover.isShown && outsideClicks == 1,
                  "outsideOwnedWindowClickDismissesWithoutConsumingClick")
        outside.close()
        controller.activate(.accessible)
        try await settle()
        NotificationCenter.default.post(name: NSApplication.didResignActiveNotification, object: NSApp)
        try check(!controller.popover.isShown, "applicationDeactivationDismissesPopover")
        try await settle()
        try check(AppRuntime.store.visibleSurfaces == 0, "repeatedPopoverDismissalsLeaveNoVisibleQueueSurface")
    }

    private func verifyApplicationMenus() async throws {
        guard let main = NSApp.mainMenu,
              let edit = main.items.first(where: { $0.title == "Edit" })?.submenu,
              let file = main.items.first(where: { $0.title == "File" })?.submenu,
              let application = main.items.first?.submenu else {
            throw ClientError.message("Standard application menus are missing")
        }
        let expected: [(String, String, String)] = [("Undo", "z", "undo:"), ("Redo", "z", "redo:"),
            ("Cut", "x", "cut:"), ("Copy", "c", "copy:"), ("Paste", "v", "paste:"), ("Select All", "a", "selectAll:")]
        try check(expected.allSatisfy { title, key, selector in
            edit.items.contains { $0.title == title && $0.keyEquivalent == key
                && $0.action == NSSelectorFromString(selector) && $0.target == nil }
        } && edit.items.first(where: { $0.title == "Redo" })?.keyEquivalentModifierMask == [.command, .shift],
                  "standardEditingShortcutsUseFirstResponderActions")
        try check(file.items.contains { $0.keyEquivalent == "w" && $0.action == #selector(NSWindow.performClose(_:)) }
            && application.items.contains { $0.keyEquivalent == "q" && $0.title == QueueAppAction.quit.title }
            && NSApp.windowsMenu != nil && !applicationMenuDelegate.applicationShouldTerminateAfterLastWindowClosed(NSApp),
                  "standardCloseQuitAndWindowMenusPreserveBackgroundApp")
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 300, height: 120),
            styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        let editor = NSTextView(frame: NSRect(x: 0, y: 0, width: 300, height: 120))
        editor.allowsUndo = true
        window.contentView = editor
        window.makeKeyAndOrderFront(nil)
        window.makeFirstResponder(editor)
        NSApp.activate()
        try await settle()
        try check(window.firstResponder === editor,
                  "ownedEditorIsWindowFirstResponder")
        guard NSApp.isActive, NSApp.keyWindow === window else {
            let reason = "macOS declined foreground activation of the isolated fixture; no key window is available for global menu routing."
            for name in ["selectAllReachesOwnedWindowFirstResponder", "undoReachesOwnedWindowFirstResponder", "closeWindowShortcutReachesOwnedWindow"] {
                skippedChecks[name] = reason
                FileHandle.standardOutput.write(Data("SKIP \(name): \(reason)\n".utf8))
            }
            window.close()
            if CommandLine.arguments.contains("--require-foreground") { throw ClientError.message(reason) }
            return
        }
        editor.insertText("Fixture editor", replacementRange: NSRange(location: 0, length: 0))
        edit.update()
        guard let select = edit.items.first(where: { $0.title == "Select All" }),
              let undo = edit.items.first(where: { $0.title == "Undo" }) else {
            throw ClientError.message("Editing actions are missing")
        }
        edit.performActionForItem(at: edit.index(of: select))
        try check(editor.selectedRange() == NSRange(location: 0, length: 14), "selectAllReachesOwnedWindowFirstResponder")
        edit.performActionForItem(at: edit.index(of: undo))
        try check(editor.string.isEmpty, "undoReachesOwnedWindowFirstResponder")
        guard let close = file.items.first(where: { $0.keyEquivalent == "w" }) else {
            throw ClientError.message("Close Window is missing")
        }
        file.performActionForItem(at: file.index(of: close))
        try check(!window.isVisible, "closeWindowShortcutReachesOwnedWindow")
    }

    private func writeReport(error: String? = nil) throws {
        guard let index = CommandLine.arguments.firstIndex(of: "--report"),
              CommandLine.arguments.indices.contains(index + 1) else {
            throw ClientError.message("Menu-bar verification requires --report OUTPUT.json")
        }
        let report = Report(checks: results, skippedChecks: skippedChecks, error: error,
                            passed: error == nil && !results.isEmpty && results.values.allSatisfy { $0 })
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        try encoder.encode(report).write(to: URL(fileURLWithPath: CommandLine.arguments[index + 1]))
    }

    private struct Report: Codable {
        let checks: [String: Bool]
        let skippedChecks: [String: String]
        let error: String?
        let passed: Bool
    }
}

@main
struct MenuBarUITests {
    @MainActor static func main() {
        if let index = CommandLine.arguments.firstIndex(of: "--pid-file"), CommandLine.arguments.indices.contains(index + 1) {
            do {
                try Data("\(ProcessInfo.processInfo.processIdentifier)\n".utf8)
                    .write(to: URL(fileURLWithPath: CommandLine.arguments[index + 1]))
            } catch { exit(EXIT_FAILURE) }
        }
        let application = NSApplication.shared
        application.setActivationPolicy(.accessory)
        let delegate = MenuBarFixtureDelegate()
        application.delegate = delegate
        withExtendedLifetime(delegate) { application.run() }
    }
}
