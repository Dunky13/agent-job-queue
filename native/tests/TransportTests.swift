import Foundation
import Darwin

private func check(_ value: Bool, _ name: String) throws {
    print("\(value ? "PASS" : "FAIL") \(name)")
    if !value { throw ClientError.message(name) }
}

@main
struct TransportTests {
    static func main() async throws {
        if let index = CommandLine.arguments.firstIndex(of: "--stub") {
            try stub(Array(CommandLine.arguments.dropFirst(index + 1)))
            return
        }
        if CommandLine.arguments.dropFirst().first == "install", CommandLine.arguments.count >= 4 {
            let marker = CommandLine.arguments[3]
            let name = URL(fileURLWithPath: marker).lastPathComponent
            try stub([["automatic-wait", "automatic-transition", "automatic-hint"].contains(name) ? name : (name == "preflight-read" ? "hold" : "mutation"), marker])
            return
        }
        let temporary = FileManager.default.temporaryDirectory.appendingPathComponent("agent-queue-transport-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: temporary, withIntermediateDirectories: false,
                                              attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: temporary) }
        let executable = URL(fileURLWithPath: CommandLine.arguments[0])
        let transport = QueueClient.Transport(executable: executable, timeout: 2)
        let short = QueueClient.Transport(executable: executable, timeout: 0.15)
        let automaticTarget = QueueClient.InstallTarget(option: .appBundle, path: "/Applications/Agent Queue.app")
        let automaticInstall = QueueClient.Command.installRepairAndContinue(automaticTarget)
        try check(automaticInstall.timeout.isInfinite,
                  "Automatic installation can wait for existing queue work without an aggregate deadline")
        for command in [QueueClient.Command.install(automaticTarget), .setup(automaticInstall.arguments + ["--preflight", "--json"]),
                        .installPreflight(automaticTarget), .installRecovery(automaticTarget, action: "repair-service")] {
            try check(command.timeout == 330,
                      "Diagnostic and ambiguous setup commands retain a finite deadline")
        }
        func stubbed(_ command: QueueClient.Command, _ scenario: String, _ marker: URL) -> QueueClient.Command {
            var stubbed = command
            stubbed.arguments += ["--stub", scenario, marker.path]
            return stubbed
        }
        func arguments(_ scenario: String, _ marker: URL, mutation: Bool = false, input: Data? = nil) -> QueueClient.Command {
            var command = stubbed(mutation ? .profilesChange("edit", "stub", payload: Data()) : .status, scenario, marker)
            command.input = input
            return command
        }
        // Warm up Foundation's shared Process machinery before counting handles.
        _ = try await QueueClient.run(arguments("empty", temporary.appendingPathComponent("warmup")), transport: transport)
        let handlesBefore = openHandleCount()
        for index in 0..<12 {
            let final = try await QueueClient.run(arguments("final", temporary.appendingPathComponent("cleanup-\(index)")), transport: transport)
            try check(String(decoding: final, as: UTF8.self) == "final response", "Final controller bytes survive the child-exit fence")
        }
        try check(openHandleCount() == handlesBefore, "Repeated private read completion closes all per-request pipe handles")

        let earlyMarker = temporary.appendingPathComponent("early")
        let early = Task {
            withUnsafeCurrentTask { $0?.cancel() }
            return try await QueueClient.run(arguments("hold", earlyMarker), transport: transport)
        }
        do { _ = try await early.value; throw ClientError.message("Cancelled read succeeded") }
        catch is CancellationError { }
        try check(!FileManager.default.fileExists(atPath: earlyMarker.path),
                  "Already-cancelled read never launches its private child")

        let heldMarker = temporary.appendingPathComponent("held")
        let held = Task { try await QueueClient.run(arguments("hold", heldMarker), transport: transport) }
        try await waitForFile(heldMarker)
        let start = ProcessInfo.processInfo.systemUptime
        held.cancel()
        do { _ = try await held.value; throw ClientError.message("In-flight cancelled read succeeded") }
        catch is CancellationError { }
        try check(ProcessInfo.processInfo.systemUptime - start < 1 && childIsGone(heldMarker),
                  "In-flight cancellation promptly terminates and reaps the private read child")

