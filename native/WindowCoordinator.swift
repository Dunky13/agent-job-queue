import AppKit
import SwiftUI

/// A popover or window that shows live data. Stores ask each tracked surface on every poll,
/// so closing, miniaturising or hiding one needs no notification.
@MainActor protocol QueueSurface: AnyObject { var isVisible: Bool { get } }
extension NSWindow: QueueSurface {}

/// The surfaces a store polls for, held weakly because their creators own them.
@MainActor struct QueueSurfaces {
    private struct Entry { weak var surface: (any QueueSurface)? }
    private var entries: [Entry] = []
    var visibleCount: Int { entries.filter { $0.surface?.isVisible == true }.count }
    mutating func add(_ surface: any QueueSurface) {
        entries.removeAll { $0.surface == nil || $0.surface === surface }
        entries.append(Entry(surface: surface))
    }
}

/// The wait between two polls. The app sleeps on the continuous clock; tests advance their own.
typealias PollSleep = @MainActor (Duration) async throws -> Void

/// The app-level requests a view may make.
struct QueueAppRoutes {
    let perform: @MainActor (QueueAppAction) -> Void
    let openLearning: @MainActor (String) -> Void
    /// Opens Sparkle's install dialog for an update a background check found.
    var installUpdate: @MainActor () -> Void = {}
}

struct QueueWindowSpec {
    let title: String
    let size: NSSize
    var minSize: NSSize?
    var style: NSWindow.StyleMask = [.titled, .closable, .miniaturizable, .resizable]

    @MainActor func makeWindow(content: NSView) -> NSWindow {
        let window = NSWindow(contentRect: NSRect(origin: .zero, size: size), styleMask: style,
                              backing: .buffered, defer: false)
        window.title = title
        window.isReleasedWhenClosed = false
        if let minSize { window.minSize = minSize }
        window.contentView = content
        return window
    }
}

extension NSWindow {
    /// Only a new window is centred; an existing one stays where the operator left it.
    func present(isNew: Bool) {
        if isNew { center() }
        makeKeyAndOrderFront(nil)
        NSApp.activate()
    }
}

enum QueueWindowKind: CaseIterable {
    case queue, learning, updates, integration, remote

    var spec: QueueWindowSpec {
        switch self {
        case .queue: QueueWindowSpec(title: "Agent Queue", size: NSSize(width: 700, height: 760),
                                     minSize: NSSize(width: 490, height: 480))
        case .learning: QueueWindowSpec(title: "Learned Workloads", size: NSSize(width: 1000, height: 780),
                                        minSize: NSSize(width: 800, height: 550))
        case .updates: QueueWindowSpec(title: "About Agent Queue", size: NSSize(width: 620, height: 620),
                                       minSize: NSSize(width: 590, height: 500))
        case .integration: QueueWindowSpec(title: "Agent Integration", size: NSSize(width: 600, height: 720),
                                           minSize: NSSize(width: 520, height: 540))
        case .remote: QueueWindowSpec(title: "Remote Access", size: NSSize(width: 580, height: 780),
                                      minSize: NSSize(width: 500, height: 570))
        }
    }
}

/// The composition root once setup is complete: owns the stores, the menu bar controller and the app windows.
@MainActor final class WindowCoordinator {
    let store: QueueStore
    // Built on first use: the update store reads the bundle's build metadata and creates Sparkle's updater when it is created.
    private(set) lazy var remoteStore = RemoteAccessStore(fixtureMode: store.fixtureMode, command: command, pollSleep: pollSleep)
    private(set) lazy var integrationStore = AgentIntegrationStore(fixtureMode: store.fixtureMode, command: command)
    private(set) lazy var updateStore = QueueUpdateStore(fixtureMode: store.fixtureMode, command: command)
    private(set) lazy var learningStore = LearningStore(fixtureMode: store.fixtureMode, command: command)
    private(set) var windows: [QueueWindowKind: NSWindow] = [:]
    var menuBarController: QueueMenuBarController?
    private let command: @Sendable (QueueClient.Command) async throws -> Data
    private let pollSleep: PollSleep

    init(command: @escaping @Sendable (QueueClient.Command) async throws -> Data = { try await QueueClient.run($0) },
         pollSleep: @escaping PollSleep = { try await Task.sleep(for: $0, clock: .continuous) },
         arguments: [String] = CommandLine.arguments) {
        self.command = command
        self.pollSleep = pollSleep
        store = QueueStore(statusLoader: { try await command(.status) }, command: command,
                           arguments: arguments, pollSleep: pollSleep)
    }

    var routes: QueueAppRoutes {
        QueueAppRoutes(perform: { [weak self] in self?.perform($0) },
                       openLearning: { [weak self] in self?.show(.learning, profileID: $0) },
                       installUpdate: { [weak self] in self?.installUpdate() })
    }

    func showMenuBar(statusItem: NSStatusItem?) {
        menuBarController = QueueMenuBarController(store: store, statusItem: statusItem, updates: updateStore, app: routes)
    }

    func installUpdate() {
        menuBarController?.closePopover()
        updateStore.checkForUpdates()
    }

    func tearDown() {
        menuBarController?.tearDown()
        menuBarController = nil
    }

    func perform(_ action: QueueAppAction) {
        menuBarController?.closePopover()
        switch action {
        case .openWindow: show(.queue)
        case .learnedWorkloads: show(.learning)
        case .updates: show(.updates)
        case .integration: show(.integration)
        case .remoteAccess: show(.remote)
        case .quit: NSApp.terminate(nil)
        }
    }

    /// Opens the window of a kind, or brings the existing one forward, even after it was closed.
    /// `profileID` selects a workload in the learning window.
    func show(_ kind: QueueWindowKind, profileID: String? = nil) {
        let existing = windows[kind]
        let window = existing ?? kind.spec.makeWindow(content: content(for: kind))
        if existing == nil {
            windows[kind] = window
            switch kind {
            case .queue: store.surfaces.add(window)
            case .remote: remoteStore.surfaces.add(window)
            case .learning, .updates, .integration: break
            }
        }
        window.present(isNew: existing == nil)
        // Polling starts after the window is on screen. A new window's view loads its own data.
        switch kind {
        case .queue: break
        case .learning:
            guard let profileID else { break }
            if existing != nil { learningStore.select(profileID); break }
            Task {
                await learningStore.loadPage(preserveSelection: true)?.value
                await learningStore.select(profileID)?.value
            }
        case .updates: if existing != nil { Task { await updateStore.refreshInstalled() } }
        case .integration: if existing != nil { Task { await integrationStore.refresh() } }
        case .remote:
            remoteStore.startPolling()
            if existing != nil { Task { await remoteStore.refresh() } }
        }
    }

    private func content(for kind: QueueWindowKind) -> NSView {
        // A view's Done button closes its own window.
        let close: () -> Void = { [weak self] in self?.windows[kind]?.close() }
        switch kind {
        case .queue: return NSHostingView(rootView: QueuePanel(store: store, app: routes, fullWindow: true, updates: updateStore))
        case .learning: return NSHostingView(rootView: LearningWindow(store: learningStore, onDone: close))
        case .updates: return NSHostingView(rootView: QueueUpdatesSettings(store: updateStore, onDone: close))
        case .integration: return NSHostingView(rootView: AgentIntegrationSettings(store: integrationStore, onDone: close))
        case .remote: return NSHostingView(rootView: RemoteAccessSettings(store: remoteStore))
        }
    }
}
