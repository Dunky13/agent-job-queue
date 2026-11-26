// Test-only hooks: `native/build-library.sh DIR ARCH test` compiles this directory
// into the library; the shipped release flavour leaves it out. See CONTRIBUTING.md.
import AppKit
import SwiftUI
import Foundation

extension AppDelegate {
    /// True when `--snapshot` takes over this launch; it terminates the app when done.
    func runSnapshotHooks(_ coordinator: WindowCoordinator) -> Bool {
        let arguments = CommandLine.arguments
        guard let index = arguments.firstIndex(of: "--snapshot"), arguments.indices.contains(index + 1) else { return false }
        guard let launched = QueueFixture.launched, launched.coordinator === coordinator else {
            diagnostic("--snapshot requires --fixture JSONFILE"); exit(EXIT_FAILURE)
        }
        let fixture = launched.fixture
        coordinator.show(.queue)
        let navigation = QueueNavigation()
        let verifyIndex = arguments.firstIndex(of: "--verify-detail-navigation")
        let verificationPath = verifyIndex.flatMap { arguments.indices.contains($0 + 1) ? arguments[$0 + 1] : nil }
        let confirmVerifyIndex = arguments.firstIndex(of: "--verify-confirmation-navigation")
        let confirmationVerificationPath = confirmVerifyIndex.flatMap { arguments.indices.contains($0 + 1) ? arguments[$0 + 1] : nil }
        let passwordVerifyIndex = arguments.firstIndex(of: "--verify-password-validation")
        let passwordVerificationPath = passwordVerifyIndex.flatMap { arguments.indices.contains($0 + 1) ? arguments[$0 + 1] : nil }
        let remoteStopVerifyIndex = arguments.firstIndex(of: "--verify-remote-stop-state")
        let remoteStopVerificationPath = remoteStopVerifyIndex.flatMap { arguments.indices.contains($0 + 1) ? arguments[$0 + 1] : nil }
        let confirmIndex = arguments.firstIndex(of: "--snapshot-confirmation")
        let requestedConfirmation = confirmIndex.flatMap {
            arguments.indices.contains($0 + 1) ? QueueConfirmationKind(rawValue: arguments[$0 + 1]) : nil
        }
        if let kind = requestedConfirmation ?? (confirmationVerificationPath == nil ? nil : .start) {
            guard let job = coordinator.store.snapshot?.jobs.first(where: {
                kind.action(for: $0).isAllowed(in: coordinator.store.snapshot)
            }) else {
                diagnostic("Confirmation snapshot requires an eligible job in the fixture."); exit(EXIT_FAILURE)
            }
            navigation.request(kind, job: job)
        } else if let detailIndex = arguments.firstIndex(of: "--snapshot-detail"), arguments.indices.contains(detailIndex + 1) {
            guard let jobID = Int(arguments[detailIndex + 1]),
                  let job = coordinator.store.snapshot?.jobs.first(where: { $0.id == jobID }) else {
                diagnostic("--snapshot-detail requires a job ID present in the fixture."); exit(EXIT_FAILURE)
            }
            navigation.show(job)
        } else if verificationPath != nil {
            guard let job = coordinator.store.snapshot?.jobs.first else {
                diagnostic("Navigation verification requires a fixture containing a job."); exit(EXIT_FAILURE)
            }
            navigation.show(job)
        }
        Task { @MainActor in
            do { try await self.loadSnapshotData(coordinator, arguments: arguments) }
            catch { self.diagnostic("Snapshot failed: \(error.localizedDescription)"); exit(EXIT_FAILURE) }
            self.showSnapshotContent(coordinator, navigation: navigation, arguments: arguments)
            try? await Task.sleep(for: .milliseconds(500))
            guard let window = coordinator.windows[.queue], let view = window.contentView else {
                self.diagnostic("Could not create snapshot view."); exit(EXIT_FAILURE)
            }
            do {
                let snapshotPath = arguments[index + 1]
                try self.writeSnapshot(view, to: snapshotPath)
                if let remoteStopVerificationPath {
                    let remote = coordinator.remoteStore
                    remote.error = nil
                    await remote.refresh()
                    let report = RemoteStopStateVerification(label: remote.statusLabel,
                        enabled: remote.status?.enabled ?? true, error: remote.effectiveError,
                        warningIndicator: remote.needsAttention,
                        preservedAfterRefresh: remote.statusLabel == "Needs attention")
                    let encoder = JSONEncoder()
                    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                    try encoder.encode(report).write(to: URL(fileURLWithPath: remoteStopVerificationPath))
                    guard report.passed else { throw ClientError.message("Remote stop-state regression failed; inspect the verification report.") }
                }
                if let passwordVerificationPath {
                    let report = await self.verifyPasswordValidation(fixture, coordinator.remoteStore)
                    let encoder = JSONEncoder()
                    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                    try encoder.encode(report).write(to: URL(fileURLWithPath: passwordVerificationPath))
                    guard report.passed else { throw ClientError.message("Password validation regression failed; inspect the verification report.") }
                }
                if let confirmationVerificationPath {
                    let report = try await self.verifyConfirmations(navigation: navigation, fixture: fixture,
                        store: coordinator.store, window: window, view: view)
                    let encoder = JSONEncoder()
                    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                    try encoder.encode(report).write(to: URL(fileURLWithPath: confirmationVerificationPath))
                    guard report.passed else { throw ClientError.message("Confirmation navigation regression failed; inspect the verification report.") }
                    let returnedPath = URL(fileURLWithPath: snapshotPath).deletingPathExtension().path + "-returned.png"
                    try self.writeSnapshot(view, to: returnedPath)
                }
                if let verificationPath {
                    let openedJob = navigation.selected?.id
                    // Done calls this same local navigation handler. No sheet or
                    // environment dismiss action participates in returning.
                    navigation.back()
                    try? await Task.sleep(for: .milliseconds(150))
                    let report = NavigationVerification(openedJob: openedJob,
                        returnedToQueue: navigation.selected == nil,
                        windowStayedVisible: window.isVisible,
                        hostingViewUnchanged: window.contentView === view,
                        noAttachedSheet: window.attachedSheet == nil)
                    let encoder = JSONEncoder()
                    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                    try encoder.encode(report).write(to: URL(fileURLWithPath: verificationPath))
                    guard report.passed else { throw ClientError.message("Detail navigation regression failed; inspect the verification report.") }
                    let returnedPath = URL(fileURLWithPath: snapshotPath).deletingPathExtension().path + "-returned.png"
                    try self.writeSnapshot(view, to: returnedPath)
                }
            } catch { self.diagnostic("Snapshot failed: \(error.localizedDescription)"); exit(EXIT_FAILURE) }
            NSApp.terminate(nil)
        }
        return true
    }

