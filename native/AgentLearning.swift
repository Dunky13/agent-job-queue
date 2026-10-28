import AppKit
import Foundation
import SwiftUI

struct LearningProfile: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let label: String
    let memberCount: Int
    let variantCount: Int
    let successCount: Int
    let incompleteCount: Int
    let peakMemoryBytes: Int64
    let peakCpuCores: Double
    let lastRunAt: Double?
    let memoryFloorBytes: Int64?
    let cpuFloorCores: Double?
}

struct LearningCatalog: Codable, Sendable {
    let revision: Int
    let profiles: [LearningProfile]
    let total: Int
    let offset: Int
    let limit: Int
    var controllerAppliedRevision: Int? = nil
    var controllerStatus: String? = nil
}

struct LearningMember: Codable, Identifiable, Sendable {
    let family: String
    let label: String
    let project: String
    let executable: String
    var measurements: LearningMeasurements? = nil
    var id: String { family }
}

struct LearningMeasurements: Codable, Sendable {
    let variantCount: Int
    let successCount: Int
    let incompleteCount: Int
    let peakMemoryBytes: Int64
    let peakCpuCores: Double
    let lastRunAt: Double?
}

struct LearningMemberPage: Codable, Sendable {
    let total: Int
    let offset: Int
    let limit: Int
    let query: String
}

struct LearningGroupReference: Sendable {
    let id: String
    let label: String
}

struct LearningMergeItem: Identifiable, Equatable, Sendable {
    let identity: String
    let label: String
    let isGroup: Bool
    let sourceGroup: String
    let revision: Int
    var id: String { "\(isGroup ? "group" : "workload"):\(identity)" }
}

private struct LearningCombine: Encodable {
    let revision: Int
    let profileIds: [String]
    let families: [String]
}

struct LearningVariant: Codable, Identifiable, Sendable {
    let id: String
    let family: String
    let successCount: Int
    let incompleteCount: Int
    let lastRunAt: Double?
    let peakMemoryBytes: Int64
    let peakCpuCores: Double
}

struct LearningRun: Codable, Identifiable, Sendable {
    let id: String
    let family: String
    let variant: String
    let machine: String
    let sampledAt: Double
    let memoryBytes: Int64
    let cpuCores: Double
    let cpuSeconds: Double
    let durationSeconds: Double
    let success: Bool
}

struct LearningTruncation: Codable, Sendable {
    let members: Bool
    let variants: Bool
    let runs: Bool
}

struct LearningDetail: Codable, Sendable {
    let revision: Int
    let profile: LearningProfile
    let members: [LearningMember]
    let variants: [LearningVariant]
    let runs: [LearningRun]
    let prediction: ResourcePrediction?
    let predictionMachine: String?
    let truncated: LearningTruncation
    var controllerAppliedRevision: Int? = nil
    var controllerStatus: String? = nil
    var memberPage: LearningMemberPage? = nil
    var inspectedFamily: String? = nil
}

struct JobLearningReference: Codable, Equatable, Sendable {
    let id: String
    let label: String
    let reason: String
}

struct LearningEdit: Encodable, Sendable {
    let revision: Int
    let label: String
    let memoryFloorBytes: Int64?
    let cpuFloorCores: Double?
    // Null explicitly clears a floor; synthesised Optional encoding omits it.
    func encode(to encoder: Encoder) throws {
        var values = encoder.container(keyedBy: CodingKeys.self)
        try values.encode(revision, forKey: .revision)
        try values.encode(label, forKey: .label)
        try values.encode(memoryFloorBytes, forKey: .memoryFloorBytes)
        try values.encode(cpuFloorCores, forKey: .cpuFloorCores)
    }
    enum CodingKeys: String, CodingKey { case revision, label, memoryFloorBytes, cpuFloorCores }
}

private struct LearningMerge: Encodable { let revision: Int; let targetId: String }
private struct LearningSplit: Encodable { let revision: Int; let family: String }
private struct LearningReset: Encodable { let revision: Int; let confirm: Bool }

enum LearningMemoryUnit: String, CaseIterable {
    case gib = "GiB", mib = "MiB"
    var bytes: Double { self == .gib ? 1_073_741_824 : 1_048_576 }
}

@MainActor
final class LearningStore: ObservableObject {
    @Published private(set) var catalog: LearningCatalog?
    @Published private(set) var detail: LearningDetail?
    @Published private(set) var selectedID: String?
    @Published private(set) var isLoading = false
    @Published private(set) var isDetailLoading = false
    @Published private(set) var isMutating = false
    @Published private(set) var error: String?
    @Published private(set) var notice: String?
    @Published private(set) var inspectedFamily: String?
    @Published private(set) var memberDetail: LearningDetail?
    @Published private(set) var memberError: String?
    @Published private(set) var isMemberLoading = false
    @Published private(set) var isMemberPageLoading = false
    @Published private(set) var ungroupedFrom: LearningGroupReference?
    @Published private(set) var mergeSelection: [LearningMergeItem] = []
    @Published var memberQuery = ""
    @Published var query = ""
    @Published var draftLabel = ""
    @Published var draftMemory = ""
    @Published var draftCPU = ""
    @Published var memoryUnit = LearningMemoryUnit.gib
    let fixtureMode: Bool
    let pageSize = 50
    private var selectionGeneration = 0
    private var selectionTask: Task<Void, Never>?
    private var memberGeneration = 0
    private var memberTask: Task<Void, Never>?
    private let command: @Sendable ([String], Data?) async throws -> Data

    init(fixtureMode: Bool = false, command: @escaping @Sendable ([String], Data?) async throws -> Data = {
        try await QueueClient.run($0, input: $1)
    }) {
        self.fixtureMode = fixtureMode
        self.command = command
        if fixtureMode {
            let preview = Self.previewDetail()
            catalog = LearningCatalog(revision: preview.revision, profiles: [preview.profile], total: 1, offset: 0, limit: 50,
                controllerAppliedRevision: preview.revision, controllerStatus: "applied")
            selectedID = preview.profile.id
            detail = preview
            restoreDraft(preview)
        }
    }

