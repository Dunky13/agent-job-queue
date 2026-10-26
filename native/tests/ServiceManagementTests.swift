import Foundation
import ServiceManagement

private typealias Management = QueueServiceManagement
private typealias Replacement = QueueAppReplacement

private struct TestFailure: Error {
    let message: String
}

private final class Fixture {
    var status: Management.Status
    var afterRegister: Management.Status = .enabled
    var afterUnregister: Management.Status = .notRegistered
    var statusError: (any Error)?
    var registerError: (any Error)?
    var unregisterError: (any Error)?
    var events: [String] = []

    init(_ status: Management.Status) { self.status = status }

    var adapter: Management.Adapter {
        Management.Adapter(status: {
            self.events.append("status")
            if let error = self.statusError { throw error }
            return self.status
        }, register: {
            self.events.append("register")
            if let error = self.registerError { throw error }
            self.status = self.afterRegister
        }, unregister: {
            self.events.append("unregister")
            if let error = self.unregisterError { throw error }
            self.status = self.afterUnregister
        })
    }
}

@MainActor
private final class AppFixture {
    var metadata: Replacement.Metadata
    var terminated = false
    var acceptsTermination = true
    var terminationError: (any Error)?
    var observationError: (any Error)?
    var metadataError: (any Error)?
    var onMetadata: (() -> Void)?
    var terminationRequests = 0
    var finishImmediately = true

    init(path: URL?, identifier: String? = "io.went.agent-queue", pid: Int32 = 42, uid: UInt32? = 501) {
        metadata = .init(processID: pid, bundleURL: path, bundleIdentifier: identifier, userID: uid)
    }

    var application: Replacement.RunningApp {
        .init(metadata: {
            self.onMetadata?()
            if let error = self.metadataError { throw error }
            return self.metadata
        }, isTerminated: {
            if let error = self.observationError { throw error }
            return self.terminated
        }, terminate: {
            self.terminationRequests += 1
            if let error = self.terminationError { throw error }
            if self.acceptsTermination && self.finishImmediately { self.terminated = true }
            return self.acceptsTermination
        })
    }
}

@MainActor
private final class ReplacementFixture {
    var apps: [AppFixture] = []
    var time: TimeInterval = 0
    var advances: [TimeInterval] = []
    var enumerationCount = 0
    var enumerationError: (any Error)?
    var advanceError: (any Error)?
    var onAdvance: (() -> Void)?
    var onEnumeration: (() -> Void)?

    var adapter: Replacement.Adapter {
        .init(applications: {
            self.enumerationCount += 1
            self.onEnumeration?()
            if let error = self.enumerationError { throw error }
            return self.apps.map(\.application)
        }, now: { self.time }, advanceRunLoop: { interval in
            self.advances.append(interval)
            if let error = self.advanceError { throw error }
            self.time += interval
            self.onAdvance?()
        })
    }
}