    /// Loads what the requested view shows through its store, before the view exists to load it itself.
    private func loadSnapshotData(_ coordinator: WindowCoordinator, arguments: [String]) async throws {
        if arguments.contains("--snapshot-remote-settings") { await coordinator.remoteStore.refresh() }
        if arguments.contains("--snapshot-agent-integration") { await coordinator.integrationStore.refresh() }
        if arguments.contains("--snapshot-learning") {
            let learning = coordinator.learningStore
            await learning.loadPage()?.value
            if !arguments.contains("--snapshot-learning-projects") {
                guard let project = learning.hierarchy?.projects.first else {
                    throw ClientError.message("The fixture has no learned project.")
                }
                learning.openProject(project)
                await learning.openOperation("tests")?.value
            }
        }
        if arguments.contains("--snapshot-updates") {
            await coordinator.updateStore.refreshInstalled()
            await coordinator.updateStore.checkForUpdates()?.value
        }
    }

    private func showSnapshotContent(_ coordinator: WindowCoordinator, navigation: QueueNavigation, arguments: [String]) {
        if let window = coordinator.windows[.queue] {
            let menuSize = arguments.contains("--snapshot-menu")
            window.contentView = NSHostingView(rootView: QueuePanel(store: coordinator.store, app: coordinator.routes,
                fullWindow: !menuSize, navigation: navigation))
            if menuSize {
                window.minSize = NSSize(width: 500, height: 650)
                window.setContentSize(NSSize(width: 500, height: 650))
                window.center()
            }
            if arguments.contains("--snapshot-remote-settings") {
                window.contentView = NSHostingView(rootView: RemoteAccessSettings(store: coordinator.remoteStore,
                    desktop: coordinator.routes.desktop))
                window.minSize = QueueWindowKind.remote.spec.minSize ?? .zero
                window.setContentSize(QueueWindowKind.remote.spec.size)
                window.center()
            }
            if arguments.contains("--snapshot-agent-integration") {
                window.contentView = NSHostingView(rootView: AgentIntegrationSettings(store: coordinator.integrationStore) {})
                window.minSize = QueueWindowKind.integration.spec.minSize ?? .zero
                window.setContentSize(QueueWindowKind.integration.spec.size)
                window.center()
            }
            if arguments.contains("--snapshot-learning") {
                window.contentView = NSHostingView(rootView: LearningWindow(store: coordinator.learningStore) {})
                window.minSize = QueueWindowKind.learning.spec.minSize ?? .zero
                window.setContentSize(QueueWindowKind.learning.spec.size)
                window.center()
            }
            if arguments.contains("--snapshot-updates") {
                window.contentView = NSHostingView(rootView: QueueUpdatesSettings(store: coordinator.updateStore,
                    desktop: coordinator.routes.desktop) {})
                window.minSize = QueueWindowKind.updates.spec.minSize ?? .zero
                window.setContentSize(QueueWindowKind.updates.spec.size)
                window.center()
            }
        }
    }