    var canSave: Bool { !fixtureMode && !isLoading && !isMutating && !isMemberPageLoading && detail != nil }
    var hasPrevious: Bool { (catalog?.offset ?? 0) > 0 }
    var hasNext: Bool {
        guard let catalog else { return false }
        return catalog.offset + catalog.profiles.count < catalog.total
    }
    var pageLabel: String {
        guard let catalog, catalog.total > 0 else { return "No learned workloads" }
        return "\(catalog.offset + 1)–\(catalog.offset + catalog.profiles.count) of \(catalog.total)"
    }
    var controllerMessage: String? {
        let revision = detail?.revision ?? catalog?.revision
        let applied = detail?.controllerAppliedRevision ?? catalog?.controllerAppliedRevision
        let status = detail?.controllerStatus ?? catalog?.controllerStatus
        if let revision, let applied, applied >= revision { return "Controller has applied these corrections." }
        if let status, !status.isEmpty, status != "applied" {
            return "Controller: \(status). Corrections take effect on its next fresh pass. Active jobs continue."
        }
        guard revision != nil else { return nil }
        return "Waiting for the controller to apply these corrections. Active jobs continue."
    }

    @discardableResult
    func loadPage(offset: Int = 0, preserveSelection: Bool = false) -> Task<Void, Never>? {
        guard !fixtureMode, !isLoading, !isMutating else { return nil }
        isLoading = true
        error = nil
        let requestedQuery = query.trimmingCharacters(in: .whitespacesAndNewlines)
        let requestedGeneration = selectionGeneration
        return Task {
            defer { isLoading = false }
            do {
                let result = try await readCatalog(offset: max(0, offset), query: requestedQuery)
                catalog = result
                if (!preserveSelection || selectedID == nil) && selectionGeneration == requestedGeneration {
                    if let id = result.profiles.first?.id { await select(id)?.value }
                    else {
                        selectionTask?.cancel()
                        selectionGeneration += 1
                        selectedID = nil
                        detail = nil
                        isDetailLoading = false
                        clearDraft()
                    }
                }
            } catch { self.error = "Could not load learned workloads. \(error.localizedDescription)" }
        }
    }

    @discardableResult
    func select(_ id: String) -> Task<Void, Never>? {
        guard !fixtureMode else { return nil }
        closeMember()
        ungroupedFrom = nil
        memberQuery = ""
        let previous = selectionTask
        previous?.cancel()
        selectionGeneration += 1
        let generation = selectionGeneration
        selectedID = id
        isDetailLoading = true
        detail = nil
        clearDraft()
        error = nil
        notice = nil
        let task = Task {
            defer { if selectionGeneration == generation { isDetailLoading = false } }
            do {
                // Wait for the obsolete private read to be reaped before starting
                // another. Rapid selections keep at most one live detail child.
                await previous?.value
                try Task.checkCancellation()
                let next = try await readDetail(id)
                if !Task.isCancelled && selectedID == id && selectionGeneration == generation { apply(next) }
            } catch is CancellationError {
                // Superseding a selection is not a workload-loading error.
            } catch {
                if selectedID == id && selectionGeneration == generation {
                    self.error = "Could not load this workload. \(error.localizedDescription)"
                }
            }
        }
        selectionTask = task
        return task
    }

    @discardableResult
    func reloadSelection() -> Task<Void, Never>? {
        guard !isMutating else { return nil }
        if let selectedID { return select(selectedID) }
        return loadPage()
    }

    func closeMember() {
        memberTask?.cancel()
        memberGeneration += 1
        inspectedFamily = nil
        memberDetail = nil
        memberError = nil
        isMemberLoading = false
        isMemberPageLoading = false
    }

    @discardableResult
    func inspectMember(_ family: String) -> Task<Void, Never>? {
        guard !isMutating, !isLoading, !isDetailLoading, !isMemberPageLoading, let detail,
              let member = detail.members.first(where: { $0.family == family }) else { return nil }
        let previous = memberTask
        closeMember()
        inspectedFamily = family
        if fixtureMode {
            memberDetail = LearningDetail(revision: detail.revision, profile: detail.profile, members: [member],
                variants: detail.variants.filter { $0.family == family }, runs: detail.runs.filter { $0.family == family },
                prediction: detail.prediction, predictionMachine: detail.predictionMachine, truncated: detail.truncated,
                inspectedFamily: family)
            return nil
        }
        let sourceID = detail.profile.id
        let revision = detail.revision
        let generation = memberGeneration
        isMemberLoading = true
        let task = Task {
            defer { if memberGeneration == generation { isMemberLoading = false } }
            do {
                await previous?.value
                try Task.checkCancellation()
                let next = try JSONDecoder().decode(LearningDetail.self, from: try await command(
                    ["profiles", "show", sourceID, "--family", family, "--json"], nil))
                try Task.checkCancellation()
                guard next.profile.id == sourceID, next.inspectedFamily == family,
                      next.members.count == 1, next.members.first?.family == family else {
                    throw ClientError.message("The backend returned a different workload. Reload the group before trying again.")
                }
                guard next.revision == revision else {
                    throw ClientError.message("This group changed. Reload it to review the current members.")
                }
                if memberGeneration == generation && selectedID == sourceID { memberDetail = next }
            } catch is CancellationError {
                // An obsolete member read must not replace another selection.
            } catch {
                if memberGeneration == generation && selectedID == sourceID { memberError = error.localizedDescription }
            }
        }
        memberTask = task
        return task
    }

    @discardableResult
    func loadMembers(offset: Int = 0) -> Task<Void, Never>? {
        guard !fixtureMode, !isMutating, !isLoading, !isDetailLoading, !isMemberPageLoading, let detail else { return nil }
        let query = memberQuery.trimmingCharacters(in: .whitespacesAndNewlines)
        guard query.count <= 120, !query.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else {
            memberError = "Search with up to 120 printable characters."
            return nil
        }
        let previous = memberTask
        closeMember()
        let generation = memberGeneration, sourceID = detail.profile.id, revision = detail.revision
        isMemberPageLoading = true
        let task = Task {
            defer { if memberGeneration == generation { isMemberPageLoading = false } }
            do {
                await previous?.value
                try Task.checkCancellation()
                let next = try JSONDecoder().decode(LearningDetail.self, from: try await command(
                    ["profiles", "show", sourceID, "--members-query", query, "--members-offset", String(max(0, offset)), "--json"], nil))
                try Task.checkCancellation()
                guard next.profile.id == sourceID, next.revision == revision else {
                    throw ClientError.message("This group changed. Reload it to review the latest members; your draft is preserved.")
                }
                guard next.memberPage?.offset == max(0, offset), next.memberPage?.query == query else {
                    throw ClientError.message("The backend did not return the requested member page. Update the app and reload the group.")
                }
                if memberGeneration == generation && selectedID == sourceID {
                    self.detail = next // Preserve unsaved name and reservation fields.
                    memberQuery = query
                }
            } catch is CancellationError {
            } catch {
                if memberGeneration == generation && selectedID == sourceID { memberError = error.localizedDescription }
            }
        }
        memberTask = task
        return task
    }