        let resistantMarker = temporary.appendingPathComponent("resistant")
        let resistant = Task { try await QueueClient.run(arguments("resist", resistantMarker), transport: transport) }
        try await waitForFile(resistantMarker)
        resistant.cancel()
        do { _ = try await resistant.value; throw ClientError.message("Resistant read succeeded") }
        catch is CancellationError { }
        try check(childIsGone(resistantMarker), "Cancellation escalates for a private child that ignores termination")

        for scenario in ["stdout", "stderr", "mixed"] {
            let marker = temporary.appendingPathComponent(scenario)
            let start = ProcessInfo.processInfo.systemUptime
            do {
                _ = try await QueueClient.run(arguments(scenario, marker), transport: transport)
                throw ClientError.message("Oversized response succeeded")
            } catch {
                let elapsed = ProcessInfo.processInfo.systemUptime - start
                try check(error.localizedDescription.contains("2 MB display limit"),
                    "Oversized \(scenario) reports the display limit: \(error.localizedDescription)")
                try check(childIsGone(marker), "Oversized \(scenario) reaps the private controller")
                try check(elapsed < 1,
                    "Oversized \(scenario) stops before the stalled child deadline (elapsed \(elapsed)s)")
            }
        }
        let exact = try await QueueClient.run(arguments("exact", temporary.appendingPathComponent("exact")), transport: transport)
        try check(exact.count == 2_097_152, "Response exactly at the capture cap remains supported without pipe deadlock")

        for scenario in ["hold", "continuous", "stdin"] {
            let marker = temporary.appendingPathComponent("deadline-\(scenario)")
            do {
                _ = try await QueueClient.run(arguments(scenario, marker, mutation: scenario == "stdin",
                    input: scenario == "stdin" ? Data(repeating: 65, count: 16_384) : nil), transport: short)
                throw ClientError.message("Stalled response succeeded")
            } catch {
                try check(error.localizedDescription.contains("did not respond within") && reapedAfterShortDeadline(marker),
                          "Deadline and cleanup survive \(scenario) controller I/O")
            }
        }

        let mutationMarker = temporary.appendingPathComponent("mutation")
        let mutation = Task { try await QueueClient.run(arguments("mutation", mutationMarker, mutation: true), transport: transport) }
        try await waitForFile(mutationMarker)
        mutation.cancel()
        let result = try await mutation.value
        try check(String(decoding: result, as: UTF8.self) == "accepted" &&
            FileManager.default.fileExists(atPath: mutationMarker.appendingPathExtension("done").path) && childIsGone(mutationMarker),
            "Cancelling an accepted mutation leaves its private controller operation intact")

        for target in [QueueClient.InstallTarget.Option.appBundle, .payload] {
            let marker = temporary.appendingPathComponent("preflight-read")
            let read = Task { try await QueueClient.run(.installPreflight(.init(option: target, path: marker.path)), transport: transport) }
            try await waitForFile(marker)
            read.cancel()
            do { _ = try await read.value; throw ClientError.message("Cancelled setup preflight succeeded") }
            catch is CancellationError { }
            try check(childIsGone(marker), "Cancelling \(target.rawValue) setup preflight reaps its private read controller")
            try FileManager.default.removeItem(at: marker)
        }
        for (index, extra) in [["--recover", "repair-service"], ["--disable-phone-access"], ["--unexpected"]].enumerated() {
            let marker = temporary.appendingPathComponent("preflight-mutation-\(index)")
            let mutation = Task { try await QueueClient.run(.setup(["install", "--app-bundle", marker.path, "--preflight", "--json"] + extra),
                transport: transport) }
            try await waitForFile(marker)
            mutation.cancel()
            let response = try await mutation.value
            try check(String(decoding: response, as: UTF8.self) == "accepted" &&
                FileManager.default.fileExists(atPath: marker.appendingPathExtension("done").path),
                "Preflight syntax with additional options retains its accepted operation")
        }

