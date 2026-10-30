import Darwin
import Foundation

/// Process-level plumbing for the helpers the menu bar app supervises. Everything
/// here is nonisolated and free of app state so it can run off the main actor.
enum QueueHelper {
    enum Name: String, CaseIterable, Sendable {
        case queue, resources, remote

        var executable: String { self == .remote ? "agent-remote" : "agent-heavy" }
        var arguments: [String] {
            switch self {
            case .queue: ["daemon"]
            case .resources: ["scheduler"]
            case .remote: ["serve"]
            }
        }
        var logPath: String {
            switch self {
            case .queue: "daemon.log"
            case .resources: "resources.log"
            case .remote: "remote/service.log"
            }
        }
    }

    enum Exit: Equatable, Sendable {
        case code(Int32)
        case signal(Int32)
        case launchFailed(Int32)

        var isSuccess: Bool { self == .code(0) }

        init(waitStatus status: Int32) {
            let signal = status & 0x7f
            if signal == 0 { self = .code((status >> 8) & 0xff) }
            else if signal != 0x7f { self = .signal(signal) }
            else { self = .code(-1) }
        }
    }

    struct Launch: Sendable {
        let name: Name
        let executable: URL
        let arguments: [String]
        let environment: [String: String]
        let log: URL?
        /// Read end of the app's lifeline pipe, handed to the child as descriptor 3. Nil for short-lived helpers.
        var lifeline: Int32?
    }

    /// The child's end of the lifeline is always this descriptor, announced by `lifelineVariable`.
    static let lifelineDescriptor: Int32 = 3
    static let lifelineVariable = "AGENT_QUEUE_LIFELINE"

    /// A pipe the app keeps the write end of for its whole life. Supervised helpers receive the
    /// read end and exit when it reaches end-of-file, which happens when the app exits for any
    /// reason, including a crash or force quit, because the kernel then closes the write end.
    /// Both ends are close-on-exec and above descriptor 9, so nothing but an explicit spawn
    /// file action ever hands them to another process.
    final class Lifeline: @unchecked Sendable {
        private let lock = NSLock()
        private var read: Int32
        private var write: Int32

        private init(read: Int32, write: Int32) {
            self.read = read
            self.write = write
        }

        static func make() -> Lifeline? {
            var ends: [Int32] = [-1, -1]
            guard pipe(&ends) == 0 else { return nil }
            defer { ends.forEach { close($0) } }
            let read = fcntl(ends[0], F_DUPFD_CLOEXEC, 10)
            guard read >= 0 else { return nil }
            let write = fcntl(ends[1], F_DUPFD_CLOEXEC, 10)
            guard write >= 0 else {
                close(read)
                return nil
            }
            return Lifeline(read: read, write: write)
        }

        /// The descriptor to duplicate into a child, or nil once released.
        var readEnd: Int32? { lock.withLock { read >= 0 ? read : nil } }
        var isReleased: Bool { lock.withLock { write < 0 } }

        /// Closes the write end so every child sees end-of-file. Safe to call repeatedly.
        func release() {
            lock.withLock {
                if write >= 0 { close(write) }
                if read >= 0 { close(read) }
                write = -1
                read = -1
            }
        }

        deinit { release() }
    }

    /// `signal` only ever reaches a process that has not been reaped, so a recycled
    /// pid is never signalled.
    struct Child: Sendable {
        let pid: Int32
        let signal: @Sendable (Int32) -> Void
    }

    /// Starts a helper and calls `onExit` exactly once from any thread.
    typealias Spawner = @Sendable (Launch, @escaping @Sendable (Exit) -> Void) throws -> Child
    typealias Probe = @Sendable () async -> Bool

    static let logLimit: Int64 = 1 << 20
    static let path = "/usr/bin:/bin:/usr/sbin:/sbin"

    /// Mirrors a launchd user agent: an allowlist, never the app's own variables
    /// (which may carry `AGENT_HEAVY_*` or `AGENT_REMOTE_*` test and override settings).
    static func childEnvironment(from environment: [String: String]) -> [String: String] {
        var result = ["PATH": path]
        for key in ["HOME", "USER", "LOGNAME", "TMPDIR", "LANG"] {
            if let value = environment[key], !value.contains("\0") { result[key] = value }
        }
        return result
    }