    private func verifyPasswordValidation(_ fixture: QueueFixture, _ remote: RemoteAccessStore) async -> PasswordValidationVerification {
        let candidates: [(String, String, Bool)] = [
            ("Empty rejected", "", false),
            ("ASCII below minimum", String(repeating: "a", count: 3), false),
            ("Simple word accepted", "word", true),
            ("Four-digit PIN accepted", "2468", true),
            ("Lowercase letters accepted", "simple", true),
            ("ASCII 72 bytes", String(repeating: "a", count: 72), true),
            ("ASCII 73 bytes", String(repeating: "a", count: 73), false),
            ("Two-byte characters at 72 bytes", String(repeating: "é", count: 36), true),
            ("Two-byte characters exceed 72 bytes", String(repeating: "é", count: 37), false),
            ("Three-byte characters at 72 bytes", String(repeating: "界", count: 24), true),
            ("Three-byte characters exceed 72 bytes", String(repeating: "界", count: 25), false),
            ("Printable ASCII spaces", "fixture password with spaces", true),
            ("Newline rejected", "fixture-password\n", false),
            ("Tab rejected", "fixture-password\t", false),
            ("Null rejected", "fixture-password\u{0}", false),
            ("Format character rejected", "fixture-password\u{200D}", false),
            ("Private-use character rejected", "fixture-password\u{E000}", false),
            ("Non-printable separator rejected", "fixture-password\u{00A0}", false)
        ]
        let cases = candidates.map { name, value, expected in
            PasswordValidationCase(name: name, expected: expected, accepted: RemoteAccessStore.acceptsPassword(value))
        }
        await remote.refresh()
        let before = fixture.changes.count
        let invalidRejected = !remote.configure(password: "abc", enable: true)
        remote.setEnabled(true)
        let rejectionsSentNothing = invalidRejected && !remote.isBusy && fixture.changes.count == before
        let accepted = remote.configure(password: "fixture-only-password", enable: true)
        for await busy in remote.$isBusy.values where !busy { break }
        let sent = fixture.changes.dropFirst(before)
        return PasswordValidationVerification(cases: cases,
            rejectionsSentNothing: rejectionsSentNothing,
            acceptedPasswordReachedTheFixture: accepted && sent.map(\.controller) == [.remote, .remote]
                && sent.map(\.arguments) == [["configure"], ["enable"]])
    }

    private func verifyConfirmations(navigation: QueueNavigation, fixture: QueueFixture, store: QueueStore,
                                     window: NSWindow, view: NSView) async throws -> ConfirmationVerification {
        guard let original = store.snapshot,
              let queued = original.jobs.first(where: { QueueAction.next($0).isAllowed(in: original) && QueueAction.cancel($0).isAllowed(in: original) }) else {
            throw ClientError.message("Confirmation verification requires a fixture with an eligible waiting job.")
        }
        let paused = QueueJob(id: -100, state: "paused", title: "Paused cancellation regression",
            project: "Fixture", branch: "", directory: "", executable: "", priority: 0, queuePosition: nil,
            submittedAt: nil, startedAt: nil, finishedAt: nil, exitCode: nil, logPath: "",
            canReorder: false, canCancel: true, phase: "Paused")
        // The store refreshes from the fixture after each change, so both must hold the same queue.
        func replaceQueue(_ snapshot: QueueSnapshot) {
            fixture.snapshot = snapshot
            store.snapshot = snapshot
        }
        replaceQueue(QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue, jobs: original.jobs + [paused]))
        defer { navigation.decline(); replaceQueue(original); store.isMutating = false }

