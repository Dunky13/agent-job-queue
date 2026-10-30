import Darwin
import Foundation

private typealias Helper = QueueHelper
private typealias Supervisor = QueueHelperSupervisor

private struct TestFailure: Error {
    let message: String
}

/// Records launches and lets a test decide when and how each fake child exits. It never
/// starts a process or signals a real pid.
private final class FakeSpawner: @unchecked Sendable {
    struct Record {
        let launch: Helper.Launch
        let at: ContinuousClock.Instant
        var signals: [Int32] = []
        var onExit: (@Sendable (Helper.Exit) -> Void)?
    }

    private let lock = NSLock()
    private var records: [Record] = []
    private var failures = 0
    private var ignoresTerm = false
    private var nextPid: Int32 = 70_000

    var spawner: Helper.Spawner {
        { launch, onExit in
            try self.begin(launch, onExit)
        }
    }

    func failNext(_ count: Int = 1) { lock.withLock { failures = count } }
    func ignoreTerm(_ value: Bool) { lock.withLock { ignoresTerm = value } }
    var launches: [Record] { lock.withLock { records } }
    func launches(_ name: Helper.Name) -> [Record] { launches.filter { $0.launch.name == name } }
    func count(_ name: Helper.Name) -> Int { launches(name).count }

    private func begin(_ launch: Helper.Launch, _ onExit: @escaping @Sendable (Helper.Exit) -> Void) throws -> Helper.Child {
        let index: Int = try lock.withLock {
            if failures > 0 {
                failures -= 1
                throw POSIXError(.ENOENT)
            }
            records.append(Record(launch: launch, at: .now, onExit: onExit))
            nextPid += 1
            return records.count - 1
        }
        let pid = lock.withLock { 70_000 + Int32(index) + 1 }
        return Helper.Child(pid: pid, signal: { signal in
            let callback: (@Sendable (Helper.Exit) -> Void)? = self.lock.withLock {
                self.records[index].signals.append(signal)
                guard signal == SIGKILL || (signal == SIGTERM && !self.ignoresTerm) else { return nil }
                defer { self.records[index].onExit = nil }
                return self.records[index].onExit
            }
            callback?(.signal(signal))
        })
    }

    /// Ends the most recent fake child of `name` as if it exited by itself.
    func finish(_ name: Helper.Name, _ exit: Helper.Exit) {
        let callback: (@Sendable (Helper.Exit) -> Void)? = lock.withLock {
            guard let index = records.lastIndex(where: { $0.launch.name == name && $0.onExit != nil }) else { return nil }
            defer { records[index].onExit = nil }
            return records[index].onExit
        }
        callback?(exit)
    }
}

private final class Flag: @unchecked Sendable {
    private let lock = NSLock()
    private var current: Bool
    private var count = 0
    init(_ value: Bool) { current = value }
    var value: Bool {
        get { lock.withLock { current } }
        set { lock.withLock { current = newValue } }
    }
    var calls: Int { lock.withLock { count } }
    var probe: Helper.Probe {
        { self.lock.withLock { self.count += 1; return self.current } }
    }
}

private func sleep(_ seconds: Double) async {
    try? await Task.sleep(for: .milliseconds(Int(seconds * 1000)))
}

private func eventually(_ seconds: Double = 10, _ condition: @MainActor () -> Bool) async -> Bool {
    let deadline = ContinuousClock.now + .milliseconds(Int(seconds * 1000))
    while ContinuousClock.now < deadline {
        if await condition() { return true }
        await sleep(0.02)
    }
    return await condition()
}

private func seconds(_ duration: Duration) -> Double {
    Double(duration.components.seconds) + Double(duration.components.attoseconds) / 1e18
}

private func text(_ url: URL) -> String { (try? String(contentsOf: url, encoding: .utf8)) ?? "" }

private func mode(_ url: URL) -> mode_t? {
    var info = stat()
    return stat(url.path, &info) == 0 ? info.st_mode & 0o777 : nil
}

private func size(_ url: URL) -> Int { (try? FileManager.default.attributesOfItem(atPath: url.path)[.size] as? Int) ?? -1 }