    static var defaultStateDirectory: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".local/state/agent-heavy", isDirectory: true)
    }

    // MARK: Logs

    private static func privateDirectory(_ url: URL) -> Bool {
        let path = url.path
        if mkdir(path, 0o700) != 0 && errno == ENOENT {
            guard (try? FileManager.default.createDirectory(
                at: url, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil else { return false }
        }
        var info = stat()
        return lstat(path, &info) == 0 && (info.st_mode & S_IFMT) == S_IFDIR && info.st_uid == getuid() && info.st_mode & 0o022 == 0
    }

    /// Returns a descriptor above 2 that appends to the bounded private log, or nil
    /// when the directory or file is unsafe or unavailable.
    static func openLog(_ url: URL, limit: Int64 = logLimit) -> Int32? {
        guard privateDirectory(url.deletingLastPathComponent()) else { return nil }
        let descriptor = open(url.path, O_WRONLY | O_APPEND | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0o600)
        guard descriptor >= 0 else { return nil }
        var info = stat()
        guard fstat(descriptor, &info) == 0, (info.st_mode & S_IFMT) == S_IFREG, info.st_uid == getuid(), info.st_nlink == 1,
              info.st_mode & 0o077 == 0 || fchmod(descriptor, 0o600) == 0,
              info.st_size <= limit || ftruncate(descriptor, 0) == 0 else {
            close(descriptor)
            return nil
        }
        return descriptor
    }

    // MARK: Spawning

    private final class Reaper: @unchecked Sendable {
        private let lock = NSLock()
        private var reaped = false
        let pid: Int32
        init(pid: Int32) { self.pid = pid }

        func send(_ signal: Int32) {
            lock.lock()
            defer { lock.unlock() }
            if !reaped { kill(pid, signal) }
        }

        func reap() -> Exit {
            lock.lock()
            defer { lock.unlock() }
            var status: Int32 = 0
            var result: pid_t
            repeat { result = waitpid(pid, &status, 0) } while result < 0 && errno == EINTR
            reaped = true
            return result == pid ? Exit(waitStatus: status) : .code(-1)
        }
    }

    private final class SourceBox: @unchecked Sendable {
        var source: (any DispatchSourceProcess)?
    }

    private static func fdActions(_ actions: inout posix_spawn_file_actions_t?, output: Int32) -> Int32 {
        var result = posix_spawn_file_actions_addopen(&actions, 0, "/dev/null", O_RDONLY, 0)
        if result == 0 { result = posix_spawn_file_actions_adddup2(&actions, output, 1) }
        if result == 0 { result = posix_spawn_file_actions_adddup2(&actions, output, 2) }
        return result
    }

    /// Own session so group signals sent to the app never reach the helper; every
    /// descriptor except 0-2 (and the lifeline, as 3, when given) is closed in the child. Each child is reaped by pid
    /// only, never with waitpid(-1), so other children in the app are untouched.
    static func spawn(_ launch: Launch, onExit: @escaping @Sendable (Exit) -> Void) throws -> Child {
        var output = launch.log.flatMap { openLog($0) } ?? -1
        if output < 0 { output = open("/dev/null", O_RDWR | O_CLOEXEC) }
        guard output >= 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
        if output <= 2 {
            let moved = fcntl(output, F_DUPFD_CLOEXEC, 3)
            close(output)
            guard moved >= 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EMFILE) }
            output = moved
        }
        defer { close(output) }

        var attributes: posix_spawnattr_t?
        var actions: posix_spawn_file_actions_t?
        guard posix_spawnattr_init(&attributes) == 0 else { throw POSIXError(.ENOMEM) }
        defer { posix_spawnattr_destroy(&attributes) }
        guard posix_spawn_file_actions_init(&actions) == 0 else { throw POSIXError(.ENOMEM) }
        defer { posix_spawn_file_actions_destroy(&actions) }
        var everything = sigset_t(), none = sigset_t()
        sigfillset(&everything)
        sigemptyset(&none)
        var result = posix_spawnattr_setflags(&attributes, Int16(POSIX_SPAWN_SETSID | POSIX_SPAWN_CLOEXEC_DEFAULT
                                                                | POSIX_SPAWN_SETSIGMASK | POSIX_SPAWN_SETSIGDEF))
        if result == 0 { result = posix_spawnattr_setsigmask(&attributes, &none) }
        if result == 0 { result = posix_spawnattr_setsigdefault(&attributes, &everything) }
        if result == 0 { result = fdActions(&actions, output: output) }
        // Last, so a log descriptor that happens to be 3 is already duplicated onto 1 and 2.
        if result == 0, let lifeline = launch.lifeline {
            result = lifeline == lifelineDescriptor ? EINVAL
                : posix_spawn_file_actions_adddup2(&actions, lifeline, lifelineDescriptor)
        }
        guard result == 0 else { throw POSIXError(POSIXErrorCode(rawValue: result) ?? .EINVAL) }

        let arguments = [launch.name.executable] + launch.arguments
        let environment = launch.environment.map { "\($0.key)=\($0.value)" }.sorted()
        let argv: [UnsafeMutablePointer<CChar>?] = arguments.map { strdup($0) } + [nil]
        let envp: [UnsafeMutablePointer<CChar>?] = environment.map { strdup($0) } + [nil]
        defer {
            argv.forEach { free($0) }
            envp.forEach { free($0) }
        }
        var pid: pid_t = 0
        result = posix_spawn(&pid, launch.executable.path, &actions, &attributes, argv, envp)
        guard result == 0 else { throw POSIXError(POSIXErrorCode(rawValue: result) ?? .EIO) }

        let reaper = Reaper(pid: pid)
        let box = SourceBox()
        // The exit source only reports; the single waitpid below reaps this pid. A
        // child that already died still reports because it remains our zombie.
        let source = DispatchSource.makeProcessSource(identifier: pid, eventMask: .exit, queue: .global(qos: .utility))
        box.source = source
        source.setEventHandler {
            let exit = reaper.reap()
            box.source?.cancel()
            box.source = nil
            onExit(exit)
        }
        source.resume()
        return Child(pid: pid, signal: { reaper.send($0) })
    }

    /// Runs a short-lived helper with output discarded; nil if it could not run or timed out.
    static func run(_ executable: URL, arguments: [String], environment: [String: String], timeout: Duration,
                    spawner: Spawner = spawn) async -> Exit? {
        let launch = Launch(name: .queue, executable: executable, arguments: arguments, environment: environment, log: nil)
        return await withCheckedContinuation { (continuation: CheckedContinuation<Exit?, Never>) in
            let once = Once(continuation)
            guard let child = try? spawner(launch, { _ = once.finish($0) }) else {
                once.finish(nil)
                return
            }
            Task.detached {
                try? await Task.sleep(for: timeout)
                if once.finish(nil) { child.signal(SIGKILL) }
            }
        }
    }

    private final class Once: @unchecked Sendable {
        private let lock = NSLock()
        private var continuation: CheckedContinuation<Exit?, Never>?
        init(_ continuation: CheckedContinuation<Exit?, Never>) { self.continuation = continuation }

        @discardableResult func finish(_ value: Exit?) -> Bool {
            lock.lock()
            let pending = continuation
            continuation = nil
            lock.unlock()
            pending?.resume(returning: value)
            return pending != nil
        }
    }

    /// Succeeds when `agent-heavy status --json` exits 0, meaning some daemon is serving.
    static func statusProbe(app: URL, environment: [String: String], timeout: Duration = .seconds(5)) -> Probe {
        let executable = app.appendingPathComponent("Contents/MacOS/agent-heavy")
        let environment = childEnvironment(from: environment)
        return { await run(executable, arguments: ["status", "--json"], environment: environment, timeout: timeout) == .code(0) }
    }
}