    var canMergeSelection: Bool {
        !fixtureMode && !isLoading && !isDetailLoading && !isMutating && !isMemberPageLoading && !mergeSelection.isEmpty
    }
    var hasUnsavedCorrections: Bool {
        guard let detail else { return false }
        guard let edit = try? editPayload() else { return true }
        return edit.label != detail.profile.label || edit.memoryFloorBytes != detail.profile.memoryFloorBytes || edit.cpuFloorCores != detail.profile.cpuFloorCores
    }
    func toggleMergeItem(_ item: LearningMergeItem) {
        guard !fixtureMode, !isLoading, !isMutating else { return }
        if let index = mergeSelection.firstIndex(where: { $0.id == item.id }) { mergeSelection.remove(at: index); return }
        guard mergeSelection.count < 32 else { error = "Select at most 32 groups or workloads per merge."; return }
        guard mergeSelection.first?.revision == nil || mergeSelection.first?.revision == item.revision else {
            error = "Learning changed. Clear the selection, reload and review the items again."; return
        }
        mergeSelection.append(item)
    }
    func clearMergeSelection() { mergeSelection = [] }
    @discardableResult
    func combine(into targetID: String, reviewed: [LearningMergeItem]? = nil) -> Task<Void, Never>? {
        guard canMergeSelection, let revision = mergeSelection.first?.revision else { return nil }
        guard !hasUnsavedCorrections else { error = "Save your correction draft before merging the selection."; return nil }
        guard reviewed == nil || reviewed == mergeSelection else { error = "The merge selection changed. Review it again before confirming."; return nil }
        guard targetID.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil else { return nil }
        let profiles = mergeSelection.filter { $0.isGroup && $0.identity != targetID }.map(\.identity)
        let families = mergeSelection.filter { !$0.isGroup }.map(\.identity)
        guard !profiles.isEmpty || !families.isEmpty else { error = "Select another group or workload for this destination."; return nil }
        guard !mergeSelection.contains(where: { !$0.isGroup && $0.sourceGroup == targetID }) else {
            error = "A selected workload already belongs to this destination. Remove it from the selection first."; return nil
        }
        do {
            return mutate("combine", payload: try JSONEncoder().encode(LearningCombine(revision: revision, profileIds: profiles, families: families)),
                success: "Selected groups and workloads merged. History and minimum reservations are preserved.", changesSelection: true,
                expectedTarget: targetID, requestID: targetID)
        } catch { self.error = error.localizedDescription; return nil }
    }

    func changeMemoryUnit(_ unit: LearningMemoryUnit) {
        guard unit != memoryUnit else { return }
        let oldBytes = (try? parseNumber(draftMemory, name: "Minimum RAM")).map { $0 * memoryUnit.bytes }
        memoryUnit = unit
        if let oldBytes { draftMemory = Self.number(oldBytes / unit.bytes) }
    }

    func editPayload() throws -> LearningEdit {
        guard let detail else { throw ClientError.message("Choose a workload first.") }
        let label = draftLabel.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !label.isEmpty, label.count <= 120, !label.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else {
            throw ClientError.message("Use a name of 1 to 120 printable characters.")
        }
        let memory = try parseNumber(draftMemory, name: "Minimum RAM")
        let cpu = try parseNumber(draftCPU, name: "Minimum CPU")
        let memoryBytes = memory.map { $0 * memoryUnit.bytes }
        if let memoryBytes, memoryBytes < 1 || memoryBytes > 1_125_899_906_842_624 {
            throw ClientError.message("Minimum RAM must be between 1 byte and 1 PiB, or blank for automatic.")
        }
        if let cpu, cpu > 1024 { throw ClientError.message("Minimum CPU must be at most 1024 cores, or blank for automatic.") }
        let memoryUnchanged = detail.profile.memoryFloorBytes.map {
            draftMemory.trimmingCharacters(in: .whitespacesAndNewlines) == Self.number(Double($0) / memoryUnit.bytes)
        } ?? false
        let cpuUnchanged = detail.profile.cpuFloorCores.map {
            draftCPU.trimmingCharacters(in: .whitespacesAndNewlines) == Self.number($0)
        } ?? false
        return LearningEdit(revision: detail.revision, label: label,
            memoryFloorBytes: memoryUnchanged ? detail.profile.memoryFloorBytes : memoryBytes.map { Int64($0.rounded()) },
            cpuFloorCores: cpuUnchanged ? detail.profile.cpuFloorCores : cpu)
    }

    func confirm(sourceID: String, revision: Int, action: () -> Void) {
        guard selectedID == sourceID, detail?.revision == revision else {
            error = "The selected workload changed. Review its details before confirming."
            notice = nil
            return
        }
        action()
    }

    @discardableResult
    func save() -> Task<Void, Never>? {
        guard canSave else { return nil }
        do { return mutate("edit", payload: try JSONEncoder().encode(editPayload()), success: "Workload corrections saved.") }
        catch { self.error = error.localizedDescription; notice = nil; return nil }
    }

    @discardableResult
    func merge(into targetID: String) -> Task<Void, Never>? {
        guard canSave, let detail, targetID != detail.profile.id,
              targetID.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil else { return nil }
        do {
            return mutate("merge", payload: try JSONEncoder().encode(LearningMerge(revision: detail.revision, targetId: targetID)),
                success: "Workloads now share learning.", changesSelection: true, expectedTarget: targetID)
        } catch { self.error = error.localizedDescription; return nil }
    }