        let setupWaitMarker = temporary.appendingPathComponent("automatic-wait")
        let setupWait = Task { try await QueueClient.run(.installRepairAndContinue(.init(option: .appBundle, path: setupWaitMarker.path)), transport: transport) }
        try await waitForFile(setupWaitMarker)
        setupWait.cancel()
        do { _ = try await setupWait.value; throw ClientError.message("Cancelled automatic wait succeeded") }
        catch is CancellationError { }
        try check(childIsGone(setupWaitMarker), "Stopping automatic waiting terminates and reaps only its private setup controller")
        let hintMarker = temporary.appendingPathComponent("automatic-hint")
        let hint = Task {
            try await QueueClient.run(.installRepairAndContinue(.init(option: .appBundle, path: hintMarker.path)),
                transport: transport, onOutput: { data in
                    if QueueSetupProgress.parse(String(decoding: data, as: UTF8.self)).isWaiting {
                        try? Data().write(to: hintMarker.appendingPathExtension("delivered"))
                    }
                })
        }
        try await waitForFile(hintMarker.appendingPathExtension("delivered"))
        hint.cancel()
        do { _ = try await hint.value; throw ClientError.message("Cancelled hint wait succeeded") }
        catch is CancellationError { }
        try check(childIsGone(hintMarker), "Throttled waiting progress is delivered even when the child emits no more output")

        let setupMutationMarker = temporary.appendingPathComponent("automatic-mutation")
        let setupMutation = Task { try await QueueClient.run(.installRepairAndContinue(.init(option: .appBundle, path: setupMutationMarker.path)), transport: transport) }
        try await waitForFile(setupMutationMarker)
        setupMutation.cancel()
        let setupMutationResult = try await setupMutation.value
        try check(String(decoding: setupMutationResult, as: UTF8.self) == "accepted" && childIsGone(setupMutationMarker),
            "Automatic setup cancellation preserves an accepted mutation until it completes or reaches another wait")

        // Hold the reader in its waiting callback while the real child enters
        // a mutation. Cancellation must not trust the reader's stale marker.
        let transitionMarker = temporary.appendingPathComponent("automatic-transition")
        let resumeReader = DispatchSemaphore(value: 0)
        defer { resumeReader.signal() }
        let transition = Task {
            try await QueueClient.run(.installRepairAndContinue(.init(option: .appBundle, path: transitionMarker.path)),
                transport: transport, onOutput: { data in
                    if QueueSetupProgress.parse(String(decoding: data, as: UTF8.self)).isWaiting {
                        try? Data().write(to: transitionMarker.appendingPathExtension("observed"))
                        resumeReader.wait()
                    }
                })
        }
        try await waitForFile(transitionMarker.appendingPathExtension("observed"))
        try Data().write(to: transitionMarker.appendingPathExtension("continue"))
        try await waitForFile(transitionMarker.appendingPathExtension("mutating"))
        transition.cancel()
        resumeReader.signal()
        let transitionResult = try await transition.value
        try check(String(decoding: transitionResult, as: UTF8.self).hasSuffix("accepted") &&
            FileManager.default.fileExists(atPath: transitionMarker.appendingPathExtension("done").path) && childIsGone(transitionMarker),
            "A stale waiting marker cannot interrupt an accepted setup mutation")

        let interleavedInput = Data(repeating: 66, count: 16_384)
        let interleaved = try await QueueClient.run(arguments("interleaved", temporary.appendingPathComponent("interleaved"), mutation: true, input: interleavedInput),
                                                    transport: transport)
        try check(interleaved.count == 81_920 && interleaved.suffix(interleavedInput.count) == interleavedInput,
                  "Private stdin and combined stderr/stdout make progress together without pipe deadlock")
        let empty = try await QueueClient.run(arguments("interleaved", temporary.appendingPathComponent("empty-input"), mutation: true, input: Data()),
                                              transport: transport)
        try check(empty.count == 65_536, "Empty private input closes promptly and lets the controller observe EOF")

        let daemonMarker = temporary.appendingPathComponent("daemon-controller")
        // The daemon holds the inherited output until released, past the read's own
        // deadline, so a read that waited for EOF fails here regardless of host load.
        _ = try await QueueClient.run(arguments("daemon", daemonMarker), transport: transport)
        let daemonPID = daemonMarker.appendingPathExtension("child")
        try check(!childIsGone(daemonPID),
                  "Read finishes after its direct child exits while an independent daemon holds inherited output")
        try Data().write(to: daemonPID.appendingPathExtension("release"))
        try await waitForFile(daemonPID.appendingPathExtension("done"))
        try check(childIsGone(daemonMarker), "Independent daemon work completes after its private read controller has been reaped")