/// Starts and supervises the queue daemon, resource scheduler and optional remote
/// service as children of the menu bar app, replacing their launchd agents. They
/// stop with the app, including when it crashes or is force quit, through the
/// lifeline pipe. Restart rules mirror the retired launchd plists, except that the
/// queue daemon also restarts after a clean exit while the app runs.
@MainActor
final class QueueHelperSupervisor {
    typealias Name = QueueHelper.Name
    typealias Exit = QueueHelper.Exit

    enum Phase: Equatable, Sendable {
        case stopped
        /// Waiting out the restart throttle, or probing for an already running daemon.
        case waiting
        /// Another daemon instance is already serving; none was started.
        case adopted
        case running(pid: Int32)
    }

    struct State: Equatable, Sendable {
        var phase: Phase = .stopped
        var lastExit: Exit?
    }

    @MainActor private final class Slot {
        let name: Name
        var state = State()
        var wanted = false
        var stopRequested = false
        var child: QueueHelper.Child?
        var generation = 0
        var lastStart: ContinuousClock.Instant?
        var pending: Task<Void, Never>?
        init(_ name: Name) { self.name = name }
    }

    private let app: URL
    private let stateDirectory: URL
    private let environment: [String: String]
    private let throttle: Duration
    private let probe: QueueHelper.Probe
    private let spawner: QueueHelper.Spawner
    private let slots: [Name: Slot]
    private var isShutDown = false
    private let lifeline: QueueHelper.Lifeline?
    private let settingsDirectory: URL
    private let settingsDebounce: Duration
    private var settingsSource: (any DispatchSourceFileSystemObject)?
    private var settingsWatchesTarget = false
    private var settingsDebouncing: Task<Void, Never>?
    private var isWatchingSettings = false
    /// Called after any process state change.
    var onChange: (@MainActor () -> Void)?