    @discardableResult
    func split(family: String) -> Task<Void, Never>? {
        guard canSave, let detail, detail.profile.memberCount > 1,
              detail.members.contains(where: { $0.family == family }) else { return nil }
        do {
            return mutate("split", payload: try JSONEncoder().encode(LearningSplit(revision: detail.revision, family: family)),
                success: "Workload ungrouped. Its measurements are preserved and it stays out of automatic grouping.", changesSelection: true,
                returnGroup: LearningGroupReference(id: detail.profile.id, label: detail.profile.label))
        } catch { self.error = error.localizedDescription; return nil }
    }

    @discardableResult
    func reset() -> Task<Void, Never>? {
        guard canSave, let detail else { return nil }
        do {
            return mutate("reset", payload: try JSONEncoder().encode(LearningReset(revision: detail.revision, confirm: true)),
                success: "Measurements reset. Naming, grouping and minimum reservations are preserved.")
        } catch { self.error = error.localizedDescription; return nil }
    }

    private func mutate(_ operation: String, payload: Data, success: String, changesSelection: Bool = false, expectedTarget: String? = nil, returnGroup: LearningGroupReference? = nil, requestID: String? = nil) -> Task<Void, Never>? {
        guard !fixtureMode, !isMutating, let sourceID = requestID ?? selectedID else { return nil }
        let originalSelection = selectedID
        let sourceGeneration = selectionGeneration
        closeMember()
        isMutating = true
        error = nil
        notice = nil
        return Task {
            defer { isMutating = false }
            do {
                let response = try await command(["profiles", operation, sourceID, "--json"], payload)
                let result = try JSONDecoder().decode(LearningDetail.self, from: response)
                guard changesSelection || result.profile.id == sourceID,
                      expectedTarget == nil || result.profile.id == expectedTarget,
                      operation != "split" || result.profile.id != sourceID else {
                    throw ClientError.message("Controller returned a different workload. Reload before trying again.")
                }
                clearMergeSelection()
                if selectedID == originalSelection && selectionGeneration == sourceGeneration {
                    if changesSelection { selectedID = result.profile.id }
                    apply(result)
                    if let returnGroup { ungroupedFrom = returnGroup }
                    else if operation == "merge" || operation == "combine" { ungroupedFrom = nil }
                    notice = success
                }
                do { catalog = try await readCatalog(offset: catalog?.offset ?? 0, query: query) }
                catch { self.error = "The correction was saved, but the list could not refresh. \(error.localizedDescription)" }
            } catch {
                self.error = "Could not confirm the correction. \(error.localizedDescription) Reload to check whether it was applied before retrying. Your edits are preserved."
            }
        }
    }

    private func readCatalog(offset: Int, query: String) async throws -> LearningCatalog {
        try JSONDecoder().decode(LearningCatalog.self, from: try await command(
            ["profiles", "list", "--offset", String(offset), "--limit", String(pageSize), "--query", query, "--json"], nil))
    }
    private func readDetail(_ id: String) async throws -> LearningDetail {
        try JSONDecoder().decode(LearningDetail.self, from: try await command(["profiles", "show", id, "--json"], nil))
    }
    private func apply(_ next: LearningDetail) { closeMember(); detail = next; memberQuery = next.memberPage?.query ?? ""; restoreDraft(next) }
    private func clearDraft() { draftLabel = ""; draftMemory = ""; draftCPU = "" }
    private func restoreDraft(_ next: LearningDetail) {
        draftLabel = next.profile.label
        draftMemory = next.profile.memoryFloorBytes.map { Self.number(Double($0) / memoryUnit.bytes) } ?? ""
        draftCPU = next.profile.cpuFloorCores.map(Self.number) ?? ""
    }
    private func parseNumber(_ text: String, name: String) throws -> Double? {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return nil }
        let separator = Locale.current.decimalSeparator ?? "."
        let normalized = separator == "." ? trimmed : trimmed.replacingOccurrences(of: separator, with: ".")
        guard let value = Double(normalized), value.isFinite, value > 0 else {
            throw ClientError.message("\(name) must be a positive number, or blank for automatic.")
        }
        return value
    }
    private static func number(_ value: Double) -> String {
        if value != 0 && abs(value) < 0.000_000_001 { return String(value) }
        return value.formatted(.number.grouping(.never).precision(.fractionLength(0...12)))
    }

    nonisolated static func previewDetail() -> LearningDetail {
        let family = String(repeating: "b", count: 64)
        let variant = String(repeating: "c", count: 64)
        let profile = LearningProfile(id: String(repeating: "a", count: 64), label: "Web test suite", memberCount: 1,
            variantCount: 3, successCount: 18, incompleteCount: 2, peakMemoryBytes: 1_342_177_280,
            peakCpuCores: 1.6, lastRunAt: 1_790_000_000, memoryFloorBytes: 2_147_483_648, cpuFloorCores: nil)
        return LearningDetail(revision: 4, profile: profile,
            members: [LearningMember(family: family, label: "Web tests", project: "Sample project", executable: "npm",
                measurements: LearningMeasurements(variantCount: 3, successCount: 18, incompleteCount: 2,
                    peakMemoryBytes: 1_342_177_280, peakCpuCores: 1.6, lastRunAt: 1_790_000_000))],
            variants: [LearningVariant(id: variant, family: family, successCount: 18, incompleteCount: 2,
                lastRunAt: 1_790_000_000, peakMemoryBytes: 1_342_177_280, peakCpuCores: 1.6)],
            runs: [LearningRun(id: String(repeating: "d", count: 64), family: family, variant: variant, machine: "preview-machine", sampledAt: 1_790_000_000,
                memoryBytes: 1_342_177_280, cpuCores: 1.6, cpuSeconds: 42, durationSeconds: 30, success: true),
                LearningRun(id: String(repeating: "e", count: 64), family: family, variant: variant, machine: "preview-machine", sampledAt: 1_789_999_000,
                    memoryBytes: 1_073_741_824, cpuCores: 1.2, cpuSeconds: 20, durationSeconds: 24, success: false)],
            prediction: ResourcePrediction(memoryBytes: 2_147_483_648, cpuCores: 2.4, durationSeconds: 45,
                samples: 0, confidence: "low", source: "family"), predictionMachine: "preview-machine",
            truncated: LearningTruncation(members: false, variants: false, runs: false),
            controllerAppliedRevision: 4, controllerStatus: "applied")
    }
}

