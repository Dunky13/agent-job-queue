// Test-only hooks: `native/build-library.sh DIR ARCH test` compiles this directory
// into the library; the shipped release flavour leaves it out. See CONTRIBUTING.md.
import AppKit
import SwiftUI
import Foundation

@MainActor
final class FixtureDispatchRecorder {
    private(set) var actions: [RecordedQueueAction] = []
    func record(action: String, jobID: Int) { actions.append(RecordedQueueAction(action: action, jobID: jobID)) }
}

struct RecordedQueueAction: Codable, Equatable {
    let action: String
    let jobID: Int
}

extension AppDelegate {
    /// True when `--snapshot` takes over this launch; it terminates the app when done.
    func runSnapshotHooks() -> Bool {
        let arguments = CommandLine.arguments
        guard let index = arguments.firstIndex(of: "--snapshot"), arguments.indices.contains(index + 1) else { return false }
        guard AppRuntime.store.fixtureMode else { diagnostic("--snapshot requires --fixture JSONFILE"); exit(EXIT_FAILURE) }
        AppRuntime.showWindow()
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
            arguments.indices.contains($0 + 1) ? ConfirmationAction(rawValue: arguments[$0 + 1]) : nil
        }
        if let action = requestedConfirmation ?? (confirmationVerificationPath == nil ? nil : .start) {
            guard let job = AppRuntime.store.snapshot?.jobs.first(where: {
                action == .start ? $0.isWaiting && $0.canReorder : $0.canCancel
            }) else {
                diagnostic("Confirmation snapshot requires an eligible job in the fixture."); exit(EXIT_FAILURE)
            }
            navigation.request(action, job: job)
        } else if let detailIndex = arguments.firstIndex(of: "--snapshot-detail"), arguments.indices.contains(detailIndex + 1) {
            guard let jobID = Int(arguments[detailIndex + 1]),
                  let job = AppRuntime.store.snapshot?.jobs.first(where: { $0.id == jobID }) else {
                diagnostic("--snapshot-detail requires a job ID present in the fixture."); exit(EXIT_FAILURE)
            }
            navigation.show(job)
        } else if verificationPath != nil {
            guard let job = AppRuntime.store.snapshot?.jobs.first else {
                diagnostic("Navigation verification requires a fixture containing a job."); exit(EXIT_FAILURE)
            }
            navigation.show(job)
        }
        if let window = AppRuntime.queueWindow {
            let menuSize = arguments.contains("--snapshot-menu")
            window.contentView = NSHostingView(rootView: QueuePanel(store: AppRuntime.store, fullWindow: !menuSize, navigation: navigation))
            if menuSize {
                window.minSize = NSSize(width: 500, height: 650)
                window.setContentSize(NSSize(width: 500, height: 650))
                window.center()
            }
            if arguments.contains("--snapshot-remote-settings") {
                window.contentView = NSHostingView(rootView: RemoteAccessSettings(store: AppRuntime.remoteStore))
                window.minSize = NSSize(width: 500, height: 570)
                window.setContentSize(NSSize(width: 580, height: 780))
                window.center()
            }
            if arguments.contains("--snapshot-agent-integration") {
                window.contentView = NSHostingView(rootView: AgentIntegrationSettings(store: AppRuntime.integrationStore) {})
                window.minSize = NSSize(width: 520, height: 540)
                window.setContentSize(NSSize(width: 600, height: 720))
                window.center()
            }
            if arguments.contains("--snapshot-learning") {
                window.contentView = NSHostingView(rootView: LearningWindow(store: AppRuntime.learningStore) {})
                window.minSize = NSSize(width: 800, height: 550)
                window.setContentSize(NSSize(width: 1000, height: 780))
                window.center()
            }
            if arguments.contains("--snapshot-updates") {
                window.contentView = NSHostingView(rootView: QueueUpdatesSettings(store: AppRuntime.updateStore) {})
                window.minSize = NSSize(width: 590, height: 500)
                window.setContentSize(NSSize(width: 620, height: 620))
                window.center()
            }
        }
        Task { @MainActor in
            try? await Task.sleep(for: .milliseconds(500))
            guard let window = AppRuntime.queueWindow, let view = window.contentView else {
                self.diagnostic("Could not create snapshot view."); exit(EXIT_FAILURE)
            }
            do {
                let snapshotPath = arguments[index + 1]
                try self.writeSnapshot(view, to: snapshotPath)
                if let remoteStopVerificationPath {
                    let remote = AppRuntime.remoteStore
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
                    let report = self.verifyPasswordValidation()
                    let encoder = JSONEncoder()
                    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
                    try encoder.encode(report).write(to: URL(fileURLWithPath: passwordVerificationPath))
                    guard report.passed else { throw ClientError.message("Password validation regression failed; inspect the verification report.") }
                }
                if let confirmationVerificationPath {
                    let report = try await self.verifyConfirmations(navigation: navigation,
                        store: AppRuntime.store, window: window, view: view)
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

    private func verifyPasswordValidation() -> PasswordValidationVerification {
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
        let remote = AppRuntime.remoteStore
        let enabledBefore = remote.status?.enabled
        let fixtureConfigurationRejected = !remote.configure(password: "fixture-only-password", enable: true)
        remote.setEnabled(true)
        return PasswordValidationVerification(cases: cases,
            fixtureConfigurationRejected: fixtureConfigurationRejected,
            fixtureEnableRejected: remote.status?.enabled == enabledBefore && !remote.isBusy)
    }

    private func verifyConfirmations(navigation: QueueNavigation, store: QueueStore,
                                     window: NSWindow, view: NSView) async throws -> ConfirmationVerification {
        guard store.fixtureMode, let recorder = store.fixtureRecorder, let original = store.snapshot,
              let queued = original.jobs.first(where: { $0.isWaiting && $0.canReorder && $0.canCancel }) else {
            throw ClientError.message("Confirmation verification requires a fixture with an eligible waiting job.")
        }
        let paused = QueueJob(id: -100, state: "paused", title: "Paused cancellation regression",
            project: "Fixture", branch: "", directory: "", executable: "", priority: 0, queuePosition: nil,
            submittedAt: nil, startedAt: nil, finishedAt: nil, exitCode: nil, logPath: "",
            canReorder: false, canCancel: true, phase: "Paused")
        store.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue, jobs: original.jobs + [paused])
        defer { navigation.decline(); store.snapshot = original; store.isMutating = false }

        func sameSurface() -> Bool {
            window.isVisible && window.contentView === view && window.attachedSheet == nil
        }
        var results: [ConfirmationCaseVerification] = []
        var cases: [(ConfirmationAction, QueueJob)] = [(.start, queued), (.cancel, queued), (.cancel, paused)]
        if let running = original.jobs.first(where: { $0.isRunning && $0.canCancel }) { cases.append((.cancel, running)) }
        for (action, job) in cases {
            let before = recorder.actions.count
            navigation.request(action, job: job)
            try await Task.sleep(for: .milliseconds(60))
            var surfaceStayedVisible = sameSurface()
            let openingDidNotDispatch = recorder.actions.count == before
            navigation.decline()
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let declineDidNotDispatch = recorder.actions.count == before && navigation.confirmation == nil

            navigation.request(action, job: job)
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let firstDispatch = navigation.confirm(in: store)
            let reservationHeldBeforeIO = store.isMutating
            let duplicateWasRejected = navigation.confirm(in: store) == nil
            let competingActionWasRejected = store.dispatch("next", job: queued) == nil
            // Exercise the old lost-confirmation seam: a refresh flag changes
            // after confirmation but before the reserved async task can execute.
            store.isRefreshing = true
            await firstDispatch?.value
            store.isRefreshing = false
            try await Task.sleep(for: .milliseconds(60))
            surfaceStayedVisible = surfaceStayedVisible && sameSurface()
            let expected = [RecordedQueueAction(action: action.rawValue, jobID: job.id)]
            let exactlyOneExpectedAction = Array(recorder.actions.dropFirst(before)) == expected

            navigation.request(action, job: job)
            store.isMutating = true
            let busyWasRejected = navigation.confirm(in: store) == nil
            store.isMutating = false
            navigation.decline()
            results.append(ConfirmationCaseVerification(action: action.rawValue, jobID: job.id,
                openingDidNotDispatch: openingDidNotDispatch, declineDidNotDispatch: declineDidNotDispatch,
                exactlyOneExpectedAction: exactlyOneExpectedAction, duplicateWasRejected: duplicateWasRejected,
                reservationHeldBeforeIO: reservationHeldBeforeIO, competingActionWasRejected: competingActionWasRejected,
                refreshDidNotLoseAcceptedAction: exactlyOneExpectedAction,
                busyWasRejected: busyWasRejected, returnedToQueue: navigation.confirmation == nil,
                surfaceStayedVisible: surfaceStayedVisible))
        }

        let beforeStale = recorder.actions.count
        navigation.request(.start, job: queued)
        // The same ID has become paused while the confirmation panel was open.
        let transitioned = QueueJob(id: queued.id, state: "paused", title: queued.title, project: queued.project,
            branch: queued.branch, directory: queued.directory, executable: queued.executable, priority: queued.priority,
            queuePosition: nil, submittedAt: queued.submittedAt, startedAt: queued.startedAt,
            finishedAt: nil, exitCode: nil, logPath: queued.logPath, canReorder: false, canCancel: true, phase: "Paused")
        store.snapshot = QueueSnapshot(updatedAt: original.updatedAt, queue: original.queue,
            jobs: original.jobs.map { $0.id == queued.id ? transitioned : $0 })
        let staleJobExplained = navigation.unavailability(in: store) != nil
        let staleJobRejected = navigation.confirm(in: store) == nil && recorder.actions.count == beforeStale
        navigation.decline()
        // The recorder makes every fixture action inert, including accidental limit changes.
        store.dispatchLimit(2)
        let fixtureLimitDidNotDispatch = recorder.actions.count == beforeStale
        let restoredSurface = sameSurface()
        store.snapshot = original
        try await Task.sleep(for: .milliseconds(80))
        return ConfirmationVerification(cases: results, staleJobExplained: staleJobExplained,
            staleJobRejected: staleJobRejected, fixtureLimitDidNotDispatch: fixtureLimitDidNotDispatch,
            finalSurfaceStayedVisible: restoredSurface, recordedActions: recorder.actions)
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
    let fixtureConfigurationRejected: Bool
    let fixtureEnableRejected: Bool
    var passed: Bool {
        !cases.isEmpty && cases.allSatisfy { $0.expected == $0.accepted }
            && fixtureConfigurationRejected && fixtureEnableRejected
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
    let fixtureLimitDidNotDispatch: Bool
    let finalSurfaceStayedVisible: Bool
    let recordedActions: [RecordedQueueAction]
    var passed: Bool {
        !cases.isEmpty && cases.allSatisfy(\.passed) && staleJobExplained && staleJobRejected
            && fixtureLimitDidNotDispatch && finalSurfaceStayedVisible
    }
}