    init(app: URL, stateDirectory: URL = QueueHelper.defaultStateDirectory,
         environment: [String: String] = ProcessInfo.processInfo.environment, throttle: Duration = .seconds(10),
         probe: QueueHelper.Probe? = nil, settingsDirectory: URL? = nil, settingsDebounce: Duration = .seconds(1),
         spawner: @escaping QueueHelper.Spawner = QueueHelper.spawn) {
        let scrubbed = QueueHelper.childEnvironment(from: environment)
        let home = scrubbed["HOME"] ?? FileManager.default.homeDirectoryForCurrentUser.path
        self.settingsDirectory = settingsDirectory
            ?? URL(fileURLWithPath: home, isDirectory: true).appendingPathComponent(".config/agent-heavy", isDirectory: true)
        self.settingsDebounce = settingsDebounce
        lifeline = QueueHelper.Lifeline.make()
        self.app = app
        self.stateDirectory = stateDirectory
        self.environment = scrubbed
        self.throttle = throttle
        self.probe = probe ?? QueueHelper.statusProbe(app: app, environment: scrubbed)
        self.spawner = spawner
        slots = Dictionary(uniqueKeysWithValues: Name.allCases.map { ($0, Slot($0)) })
    }

    func state(_ name: Name) -> State { slots[name]!.state }

    /// Starts the queue daemon and resource scheduler; the remote service is on demand.
    func start() {
        start(.queue)
        start(.resources)
    }

    /// What the app runs after setup: everything, and a watch on the phone access settings.
    /// `agent-remote serve` exits 0 at once when phone access is off, which the restart
    /// policy treats as "not wanted", so starting it is always safe.
    func startAll() {
        start()
        start(.remote)
        watchRemoteSettings()
    }

    func start(_ name: Name) {
        guard !isShutDown else { return }
        let slot = slots[name]!
        slot.wanted = true
        schedule(slot)
    }

    func stop(_ name: Name) async {
        let slot = slots[name]!
        slot.wanted = false
        slot.stopRequested = true
        cancelPending(slot)
        await terminate([slot], grace: .seconds(8))
        slot.stopRequested = false
        if slot.child == nil { setPhase(slot, .stopped) }
        if slot.wanted { schedule(slot) }
    }