private let helperScript = """
#!/bin/sh
d=$(dirname "$0")
if [ "$1" = status ]; then
  code=$(cat "$d/status.code" 2>/dev/null || echo 1)
  [ "$code" = hang ] && exec /bin/sleep 600
  exit "$code"
fi
echo "$$" >> "$d/$1.runs"
env > "$d/$1.env"
echo "$0 $*" > "$d/$1.args"
n=$(cat "$d/leak" 2>/dev/null)
if [ -n "$n" ]; then
  if ( : >&"$n" ) 2>/dev/null; then echo leaked; else echo clean; fi > "$d/$1.fd"
fi
echo "hello from $1"
case "$(cat "$d/$1.mode" 2>/dev/null)" in
  exit0) exit 0 ;;
  exit3) exit 3 ;;
  selfkill) kill -9 $$ ;;
  ignoreterm) trap '' TERM; echo ready > "$d/$1.ready"; while :; do /bin/sleep 0.05; done ;;
  *) exec /bin/sleep 600 ;;
esac
"""

@MainActor
private final class Fixture {
    let root: URL
    let app: URL
    let macOS: URL
    let state: URL
    let throttle: Duration

    init(throttle: Duration = .milliseconds(300)) throws {
        root = URL(fileURLWithPath: try FileManager.default.url(
            for: .itemReplacementDirectory, in: .userDomainMask, appropriateFor: FileManager.default.temporaryDirectory, create: true
        ).path).resolvingSymlinksInPath()
        app = root.appendingPathComponent("Agent Queue.app")
        macOS = app.appendingPathComponent("Contents/MacOS")
        state = root.appendingPathComponent("state/agent-heavy")
        self.throttle = throttle
        try FileManager.default.createDirectory(at: macOS, withIntermediateDirectories: true)
        for name in ["agent-heavy", "agent-remote"] {
            let url = macOS.appendingPathComponent(name)
            try helperScript.write(to: url, atomically: true, encoding: .utf8)
            chmod(url.path, 0o755)
        }
    }

    func cleanup() { try? FileManager.default.removeItem(at: root) }
    func file(_ name: String) -> URL { macOS.appendingPathComponent(name) }
    func setMode(_ subcommand: String, _ value: String) { try? value.write(to: file("\(subcommand).mode"), atomically: true, encoding: .utf8) }
    func runs(_ subcommand: String) -> [String] { text(file("\(subcommand).runs")).split(separator: "\n").map(String.init) }

    var environment: [String: String] {
        ["HOME": root.path, "USER": "tester", "LOGNAME": "tester", "TMPDIR": "/tmp/", "LANG": "C", "PATH": "/evil/bin",
         "AGENT_HEAVY_HOME": "/nowhere", "AGENT_REMOTE_PORT": "1", "AGENT_QUEUE_TEST": "1", "FOO": "bar"]
    }

    func real(probe: Helper.Probe? = { false }) -> Supervisor {
        Supervisor(app: app, stateDirectory: state, environment: environment, throttle: throttle, probe: probe)
    }

    func fake(_ spawner: FakeSpawner, probe: @escaping Helper.Probe = { false }) -> Supervisor {
        Supervisor(app: app, stateDirectory: state, environment: environment, throttle: throttle, probe: probe,
                   spawner: spawner.spawner)
    }
}