private enum LearningConfirmation: Identifiable {
    case merge(id: String, label: String), combine(id: String, label: String, items: [LearningMergeItem]), split(family: String, label: String), reset
    var id: String {
        switch self { case .merge(let id, _): "merge-\(id)"; case .combine(let id, _, _): "combine-\(id)"; case .split(let family, _): "split-\(family)"; case .reset: "reset" }
    }
    var title: String {
        switch self { case .merge: "Share learning?"; case .combine: "Merge selected items?"; case .split: "Ungroup this workload?"; case .reset: "Reset measured history?" }
    }
    var message: String {
        switch self {
        case .merge(_, let label):
            "Move this workload's job families into ‘\(label)’. Their future predictions will share conservative learning. Commands and exact-variant success counts stay independent."
        case .combine(_, let label, let items):
            "Merge \(items.count) selected groups and workloads into ‘\(label)’. Keep its name and preserve the highest minimum reservations. All matching history moves together and future runs share learning. Unselected workloads stay in their source group. Active jobs continue. Selected: \(items.prefix(3).map(\.label).joined(separator: ", "))\(items.count > 3 ? " and \(items.count - 3) more" : ""). Review the full list before confirming."
        case .split(_, let label):
            "Move ‘\(label)’ to its own group with all of its variants and measurements. It will stay excluded from automatic grouping. Active jobs continue."
        case .reset:
            "Remove this workload's measured history and return it to conservative cold-start learning. Names, grouping and minimum reservations stay in place. Active jobs continue and can add new measurements."
        }
    }
    var actionLabel: String {
        switch self { case .merge: "Share learning"; case .combine: "Merge selected items"; case .split: "Ungroup workload"; case .reset: "Reset measurements" }
    }
}