    /// SIGTERM every child, SIGKILL survivors after `grace`, and never restart anything.
    /// Adopted daemons are not ours and are left running.
    func stopAll(grace: Duration = .seconds(8)) async {
        isShutDown = true
        stopWatchingSettings()
        let all = Array(slots.values)
        for slot in all {
            slot.wanted = false
            slot.stopRequested = true
            cancelPending(slot)
        }
        await terminate(all, grace: grace, releasingLifeline: true)
        for slot in all where slot.child == nil { setPhase(slot, .stopped) }
    }

    /// Synchronous last resort for the moment the app exits: SIGTERM whatever is still
    /// alive without waiting, and never restart anything afterwards.
    func terminateRemaining() {
        isShutDown = true
        stopWatchingSettings()
        for slot in slots.values {
            slot.wanted = false
            slot.stopRequested = true
            cancelPending(slot)
            slot.child?.signal(SIGTERM)
        }
        lifeline?.release()
    }

    /// True once some daemon answers the probe, so the menu need not flash an error at launch.
    func waitUntilQueueReachable(timeout: Duration = .seconds(10)) async -> Bool {
        let deadline = ContinuousClock.now + timeout
        while true {
            if await probe() { return true }
            guard ContinuousClock.now < deadline, !Task.isCancelled else { return false }
            try? await Task.sleep(for: .milliseconds(250))
        }
    }

    // MARK: Lifecycle

    private func setPhase(_ slot: Slot, _ phase: Phase) {
        guard slot.state.phase != phase else { return }
        slot.state.phase = phase
        onChange?()
    }

    private func cancelPending(_ slot: Slot) {
        slot.pending?.cancel()
        slot.pending = nil
    }

    private func schedule(_ slot: Slot, after delay: Duration? = nil) {
        guard slot.wanted, !isShutDown, !slot.stopRequested, slot.child == nil, slot.pending == nil else { return }
        let wait = delay ?? slot.lastStart.map { throttle - ($0.duration(to: .now)) } ?? .zero
        if slot.name == .queue || wait > .zero { setPhase(slot, .waiting) }
        slot.pending = Task { [weak self] in
            if wait > .zero { try? await Task.sleep(for: wait) }
            guard !Task.isCancelled, let self else { return }
            await self.attempt(slot)
        }
    }

    private func attempt(_ slot: Slot) async {
        // An existing daemon, such as an orphan of a crashed app, is adopted rather than raced.
        let adopted = slot.name == .queue ? await probe() : false
        guard !Task.isCancelled else { return }
        slot.pending = nil
        guard slot.wanted, !isShutDown, slot.child == nil else { return }
        if adopted {
            schedule(slot, after: throttle)
            setPhase(slot, .adopted)
        } else {
            launch(slot)
        }
    }

    private func launch(_ slot: Slot) {
        slot.lastStart = .now
        slot.generation += 1
        let generation = slot.generation
        let launch = QueueHelper.Launch(
            name: slot.name, executable: app.appendingPathComponent("Contents/MacOS/\(slot.name.executable)"),
            arguments: slot.name.arguments, environment: helperEnvironment(lifeline: lifeline?.readEnd),
            log: stateDirectory.appendingPathComponent(slot.name.logPath), lifeline: lifeline?.readEnd)
        do {
            let child = try spawner(launch) { [weak self] exit in
                Task { @MainActor in self?.exited(slot, generation: generation, exit) }
            }
            slot.child = child
            setPhase(slot, .running(pid: child.pid))
        } catch {
            let code = Int32((error as? POSIXError)?.errorCode ?? Int(EIO))
            finish(slot, .launchFailed(code))
        }
    }

    private func helperEnvironment(lifeline: Int32?) -> [String: String] {
        guard lifeline != nil else { return environment }
        return environment.merging([QueueHelper.lifelineVariable: String(QueueHelper.lifelineDescriptor)]) { _, new in new }
    }

    private func exited(_ slot: Slot, generation: Int, _ exit: Exit) {
        guard slot.generation == generation, slot.child != nil else { return }
        slot.child = nil
        finish(slot, exit)
    }