@main
struct SupervisorTests {
    @MainActor static func main() async throws {
        setvbuf(stdout, nil, _IOLBF, 0)
        var passed = 0
        func check(_ condition: Bool, _ name: String) throws {
            guard condition else { throw TestFailure(message: name) }
            passed += 1
            print("PASS \(name)")
        }
        func scenario(throttle: Duration = .milliseconds(300), _ body: (Fixture) async throws -> Void) async throws {
            let fixture = try Fixture(throttle: throttle)
            defer { fixture.cleanup() }
            try await body(fixture)
        }
        func alive(_ pid: Int32) -> Bool { kill(pid, 0) == 0 }

        // Pure helpers.
        let scrubbed = Helper.childEnvironment(from: [
            "HOME": "/Users/x", "USER": "x", "LOGNAME": "x", "TMPDIR": "/var/t/", "LANG": "en_NL.UTF-8", "PATH": "/opt/evil",
            "AGENT_HEAVY_HOME": "/y", "AGENT_REMOTE_TOKEN": "z", "SSH_AUTH_SOCK": "/s", "DYLD_INSERT_LIBRARIES": "/lib",
        ])
        try check(scrubbed == ["HOME": "/Users/x", "USER": "x", "LOGNAME": "x", "TMPDIR": "/var/t/", "LANG": "en_NL.UTF-8",
                               "PATH": "/usr/bin:/bin:/usr/sbin:/sbin"], "environment is an allowlist with a fixed PATH")
        try check(Helper.childEnvironment(from: [:]) == ["PATH": "/usr/bin:/bin:/usr/sbin:/sbin"], "absent variables are not invented")
        try check(Helper.Exit(waitStatus: 0) == .code(0) && Helper.Exit(waitStatus: 3 << 8) == .code(3)
                  && Helper.Exit(waitStatus: 9) == .signal(9) && Helper.Exit(waitStatus: 15) == .signal(15), "wait status decoding")
        try check(Helper.Name.queue.executable == "agent-heavy" && Helper.Name.resources.arguments == ["scheduler"]
                  && Helper.Name.remote.executable == "agent-remote" && Helper.Name.remote.logPath == "remote/service.log",
                  "process definitions mirror the launchd plists")

        // Launch shape with a fake spawner.
        try await scenario { f in
            let spawner = FakeSpawner()
            let supervisor = f.fake(spawner)
            supervisor.start()
            _ = await eventually { spawner.launches.count == 2 }
            try check(spawner.count(.queue) == 1 && spawner.count(.resources) == 1 && spawner.count(.remote) == 0,
                      "start launches queue and resources but not remote")
            let queue = spawner.launches(.queue)[0].launch, resources = spawner.launches(.resources)[0].launch
            try check(queue.executable == f.file("agent-heavy") && queue.name.executable == "agent-heavy" && queue.arguments == ["daemon"],
                      "queue runs agent-heavy daemon")
            try check(resources.executable == f.file("agent-heavy") && resources.arguments == ["scheduler"], "resources runs agent-heavy scheduler")
            try check(queue.log == f.state.appendingPathComponent("daemon.log")
                      && resources.log == f.state.appendingPathComponent("resources.log"), "log files live in the state directory")
            try check(queue.environment["PATH"] == "/usr/bin:/bin:/usr/sbin:/sbin" && queue.environment["FOO"] == nil
                      && !queue.environment.keys.contains { $0.hasPrefix("AGENT_") } && queue.environment["HOME"] == f.root.path,
                      "launch environment is scrubbed")
            supervisor.start(.remote)
            _ = await eventually { spawner.count(.remote) == 1 }
            let remote = spawner.launches(.remote)[0].launch
            try check(remote.executable == f.file("agent-remote") && remote.name.executable == "agent-remote" && remote.arguments == ["serve"]
                      && remote.log == f.state.appendingPathComponent("remote/service.log"), "remote runs agent-remote serve on demand")
            try check(supervisor.state(.queue).phase == .running(pid: 70_001), "state exposes the running pid")
            await supervisor.stopAll(grace: .seconds(1))
        }

        // Restart policy.
        try await scenario { f in
            let spawner = FakeSpawner()
            let supervisor = f.fake(spawner)
            var changes = 0
            supervisor.onChange = { changes += 1 }
            supervisor.start()
            _ = await eventually { spawner.launches.count == 2 }
            spawner.finish(.queue, .code(0))
            _ = await eventually { supervisor.state(.queue).lastExit == .code(0) }
            await sleep(0.8)
            try check(spawner.count(.queue) == 1 && supervisor.state(.queue).phase == .stopped, "queue exit 0 is an intentional stop")
            try check(changes > 0, "state changes are announced")
            supervisor.start()
            _ = await eventually { spawner.count(.queue) == 2 }
            try check(spawner.count(.queue) == 2 && spawner.count(.resources) == 1, "start brings an intentionally stopped queue back")

            spawner.finish(.queue, .code(1))
            await sleep(0.1)
            try check(spawner.count(.queue) == 2 && supervisor.state(.queue).phase == .waiting, "non-zero exit waits out the throttle")
            _ = await eventually { spawner.count(.queue) == 3 }
            try check(spawner.count(.queue) == 3, "queue restarts after a non-zero exit")
            spawner.finish(.queue, .signal(SIGSEGV))
            _ = await eventually { spawner.count(.queue) == 4 }
            try check(spawner.count(.queue) == 4 && supervisor.state(.queue).lastExit == .signal(SIGSEGV), "queue restarts after signal death")

            spawner.finish(.resources, .code(0))
            _ = await eventually { spawner.count(.resources) == 2 }
            try check(spawner.count(.resources) == 2, "resources restarts even after exit 0")
            spawner.finish(.resources, .code(7))
            _ = await eventually { spawner.count(.resources) == 3 }
            try check(spawner.count(.resources) == 3, "resources restarts after failure")
            await supervisor.stopAll(grace: .seconds(1))
        }

        // Throttle: measured from the start, not the exit.
        try await scenario(throttle: .milliseconds(500)) { f in
            let spawner = FakeSpawner()
            let supervisor = f.fake(spawner)
            supervisor.start(.remote)
            _ = await eventually { spawner.count(.remote) == 1 }
            spawner.finish(.remote, .code(1))
            _ = await eventually { spawner.count(.remote) == 2 }
            let starts = spawner.launches(.remote).map(\.at)
            try check(seconds(starts[0].duration(to: starts[1])) >= 0.49, "fast failures are spaced by the throttle interval")
            await sleep(0.6)
            let exited = ContinuousClock.now
            spawner.finish(.remote, .code(1))
            _ = await eventually { spawner.count(.remote) == 3 }
            try check(seconds(exited.duration(to: spawner.launches(.remote)[2].at)) < 0.3, "a child that outlived the throttle restarts at once")
            await supervisor.stopAll(grace: .seconds(1))
        }

        // Spawn failures are retried after the throttle.
        try await scenario { f in
            let spawner = FakeSpawner()
            spawner.failNext()
            let supervisor = f.fake(spawner)
            supervisor.start(.resources)
            _ = await eventually { supervisor.state(.resources).lastExit == .launchFailed(ENOENT) }
            try check(supervisor.state(.resources).lastExit == .launchFailed(ENOENT) && spawner.count(.resources) == 0, "spawn failure is recorded")
            _ = await eventually { spawner.count(.resources) == 1 }
            try check(spawner.count(.resources) == 1, "spawn failure is retried")
            await supervisor.stopAll(grace: .seconds(1))
        }

        // Remote: on demand, stoppable, clean exit disables it.
        try await scenario { f in
            let spawner = FakeSpawner()
            let supervisor = f.fake(spawner)
            supervisor.start()
            await sleep(0.4)
            try check(spawner.count(.remote) == 0 && supervisor.state(.remote).phase == .stopped, "remote is not started automatically")
            supervisor.start(.remote)
            _ = await eventually { spawner.count(.remote) == 1 }
            spawner.finish(.remote, .code(0))
            await sleep(0.8)
            try check(spawner.count(.remote) == 1 && supervisor.state(.remote).phase == .stopped, "remote exit 0 means disabled; no restart")
            supervisor.start(.remote)
            _ = await eventually { spawner.count(.remote) == 2 }
            try check(spawner.count(.remote) == 2, "start(.remote) enables it again")
            spawner.finish(.remote, .code(2))
            _ = await eventually { spawner.count(.remote) == 3 }
            try check(spawner.count(.remote) == 3, "remote restarts after a failure")
            await supervisor.stop(.remote)
            try check(spawner.launches(.remote)[2].signals == [SIGTERM] && supervisor.state(.remote).phase == .stopped, "stop(.remote) sends SIGTERM")
            await sleep(0.8)
            try check(spawner.count(.remote) == 3 && spawner.count(.queue) == 1, "stopped remote is not restarted and others are unaffected")
            supervisor.start(.remote)
            _ = await eventually { spawner.count(.remote) == 4 }
            try check(spawner.count(.remote) == 4, "remote can be started after stop")
            await supervisor.stopAll(grace: .seconds(1))
        }

        // Adopt-if-running.
        try await scenario { f in
            let spawner = FakeSpawner()
            let serving = Flag(true)
            let supervisor = f.fake(spawner, probe: serving.probe)
            supervisor.start()
            _ = await eventually { spawner.count(.resources) == 1 && supervisor.state(.queue).phase == .adopted }
            try check(spawner.count(.queue) == 0 && supervisor.state(.queue).phase == .adopted, "a serving daemon is adopted, not duplicated")
            try check(spawner.count(.resources) == 1, "the scheduler needs no probe")
            await sleep(1.0)
            try check(serving.calls >= 3 && spawner.count(.queue) == 0, "adoption is re-probed every throttle interval")
            serving.value = false
            _ = await eventually { spawner.count(.queue) == 1 }
            try check(spawner.count(.queue) == 1 && supervisor.state(.queue).phase == .running(pid: 70_000 + Int32(spawner.launches.count)),
                      "the daemon is spawned once the probe fails")
            serving.value = true
            spawner.finish(.queue, .code(1))
            _ = await eventually { supervisor.state(.queue).phase == .adopted }
            try check(spawner.count(.queue) == 1, "a crashed child whose replacement already serves is adopted")
            try check(await supervisor.waitUntilQueueReachable(timeout: .seconds(1)), "reachable when the probe succeeds")
            serving.value = false
            let begin = ContinuousClock.now
            try check(!(await supervisor.waitUntilQueueReachable(timeout: .milliseconds(500))) && seconds(begin.duration(to: .now)) >= 0.5,
                      "unreachable after the timeout")
            await supervisor.stopAll(grace: .seconds(1))
        }

        // stopAll.
        try await scenario { f in
            let spawner = FakeSpawner()
            let supervisor = f.fake(spawner)
            supervisor.start()
            supervisor.start(.remote)
            _ = await eventually { spawner.launches.count == 3 }
            await supervisor.stopAll()
            try check(spawner.launches.allSatisfy { $0.signals == [SIGTERM] }, "stopAll sends SIGTERM only when children exit")
            try check(Helper.Name.allCases.allSatisfy { supervisor.state($0).phase == .stopped }, "everything is stopped")
            await sleep(0.8)
            supervisor.start()
            supervisor.start(.remote)
            await sleep(0.5)
            try check(spawner.launches.count == 3, "nothing starts after stopAll")
            await supervisor.stopAll()
            try check(spawner.launches.allSatisfy { $0.signals == [SIGTERM] }, "stopAll is idempotent")
        }
        try await scenario { f in
            let spawner = FakeSpawner()
            let supervisor = f.fake(spawner)
            supervisor.start()
            _ = await eventually { spawner.launches.count == 2 }
            spawner.finish(.queue, .code(1))
            _ = await eventually { supervisor.state(.queue).phase == .waiting }
            await supervisor.stopAll()
            await sleep(0.8)
            try check(spawner.count(.queue) == 1, "a pending restart is cancelled by stopAll")
        }
        try await scenario { f in
            let spawner = FakeSpawner()
            spawner.ignoreTerm(true)
            let supervisor = f.fake(spawner)
            supervisor.start()
            _ = await eventually { spawner.launches.count == 2 }
            let begin = ContinuousClock.now
            await supervisor.stopAll(grace: .milliseconds(500))
            try check(seconds(begin.duration(to: .now)) >= 0.5 && spawner.launches.allSatisfy { $0.signals == [SIGTERM, SIGKILL] },
                      "survivors get SIGKILL after the grace period")
            await sleep(0.8)
            try check(spawner.launches.count == 2, "killed children are not restarted")
        }

        // Real posix_spawn path with fake helper scripts.
        try await scenario { f in
            let leaked = open("/dev/null", O_RDONLY)
            defer { close(leaked) }
            try String(leaked).write(to: f.file("leak"), atomically: true, encoding: .utf8)
            let supervisor = f.real()
            supervisor.start()
            _ = await eventually { f.runs("daemon").count == 1 && f.runs("scheduler").count == 1 && !text(f.file("daemon.fd")).isEmpty }
            guard case .running(let pid) = supervisor.state(.queue).phase else { throw TestFailure(message: "queue is running") }
            try check(Int32(f.runs("daemon")[0]) == pid, "state reports the real child pid")
            try check(text(f.file("daemon.args")).hasSuffix("agent-heavy daemon\n") && text(f.file("scheduler.args")).hasSuffix("agent-heavy scheduler\n"),
                      "helpers receive their subcommand")
            let environment = Set(text(f.file("daemon.env")).split(separator: "\n").map(String.init))
            try check(!environment.contains { $0.hasPrefix("AGENT_") } && !environment.contains("FOO=bar") && environment.contains("PATH=/usr/bin:/bin:/usr/sbin:/sbin"),
                      "real child has no AGENT_ variables and a fixed PATH")
            try check(environment.contains("HOME=\(f.root.path)") && environment.contains("USER=tester") && environment.contains("LOGNAME=tester")
                      && environment.contains("TMPDIR=/tmp/") && environment.contains("LANG=C"), "real child keeps the launchd-style variables")
            try check(getsid(pid) != getsid(0) && getsid(pid) == pid && getpgid(pid) == pid, "child runs in its own session")
            try check(text(f.file("daemon.fd")) == "clean\n", "inherited descriptors are closed")
            try check(mode(f.state) == 0o700 && mode(f.state.appendingPathComponent("daemon.log")) == 0o600
                      && mode(f.state.appendingPathComponent("resources.log")) == 0o600, "state directory 0700 and logs 0600")
            try check(text(f.state.appendingPathComponent("daemon.log")) == "hello from daemon\n"
                      && text(f.state.appendingPathComponent("resources.log")) == "hello from scheduler\n", "stdout and stderr go to the log")
            supervisor.start(.remote)
            _ = await eventually { f.runs("serve").count == 1 }
            try check(mode(f.state.appendingPathComponent("remote/service.log")) == 0o600 && mode(f.state.appendingPathComponent("remote")) == 0o700,
                      "remote log lives in its own private directory")
            await supervisor.stopAll(grace: .seconds(2))
            try check(!alive(pid), "stopAll ends the real child")
        }
        try await scenario { f in
            f.setMode("daemon", "exit0")
            f.setMode("scheduler", "exit0")
            f.setMode("serve", "exit3")
            let supervisor = f.real()
            supervisor.start()
            supervisor.start(.remote)
            _ = await eventually { f.runs("scheduler").count >= 2 && f.runs("serve").count >= 2 }
            try check(f.runs("daemon").count == 1 && supervisor.state(.queue).lastExit == .code(0), "real queue exit 0 is not restarted")
            try check(f.runs("scheduler").count >= 2, "real scheduler restarts after exit 0")
            try check(supervisor.state(.remote).lastExit == .code(3) && f.runs("serve").count >= 2, "real remote restarts after exit 3")
            f.setMode("serve", "exit0")
            let before = f.runs("serve").count
            _ = await eventually { supervisor.state(.remote).lastExit == .code(0) }
            await sleep(0.9)
            try check(supervisor.state(.remote).phase == .stopped && f.runs("serve").count <= before + 1, "real remote exit 0 is not restarted")
            f.setMode("daemon", "selfkill")
            supervisor.start(.queue)
            _ = await eventually { f.runs("daemon").count >= 3 }
            try check(supervisor.state(.queue).lastExit == .signal(SIGKILL) && f.runs("daemon").count >= 3, "real signal death restarts the queue")
            await supervisor.stopAll(grace: .seconds(2))
        }
        try await scenario { f in
            f.setMode("daemon", "ignoreterm")
            let supervisor = f.real()
            supervisor.start()
            _ = await eventually { !text(f.file("daemon.ready")).isEmpty && f.runs("scheduler").count == 1 }
            guard case .running(let stubborn) = supervisor.state(.queue).phase, case .running(let polite) = supervisor.state(.resources).phase else {
                throw TestFailure(message: "children are running")
            }
            let begin = ContinuousClock.now
            await supervisor.stopAll(grace: .milliseconds(600))
            try check(seconds(begin.duration(to: .now)) >= 0.6 && !alive(stubborn) && !alive(polite), "real stopAll escalates to SIGKILL")
            try check(supervisor.state(.queue).lastExit == .signal(SIGKILL) && supervisor.state(.resources).lastExit == .signal(SIGTERM),
                      "cooperative child gets SIGTERM, stubborn child SIGKILL")
            await sleep(0.8)
            try check(f.runs("daemon").count == 1 && f.runs("scheduler").count == 1, "real children are not restarted after stopAll")
            await supervisor.stopAll()
            try check(true, "stopAll twice is harmless")
        }
        try await scenario { f in
            // A group signal to the app must not reach the helpers: ours sit in their own session.
            let supervisor = f.real()
            supervisor.start(.resources)
            _ = await eventually { f.runs("scheduler").count == 1 }
            guard case .running(let pid) = supervisor.state(.resources).phase else { throw TestFailure(message: "scheduler is running") }
            try check(getpgid(pid) != getpgid(0), "child is outside the app's process group")
            await supervisor.stopAll(grace: .seconds(2))
        }

        // Logs.
        try await scenario { f in
            try FileManager.default.createDirectory(at: f.state, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let big = f.state.appendingPathComponent("daemon.log")
            try Data(repeating: 0x61, count: 2 << 20).write(to: big)
            chmod(big.path, 0o644)
            let small = f.state.appendingPathComponent("resources.log")
            try "older output\n".write(to: small, atomically: true, encoding: .utf8)
            let supervisor = f.real()
            supervisor.start()
            _ = await eventually { f.runs("daemon").count == 1 && f.runs("scheduler").count == 1 && size(big) > 0 }
            await sleep(0.2)
            try check(size(big) < 1 << 20 && text(big).hasSuffix("hello from daemon\n") && mode(big) == 0o600, "an oversized log is truncated and made private")
            try check(text(small) == "older output\nhello from scheduler\n", "a small log is appended to")
            await supervisor.stopAll(grace: .seconds(2))
        }
        try await scenario { f in
            try FileManager.default.createDirectory(at: f.state, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let target = f.root.appendingPathComponent("victim")
            try "keep".write(to: target, atomically: true, encoding: .utf8)
            try FileManager.default.createSymbolicLink(at: f.state.appendingPathComponent("daemon.log"), withDestinationURL: target)
            let supervisor = f.real()
            supervisor.start()
            _ = await eventually { f.runs("daemon").count == 1 && f.runs("scheduler").count == 1 }
            try check(f.runs("daemon").count == 1 && text(target) == "keep", "a symlinked log is never followed, and the start still happens")
            await supervisor.stopAll(grace: .seconds(2))
        }
        try await scenario { f in
            try FileManager.default.createDirectory(at: f.state, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            chmod(f.state.path, 0o777)
            let supervisor = f.real()
            supervisor.start()
            _ = await eventually { f.runs("daemon").count == 1 && f.runs("scheduler").count == 1 }
            try check(f.runs("daemon").count == 1 && !FileManager.default.fileExists(atPath: f.state.appendingPathComponent("daemon.log").path),
                      "an unsafe log directory falls back to /dev/null without failing the start")
            await supervisor.stopAll(grace: .seconds(2))
        }
        try await scenario { f in
            let supervisor = Supervisor(app: f.app, stateDirectory: URL(fileURLWithPath: "/dev/null/nowhere"), environment: f.environment,
                                        throttle: f.throttle, probe: { false })
            supervisor.start(.resources)
            _ = await eventually { f.runs("scheduler").count == 1 }
            try check(f.runs("scheduler").count == 1, "an unavailable log directory does not fail the start")
            await supervisor.stopAll(grace: .seconds(2))
        }

        // Real probe and adoption.
        try await scenario { f in
            let probe = Helper.statusProbe(app: f.app, environment: f.environment, timeout: .milliseconds(600))
            try String("0").write(to: f.file("status.code"), atomically: true, encoding: .utf8)
            try check(await probe(), "status exit 0 means a daemon is serving")
            try String("1").write(to: f.file("status.code"), atomically: true, encoding: .utf8)
            try check(!(await probe()), "status exit 1 means none is")
            try String("hang").write(to: f.file("status.code"), atomically: true, encoding: .utf8)
            let begin = ContinuousClock.now
            try check(!(await probe()) && seconds(begin.duration(to: .now)) < 3, "a hanging status call times out as unreachable")
            let missing = Helper.statusProbe(app: f.root.appendingPathComponent("missing.app"), environment: f.environment)
            try check(!(await missing()), "a missing helper is unreachable")

            let supervisor = f.real(probe: Helper.statusProbe(app: f.app, environment: f.environment, timeout: .seconds(2)))
            try String("0").write(to: f.file("status.code"), atomically: true, encoding: .utf8)
            supervisor.start()
            _ = await eventually { supervisor.state(.queue).phase == .adopted && f.runs("scheduler").count == 1 }
            await sleep(0.4)
            try check(f.runs("daemon").isEmpty && supervisor.state(.queue).phase == .adopted, "real adoption never starts a second daemon")
            try String("1").write(to: f.file("status.code"), atomically: true, encoding: .utf8)
            _ = await eventually { f.runs("daemon").count == 1 }
            try check(f.runs("daemon").count == 1, "the daemon starts once the real probe fails")
            await supervisor.stopAll(grace: .seconds(2))

            let waiter = f.real(probe: Helper.statusProbe(app: f.app, environment: f.environment, timeout: .seconds(2)))
            try String("1").write(to: f.file("status.code"), atomically: true, encoding: .utf8)
            Task {
                await sleep(0.5)
                try? String("0").write(to: f.file("status.code"), atomically: true, encoding: .utf8)
            }
            try check(await waiter.waitUntilQueueReachable(timeout: .seconds(5)), "waiting succeeds as soon as the queue answers")
        }

        print("PASS \(passed) supervisor checks; no real helper or user state touched")
    }
}