        do {
            _ = try await QueueClient.run(arguments("failure", temporary.appendingPathComponent("failure"), mutation: true), transport: transport)
            throw ClientError.message("Failed controller succeeded")
        } catch {
            try check(error.localizedDescription == "Useful controller diagnostic", "Bounded stderr preserves controller diagnostics")
        }
        for _ in 0..<5 {
            let setup = try await QueueClient.run(.setup(["--stub", "setup-output", temporary.appendingPathComponent("setup-output").path]),
                transport: transport)
            try check(setup.count == 32_768 && String(decoding: setup, as: UTF8.self).hasSuffix("final setup bytes"),
                "Verbose setup output stays bounded and preserves the macOS child-exit tail")
        }
        do {
            _ = try await QueueClient.run(.setup(["--stub", "setup-failure", temporary.appendingPathComponent("setup-failure").path]),
                transport: transport)
            throw ClientError.message("Failed setup succeeded")
        } catch {
            try check(error.localizedDescription.utf8.count <= 8_000 && error.localizedDescription.hasSuffix("final setup bytes"),
                "Setup failure retains the actionable bounded stderr tail")
        }
        do {
            _ = try await QueueClient.run(stubbed(.profilesShow("stub"), "profile-failure", temporary.appendingPathComponent("profile-failure")), transport: transport)
            throw ClientError.message("Failed profile controller succeeded")
        } catch {
            try check(error.localizedDescription == "Workload changed", "Structured workload errors preserve their diagnostic")
        }