    private func finish(_ slot: Slot, _ exit: Exit) {
        slot.state.lastExit = exit
        guard slot.wanted, !isShutDown, !slot.stopRequested else {
            setPhase(slot, .stopped)
            onChange?()
            return
        }
        // A clean exit from the phone service means phone access is off. The queue and the
        // scheduler restart after any exit while the app runs, even an external SIGTERM.
        if slot.name == .remote && exit.isSuccess {
            slot.wanted = false
            setPhase(slot, .stopped)
            onChange?()
        } else {
            setPhase(slot, .waiting)
            onChange?()
            schedule(slot)
        }
    }

    private func terminate(_ targets: [Slot], grace: Duration, releasingLifeline: Bool = false) async {
        for slot in targets { slot.child?.signal(SIGTERM) }
        // Belt and braces: children that miss the signal still exit on end-of-file.
        if releasingLifeline { lifeline?.release() }
        await waitUntilGone(targets, within: grace)
        for slot in targets { slot.child?.signal(SIGKILL) }
        await waitUntilGone(targets, within: .seconds(2))
    }

    private func waitUntilGone(_ targets: [Slot], within limit: Duration) async {
        let deadline = ContinuousClock.now + limit
        while targets.contains(where: { $0.child != nil }), ContinuousClock.now < deadline {
            try? await Task.sleep(for: .milliseconds(10))
        }
    }

    // MARK: Phone access settings

    private var settingsFile: URL { settingsDirectory.appendingPathComponent("remote.json") }

    /// `agent-remote enable` run in a terminal only writes remote.json, so watch its
    /// directory and start the service when it appears or changes. Creates nothing: when
    /// the directory is missing, the nearest existing ancestor is watched until it exists.
    func watchRemoteSettings() {
        guard !isShutDown, !isWatchingSettings else { return }
        isWatchingSettings = true
        armSettingsWatch()
    }

    private func stopWatchingSettings() {
        isWatchingSettings = false
        settingsDebouncing?.cancel()
        settingsDebouncing = nil
        settingsSource?.cancel()
        settingsSource = nil
    }

    /// Opens the settings directory, or its nearest existing ancestor, for events only.
    private func armSettingsWatch() {
        settingsSource?.cancel()
        settingsSource = nil
        var directory = settingsDirectory
        while true {
            let descriptor = open(directory.path, O_EVTONLY | O_DIRECTORY | O_CLOEXEC)
            if descriptor >= 0 {
                let source = DispatchSource.makeFileSystemObjectSource(
                    fileDescriptor: descriptor, eventMask: [.write, .delete, .rename, .revoke], queue: .main)
                source.setEventHandler { [weak self] in
                    Task { @MainActor in self?.settingsChanged() }
                }
                source.setCancelHandler { close(descriptor) }
                source.resume()
                settingsSource = source
                settingsWatchesTarget = directory.path == settingsDirectory.path
                return
            }
            let parent = directory.deletingLastPathComponent()
            guard parent.path != directory.path else { break }
            directory = parent
        }
        settingsWatchesTarget = false
    }

    private func settingsChanged() {
        guard isWatchingSettings, !isShutDown else { return }
        settingsDebouncing?.cancel()
        settingsDebouncing = Task { [weak self, settingsDebounce] in
            try? await Task.sleep(for: settingsDebounce)
            guard !Task.isCancelled, let self else { return }
            self.settingsSettled()
        }
    }

    private func settingsSettled() {
        settingsDebouncing = nil
        guard isWatchingSettings, !isShutDown else { return }
        // The directory may have been replaced or created since the source was opened.
        armSettingsWatch()
        guard settingsWatchesTarget, state(.remote).phase == .stopped, remoteSettingsEnabled() else { return }
        start(.remote)
    }

    /// Whether remote.json asks for phone access. Anything unreadable counts as off, since
    /// the service would only exit again at once.
    private func remoteSettingsEnabled() -> Bool {
        var info = stat()
        guard lstat(settingsFile.path, &info) == 0, (info.st_mode & S_IFMT) == S_IFREG, info.st_size <= 1 << 20,
              let data = try? Data(contentsOf: settingsFile),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return false }
        return object["enabled"] as? Bool == true
    }
}
