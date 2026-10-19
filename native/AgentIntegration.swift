import AppKit
import Foundation
import SwiftUI

struct AgentRunnerConfiguration: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let name: String
    let path: String?
    let configured: Bool
    let error: String?
}

struct AgentIntegrationSnapshot: Codable, Sendable {
    let runners: [AgentRunnerConfiguration]
    let instruction: String
}

enum AgentOnboarding {
    static let offeredKey = "agentIntegrationOffered.v1"
    static func shouldOffer(defaults: UserDefaults, fixtureMode: Bool) -> Bool {
        !fixtureMode && !defaults.bool(forKey: offeredKey)
    }
}

@MainActor
final class AgentIntegrationStore: ObservableObject {
    @Published private(set) var snapshot: AgentIntegrationSnapshot?
    @Published private(set) var isBusy = false
    @Published private(set) var error: String?
    @Published private(set) var notice: String?
    @Published private(set) var selected: Set<String> = []
    let fixtureMode: Bool
    private let command: @Sendable ([String]) async throws -> Data

    init(fixtureMode: Bool = false, command: @escaping @Sendable ([String]) async throws -> Data = {
        try await QueueClient.run($0)
    }) {
        self.fixtureMode = fixtureMode
        self.command = command
        if fixtureMode {
            snapshot = AgentIntegrationSnapshot(runners: [
                AgentRunnerConfiguration(id: "codex", name: "Codex", path: "~/.codex/AGENTS.md", configured: false, error: nil),
                AgentRunnerConfiguration(id: "claude", name: "Claude Code", path: "~/.claude/CLAUDE.md", configured: false, error: nil)
            ], instruction: "Run heavy builds and broad tests through agent-heavy -- <command>.\nKeep lightweight checks outside the queue.")
        }
    }

    var hasChanges: Bool {
        snapshot?.runners.contains { $0.error == nil && $0.configured != selected.contains($0.id) } ?? false
    }
    var canSave: Bool { !fixtureMode && !isBusy && hasChanges }

    func select(_ id: String, enabled: Bool) {
        guard !isBusy, snapshot?.runners.contains(where: { $0.id == id && $0.error == nil }) == true else { return }
        if enabled { selected.insert(id) } else { selected.remove(id) }
    }

    func refresh() async {
        guard !fixtureMode, !isBusy else { return }
        isBusy = true
        defer { isBusy = false }
        do {
            let result = try decode(try await command(["agents", "status", "--json"]))
            snapshot = result
            selected = Set(result.runners.filter(\.configured).map(\.id))
            error = nil
        } catch { self.error = error.localizedDescription }
    }

    @discardableResult
    func save() -> Task<Void, Never>? {
        guard canSave, let snapshot else { return nil }
        let changes = snapshot.runners.filter { $0.error == nil && $0.configured != selected.contains($0.id) }
        let desired = selected
        isBusy = true
        notice = nil
        error = nil
        return Task {
            defer { isBusy = false }
            do {
                for runner in changes {
                    let action = desired.contains(runner.id) ? "install" : "remove"
                    self.snapshot = try decode(try await command(["agents", action, "--runner", runner.id, "--json"]))
                }
                notice = "Instructions saved. Start a new agent session to load them."
            } catch {
                // A preceding runner may have succeeded. Reconcile before offering a retry.
                self.error = "\(error.localizedDescription) Refresh status before retrying; some changes may have been saved."
                if let data = try? await command(["agents", "status", "--json"]), let latest = try? decode(data) {
                    self.snapshot = latest
                }
            }
        }
    }

    private func decode(_ data: Data) throws -> AgentIntegrationSnapshot {
        let result = try JSONDecoder().decode(AgentIntegrationSnapshot.self, from: data)
        guard result.runners.count == 2, Set(result.runners.map(\.id)) == ["codex", "claude"] else {
            throw ClientError.message("Agent integration returned an unexpected runner list.")
        }
        return result
    }
}

struct AgentIntegrationSettings: View {
    @ObservedObject var store: AgentIntegrationStore
    var onDone: () -> Void
    @State private var showInstructions = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    Label("Agent integration", systemImage: "terminal").font(.title2.weight(.semibold))
                    Spacer()
                    if store.fixtureMode { Text("PREVIEW").font(.caption2).foregroundStyle(.secondary) }
                    Button { Task { await store.refresh() } } label: { Image(systemName: "arrow.clockwise") }
                        .buttonStyle(.borderless).help("Refresh agent integration status")
                        .accessibilityLabel("Refresh agent integration status")
                        .disabled(store.fixtureMode || store.isBusy)
                }
                Text("Help your coding agents share the heavy-command queue.")
                    .font(.callout).foregroundStyle(.secondary)
            }.padding(22)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    Text("Select the agents that should receive queue instructions for this macOS account.")
                    if let snapshot = store.snapshot {
                        ForEach(snapshot.runners) { runner in
                            VStack(alignment: .leading, spacing: 6) {
                                Toggle(runner.name, isOn: Binding(
                                    get: { store.selected.contains(runner.id) },
                                    set: { store.select(runner.id, enabled: $0) }))
                                    .font(.headline).disabled(store.isBusy || runner.error != nil)
                                Label(runner.configured ? "Instructions installed" : "Not configured by this app",
                                      systemImage: runner.configured ? "checkmark.circle.fill" : "circle")
                                    .font(.caption).foregroundStyle(runner.configured ? .green : .secondary)
                                Text(runner.path ?? "Instruction path unavailable").font(.system(.caption, design: .monospaced))
                                    .foregroundStyle(.secondary).textSelection(.enabled)
                                if let error = runner.error {
                                    Label(error, systemImage: "exclamationmark.triangle")
                                        .font(.caption).foregroundStyle(.orange).textSelection(.enabled)
                                }
                            }.padding(14).frame(maxWidth: .infinity, alignment: .leading)
                                .background(.quaternary.opacity(0.35)).clipShape(RoundedRectangle(cornerRadius: 10))
                        }
                        DisclosureGroup("Preview queue instructions", isExpanded: $showInstructions) {
                            Text(snapshot.instruction).font(.system(.caption, design: .monospaced))
                                .frame(maxWidth: .infinity, alignment: .leading).textSelection(.enabled).padding(.top, 8)
                        }
                    } else if store.isBusy { ProgressView("Checking instruction files…") }
                    Text("Existing instructions stay in place. Saving adds a marked queue section and backs up existing files. Deselecting an agent removes only the section added by this app.")
                        .font(.caption).foregroundStyle(.secondary)
                    Text("These are instructions, not enforcement. Agents can still run commands directly. Start new sessions after saving. Other runners need the queue rule added manually.")
                        .font(.caption).foregroundStyle(.secondary)
                    if let error = store.error {
                        Label(error, systemImage: "exclamationmark.triangle")
                            .font(.callout).foregroundStyle(.orange).textSelection(.enabled)
                    }
                    if let notice = store.notice {
                        Label(notice, systemImage: "checkmark.circle").font(.callout).foregroundStyle(.green)
                    }
                }.padding(22)
            }
            Divider()
            HStack {
                if store.isBusy { ProgressView().controlSize(.small); Text("Updating…").font(.caption) }
                Spacer()
                Button(store.hasChanges || store.snapshot?.runners.contains(where: \.configured) != true ? "Not now" : "Done",
                       action: onDone).disabled(store.isBusy)
                Button("Save instructions") { store.save() }.buttonStyle(.borderedProminent).disabled(!store.canSave)
            }.padding(18)
        }.background(Color(nsColor: .windowBackgroundColor))
            .task { await store.refresh() }
    }
}