struct LearningWindow: View {
    @ObservedObject var store: LearningStore
    var onDone: () -> Void
    @State private var confirmation: LearningConfirmation?
    @State private var choosingTarget = false
    @State private var pickingSelection = false
    @State private var pendingConfirmation: LearningConfirmation?
    @State private var confirmationSource: String?
    @State private var confirmationRevision: Int?

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Learned workloads").font(.title2.weight(.semibold))
                    Text("Inspect measured costs and correct how future jobs share learning.")
                        .font(.callout).foregroundStyle(.secondary)
                }
                Spacer()
                if store.fixtureMode { Text("PREVIEW").font(.caption).foregroundStyle(.secondary) }
                Button("Done", action: onDone).keyboardShortcut(.defaultAction)
            }.padding(20)
            Divider()
            if !store.mergeSelection.isEmpty {
                selectionPanel
                Divider()
            }
            if let error = store.error {
                HStack(alignment: .top) {
                    Label(error, systemImage: "exclamationmark.triangle").textSelection(.enabled)
                    Spacer()
                    Button("Reload selected") { store.reloadSelection() }.disabled(store.isMutating || store.fixtureMode)
                        .help("Reload the current workload and discard its unsaved edits")
                }.font(.callout).padding(12).background(Color.orange.opacity(0.1))
                Divider()
            }
            if let notice = store.notice {
                Label(notice, systemImage: "checkmark.circle").font(.callout)
                    .frame(maxWidth: .infinity, alignment: .leading).padding(12).background(Color.green.opacity(0.08))
                Divider()
            }
            HSplitView {
                catalog.frame(minWidth: 250, idealWidth: 280, maxWidth: 340)
                ScrollView {
                    if let detail = store.detail {
                        detailView(detail).padding(20)
                    } else if store.isDetailLoading || store.isLoading {
                        ProgressView("Loading workload…").frame(maxWidth: .infinity, minHeight: 160)
                    } else if store.selectedID != nil {
                        ContentUnavailableView("Workload unavailable", systemImage: "exclamationmark.triangle",
                            description: Text("Reload the selected workload to try again."))
                            .frame(maxWidth: .infinity, minHeight: 250)
                    } else {
                        ContentUnavailableView(store.query.isEmpty ? "No learned workloads" : "No matching workloads", systemImage: "chart.bar.doc.horizontal",
                            description: Text(store.query.isEmpty ? "Run jobs with resource monitoring enabled to build a local history." : "Try another search or refresh the list."))
                            .frame(maxWidth: .infinity, minHeight: 250)
                    }
                }.frame(minWidth: 480, maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .task { if store.catalog == nil { await store.loadPage()?.value } }
        .sheet(isPresented: $choosingTarget, onDismiss: {
            if let pendingConfirmation {
                confirmation = pendingConfirmation
                self.pendingConfirmation = nil
            }
        }) {
            LearningTargetPicker(sourceID: pickingSelection ? "" : store.selectedID ?? "", fixtureMode: store.fixtureMode) { id, label in
                pendingConfirmation = pickingSelection ? .combine(id: id, label: label, items: store.mergeSelection) : .merge(id: id, label: label)
                choosingTarget = false
            } onCancel: { choosingTarget = false }
        }
        .alert(confirmation?.title ?? "Confirm change", isPresented: Binding(
            get: { confirmation != nil }, set: { if !$0 { confirmation = nil } })) {
                if let confirmation {
                    Button(confirmation.actionLabel, role: confirmation.id == "reset" ? .destructive : nil) {
                        if case .combine(let id, _, let items) = confirmation {
                            store.combine(into: id, reviewed: items)
                        } else if let confirmationSource, let confirmationRevision {
                            store.confirm(sourceID: confirmationSource, revision: confirmationRevision) {
                                switch confirmation {
                                case .merge(let id, _): store.merge(into: id)
                                case .combine: break
                                case .split(let family, _): store.split(family: family)
                                case .reset: store.reset()
                                }
                            }
                        }
                        self.confirmation = nil
                    }
                }
                Button("Cancel", role: .cancel) { confirmation = nil }
            } message: { Text(confirmation?.message ?? "") }
    }

    private var selectionPanel: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("\(store.mergeSelection.count) selected across groups and pages").font(.callout)
                Spacer()
                Button("Merge selected…") { pickingSelection = true; choosingTarget = true }.disabled(!store.canMergeSelection)
                Button("Clear selection") { store.clearMergeSelection() }.disabled(store.isMutating)
            }
            DisclosureGroup("Review selected items") {
                ForEach(store.mergeSelection) { item in
                    HStack {
                        Text("\(item.isGroup ? "Group" : "Workload"): \(item.label)").font(.caption)
                        Spacer()
                        Button("Remove") { store.toggleMergeItem(item) }.help("Remove \(item.label) from the merge selection")
                    }.disabled(store.isMutating)
                }
            }
        }.padding(12)
    }

    private var catalog: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                TextField("Search workloads", text: $store.query).textFieldStyle(.roundedBorder)
                    .onSubmit { store.loadPage() }
                Button("Search", systemImage: "magnifyingglass") { store.loadPage() }
                    .labelStyle(.iconOnly).help("Search workload names and safe job descriptions")
            }.disabled(store.fixtureMode || store.isLoading || store.isMutating)
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 4) {
                    ForEach(store.catalog?.profiles ?? []) { profile in
                        HStack(alignment: .top, spacing: 6) {
                            mergeToggle(LearningMergeItem(identity: profile.id, label: profile.label, isGroup: true, sourceGroup: profile.id, revision: store.catalog?.revision ?? 0))
                            Button { store.select(profile.id) } label: {
                                VStack(alignment: .leading, spacing: 5) {
                                    Text(profile.label).font(.headline).lineLimit(2)
                                    Text("\(profile.memberCount) families · \(profile.variantCount) variants")
                                    Text("\(profile.successCount) successful · \(profile.incompleteCount) incomplete")
                                }.font(.caption).frame(maxWidth: .infinity, alignment: .leading).padding(10)
                                    .contentShape(Rectangle())
                                    .background(store.selectedID == profile.id ? Color.accentColor.opacity(0.13) : Color.clear)
                                    .clipShape(RoundedRectangle(cornerRadius: 7))
                            }.buttonStyle(.plain).disabled(store.fixtureMode)
                        }
                    }
                }
            }
            Divider()
            HStack {
                Button("Previous", systemImage: "chevron.left") {
                    store.loadPage(offset: max(0, (store.catalog?.offset ?? 0) - store.pageSize))
                }.labelStyle(.iconOnly).disabled(!store.hasPrevious)
                Spacer()
                Text(store.pageLabel).font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button("Next", systemImage: "chevron.right") {
                    store.loadPage(offset: (store.catalog?.offset ?? 0) + store.pageSize)
                }.labelStyle(.iconOnly).disabled(!store.hasNext)
            }.disabled(store.fixtureMode || store.isLoading || store.isMutating)
            Button("Refresh list", systemImage: "arrow.clockwise") {
                store.loadPage(offset: store.catalog?.offset ?? 0, preserveSelection: true)
            }.disabled(store.fixtureMode || store.isLoading || store.isMutating)
        }.padding(16)
    }

    private func detailView(_ detail: LearningDetail) -> some View {
        VStack(alignment: .leading, spacing: 22) {
            if let group = store.ungroupedFrom, group.id != detail.profile.id {
                Button("Back to group: \(group.label)", systemImage: "chevron.left") { store.select(group.id) }
                    .disabled(store.isMutating)
            }
            VStack(alignment: .leading, spacing: 7) {
                Text(detail.profile.label).font(.title3.weight(.semibold)).textSelection(.enabled)
                Text("\(detail.profile.memberCount) job families · \(detail.profile.variantCount) variants")
                    .font(.callout).foregroundStyle(.secondary)
                Text("Jobs match a worktree-independent family. Families in this profile share conservative estimates; exact variants keep their own success counts.")
                    .font(.caption).foregroundStyle(.secondary)
                if let message = store.controllerMessage {
                    Label(message, systemImage: "arrow.triangle.2.circlepath").font(.caption).foregroundStyle(.secondary)
                }
            }
            measuredSection(detail)
            predictionSection(detail)
            membershipSection(detail)
            editorSection(detail)
            variantSection(detail)
            runsSection(detail)
            Divider()
            HStack {
                Button("Reset measurements…", role: .destructive) { request(.reset) }
                    .disabled(!store.canSave)
                Spacer()
                Button("Reload selected", systemImage: "arrow.clockwise") { store.reloadSelection() }
                    .disabled(store.fixtureMode || store.isMutating)
                    .help("Discard unsaved edits and read the current workload")
            }
        }.frame(maxWidth: .infinity, alignment: .leading)
    }

    private func measuredSection(_ detail: LearningDetail) -> some View {
        VStack(alignment: .leading, spacing: 9) {
            Text("Measured history").font(.headline)
            Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 8) {
                row("Successful runs", String(detail.profile.successCount))
                row("Incomplete or failed", String(detail.profile.incompleteCount))
                row("Peak RAM", resourceBytes(detail.profile.peakMemoryBytes))
                row("Peak CPU", resourceCores(detail.profile.peakCpuCores))
                if let lastRun = detail.profile.lastRunAt { row("Latest observation", learningDate(lastRun)) }
            }.font(.callout)
            Text("Measured peaks span retained machine histories. Incomplete runs can raise reservations, but never count as successful evidence.")
                .font(.caption).foregroundStyle(.secondary)
        }
    }

    private func predictionSection(_ detail: LearningDetail) -> some View {
        VStack(alignment: .leading, spacing: 9) {
            Text("Group estimate on this Mac").font(.headline)
            if let prediction = detail.prediction {
                Text("\(resourceBytes(prediction.memoryBytes)) · \(resourceCores(prediction.cpuCores))").font(.callout)
                Text("\(prediction.confidence.capitalized) confidence · \(prediction.samples) successful exact-variant runs · \(prediction.source)")
                    .font(.caption).foregroundStyle(.secondary)
                if let duration = prediction.durationSeconds {
                    Text("Expected duration \(duration.formatted(.number.precision(.fractionLength(0...1)))) seconds").font(.caption)
                }
                Text("This group envelope is conservative. A particular job can use its exact variant's better-established history.")
                    .font(.caption).foregroundStyle(.secondary)
            } else {
                Text("Unavailable until a current machine identity is known. No estimate has been invented.")
                    .font(.callout).foregroundStyle(.secondary)
            }
            Text("Operator minimums: RAM \(detail.profile.memoryFloorBytes.map(learningMemoryFloor) ?? "automatic") · CPU \(detail.profile.cpuFloorCores.map(learningCPUFloor) ?? "automatic")")
                .font(.caption).textSelection(.enabled)
        }.padding(14).frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.accentColor.opacity(0.06)).clipShape(RoundedRectangle(cornerRadius: 8))
    }

    private func editorSection(_ detail: LearningDetail) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Corrections for future jobs").font(.headline)
            LabeledContent("Name") { TextField("Workload name", text: $store.draftLabel).textFieldStyle(.roundedBorder) }
            HStack {
                Text("Minimum RAM").frame(width: 110, alignment: .leading)
                TextField("Automatic", text: $store.draftMemory).textFieldStyle(.roundedBorder)
                Picker("RAM unit", selection: Binding(get: { store.memoryUnit }, set: { store.changeMemoryUnit($0) })) {
                    ForEach(LearningMemoryUnit.allCases, id: \.self) { Text($0.rawValue).tag($0) }
                }.labelsHidden().frame(width: 80)
            }
            HStack {
                Text("Minimum CPU").frame(width: 110, alignment: .leading)
                TextField("Automatic", text: $store.draftCPU).textFieldStyle(.roundedBorder)
                Text("cores").font(.caption).foregroundStyle(.secondary).frame(width: 70)
            }
            Text("Minimums only raise the learned reservation. Leave a field blank to return it to automatic. Measurements stay unchanged.")
                .font(.caption).foregroundStyle(.secondary)
            HStack {
                Button("Save corrections") { store.save() }.disabled(!store.canSave)
                if store.isMutating { ProgressView().controlSize(.small); Text("Applying correction…").font(.caption) }
            }
        }.disabled(store.fixtureMode || store.isMutating)
    }

    private func membershipSection(_ detail: LearningDetail) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Group members").font(.headline)
                Spacer()
                Button("Share learning with…") {
                    confirmationSource = store.selectedID
                    confirmationRevision = store.detail?.revision
                    pickingSelection = false
                    choosingTarget = true
                }.disabled(!store.canSave)
            }
            Text("Expand a workload to inspect its own measurements. Ungroup mismatches to keep their learning independent.")
                .font(.caption).foregroundStyle(.secondary)
            HStack {
                TextField("Find a member in this group", text: $store.memberQuery).textFieldStyle(.roundedBorder)
                    .onSubmit { store.loadMembers() }
                Button("Search members") { store.loadMembers() }
            }.disabled(store.fixtureMode || store.isMutating || store.isMemberPageLoading)
            if let error = store.memberError, store.inspectedFamily == nil {
                Text(error).font(.caption).foregroundStyle(.orange)
            }
            if store.isMemberPageLoading { ProgressView("Loading group members…").controlSize(.small) }
            ForEach(detail.members) { member in
                HStack(alignment: .top, spacing: 8) {
                    mergeToggle(LearningMergeItem(identity: member.family, label: member.label, isGroup: false, sourceGroup: detail.profile.id, revision: detail.revision))
                    memberRow(member, group: detail)
                }
            }
            if detail.members.isEmpty { Text("No members match this search. Try another name or clear the search.").font(.caption).foregroundStyle(.secondary) }
            if let page = detail.memberPage {
                HStack {
                    Button("Previous members") { store.loadMembers(offset: max(0, page.offset - page.limit)) }
                        .disabled(page.offset == 0)
                    Text(detail.members.isEmpty ? "No members on this page" : "\(page.offset + 1)–\(page.offset + detail.members.count) of \(page.total) members")
                        .font(.caption).foregroundStyle(.secondary)
                    Button("Next members") { store.loadMembers(offset: page.offset + page.limit) }
                        .disabled(page.offset + detail.members.count >= page.total)
                }.disabled(store.fixtureMode || store.isMutating || store.isMemberPageLoading)
            } else if detail.truncated.members {
                Text("Update the app to search and page through every group member.").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private func mergeToggle(_ item: LearningMergeItem) -> some View {
        Toggle("Select \(item.isGroup ? "group" : "workload"): \(item.label)", isOn: Binding(
            get: { store.mergeSelection.contains { $0.id == item.id } },
            set: { _ in store.toggleMergeItem(item) }))
            .toggleStyle(.checkbox).labelsHidden()
            .accessibilityLabel("Select \(item.isGroup ? "group" : "workload"): \(item.label)")
            .help("Select for merging. Workloads include all their run history and future matching runs.")
            .disabled(store.fixtureMode || store.isLoading || store.isMutating)
    }

    private func memberRow(_ member: LearningMember, group: LearningDetail) -> some View {
        DisclosureGroup(isExpanded: Binding(get: { store.inspectedFamily == member.family }, set: { expanded in
            if expanded { store.inspectMember(member.family) }
            else if store.inspectedFamily == member.family { store.closeMember() }
        })) {
            memberEvidence(member, group: group)
        } label: {
            VStack(alignment: .leading, spacing: 5) {
                Text(member.label).font(.callout.weight(.medium))
                Text([member.project, member.executable].filter { !$0.isEmpty }.joined(separator: " · "))
                    .font(.caption).foregroundStyle(.secondary)
                if let stats = member.measurements {
                    Text("\(stats.successCount) successful · \(stats.incompleteCount) incomplete or failed · \(resourceBytes(stats.peakMemoryBytes)) peak · \(resourceCores(stats.peakCpuCores))")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        }.padding(.vertical, 5).disabled(store.isMutating || store.isDetailLoading || store.isMemberPageLoading)
    }

    private func memberEvidence(_ member: LearningMember, group: LearningDetail) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            if store.isMemberLoading {
                ProgressView("Loading workload measurements…").controlSize(.small)
            } else if let error = store.memberError {
                Text(error).font(.caption).foregroundStyle(.orange)
                Button("Retry inspection") { store.inspectMember(member.family) }
            } else if let inspected = store.memberDetail, inspected.inspectedFamily == member.family {
                Text("Measurements for this workload").font(.callout.weight(.semibold))
                if let stats = inspected.members.first?.measurements {
                    Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 6) {
                        row("Successful runs", String(stats.successCount))
                        row("Incomplete or failed", String(stats.incompleteCount))
                        row("Variants", String(stats.variantCount))
                        row("Peak RAM", resourceBytes(stats.peakMemoryBytes))
                        row("Peak CPU", resourceCores(stats.peakCpuCores))
                        if let last = stats.lastRunAt { row("Last run", learningDate(last)) }
                    }.font(.caption)
                }
                Text("Only this workload's retained history is shown. Machine identifiers distinguish where each run was measured.")
                    .font(.caption).foregroundStyle(.secondary)
                variantSection(inspected)
                runsSection(inspected)
                Text("Workload ID: \(member.family)").font(.system(size: 10, design: .monospaced))
                    .foregroundStyle(.secondary).textSelection(.enabled)
                if group.profile.memberCount > 1 {
                    Button("Ungroup workload…") { request(.split(family: member.family, label: member.label)) }
                        .disabled(!store.canSave)
                }
            }
        }.padding(.top, 10).frame(maxWidth: .infinity, alignment: .leading)
    }

    private func variantSection(_ detail: LearningDetail) -> some View {
        let count = detail.inspectedFamily == nil ? detail.profile.variantCount : detail.members.first?.measurements?.variantCount ?? detail.variants.count
        return DisclosureGroup("Variants (\(count))") {
            VStack(alignment: .leading, spacing: 10) {
                ForEach(detail.variants) { variant in
                    VStack(alignment: .leading, spacing: 4) {
                        Text("\(memberLabel(variant.family, in: detail)) · \(variant.id.prefix(14))…").font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                        Text("\(variant.successCount) successful · \(variant.incompleteCount) incomplete · \(resourceBytes(variant.peakMemoryBytes)) · \(resourceCores(variant.peakCpuCores))")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
                if detail.truncated.variants { truncated("variants") }
            }.padding(.top, 8).frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func runsSection(_ detail: LearningDetail) -> some View {
        DisclosureGroup("Recent measured runs (\(detail.runs.count))") {
            VStack(alignment: .leading, spacing: 12) {
                ForEach(detail.runs) { run in
                    VStack(alignment: .leading, spacing: 5) {
                        Text("\(memberLabel(run.family, in: detail)) · \(learningDate(run.sampledAt)) · \(run.success ? "Successful" : "Incomplete or failed")")
                            .font(.caption.weight(.medium))
                        Text("\(resourceBytes(run.memoryBytes)) · \(resourceCores(run.cpuCores)) · \(run.cpuSeconds.formatted(.number.precision(.fractionLength(0...1)))) CPU-seconds over \(run.durationSeconds.formatted(.number.precision(.fractionLength(0...1)))) seconds")
                            .font(.caption).foregroundStyle(.secondary)
                        Button("Select this run’s workload") {
                            store.toggleMergeItem(LearningMergeItem(identity: run.family, label: memberLabel(run.family, in: detail), isGroup: false, sourceGroup: detail.profile.id, revision: detail.revision))
                        }.disabled(store.fixtureMode || store.isMutating || store.isLoading)
                        Text("Variant \(run.variant.prefix(12))… · machine \(run.machine.prefix(12))…")
                            .font(.system(size: 10, design: .monospaced)).foregroundStyle(.secondary)
                    }
                }
                if detail.runs.isEmpty { Text("No retained measurements.").font(.caption).foregroundStyle(.secondary) }
                if detail.truncated.runs { truncated("runs") }
            }.padding(.top, 8).frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func request(_ value: LearningConfirmation) {
        confirmationSource = store.selectedID
        confirmationRevision = store.detail?.revision
        confirmation = value
    }

    private func memberLabel(_ family: String, in detail: LearningDetail) -> String {
        detail.members.first(where: { $0.family == family })?.label ?? "Workload \(family.prefix(12))"
    }

    private func row(_ title: String, _ value: String) -> some View {
        GridRow { Text(title).foregroundStyle(.secondary); Text(value).textSelection(.enabled) }
    }
    private func truncated(_ noun: String) -> some View {
        Text("Showing a bounded selection of retained \(noun).").font(.caption).foregroundStyle(.secondary)
    }
}

private struct LearningTargetPicker: View {
    let sourceID: String
    let onChoose: (String, String) -> Void
    let onCancel: () -> Void
    @StateObject private var store: LearningStore

    init(sourceID: String, fixtureMode: Bool, onChoose: @escaping (String, String) -> Void, onCancel: @escaping () -> Void) {
        self.sourceID = sourceID
        self.onChoose = onChoose
        self.onCancel = onCancel
        _store = StateObject(wrappedValue: LearningStore(fixtureMode: fixtureMode))
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Share learning with a workload").font(.title3.weight(.semibold))
            Text("Choose the target profile. You will confirm the change before any family is moved.")
                .font(.callout).foregroundStyle(.secondary)
            HStack {
                TextField("Search target workloads", text: $store.query).textFieldStyle(.roundedBorder).onSubmit { store.loadPage() }
                Button("Search") { store.loadPage() }
            }.disabled(store.isLoading || store.fixtureMode)
            if let error = store.error { Text(error).font(.caption).foregroundStyle(.orange) }
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 8) {
                    ForEach((store.catalog?.profiles ?? []).filter { $0.id != sourceID }) { profile in
                        HStack {
                            VStack(alignment: .leading) {
                                Text(profile.label).font(.headline)
                                Text("\(profile.memberCount) families · \(profile.successCount) successful observations").font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Button("Choose") { onChoose(profile.id, profile.label) }.disabled(store.fixtureMode)
                        }.padding(8)
                    }
                }
            }.frame(minHeight: 160)
            HStack {
                Button("Previous") { store.loadPage(offset: max(0, (store.catalog?.offset ?? 0) - store.pageSize)) }.disabled(!store.hasPrevious)
                Text(store.pageLabel).font(.caption).foregroundStyle(.secondary)
                Button("Next") { store.loadPage(offset: (store.catalog?.offset ?? 0) + store.pageSize) }.disabled(!store.hasNext)
                Spacer()
                Button("Cancel", action: onCancel).keyboardShortcut(.cancelAction)
            }.disabled(store.isLoading)
        }.padding(22).frame(width: 560, height: 430)
            .task { if store.catalog == nil { await store.loadPage()?.value } }
    }
}

private func learningDate(_ timestamp: Double) -> String {
    Date(timeIntervalSince1970: timestamp).formatted(date: .abbreviated, time: .shortened)
}

func learningCPUFloor(_ value: Double) -> String {
    "\(String(value)) CPU cores"
}

func learningMemoryFloor(_ value: Int64) -> String {
    if value < 1_048_576 { return "\(value.formatted()) bytes" }
    if value < 1_073_741_824 {
        return "\((Double(value) / 1_048_576).formatted(.number.precision(.fractionLength(0...2)))) MiB"
    }
    return resourceBytes(value)
}