@main
struct ServiceManagementTests {
    @MainActor static func main() throws {
        let temporary = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: temporary, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: temporary) }
        var passed = 0

        func check(_ condition: Bool, _ name: String) throws {
            guard condition else { throw TestFailure(message: name) }
            passed += 1
            print("PASS \(name)")
        }

        func failure(_ expected: Management.Failure, _ name: String,
                     operation: () throws -> Void) throws {
            do {
                try operation()
                throw TestFailure(message: "\(name): expected failure")
            } catch let error as Management.Failure {
                try check(error == expected, name)
            }
        }

        func propagated(_ expected: NSError, _ name: String, operation: () throws -> Void) throws {
            do {
                try operation()
                throw TestFailure(message: "\(name): expected framework error")
            } catch let error as NSError where error.domain == expected.domain && error.code == expected.code {
                try check(error === expected, name)
            }
        }

        func bundle(overrides: [String: Any] = [:], omitted: [String] = [],
                    extension suffix: String = "app", executable: Bool = true) throws -> Bundle {
            let url = temporary.appendingPathComponent(UUID().uuidString).appendingPathExtension(suffix)
            let macOS = url.appendingPathComponent("Contents/MacOS")
            try FileManager.default.createDirectory(at: macOS, withIntermediateDirectories: true)
            var info: [String: Any] = [
                "CFBundleIdentifier": "io.went.agent-queue",
                "CFBundleExecutable": "Fixture",
                "CFBundlePackageType": "APPL",
                "CFBundleVersion": "1",
                "AgentQueueServiceManagement": true
            ]
            info.merge(overrides) { _, replacement in replacement }
            for key in omitted { info.removeValue(forKey: key) }
            let plist = try PropertyListSerialization.data(fromPropertyList: info, format: .xml, options: 0)
            try plist.write(to: url.appendingPathComponent("Contents/Info.plist"))
            if executable {
                let executableURL = macOS.appendingPathComponent("Fixture")
                try Data("#!/bin/sh\nexit 0\n".utf8).write(to: executableURL)
                try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: executableURL.path)
            }
            guard let result = Bundle(url: url) else { throw TestFailure(message: "Cannot load fixture bundle") }
            return result
        }

        for action in Management.Action.allCases {
            for service in Management.Service.allCases {
                let arguments = ["--service-management", action.rawValue, service.rawValue]
                try check(try Management.parse(arguments: arguments) == .init(action: action, service: service),
                          "parse \(action.rawValue) \(service.rawValue)")
            }
        }
        for arguments in [[], ["--fixture", "example.json"]] {
            try check(try Management.parse(arguments: arguments) == nil, "ordinary app arguments are unhandled")
        }
        let invalidArguments = [
            ["--service-management"], ["--service-management", "status"],
            ["--service-management", "enable", "queue"], ["--service-management", "status", "daemon"],
            ["--service-management", "STATUS", "queue"], ["--service-management", "status", "Queue"],
            ["--service-management", "register", "queue", "extra"],
            ["extra", "--service-management", "register", "queue"],
            ["--service-management=status", "queue"],
            ["--service-management", "register", "../io.went.agent-heavy.plist"]
        ]
        for arguments in invalidArguments {
            try failure(.invalidArguments, "reject malformed CLI: \(arguments.joined(separator: " "))") {
                _ = try Management.parse(arguments: arguments)
            }
        }

        let owner = try bundle()
        try Management.validateOwningBundle(owner)
        guard let ownedExecutable = owner.executableURL else {
            throw TestFailure(message: "Fixture executable is missing")
        }
        try check(QueueExecutableNamespace.allows(executable: ownedExecutable, root: owner.bundleURL),
                  "protected executable namespace is accepted")
        let mutableParent = temporary.appendingPathComponent("Shared Applications")
        try FileManager.default.createDirectory(at: mutableParent, withIntermediateDirectories: false)
        let mutableURL = mutableParent.appendingPathComponent(owner.bundleURL.lastPathComponent)
        try FileManager.default.copyItem(at: owner.bundleURL, to: mutableURL)
        guard let mutableBundle = Bundle(url: mutableURL), let mutableExecutable = mutableBundle.executableURL else {
            throw TestFailure(message: "Mutable namespace fixture is missing")
        }
        for mode in [0o770, 0o777] {
            try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: mutableParent.path)
            try check(!QueueExecutableNamespace.allows(executable: mutableExecutable, root: mutableURL),
                      "foreign-writable executable ancestor is refused: \(mode)")
            try failure(.invalidOwningBundle, "unsafe namespace is refused before framework access: \(mode)") {
                _ = try Management.handle(arguments: ["--service-management", "status", "queue"],
                                          owningBundle: mutableBundle) { _ in
                    throw TestFailure(message: "Unsafe namespace constructed a native adapter")
                }
            }
        }
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: mutableParent.path)
        try check(QueueExecutableNamespace.allows(executable: mutableExecutable, root: mutableURL),
                  "protected shared ancestor is accepted")
        let alias = owner.bundleURL.appendingPathComponent("Contents/MacOS/Alias")
        try FileManager.default.createSymbolicLink(at: alias, withDestinationURL: ownedExecutable)
        try check(!QueueExecutableNamespace.allows(executable: alias, root: owner.bundleURL),
                  "executable symlink is refused")
        try FileManager.default.removeItem(at: alias)
        try FileManager.default.linkItem(at: ownedExecutable, to: alias)
        try check(!QueueExecutableNamespace.allows(executable: ownedExecutable, root: owner.bundleURL),
                  "multiply linked executable is refused")
        try FileManager.default.removeItem(at: alias)
        let expectedIdentities: [(Management.Service, Management.Identity)] = [
            (.queue, .agent(plistName: "io.went.agent-heavy.plist")),
            (.resources, .agent(plistName: "io.went.agent-resources.plist")),
            (.remote, .agent(plistName: "io.went.agent-remote.plist")),
            (.login, .mainApp)
        ]
        for (service, identity) in expectedIdentities {
            var received: [Management.Identity] = []
            let fixture = Fixture(.enabled)
            let result = try Management.handle(arguments: ["--service-management", "status", service.rawValue],
                                               owningBundle: owner) {
                received.append($0)
                return fixture.adapter
            }
            try check(received == [identity], "\(service.rawValue) selects exact Apple service identity")
            let expected = "{\"schemaVersion\":1,\"service\":\"\(service.rawValue)\",\"status\":\"enabled\"}"
            try check(result == .init(json: expected, exitCode: 0), "\(service.rawValue) deterministic JSON")
        }

        let appleStatuses: [(SMAppService.Status, Management.Status)] = [
            (.notRegistered, .notRegistered), (.enabled, .enabled),
            (.requiresApproval, .requiresApproval), (.notFound, .notFound)
        ]
        for (apple, expected) in appleStatuses {
            try check(try Management.mapStatus(apple) == expected, "Apple status maps \(expected.rawValue)")
            let fixture = Fixture(expected)
            let result = try Management.handle(arguments: ["--service-management", "status", "queue"],
                                               owningBundle: owner, makeAdapter: { _ in fixture.adapter })
            try check(result == .init(json: "{\"schemaVersion\":1,\"service\":\"queue\",\"status\":\"\(expected.rawValue)\"}",
                                      exitCode: 0) && fixture.events == ["status"],
                      "status reports \(expected.rawValue) without mutation")
        }
        if let unknown = SMAppService.Status(rawValue: 999) {
            try failure(.unknownStatus(999), "unknown future Apple state fails closed") {
                _ = try Management.mapStatus(unknown)
            }
        }

        for current in [Management.Status.enabled, .requiresApproval, .notFound] {
            let fixture = Fixture(current)
            let result = try Management.handle(arguments: ["--service-management", "register", "login"],
                                               owningBundle: owner, makeAdapter: { _ in fixture.adapter })
            let expectedCode: Int32 = current == .enabled ? 0 : (current == .requiresApproval ? 2 : 1)
            try check(result?.exitCode == expectedCode && fixture.events == ["status"],
                      "register preserves \(current.rawValue) without retrying or overriding consent")
        }
        for end in Management.Status.allCases {
            let fixture = Fixture(.notRegistered)
            fixture.afterRegister = end
            let result = try Management.handle(arguments: ["--service-management", "register", "queue"],
                                               owningBundle: owner, makeAdapter: { _ in fixture.adapter })
            let expectedCode: Int32 = end == .enabled ? 0 : (end == .requiresApproval ? 2 : 1)
            try check(result?.exitCode == expectedCode && result?.json.contains("\"status\":\"\(end.rawValue)\"") == true
                      && fixture.events == ["status", "register", "status"],
                      "register inspects actual end state \(end.rawValue)")
        }
        for end in Management.Status.allCases {
            let fixture = Fixture(.enabled)
            fixture.afterUnregister = end
            let result = try Management.handle(arguments: ["--service-management", "unregister", "remote"],
                                               owningBundle: owner, makeAdapter: { _ in fixture.adapter })
            let expectedCode: Int32 = end == .notRegistered ? 0 : (end == .requiresApproval ? 2 : 1)
            try check(result?.exitCode == expectedCode && fixture.events == ["unregister", "status"],
                      "unregister inspects actual end state \(end.rawValue)")
        }

        let denied = NSError(domain: SMAppServiceErrorDomain, code: Int(kSMErrorLaunchDeniedByUser))
        let deniedFixture = Fixture(.notRegistered)
        deniedFixture.registerError = denied
        try propagated(denied, "denied registration error propagates unchanged") {
            _ = try Management.run(.init(action: .register, service: .queue), adapter: deniedFixture.adapter)
        }
        try check(deniedFixture.events == ["status", "register"], "denied registration is not retried")
        deniedFixture.status = .requiresApproval
        deniedFixture.events = []
        let revoked = try Management.run(.init(action: .register, service: .queue), adapter: deniedFixture.adapter)
        try check(revoked.status == .requiresApproval && deniedFixture.events == ["status"],
                  "revoked approval is reported without calling denied register again")

        let unavailable = NSError(domain: SMAppServiceErrorDomain, code: Int(kSMErrorServiceUnavailable))
        let readFixture = Fixture(.notRegistered)
        readFixture.statusError = unavailable
        try propagated(unavailable, "pre-registration status error prevents mutation") {
            _ = try Management.run(.init(action: .register, service: .queue), adapter: readFixture.adapter)
        }
        try check(readFixture.events == ["status"], "failed status cannot authorize registration")
        for code in [kSMErrorJobNotFound, kSMErrorAuthorizationFailure, kSMErrorServiceUnavailable] {
            let error = NSError(domain: SMAppServiceErrorDomain, code: Int(code))
            let fixture = Fixture(.enabled)
            fixture.unregisterError = error
            try propagated(error, "unregister framework error \(code) propagates unchanged") {
                _ = try Management.run(.init(action: .unregister, service: .queue), adapter: fixture.adapter)
            }
            try check(fixture.events == ["unregister"], "failed unregister does not fabricate a status")
        }
        let postRead = Fixture(.enabled)
        postRead.statusError = unavailable
        try propagated(unavailable, "post-unregister status errors propagate") {
            _ = try Management.run(.init(action: .unregister, service: .queue), adapter: postRead.adapter)
        }
        try check(postRead.events == ["unregister", "status"], "unregister completion requires readable status")
        try propagated(unavailable, "adapter creation errors propagate") {
            _ = try Management.handle(arguments: ["--service-management", "status", "queue"],
                                      owningBundle: owner, makeAdapter: { _ in throw unavailable })
        }

        let rejectedBundles = try [
            bundle(overrides: ["CFBundleIdentifier": "io.went.agent-queue.tests"]),
            bundle(omitted: ["AgentQueueServiceManagement"]),
            bundle(overrides: ["AgentQueueServiceManagement": false]),
            bundle(overrides: ["AgentQueueServiceManagement": "true"]),
            bundle(overrides: ["AgentQueueServiceManagement": 1]),
            bundle(overrides: ["CFBundlePackageType": "BNDL"]),
            bundle(overrides: ["CFBundleExecutable": "../Fixture"]),
            bundle(extension: "bundle"), bundle(executable: false)
        ]
        for (index, rejected) in rejectedBundles.enumerated() {
            for action in Management.Action.allCases {
                var constructed = false
                try failure(.invalidOwningBundle, "bundle gate \(index) rejects \(action.rawValue)") {
                    _ = try Management.handle(arguments: ["--service-management", action.rawValue, "queue"],
                                              owningBundle: rejected) { _ in
                        constructed = true
                        return Fixture(.notRegistered).adapter
                    }
                }
                try check(!constructed, "bundle gate \(index) precedes adapter construction")
            }
        }
        let escapedBundle = try bundle()
        guard let escapedExecutable = escapedBundle.executableURL else {
            throw TestFailure(message: "Fixture executable URL missing")
        }
        let external = temporary.appendingPathComponent("ExternalFixture")
        try FileManager.default.moveItem(at: escapedExecutable, to: external)
        try FileManager.default.createSymbolicLink(at: escapedExecutable, withDestinationURL: external)
        try failure(.invalidOwningBundle, "executable symlink escaping owning bundle is rejected") {
            try Management.validateOwningBundle(escapedBundle)
        }
        // Only the read-only production route is exercised. All mutations above
        // use closures backed by Fixture, never Apple's real mutation methods.
        try failure(.invalidOwningBundle, "standalone test executable cannot use production status") {
            _ = try Management.handle(arguments: ["--service-management", "status", "queue"])
        }
        for arguments in invalidArguments {
            var constructed = false
            try failure(.invalidArguments, "invalid CLI fails before adapter creation") {
                _ = try Management.handle(arguments: arguments, owningBundle: owner) { _ in
                    constructed = true
                    return Fixture(.notRegistered).adapter
                }
            }
            try check(!constructed, "invalid CLI performs no service operation")
        }
        let ordinary = try Management.handle(arguments: [], owningBundle: rejectedBundles[0]) { _ in
            throw TestFailure(message: "Ordinary launch constructed a service")
        }
        try check(ordinary == nil, "ordinary app launch bypasses service mode")
        try Management.waitForUnregistration { completion in
            DispatchQueue.global().async { completion(nil) }
        }
        try check(true, "unregistration waits for an asynchronous successful completion")
        let shutdownError = NSError(domain: "ServiceManagementTests.shutdown", code: 7)
        try propagated(shutdownError, "asynchronous unregistration failure is preserved") {
            try Management.waitForUnregistration { completion in
                DispatchQueue.global().async { completion(shutdownError) }
            }
        }

        func replacementFailure(_ expected: Replacement.Failure, _ name: String,
                                operation: () throws -> Void) throws {
            do {
                try operation()
                throw TestFailure(message: "\(name): expected replacement failure")
            } catch let error as Replacement.Failure {
                try check(error == expected, name)
            }
        }

        let home = temporary.resolvingSymlinksInPath().appendingPathComponent("User")
        let target = home.appendingPathComponent("Applications/Agent Queue.app")
        let replacementArguments = ["--prepare-app-replacement", target.path]
        func prepare(_ fixture: ReplacementFixture) throws -> Management.CLIResult? {
            try Replacement.handle(arguments: replacementArguments, owningBundle: owner,
                                   home: home, currentUserID: 501, currentProcessID: 99,
                                   makeAdapter: { fixture.adapter })
        }
        try check(try Replacement.parse(arguments: replacementArguments) == target.path,
                  "replacement parses exact command and absolute target")
        for arguments in [[], ["--setup"], ["--fixture", "private.json"],
                          ["--service-management", "status", "login"]] {
            try check(try Replacement.parse(arguments: arguments) == nil,
                      "replacement leaves ordinary and existing service arguments unhandled")
            let result = try Replacement.handle(arguments: arguments, owningBundle: rejectedBundles[0],
                                                home: home, currentUserID: 501, currentProcessID: 99) {
                throw TestFailure(message: "Ordinary launch constructed replacement adapter")
            }
            try check(result == nil, "ordinary launch bypasses replacement owner and adapter")
        }
        let malformedReplacementArguments = [
            ["--prepare-app-replacement"], ["--prepare-app-replacement", ""],
            ["--prepare-app-replacement", "relative/Agent Queue.app"],
            ["--prepare-app-replacement", "~/Applications/Agent Queue.app"],
            ["--prepare-app-replacement", target.path, "extra"],
            ["--prepare-app-replacement", target.path, "--prepare-app-replacement", target.path],
            ["extra", "--prepare-app-replacement", target.path],
            ["--prepare-app-replacement=\(target.path)"],
            ["--prepare-app-replacement-extra", target.path],
            ["--service-management", "status", "login", "--prepare-app-replacement", target.path],
            ["--prepare-app-replacement", "--prepare-app-replacement"],
            ["--prepare-app-replacement", target.path, "--setup"]
        ]
        for arguments in malformedReplacementArguments {
            try replacementFailure(.invalidArguments, "malformed replacement arguments fail before adapters") {
                _ = try Replacement.handle(arguments: arguments, owningBundle: owner,
                                           home: home, currentUserID: 501, currentProcessID: 99) {
                    throw TestFailure(message: "Malformed replacement constructed adapter")
                }
            }
        }
        for rejected in rejectedBundles + [escapedBundle] {
            try failure(.invalidOwningBundle, "replacement uses existing strict owning bundle gate") {
                _ = try Replacement.handle(arguments: replacementArguments, owningBundle: rejected,
                                           home: home, currentUserID: 501, currentProcessID: 99) {
                    throw TestFailure(message: "Rejected owner constructed replacement adapter")
                }
            }
        }
        try failure(.invalidOwningBundle, "standalone production replacement cannot enumerate host apps") {
            _ = try Replacement.handle(arguments: replacementArguments)
        }
        for path in [target.path, "/Applications/Agent Queue.app"] {
            try check(try Replacement.validateTarget(path, home: home).path == path,
                      "replacement accepts canonical user and system install locations")
        }
        for path in [temporary.appendingPathComponent("Other/Agent Queue.app").path,
                     home.appendingPathComponent("Applications/Other").path,
                     home.appendingPathComponent("Applications/Nested/Other.app").path,
                     target.path + "/", target.path + "/.", target.path + "/../Agent Queue.app",
                     target.path.replacingOccurrences(of: "/Applications/", with: "//Applications/"),
                     "/Applications/Nested/Agent Queue.app"] {
            try replacementFailure(.invalidTarget, "noncanonical or different install target is rejected: \(path)") {
                _ = try Replacement.handle(arguments: ["--prepare-app-replacement", path], owningBundle: owner,
                                           home: home, currentUserID: 501, currentProcessID: 99) {
                    throw TestFailure(message: "Invalid target constructed adapter")
                }
            }
        }
        let empty = ReplacementFixture()
        let stoppedJSON = "{\"app\":\"\(target.path)\",\"schemaVersion\":1,\"status\":\"stopped\"}"
        try check(!FileManager.default.fileExists(atPath: target.path), "no-running fixture has no target files")
        try check(try prepare(empty) == .init(json: stoppedJSON, exitCode: 0),
                  "no running target succeeds with deterministic stopped JSON and no target files")
        try check(empty.advances.isEmpty, "no running target needs no lifecycle wait")

        let otherCopy = temporary.appendingPathComponent("Other/Agent Queue.app")
        let excluded = [
            AppFixture(path: otherCopy), AppFixture(path: target, identifier: "io.went.unrelated"),
            AppFixture(path: target, uid: 502), AppFixture(path: target, pid: 99),
            AppFixture(path: nil), AppFixture(path: target, identifier: nil),
            AppFixture(path: target, pid: -1), AppFixture(path: URL(string: "https://example.invalid/Agent%20Queue.app")),
            AppFixture(path: URL(fileURLWithPath: "/Applications/Agent Queue.app"))
        ]
        let exclusions = ReplacementFixture()
        exclusions.apps = excluded
        try check(try prepare(exclusions)?.exitCode == 0, "unrelated copies and foreign processes do not block target")
        try check(excluded.allSatisfy { $0.terminationRequests == 0 },
                  "other path, identifier, UID, own PID and missing identity are never terminated")
        let unknownOwner = ReplacementFixture()
        unknownOwner.apps = [AppFixture(path: target, uid: nil)]
        try replacementFailure(.unverifiedOwnership, "unreadable owner of exact live app fails closed") {
            _ = try prepare(unknownOwner)
        }
        try check(unknownOwner.apps[0].terminationRequests == 0, "unverified owner receives no quit request")

        let live = ReplacementFixture()
        let matchingApp = AppFixture(path: target)
        live.apps = excluded + [matchingApp]
        try check(try prepare(live)?.json == stoppedJSON, "exact live metadata authorizes missing target repair")
        try check(matchingApp.terminationRequests == 1 && excluded.allSatisfy { $0.terminationRequests == 0 },
                  "only verified target receives one polite quit request")
        let systemInstall = ReplacementFixture()
        systemInstall.apps = [AppFixture(path: URL(fileURLWithPath: "/Applications/Agent Queue.app")),
                              AppFixture(path: target)]
        let systemResult = try Replacement.handle(
            arguments: ["--prepare-app-replacement", "/Applications/Agent Queue.app"], owningBundle: owner,
            home: home, currentUserID: 501, currentProcessID: 99, makeAdapter: { systemInstall.adapter })
        try check(systemResult == .init(json: "{\"app\":\"/Applications/Agent Queue.app\",\"schemaVersion\":1,\"status\":\"stopped\"}", exitCode: 0),
                  "system install returns the same deterministic replacement contract")
        try check(systemInstall.apps[0].terminationRequests == 1 && systemInstall.apps[1].terminationRequests == 0,
                  "system target never terminates independently installed user copy")
        for renamed in [home.appendingPathComponent("Applications/Finder Copied Queue.app"),
                        URL(fileURLWithPath: "/Applications/Finder Copied Queue.app")] {
            let fixture = ReplacementFixture()
            fixture.apps = [AppFixture(path: renamed), AppFixture(path: target)]
            let result = try Replacement.handle(
                arguments: ["--prepare-app-replacement", renamed.path], owningBundle: owner,
                home: home, currentUserID: 501, currentProcessID: 99, makeAdapter: { fixture.adapter })
            try check(result?.exitCode == 0 && result?.json.contains(renamed.path) == true,
                      "renamed Applications target retains exact shutdown identity")
            try check(fixture.apps[0].terminationRequests == 1 && fixture.apps[1].terminationRequests == 0,
                      "renamed target quits only its own verified process")
        }

        // A broken target is accepted solely using live metadata. No Bundle is
        // loaded from the target, and the label alone never authorizes a quit.
        try FileManager.default.createDirectory(at: target, withIntermediateDirectories: true)
        try Data("damaged plist".utf8).write(to: target.appendingPathComponent("Info.plist"))
        let damaged = ReplacementFixture()
        damaged.apps = [AppFixture(path: target)]
        try check(try prepare(damaged)?.exitCode == 0 && damaged.apps[0].terminationRequests == 1,
                  "damaged disk bundle is repairable from verified live bundle metadata")
        let labelOnly = ReplacementFixture()
        labelOnly.apps = [AppFixture(path: target, identifier: "io.went.impostor")]
        try check(try prepare(labelOnly)?.exitCode == 0 && labelOnly.apps[0].terminationRequests == 0,
                  "matching app label never substitutes for live bundle identifier")
        try FileManager.default.removeItem(at: target)
        try FileManager.default.createDirectory(at: otherCopy, withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(at: target, withDestinationURL: otherCopy)
        try replacementFailure(.invalidTarget, "canonical install location cannot redirect to another copy") {
            _ = try Replacement.validateTarget(target.path, home: home)
        }
        try FileManager.default.removeItem(at: target)

        let alreadyStopped = ReplacementFixture()
        alreadyStopped.apps = [AppFixture(path: target)]
        alreadyStopped.apps[0].terminated = true
        try check(try prepare(alreadyStopped)?.exitCode == 0 && alreadyStopped.apps[0].terminationRequests == 0,
                  "already exited live record receives no quit request")
        alreadyStopped.apps[0].metadata = .init(processID: 42, bundleURL: target,
                                              bundleIdentifier: "io.went.agent-queue", userID: nil)
        try check(try prepare(alreadyStopped)?.exitCode == 0 && alreadyStopped.apps[0].terminationRequests == 0,
                  "exited app needs no process owner after its process metadata disappears")
        let refused = ReplacementFixture()
        refused.apps = [AppFixture(path: target)]
        refused.apps[0].acceptsTermination = false
        try replacementFailure(.terminationRefused, "refused polite quit fails replacement") { _ = try prepare(refused) }
        try check(refused.apps[0].terminationRequests == 1 && refused.advances.isEmpty,
                  "refusal is not retried or escalated")
        let asynchronous = ReplacementFixture()
        asynchronous.apps = [AppFixture(path: target), AppFixture(path: target, pid: 43)]
        for app in asynchronous.apps { app.finishImmediately = false }
        asynchronous.onAdvance = {
            if asynchronous.time >= 0.1 { asynchronous.apps[0].terminated = true }
            if asynchronous.time >= 0.2 { asynchronous.apps[1].terminated = true }
        }
        try check(try prepare(asynchronous)?.exitCode == 0 && asynchronous.time >= 0.2,
                  "replacement waits for every matching instance through injected run-loop turns")
        try check(asynchronous.apps.allSatisfy { $0.terminationRequests == 1 }
                  && asynchronous.advances.allSatisfy { $0 > 0 && $0 <= 0.05 },
                  "asynchronous quit sends once and advances bounded event intervals")
        let timeout = ReplacementFixture()
        timeout.apps = [AppFixture(path: target)]
        timeout.apps[0].finishImmediately = false
        try replacementFailure(.terminationTimedOut, "accepted request without exit times out") { _ = try prepare(timeout) }
        try check(timeout.time == 30 && timeout.apps[0].terminationRequests == 1,
                  "timeout is bounded to 30 simulated seconds and never force kills")

        let changed = ReplacementFixture()
        let changingApp = AppFixture(path: target)
        changed.apps = [changingApp]
        var metadataReads = 0
        changingApp.onMetadata = {
            metadataReads += 1
            if metadataReads == 2 {
                changingApp.metadata = .init(processID: 42, bundleURL: target,
                                            bundleIdentifier: "io.went.agent-queue", userID: 502)
            }
        }
        try replacementFailure(.processChanged, "identity is checked again immediately before polite quit") {
            _ = try prepare(changed)
        }
        try check(changingApp.terminationRequests == 0, "changed ownership receives no quit request")
        let relaunched = ReplacementFixture()
        relaunched.apps = [AppFixture(path: target)]
        relaunched.onEnumeration = {
            if relaunched.enumerationCount == 2 { relaunched.apps.append(AppFixture(path: target, pid: 43)) }
        }
        try replacementFailure(.processChanged, "new instance during shutdown blocks payload replacement") {
            _ = try prepare(relaunched)
        }
        try check(relaunched.apps[1].terminationRequests == 0, "newly launched instance is never chased")

        let replacementError = NSError(domain: "ServiceManagementTests.replacement", code: 9)
        for stage in ["enumeration", "metadata", "observation", "termination", "runloop"] {
            let errors = ReplacementFixture()
            let app = AppFixture(path: target)
            errors.apps = [app]
            switch stage {
            case "enumeration": errors.enumerationError = replacementError
            case "metadata": app.metadataError = replacementError
            case "observation": app.observationError = replacementError
            case "termination": app.terminationError = replacementError
            default:
                app.finishImmediately = false
                errors.advanceError = replacementError
            }
            try propagated(replacementError, "replacement \(stage) error propagates unchanged") {
                _ = try prepare(errors)
            }
            try check(app.terminationRequests <= 1, "replacement \(stage) error never retries termination")
        }
        try propagated(replacementError, "replacement adapter construction error propagates") {
            _ = try Replacement.handle(arguments: replacementArguments, owningBundle: owner,
                                       home: home, currentUserID: 501, currentProcessID: 99) { throw replacementError }
        }
        print("PASS \(passed) service-management checks; all mutations simulated")
    }
}