        try await selectionChecks(temporary: temporary, transport: transport)
        try await policyChecks(temporary: temporary, executable: executable, transport: transport, short: short)
    }

    /// The old-to-new call-site table: every command the app sends, every policy column.
    private static func policyTable() -> [(name: String, command: QueueClient.Command, expected: QueueClient.Command)] {
        typealias Command = QueueClient.Command
        let payload = Data("{}".utf8)
        let app = QueueClient.InstallTarget(option: .appBundle, path: "/Applications/Agent Queue.app")
        let appArguments = ["install", "--app-bundle", app.path]
        func queue(_ arguments: [String], timeout: TimeInterval = 25, cancellation: Command.Cancellation = .retain) -> Command {
            Command(controller: .heavy, arguments: arguments, input: nil, timeout: timeout, cancellation: cancellation,
                    reporting: .queue, decodesStructuredErrors: false)
        }
        func profiles(_ arguments: [String], input: Data? = nil, cancellation: Command.Cancellation) -> Command {
            Command(controller: .heavy, arguments: ["profiles"] + arguments + ["--json"], input: input, timeout: 25,
                    cancellation: cancellation, reporting: .queue, decodesStructuredErrors: true)
        }
        func updates(_ arguments: [String]) -> Command {
            Command(controller: .heavy, arguments: arguments, input: nil, timeout: 130, cancellation: .retain,
                    reporting: .updates, decodesStructuredErrors: false)
        }
        func remote(_ arguments: [String], input: Data? = nil, timeout: TimeInterval = 125,
                    cancellation: Command.Cancellation = .retain) -> Command {
            Command(controller: .remote, arguments: arguments, input: input, timeout: timeout, cancellation: cancellation,
                    reporting: .queue, decodesStructuredErrors: false)
        }
        func setup(_ arguments: [String], timeout: TimeInterval = 330, cancellation: Command.Cancellation = .retain) -> Command {
            Command(controller: .heavy, arguments: arguments, input: nil, timeout: timeout, cancellation: cancellation,
                    reporting: .setup, decodesStructuredErrors: false)
        }
        var table: [(name: String, command: Command, expected: Command)] = [
            ("status", .status, queue(["status", "--json"], timeout: 8, cancellation: .discard)),
            ("resume", .resume, queue(["resume"])),
            ("limit", .limit(3), queue(["limit", "3"])),
            ("scheduling", .scheduling(.adaptive), queue(["scheduling", SchedulingMode.adaptive.rawValue])),
            ("version", .version, queue(["version", "--json"])),
            ("agents status", .agentsStatus, queue(["agents", "status", "--json"])),
            ("agents install", .agents("install", runner: "codex"), queue(["agents", "install", "--runner", "codex", "--json"])),
            ("agents remove", .agents("remove", runner: "claude"), queue(["agents", "remove", "--runner", "claude", "--json"])),
            ("profiles list", .profilesList(offset: 50, limit: 50, query: "go"),
             profiles(["list", "--offset", "50", "--limit", "50", "--query", "go"], cancellation: .discard)),
            ("profiles show", .profilesShow("id"), profiles(["show", "id"], cancellation: .discard)),
            ("profiles show family", .profilesShow("id", family: "f"), profiles(["show", "id", "--family", "f"], cancellation: .discard)),
            ("profiles show members", .profilesShow("id", membersQuery: "q", membersOffset: 2),
             profiles(["show", "id", "--members-query", "q", "--members-offset", "2"], cancellation: .discard)),
            ("updates check", .updatesCheck, updates(["updates", "check", "--json"])),
            ("updates download", .updatesDownload(build: 7), updates(["updates", "download", "--build", "7", "--json"])),
            ("remote status", .remoteStatus, remote(["status", "--json"], timeout: 8, cancellation: .discard)),
            ("remote enable", .remoteEnabled(true), remote(["enable"])),
            ("remote disable", .remoteEnabled(false), remote(["disable"])),
            ("remote configure", .remoteConfigure(payload), remote(["configure"], input: payload)),
            ("install", .install(app), setup(appArguments)),
            ("install preflight", .installPreflight(app), setup(appArguments + ["--preflight", "--json"], cancellation: .discard)),
            ("install repair and continue", .installRepairAndContinue(app),
             setup(appArguments + ["--repair-and-continue"], timeout: .infinity, cancellation: .stopWhileWaiting)),
            ("install disabling phone access", .installDisablingPhoneAccess(app), setup(appArguments + ["--disable-phone-access"])),
            ("install recovery", .installRecovery(app, action: "repair-service"), setup(appArguments + ["--recover", "repair-service"])),
            ("install recovery job", .installRecovery(app, action: "cleanup-job", jobID: 4),
             setup(appArguments + ["--recover", "cleanup-job", "--job-id", "4"])),
            ("install recovery revision", .installRecovery(app, action: "reset-callback", expectedRevision: "abc"),
             setup(appArguments + ["--recover", "reset-callback", "--expected-revision", "abc"])),
            // A path that is not exactly absolute earns neither a discardable nor an unbounded policy.
            ("relative preflight", .installPreflight(.init(option: .appBundle, path: "relative")),
             setup(["install", "--app-bundle", "relative", "--preflight", "--json"])),
            ("relative repair and continue", .installRepairAndContinue(.init(option: .appBundle, path: "relative")),
             setup(["install", "--app-bundle", "relative", "--repair-and-continue"])),
        ]
        for action in ["next", "last", "cancel", "start"] {
            table.append(("job \(action)", .jobAction(action, id: 9), queue([action, "9"])))
        }
        for operation in ["edit", "merge", "split", "combine", "reset"] {
            table.append(("profiles \(operation)", .profilesChange(operation, "id", payload: payload),
                          profiles([operation, "id"], input: payload, cancellation: .retain)))
        }
        for service in QueueServiceManagement.Service.allCases {
            table.append(("service status \(service.rawValue)", .serviceStatus(service),
                          setup(["--service-management", "status", service.rawValue], timeout: 5, cancellation: .discard)))
        }
        return table
    }

    private static func policyChecks(temporary: URL, executable: URL, transport: QueueClient.Transport,
                                     short: QueueClient.Transport) async throws {
        let directory = temporary.appendingPathComponent("policy")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false)
        func stubbed(_ command: QueueClient.Command, _ scenario: String, _ marker: URL) -> QueueClient.Command {
            var stubbed = command
            stubbed.arguments += ["--stub", scenario, marker.path]
            return stubbed
        }
        // Policy only: Process itself rejects an argument containing NUL, so this cannot launch.
        try check(QueueClient.Command.installRepairAndContinue(.init(option: .appBundle, path: "/a\0b")) ==
            .setup(["install", "--app-bundle", "/a\0b", "--repair-and-continue"]),
            "A path containing NUL earns neither a discardable nor an unbounded policy")
        for (index, row) in policyTable().enumerated() {
            try check(row.command == row.expected, "\(row.name): argv, controller, input, timeout, cancellation, reporting and decoder")

            // Cancellation outcome through the real process runner.
            let cancelMarker = directory.appendingPathComponent("cancel-\(index)")
            switch row.command.cancellation {
            case .discard:
                let read = Task { try await QueueClient.run(stubbed(row.command, "hold", cancelMarker), transport: transport) }
                try await waitForFile(cancelMarker)
                read.cancel()
                do { _ = try await read.value; throw ClientError.message("\(row.name): cancelled read succeeded") }
                catch is CancellationError { }
                try check(childIsGone(cancelMarker), "\(row.name): cancelling discards the response and reaps the child")
            case .retain:
                let change = Task { try await QueueClient.run(stubbed(row.command, "mutation", cancelMarker), transport: transport) }
                try await waitForFile(cancelMarker)
                change.cancel()
                let response = try await change.value
                try check(String(decoding: response, as: UTF8.self) == "accepted" &&
                    FileManager.default.fileExists(atPath: cancelMarker.appendingPathExtension("done").path) && childIsGone(cancelMarker),
                    "\(row.name): cancelling keeps the accepted operation and its response")
            case .stopWhileWaiting:
                let waiting = Task { try await QueueClient.run(stubbed(row.command, "automatic-wait", cancelMarker), transport: transport) }
                try await waitForFile(cancelMarker)
                waiting.cancel()
                do { _ = try await waiting.value; throw ClientError.message("\(row.name): cancelled wait succeeded") }
                catch is CancellationError { }
                try check(childIsGone(cancelMarker), "\(row.name): cancelling a reported wait stops only its child")
                let mutationMarker = directory.appendingPathComponent("cancel-\(index)-mutation")
                let mutation = Task { try await QueueClient.run(stubbed(row.command, "mutation", mutationMarker), transport: transport) }
                try await waitForFile(mutationMarker)
                mutation.cancel()
                let response = try await mutation.value
                try check(String(decoding: response, as: UTF8.self) == "accepted" && childIsGone(mutationMarker),
                    "\(row.name): cancelling outside a reported wait keeps the accepted operation")
            }

            // Deadline wording through the real process runner.
            let deadlineMarker = directory.appendingPathComponent("deadline-\(index)")
            do {
                _ = try await QueueClient.run(stubbed(row.command, "hold", deadlineMarker), transport: short)
                throw ClientError.message("\(row.name): stalled command succeeded")
            } catch {
                let wording = switch row.command.reporting {
                case .queue: "The queue did not respond within"
                case .updates: "The update operation did not finish within"
                case .setup: "Setup did not finish within"
                }
                try check(error.localizedDescription.hasPrefix(wording) && reapedAfterShortDeadline(deadlineMarker),
                    "\(row.name): deadline reports its own wording and reaps the child")
            }

            // Failure decoding through the real process runner.
            do {
                _ = try await QueueClient.run(stubbed(row.command, "profile-failure", directory.appendingPathComponent("failure-\(index)")), transport: transport)
                throw ClientError.message("\(row.name): failed command succeeded")
            } catch {
                try check(error.localizedDescription == (row.command.decodesStructuredErrors ? "Workload changed" : "{\"error\":\"Workload changed\"}"),
                          "\(row.name): failure uses its own decoder")
            }
        }

        // Without a transport override, the command's own deadline applies.
        var quick = QueueClient.Command.status
        quick.timeout = 0.15
        let quickMarker = directory.appendingPathComponent("own-deadline")
        do {
            _ = try await QueueClient.run(stubbed(quick, "hold", quickMarker), transport: .init(executable: executable))
            throw ClientError.message("Command deadline did not apply")
        } catch {
            try check(error.localizedDescription.contains("did not respond within") && reapedAfterShortDeadline(quickMarker),
                      "A transport without a timeout applies the command's own deadline")
        }
        var quickSetup = QueueClient.Command.serviceStatus(.queue)
        quickSetup.timeout = 0.15
        let installerMarker = directory.appendingPathComponent("installer-deadline")
        do {
            _ = try await QueueBootstrapModel.runInstaller(.init(executable: executable,
                command: stubbed(quickSetup, "hold", installerMarker))) { _ in }
            throw ClientError.message("Installer deadline did not apply")
        } catch {
            try check(error.localizedDescription.hasPrefix("Setup did not finish within") && reapedAfterShortDeadline(installerMarker),
                      "Setup requests run with their command's own deadline and wording")
        }
    }

    private static func waitForFile(_ url: URL) async throws {
        let deadline = ProcessInfo.processInfo.systemUptime + 3
        while !FileManager.default.fileExists(atPath: url.path) {
            guard ProcessInfo.processInfo.systemUptime < deadline else { throw ClientError.message("Private stub did not launch") }
            try await Task.sleep(for: .milliseconds(5))
        }
    }

    private static func childIsGone(_ marker: URL) -> Bool {
        guard let text = try? String(contentsOf: marker, encoding: .utf8), let pid = Int32(text) else { return false }
        return kill(pid, 0) == -1 && errno == ESRCH
    }

    // A child killed at a very short deadline may not have written its marker yet.
    private static func reapedAfterShortDeadline(_ marker: URL) -> Bool {
        !FileManager.default.fileExists(atPath: marker.path) || childIsGone(marker)
    }

    private static func openHandleCount() -> Int {
        (0..<getdtablesize()).reduce(0) { count, descriptor in
            count + (fcntl(descriptor, F_GETFD) == -1 ? 0 : 1)
        }
    }

    @MainActor private static func selectionChecks(temporary: URL, transport: QueueClient.Transport) async throws {
        let directory = temporary.appendingPathComponent("selections")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false)
        let store = LearningStore(command: { command in
            var stubbed = command
            stubbed.arguments += ["--stub", "detail", directory.appendingPathComponent(command.arguments[2]).path]
            return try await QueueClient.run(stubbed, transport: transport)
        })
        let first = store.select("first")
        try await waitForFile(directory.appendingPathComponent("first"))
        var tasks: [Task<Void, Never>] = []
        for index in 0..<40 {
            if let task = store.select("selection-\(index)") { tasks.append(task) }
            await Task.yield()
        }
        let latest = store.select("latest")
        await first?.value
        for task in tasks { await task.value }
        await latest?.value
        let markers = try FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: nil)
        try check(store.selectedID == "latest" && store.detail?.profile.id == "latest" && store.error == nil &&
                  !store.isDetailLoading && markers.allSatisfy(childIsGone),
                  "Rapid real-process selections publish only the current detail and reap all read children")
        // Each child checks all predecessors on entry. Overlap creates this sentinel.
        try check(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("overlap").path),
                  "Rapid selections never have more than one live private detail child")
    }

    private static func stub(_ arguments: [String]) throws {
        let scenario = arguments[0]
        let marker = URL(fileURLWithPath: arguments[1])
        if CommandLine.arguments.contains("--repair-and-continue") {
            if ["automatic-wait", "automatic-hint"].contains(scenario) { signal(SIGUSR1) { _ in _exit(0) } }
            else { signal(SIGUSR1, SIG_IGN) }
        }
        if scenario == "resist" { signal(SIGTERM, SIG_IGN) }
        if scenario == "detail" {
            let others = try FileManager.default.contentsOfDirectory(at: marker.deletingLastPathComponent(), includingPropertiesForKeys: nil)
            for other in others where !childIsGone(other) {
                try Data().write(to: marker.deletingLastPathComponent().appendingPathComponent("overlap"))
            }
        }
        try Data(String(getpid()).utf8).write(to: marker)
        let bytes = Data(repeating: 65, count: 16_384)
        switch scenario {
        case "empty": return
        case "setup-output", "setup-failure":
            for _ in 0..<140 { try FileHandle.standardError.write(contentsOf: bytes) }
            try FileHandle.standardError.write(contentsOf: Data("final setup bytes".utf8))
            if scenario == "setup-failure" { exit(3) }
            return
        case "final":
            try FileHandle.standardOutput.write(contentsOf: Data("final response".utf8))
            return
        case "stdout", "stderr", "mixed", "exact":
            let count = scenario == "exact" ? 128 : 129
            for index in 0..<count {
                let output = scenario == "stderr" || (scenario == "mixed" && index % 2 == 0) ? FileHandle.standardError : .standardOutput
                try output.write(contentsOf: bytes)
            }
            if scenario == "exact" { return }
        case "continuous":
            while true { try FileHandle.standardOutput.write(contentsOf: Data("tick".utf8)); Thread.sleep(forTimeInterval: 0.002) }
        case "automatic-transition":
            try FileHandle.standardOutput.write(contentsOf: Data((QueueSetupProgress.waitingMarker + "\nWaiting\n").utf8))
            let deadline = ProcessInfo.processInfo.systemUptime + 3
            while !FileManager.default.fileExists(atPath: marker.appendingPathExtension("continue").path) {
                guard ProcessInfo.processInfo.systemUptime < deadline else { throw ClientError.message("Transition fixture was not released") }
                Thread.sleep(forTimeInterval: 0.005)
            }
            try FileHandle.standardOutput.write(contentsOf: Data((QueueSetupProgress.installingMarker + "\n").utf8))
            try Data().write(to: marker.appendingPathExtension("mutating"))
            Thread.sleep(forTimeInterval: 0.25)
            try Data().write(to: marker.appendingPathExtension("done"))
            try FileHandle.standardOutput.write(contentsOf: Data("accepted".utf8))
            return
        case "automatic-hint":
            try FileHandle.standardOutput.write(contentsOf: Data("Preparing\n".utf8))
            Thread.sleep(forTimeInterval: 0.01)
            try FileHandle.standardOutput.write(contentsOf: Data((QueueSetupProgress.waitingMarker + "\nWaiting\n").utf8))
        case "automatic-wait":
            try FileHandle.standardOutput.write(contentsOf: Data((QueueSetupProgress.waitingMarker + "\nWaiting for original callers\n").utf8))
        case "mutation":
            _ = try FileHandle.standardInput.readToEnd()
            Thread.sleep(forTimeInterval: 0.25)
            try Data().write(to: marker.appendingPathExtension("done"))
            try FileHandle.standardOutput.write(contentsOf: Data("accepted".utf8))
            return
        case "interleaved":
            for index in 0..<4 {
                try (index % 2 == 0 ? FileHandle.standardError : .standardOutput).write(contentsOf: bytes)
            }
            let input = try FileHandle.standardInput.readToEnd() ?? Data()
            try FileHandle.standardOutput.write(contentsOf: input)
            return
        case "daemon":
            let daemon = Process()
            daemon.executableURL = URL(fileURLWithPath: CommandLine.arguments[0])
            daemon.arguments = ["--stub", "independent-daemon", marker.appendingPathExtension("child").path]
            daemon.standardOutput = FileHandle.standardOutput
            daemon.standardError = FileHandle.standardError
            try daemon.run()
            let deadline = ProcessInfo.processInfo.systemUptime + 2
            while !FileManager.default.fileExists(atPath: marker.appendingPathExtension("child").path) {
                guard ProcessInfo.processInfo.systemUptime < deadline else { throw ClientError.message("Daemon fixture did not start") }
                Thread.sleep(forTimeInterval: 0.005)
            }
            return
        case "independent-daemon":
            let deadline = ProcessInfo.processInfo.systemUptime + 5
            while !FileManager.default.fileExists(atPath: marker.appendingPathExtension("release").path),
                  ProcessInfo.processInfo.systemUptime < deadline {
                Thread.sleep(forTimeInterval: 0.005)
            }
            try Data().write(to: marker.appendingPathExtension("done"))
            return
        case "failure":
            try FileHandle.standardError.write(contentsOf: Data("Useful controller diagnostic".utf8))
            exit(3)
        case "profile-failure":
            _ = try FileHandle.standardInput.readToEnd()
            try FileHandle.standardError.write(contentsOf: Data("{\"error\":\"Workload changed\"}".utf8))
            exit(3)
        case "detail":
            if marker.lastPathComponent == "latest" {
                var detail = LearningStore.previewDetail()
                let profile = detail.profile
                detail = LearningDetail(revision: detail.revision,
                    profile: LearningProfile(id: "latest", label: profile.label, memberCount: profile.memberCount,
                        variantCount: profile.variantCount, successCount: profile.successCount,
                        incompleteCount: profile.incompleteCount, peakMemoryBytes: profile.peakMemoryBytes,
                        peakCpuCores: profile.peakCpuCores, lastRunAt: profile.lastRunAt,
                        memoryFloorBytes: profile.memoryFloorBytes, cpuFloorCores: profile.cpuFloorCores),
                    members: detail.members, variants: detail.variants, runs: detail.runs,
                    prediction: detail.prediction, predictionMachine: detail.predictionMachine, truncated: detail.truncated)
                try FileHandle.standardOutput.write(contentsOf: JSONEncoder().encode(detail))
                return
            }
        default: break
        }
        Thread.sleep(forTimeInterval: 5)
    }
}