        func sameSurface() -> Bool {
            window.isVisible && window.contentView === view && window.attachedSheet == nil
        }
        var results: [ConfirmationCaseVerification] = []
        var cases: [(QueueConfirmationKind, QueueJob)] = [(.start, queued), (.cancel, queued), (.cancel, paused)]
        if let running = original.jobs.first(where: { $0.isRunning && QueueAction.cancel($0).isAllowed(in: original) }) { cases.append((.cancel, running)) }
        for (kind, job) in cases {
            let before = fixture.changes.count
            navigation.request(kind, job: job)
            try await Task.sleep(for: .milliseconds(60))
            var surfaceStayedVisible = sameSurface()
            let openingDidNotDispatch = fixture.changes.count == before
            navigation.decline()
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let declineDidNotDispatch = fixture.changes.count == before && navigation.confirmation == nil

            navigation.request(kind, job: job)
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let firstDispatch = navigation.confirm(in: store)
            let reservationHeldBeforeIO = store.isMutating
            let duplicateWasRejected = navigation.confirm(in: store) == nil
            let competingActionWasRejected = store.dispatch(.next(queued)) == nil
            // Exercise the old lost-confirmation seam: a refresh flag changes
            // after confirmation but before the reserved async task can execute.
            store.isRefreshing = true
            await firstDispatch?.value
            store.isRefreshing = false
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let exactlyOneExpectedAction = Array(fixture.changes.dropFirst(before)) == [kind.action(for: job).command]

            navigation.request(kind, job: job)
            store.isMutating = true
            let busyWasRejected = navigation.confirm(in: store) == nil
            store.isMutating = false
            navigation.decline()
            results.append(ConfirmationCaseVerification(action: kind.rawValue, jobID: job.id,
                openingDidNotDispatch: openingDidNotDispatch, declineDidNotDispatch: declineDidNotDispatch,
                exactlyOneExpectedAction: exactlyOneExpectedAction, duplicateWasRejected: duplicateWasRejected,
                reservationHeldBeforeIO: reservationHeldBeforeIO, competingActionWasRejected: competingActionWasRejected,
                refreshDidNotLoseAcceptedAction: exactlyOneExpectedAction,
                busyWasRejected: busyWasRejected, returnedToQueue: navigation.confirmation == nil,
                surfaceStayedVisible: surfaceStayedVisible))
        }

        let beforeStale = fixture.changes.count
        navigation.request(.start, job: queued)
        // The same ID has become paused while the confirmation panel was open.
        let transitioned = QueueJob(id: queued.id, state: "paused", title: queued.title, project: queued.project,
            branch: queued.branch, directory: queued.directory, executable: queued.executable, priority: queued.priority,
            queuePosition: nil, submittedAt: queued.submittedAt, startedAt: queued.startedAt,
            finishedAt: nil, exitCode: nil, logPath: queued.logPath, canReorder: false, canCancel: true, phase: "Paused")
        replaceQueue(QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue,
            jobs: original.jobs.map { $0.id == queued.id ? transitioned : $0 }))
        let staleJobExplained = navigation.unavailability(in: store) != nil
        let staleJobRejected = navigation.confirm(in: store) == nil && fixture.changes.count == beforeStale
        navigation.decline()
        // A queue-level change runs the same pipeline and ends at the fixture, once.
        let limit = original.queue.limit % 8 + 1
        let limitDispatch = store.dispatch(.limit(limit))
        await limitDispatch?.value
        let limitReachedOnlyTheFixture = limitDispatch != nil && !store.isMutating
            && Array(fixture.changes.dropFirst(beforeStale)) == [.limit(limit)]
        let restoredSurface = sameSurface()
        replaceQueue(original)
        try await Task.sleep(for: .milliseconds(80))
        return ConfirmationVerification(cases: results, staleJobExplained: staleJobExplained,
            staleJobRejected: staleJobRejected, limitReachedOnlyTheFixture: limitReachedOnlyTheFixture,
            finalSurfaceStayedVisible: restoredSurface, recordedActions: fixture.actions)
    }

    private func writeSnapshot(_ view: NSView, to path: String) throws {
        view.layoutSubtreeIfNeeded()
        guard let representation = view.bitmapImageRepForCachingDisplay(in: view.bounds) else {
            throw ClientError.message("Could not create snapshot bitmap.")
        }
        view.cacheDisplay(in: view.bounds, to: representation)
        var background = NSColor.windowBackgroundColor.cgColor
        view.effectiveAppearance.performAsCurrentDrawingAppearance {
            background = (view.window?.backgroundColor ?? NSColor.windowBackgroundColor).cgColor
        }
        let png = try NativeSnapshotRenderer.png(from: representation, background: background)
        try png.write(to: URL(fileURLWithPath: path))
    }
}

