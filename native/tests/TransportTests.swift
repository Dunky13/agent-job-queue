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
        let automaticInstall = ["install", "--app-bundle", "/Applications/Agent Queue.app", "--repair-and-continue"]
        try check(QueueClient.timeout(for: automaticInstall, budget: .setup).isInfinite,
                  "Automatic installation can wait for existing queue work without an aggregate deadline")
        for arguments in [Array(automaticInstall.dropLast()), automaticInstall + ["--preflight", "--json"],
                          ["install", "--app-bundle", "/Applications/Agent Queue.app", "--preflight", "--json"],
                          ["install", "--app-bundle", "/Applications/Agent Queue.app", "--recover", "repair-service"]] {
            try check(QueueClient.timeout(for: arguments, budget: .setup) == 330,
                      "Diagnostic and ambiguous setup commands retain a finite deadline")
        }
        func arguments(_ scenario: String, _ marker: URL, mutation: Bool = false) -> [String] {
            (mutation ? ["profiles", "edit"] : ["status"]) + ["--stub", scenario, marker.path]
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
                _ = try await QueueClient.run(arguments(scenario, marker, mutation: scenario == "stdin"),
                    input: scenario == "stdin" ? Data(repeating: 65, count: 16_384) : nil, transport: short)
                throw ClientError.message("Stalled response succeeded")
            } catch {
                try check(error.localizedDescription.contains("did not respond within") && childIsGone(marker),
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

        for target in ["--app-bundle", "--payload"] {
            let marker = temporary.appendingPathComponent("preflight-read")
            let read = Task { try await QueueClient.run(["install", target, marker.path, "--preflight", "--json"],
                budget: .setup, transport: transport) }
            try await waitForFile(marker)
            read.cancel()
            do { _ = try await read.value; throw ClientError.message("Cancelled setup preflight succeeded") }
            catch is CancellationError { }
            try check(childIsGone(marker), "Cancelling \(target) setup preflight reaps its private read controller")
            try FileManager.default.removeItem(at: marker)
        }
        for (index, extra) in [["--recover", "repair-service"], ["--disable-phone-access"], ["--unexpected"]].enumerated() {
            let marker = temporary.appendingPathComponent("preflight-mutation-\(index)")
            let mutation = Task { try await QueueClient.run(["install", "--app-bundle", marker.path, "--preflight", "--json"] + extra,
                budget: .setup, transport: transport) }
            try await waitForFile(marker)
            mutation.cancel()
            let response = try await mutation.value
            try check(String(decoding: response, as: UTF8.self) == "accepted" &&
                FileManager.default.fileExists(atPath: marker.appendingPathExtension("done").path),
                "Preflight syntax with additional options retains its accepted operation")
        }

        let setupWaitMarker = temporary.appendingPathComponent("automatic-wait")
        let setupWait = Task { try await QueueClient.run(["install", "--app-bundle", setupWaitMarker.path, "--repair-and-continue"],
            budget: .setup, transport: transport) }
        try await waitForFile(setupWaitMarker)
        setupWait.cancel()
        do { _ = try await setupWait.value; throw ClientError.message("Cancelled automatic wait succeeded") }
        catch is CancellationError { }
        try check(childIsGone(setupWaitMarker), "Stopping automatic waiting terminates and reaps only its private setup controller")
        let hintMarker = temporary.appendingPathComponent("automatic-hint")
        let hint = Task {
            try await QueueClient.run(["install", "--app-bundle", hintMarker.path, "--repair-and-continue"],
                budget: .setup, transport: transport, onOutput: { data in
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
        let setupMutation = Task { try await QueueClient.run(["install", "--app-bundle", setupMutationMarker.path, "--repair-and-continue"],
            budget: .setup, transport: transport) }
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
            try await QueueClient.run(["install", "--app-bundle", transitionMarker.path, "--repair-and-continue"],
                budget: .setup, transport: transport, onOutput: { data in
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
        let interleaved = try await QueueClient.run(arguments("interleaved", temporary.appendingPathComponent("interleaved"), mutation: true),
                                                    input: interleavedInput, transport: transport)
        try check(interleaved.count == 81_920 && interleaved.suffix(interleavedInput.count) == interleavedInput,
                  "Private stdin and combined stderr/stdout make progress together without pipe deadlock")
        let empty = try await QueueClient.run(arguments("interleaved", temporary.appendingPathComponent("empty-input"), mutation: true),
                                              input: Data(), transport: transport)
        try check(empty.count == 65_536, "Empty private input closes promptly and lets the controller observe EOF")

        let daemonMarker = temporary.appendingPathComponent("daemon-controller")
        let daemonStart = ProcessInfo.processInfo.systemUptime
        _ = try await QueueClient.run(arguments("daemon", daemonMarker), transport: transport)
        let daemonPID = daemonMarker.appendingPathExtension("child")
        try check(ProcessInfo.processInfo.systemUptime - daemonStart < 0.5 && !childIsGone(daemonPID),
                  "Read finishes after its direct child exits while an independent daemon holds inherited output")
        try await waitForFile(daemonPID.appendingPathExtension("done"))
        try check(childIsGone(daemonMarker), "Independent daemon work completes after its private read controller has been reaped")

        do {
            _ = try await QueueClient.run(arguments("failure", temporary.appendingPathComponent("failure"), mutation: true), transport: transport)
            throw ClientError.message("Failed controller succeeded")
        } catch {
            try check(error.localizedDescription == "Useful controller diagnostic", "Bounded stderr preserves controller diagnostics")
        }
        for _ in 0..<5 {
            let setup = try await QueueClient.run(arguments("setup-output", temporary.appendingPathComponent("setup-output"), mutation: true),
                budget: .setup, transport: transport)
            try check(setup.count == 32_768 && String(decoding: setup, as: UTF8.self).hasSuffix("final setup bytes"),
                "Verbose setup output stays bounded and preserves the macOS child-exit tail")
        }
        do {
            _ = try await QueueClient.run(arguments("setup-failure", temporary.appendingPathComponent("setup-failure"), mutation: true),
                budget: .setup, transport: transport)
            throw ClientError.message("Failed setup succeeded")
        } catch {
            try check(error.localizedDescription.utf8.count <= 8_000 && error.localizedDescription.hasSuffix("final setup bytes"),
                "Setup failure retains the actionable bounded stderr tail")
        }
        do {
            _ = try await QueueClient.run(["profiles", "show", "--stub", "profile-failure", temporary.appendingPathComponent("profile-failure").path], transport: transport)
            throw ClientError.message("Failed profile controller succeeded")
        } catch {
            try check(error.localizedDescription == "Workload changed", "Structured workload errors preserve their diagnostic")
        }

        try await selectionChecks(temporary: temporary, transport: transport)
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

    private static func openHandleCount() -> Int {
        (0..<getdtablesize()).reduce(0) { count, descriptor in
            count + (fcntl(descriptor, F_GETFD) == -1 ? 0 : 1)
        }
    }

    @MainActor private static func selectionChecks(temporary: URL, transport: QueueClient.Transport) async throws {
        let directory = temporary.appendingPathComponent("selections")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false)
        let store = LearningStore(command: { arguments, input in
            try await QueueClient.run(arguments + ["--stub", "detail", directory.appendingPathComponent(arguments[2]).path],
                                      input: input, transport: transport)
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
            Thread.sleep(forTimeInterval: 0.75)
            try Data().write(to: marker.appendingPathExtension("done"))
            return
        case "failure":
            try FileHandle.standardError.write(contentsOf: Data("Useful controller diagnostic".utf8))
            exit(3)
        case "profile-failure":
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