enum NativeSnapshotRenderer {
    // cacheDisplay captures the hosting view without the window behind it.
    // Include that background while rendering, so transparent native controls
    // keep the appearance they have on screen in every PNG viewer.
    static func png(from representation: NSBitmapImageRep, background: CGColor) throws -> Data {
        guard let image = representation.cgImage,
              let space = CGColorSpace(name: CGColorSpace.sRGB),
              let context = CGContext(data: nil, width: image.width, height: image.height,
                bitsPerComponent: 8, bytesPerRow: 0, space: space,
                bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue) else {
            throw ClientError.message("Could not prepare opaque snapshot rendering.")
        }
        let bounds = CGRect(x: 0, y: 0, width: image.width, height: image.height)
        context.setFillColor(background)
        context.fill(bounds)
        context.draw(image, in: bounds)
        guard let result = context.makeImage(),
              let png = NSBitmapImageRep(cgImage: result).representation(using: .png, properties: [:]) else {
            throw ClientError.message("PNG encoding failed.")
        }
        return png
    }
}

struct NavigationVerification: Encodable {
    let openedJob: Int?
    let returnedToQueue: Bool
    let windowStayedVisible: Bool
    let hostingViewUnchanged: Bool
    let noAttachedSheet: Bool
    var passed: Bool { openedJob != nil && returnedToQueue && windowStayedVisible && hostingViewUnchanged && noAttachedSheet }
}

struct RemoteStopStateVerification: Encodable {
    let label: String
    let enabled: Bool
    let error: String?
    let warningIndicator: Bool
    let preservedAfterRefresh: Bool
    var passed: Bool {
        label == "Needs attention" && !enabled && error == "Remote stop incomplete"
            && warningIndicator && preservedAfterRefresh
    }
}

struct PasswordValidationCase: Encodable {
    let name: String
    let expected: Bool
    let accepted: Bool
}

struct PasswordValidationVerification: Encodable {
    let cases: [PasswordValidationCase]
    let rejectionsSentNothing: Bool
    let acceptedPasswordReachedTheFixture: Bool
    var passed: Bool {
        !cases.isEmpty && cases.allSatisfy { $0.expected == $0.accepted }
            && rejectionsSentNothing && acceptedPasswordReachedTheFixture
    }
}

struct ConfirmationCaseVerification: Encodable {
    let action: String
    let jobID: Int
    let openingDidNotDispatch: Bool
    let declineDidNotDispatch: Bool
    let exactlyOneExpectedAction: Bool
    let duplicateWasRejected: Bool
    let reservationHeldBeforeIO: Bool
    let competingActionWasRejected: Bool
    let refreshDidNotLoseAcceptedAction: Bool
    let busyWasRejected: Bool
    let returnedToQueue: Bool
    let surfaceStayedVisible: Bool
    var passed: Bool {
        openingDidNotDispatch && declineDidNotDispatch && exactlyOneExpectedAction && duplicateWasRejected
            && reservationHeldBeforeIO && competingActionWasRejected && refreshDidNotLoseAcceptedAction
            && busyWasRejected && returnedToQueue && surfaceStayedVisible
    }
}

struct ConfirmationVerification: Encodable {
    let cases: [ConfirmationCaseVerification]
    let staleJobExplained: Bool
    let staleJobRejected: Bool
    let limitReachedOnlyTheFixture: Bool
    let finalSurfaceStayedVisible: Bool
    let recordedActions: [RecordedQueueAction]
    var passed: Bool {
        !cases.isEmpty && cases.allSatisfy(\.passed) && staleJobExplained && staleJobRejected
            && limitReachedOnlyTheFixture && finalSurfaceStayedVisible
    }
}
