import AppKit
import Foundation

private let sourceID = String(repeating: "a", count: 64)
private let targetID = String(repeating: "f", count: 64)
private let splitID = String(repeating: "1", count: 64)
private let familyID = String(repeating: "b", count: 64)

private struct LearningCall: Sendable {
    let arguments: [String]
    let input: Data?
}

private struct EditInput: Decodable {
    let revision: Int
    let label: String
    let memoryFloorBytes: Int64?
    let cpuFloorCores: Double?
    let explicitMemoryNull: Bool
    let explicitCPUNull: Bool
    enum CodingKeys: String, CodingKey { case revision, label, memoryFloorBytes, cpuFloorCores }
    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        revision = try values.decode(Int.self, forKey: .revision)
        label = try values.decode(String.self, forKey: .label)
        memoryFloorBytes = try values.decodeIfPresent(Int64.self, forKey: .memoryFloorBytes)
        cpuFloorCores = try values.decodeIfPresent(Double.self, forKey: .cpuFloorCores)
        explicitMemoryNull = try values.contains(.memoryFloorBytes) && values.decodeNil(forKey: .memoryFloorBytes)
        explicitCPUNull = try values.contains(.cpuFloorCores) && values.decodeNil(forKey: .cpuFloorCores)
    }
}
private struct CombineInput: Decodable { let revision: Int; let profileIds: [String]; let families: [String] }
private struct MergeInput: Decodable { let revision: Int; let targetId: String }
private struct SplitInput: Decodable { let revision: Int; let family: String }
private struct ResetInput: Decodable { let revision: Int; let confirm: Bool }

private func testDetail(id: String = sourceID, revision: Int = 7, label: String = "Web tests",
                        memory: Int64? = 2_147_483_648, cpu: Double? = nil,
                        prediction: ResourcePrediction? = LearningStore.previewDetail().prediction) -> LearningDetail {
    let preview = LearningStore.previewDetail()
    let members = [LearningMember(family: familyID, label: "Web tests", project: "Demo", executable: "npm"),
        LearningMember(family: String(repeating: "2", count: 64), label: "Related tests", project: "Demo", executable: "npm")]
    return LearningDetail(revision: revision,
        profile: LearningProfile(id: id, label: label, memberCount: 2, variantCount: 3, successCount: 18,
            incompleteCount: 2, peakMemoryBytes: 1_342_177_280, peakCpuCores: 1.6, lastRunAt: 1_790_000_000,
            memoryFloorBytes: memory, cpuFloorCores: cpu),
        members: members, variants: preview.variants, runs: preview.runs, prediction: prediction,
        predictionMachine: prediction == nil ? nil : "fixture-machine", truncated: preview.truncated,
        controllerAppliedRevision: revision - 1, controllerStatus: "pending")
}

private actor LearningFixture {
    private(set) var calls: [LearningCall] = []
    var revision = 7
    var shouldFail = false
    var shouldFailRead = false
    private var shouldHold = false
    private var held: CheckedContinuation<Void, Never>?
    private var waiter: CheckedContinuation<Void, Never>?

    func holdNextCall() { shouldHold = true }
    func failNextMutation(_ value: Bool) { shouldFail = value }
    func failRead(_ value: Bool) { shouldFailRead = value }
    func waitUntilHeld() async {
        if held != nil { return }
        await withCheckedContinuation { waiter = $0 }
    }
    func release() { held?.resume(); held = nil }
    func load(_ arguments: [String], _ input: Data?) async throws -> Data {
        calls.append(LearningCall(arguments: arguments, input: input))
        if shouldHold {
            shouldHold = false
            await withCheckedContinuation {
                held = $0
                waiter?.resume()
                waiter = nil
            }
        }
        guard arguments.first == "profiles", arguments.count >= 3 else { throw ClientError.message("Unexpected command") }
        let encoder = JSONEncoder()
        let operation = arguments[1]
        if operation == "list" {
            let offset = arguments.firstIndex(of: "--offset").flatMap { Int(arguments[$0 + 1]) } ?? 0
            return try encoder.encode(LearningCatalog(revision: revision,
                profiles: [testDetail(revision: revision).profile, testDetail(id: targetID, revision: revision, label: "Build tests").profile],
                total: 104, offset: offset, limit: 50, controllerAppliedRevision: revision - 1, controllerStatus: "pending"))
        }
        if operation == "show" {
            if shouldFailRead { throw ClientError.message("Profile no longer available") }
            let detail = testDetail(id: arguments[2], revision: revision,
                label: arguments[2] == sourceID ? "Web tests" : "Build tests")
            if let index = arguments.firstIndex(of: "--family") {
                let family = arguments[index + 1]
                return try encoder.encode(LearningDetail(revision: revision, profile: detail.profile,
                    members: detail.members.filter { $0.family == family },
                    variants: detail.variants.filter { $0.family == family }, runs: detail.runs.filter { $0.family == family },
                    prediction: detail.prediction, predictionMachine: detail.predictionMachine, truncated: detail.truncated,
                    inspectedFamily: family))
            }
            if let index = arguments.firstIndex(of: "--members-query") {
                let query = arguments[index + 1]
                let offset = arguments.firstIndex(of: "--members-offset").flatMap { Int(arguments[$0 + 1]) } ?? 0
                return try encoder.encode(LearningDetail(revision: revision, profile: detail.profile,
                    members: detail.members.filter { query.isEmpty || $0.label.localizedCaseInsensitiveContains(query) },
                    variants: detail.variants, runs: detail.runs, prediction: detail.prediction,
                    predictionMachine: detail.predictionMachine, truncated: detail.truncated,
                    memberPage: LearningMemberPage(total: 1, offset: offset, limit: 256, query: query)))
            }
            return try encoder.encode(detail)
        }
        guard let input else { throw ClientError.message("Missing private mutation input") }
        if shouldFail { throw ClientError.message("This workload changed. Reload and review the latest revision.") }
        revision += 1
        switch operation {
        case "edit":
            let edit = try JSONDecoder().decode(EditInput.self, from: input)
            return try encoder.encode(testDetail(revision: revision, label: edit.label, memory: edit.memoryFloorBytes, cpu: edit.cpuFloorCores))
        case "combine": return try encoder.encode(testDetail(id: arguments[2], revision: revision, label: "Combined destination"))
        case "merge": return try encoder.encode(testDetail(id: targetID, revision: revision, label: "Build tests"))
        case "split": return try encoder.encode(testDetail(id: splitID, revision: revision, label: "Separated web tests"))
        case "reset": return try encoder.encode(testDetail(revision: revision))
        default: throw ClientError.message("Unexpected mutation")
        }
    }
}

@main
struct LearningTests {
    @MainActor static func main() async throws {
        func check(_ value: Bool, _ name: String) throws {
            FileHandle.standardOutput.write(Data("\(value ? "PASS" : "FAIL") \(name)\n".utf8))
            if !value { throw ClientError.message(name) }
        }
        guard let transparent = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: 2, pixelsHigh: 2,
            bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
            bytesPerRow: 0, bitsPerPixel: 0) else { throw ClientError.message("Could not create transparent capture fixture") }
        for x in 0..<2 { for y in 0..<2 { transparent.setColor(NSColor(deviceRed: 0, green: 0, blue: 0, alpha: 0), atX: x, y: y) } }
        let captured = try NativeSnapshotRenderer.png(from: transparent, background: NSColor.green.cgColor)
        guard let opaque = NSBitmapImageRep(data: captured), let background = opaque.colorAt(x: 0, y: 0)?.usingColorSpace(.sRGB) else {
            throw ClientError.message("Could not read rendered capture fixture")
        }
        try check(!opaque.hasAlpha && background.alphaComponent == 1 && background.greenComponent > 0.9,
            "Native snapshot rendering includes the window background and emits an opaque PNG")
        try check(LearningStore.previewDetail().prediction?.source == "family" &&
            LearningStore.previewDetail().prediction?.confidence == "low" && LearningStore.previewDetail().prediction?.samples == 0,
            "Learning preview uses a conservative group envelope without invented exact-variant confidence")
        let unusual = testDetail(memory: 1_125_899_906_842_623, cpu: 0.123456789012345)
        let unusualData = try JSONEncoder().encode(unusual)
        let precise = LearningStore(command: { _, _ in unusualData })
        await precise.select(sourceID)?.value
        let unchanged = try precise.editPayload()
        try check(unchanged.memoryFloorBytes == unusual.profile.memoryFloorBytes && unchanged.cpuFloorCores == unusual.profile.cpuFloorCores,
            "Renaming preserves unchanged byte and high-precision CPU floors exactly")
        precise.changeMemoryUnit(.mib)
        let converted = try precise.editPayload()
        try check(converted.memoryFloorBytes == unusual.profile.memoryFloorBytes,
            "Changing RAM display units preserves an odd byte count below the maximum floor")
        try check(learningCPUFloor(1e-20) == "1e-20 CPU cores" && learningCPUFloor(0.125) == "0.125 CPU cores",
            "Explicit CPU floor labels retain positive values and clear units without rounding to zero")
        try check(learningMemoryFloor(1).hasSuffix(" bytes") && learningMemoryFloor(1_048_576).hasSuffix(" MiB") &&
            learningMemoryFloor(1_073_741_824).hasSuffix(" GiB"),
            "Explicit RAM floor labels use byte or MiB units for values below one GiB")
        let tiny = testDetail(memory: 1, cpu: 1e-20)
        let tinyData = try JSONEncoder().encode(tiny)
        let tinyStore = LearningStore(command: { _, _ in tinyData })
        await tinyStore.select(sourceID)?.value
        let tinyEdit = try tinyStore.editPayload()
        try check(tinyEdit.memoryFloorBytes == 1 && tinyEdit.cpuFloorCores == 1e-20,
            "Small positive floors remain visible and editable without rounding to zero")

        let linkingFixture = LearningFixture()
        let linkingStore = LearningStore(command: { try await linkingFixture.load($0, $1) })
        await linkingFixture.holdNextCall()
        let catalogLoad = linkingStore.loadPage()
        await linkingFixture.waitUntilHeld()
        await linkingStore.select(targetID)?.value
        await linkingFixture.release()
        await catalogLoad?.value
        try check(linkingStore.selectedID == targetID && linkingStore.detail?.profile.id == targetID,
            "Initial catalog I/O cannot overwrite a profile selected through a job link")

        let fixture = LearningFixture()
        let store = LearningStore(command: { try await fixture.load($0, $1) })
        let openingCalls = await fixture.calls
        try check(openingCalls.isEmpty && store.catalog == nil, "Constructing learning state performs no implicit I/O")
        await store.loadPage()?.value
        let initialCalls = await fixture.calls
        try check(initialCalls.first?.arguments == ["profiles", "list", "--offset", "0", "--limit", "50", "--query", "", "--json"] &&
            initialCalls.allSatisfy { $0.input == nil }, "Catalog reads use bounded pagination without mutation input")
        try check(store.detail?.profile.id == sourceID && store.detail?.profile.successCount == 18 &&
            store.detail?.profile.incompleteCount == 2 && store.hasNext && !store.hasPrevious,
            "Catalog selection shows successful and incomplete measured history with next page")
        try check(store.controllerMessage?.contains("pending") == true && store.controllerMessage?.contains("next fresh pass") == true,
            "Pending controller revision is visible without claiming corrections are active")
        try check(store.draftMemory == "2" && store.draftCPU.isEmpty && store.draftLabel == "Web tests",
            "Operator minimums populate distinct editable fields and automatic remains blank")

        let inspectingFixture = LearningFixture()
        let inspecting = LearningStore(command: { try await inspectingFixture.load($0, $1) })
        await inspecting.select(sourceID)?.value
        inspecting.draftLabel = "Unsaved group name"
        await inspecting.inspectMember(familyID)?.value
        try check(inspecting.memberDetail?.inspectedFamily == familyID && inspecting.memberDetail?.members.count == 1 &&
            inspecting.memberDetail?.runs.allSatisfy { $0.family == familyID } == true && inspecting.draftLabel == "Unsaved group name",
            "Inspecting a member uses its own bounded history and preserves the group correction draft")
        try check(inspecting.inspectMember(String(repeating: "9", count: 64)) == nil,
            "A workload outside the visible group cannot be inspected")
        await inspectingFixture.failRead(true)
        await inspecting.inspectMember(familyID)?.value
        try check(inspecting.memberDetail == nil && inspecting.memberError?.contains("no longer available") == true && !inspecting.isMemberLoading,
            "Failed member reads expose retryable errors without stale evidence")
        await inspectingFixture.failRead(false)
        inspecting.memberQuery = "Related"
        await inspecting.loadMembers()?.value
        try check(inspecting.detail?.members.count == 1 && inspecting.detail?.members.first?.label == "Related tests" &&
            inspecting.draftLabel == "Unsaved group name" && inspecting.inspectedFamily == nil,
            "Searching all group members preserves unsaved corrections and closes the old inspection")
        inspecting.memberQuery = ""
        await inspecting.loadMembers()?.value
        await inspectingFixture.holdNextCall()
        let obsoleteMember = inspecting.inspectMember(familyID)
        await inspectingFixture.waitUntilHeld()
        let currentMember = inspecting.inspectMember(String(repeating: "2", count: 64))
        await inspectingFixture.release()
        await obsoleteMember?.value
        await currentMember?.value
        try check(inspecting.memberDetail?.inspectedFamily == String(repeating: "2", count: 64) && inspecting.memberError == nil,
            "A cancelled member read cannot replace a newer inspection")
        await inspectingFixture.holdNextCall()
        let obsoletePage = inspecting.loadMembers()
        await inspectingFixture.waitUntilHeld()
        await inspecting.select(targetID)?.value
        await inspectingFixture.release()
        await obsoletePage?.value
        try check(inspecting.detail?.profile.id == targetID && inspecting.memberDetail == nil && !inspecting.isMemberPageLoading,
            "A cancelled member page cannot overwrite a newly selected group")
        let inspectionCalls = await inspectingFixture.calls
        try check(inspectionCalls.allSatisfy { $0.input == nil && $0.arguments[1] == "show" } &&
            inspectionCalls.contains { $0.arguments == ["profiles", "show", sourceID, "--family", familyID, "--json"] },
            "Inspection and member search send only read commands with bounded explicit options")

        let bulkFixture = LearningFixture()
        let bulk = LearningStore(command: { try await bulkFixture.load($0, $1) })
        await bulk.select(sourceID)?.value
        let selectedGroup = LearningMergeItem(identity: sourceID, label: "Web group", isGroup: true, sourceGroup: sourceID, revision: 7)
        let selectedTarget = LearningMergeItem(identity: targetID, label: "Build group", isGroup: true, sourceGroup: targetID, revision: 7)
        let selectedWorkload = LearningMergeItem(identity: familyID, label: "Selected run workload", isGroup: false, sourceGroup: sourceID, revision: 7)
        bulk.toggleMergeItem(selectedGroup)
        bulk.toggleMergeItem(selectedTarget)
        bulk.toggleMergeItem(selectedWorkload)
        await bulk.select(targetID)?.value
        try check(bulk.mergeSelection.count == 3 && bulk.canMergeSelection,
            "Mixed group and run-workload selections persist when changing the visible group")
        let reviewed = bulk.mergeSelection
        bulk.toggleMergeItem(selectedWorkload)
        try check(bulk.combine(into: targetID, reviewed: reviewed) == nil && bulk.error?.contains("selection changed") == true,
            "A changed selection cannot execute an obsolete merge confirmation")
        bulk.toggleMergeItem(selectedWorkload)
        bulk.draftLabel = "Unsaved destination draft"
        try check(bulk.combine(into: targetID) == nil && bulk.mergeSelection.count == 3,
            "Bulk merge preserves and blocks on unsaved correction drafts")
        await bulk.reloadSelection()?.value
        await bulkFixture.failNextMutation(true)
        await bulk.combine(into: targetID)?.value
        try check(bulk.mergeSelection.count == 3 && bulk.error != nil,
            "A rejected bulk merge retains the reviewed items for recovery")
        await bulkFixture.failNextMutation(false)
        await bulk.combine(into: targetID)?.value
        let bulkCalls = await bulkFixture.calls
        guard let combineCall = bulkCalls.last(where: { $0.arguments[1] == "combine" }), let combineData = combineCall.input else {
            throw ClientError.message("Missing bulk merge input")
        }
        let combined = try JSONDecoder().decode(CombineInput.self, from: combineData)
        try check(combineCall.arguments == ["profiles", "combine", targetID, "--json"] && combined.revision == 7 &&
            combined.profileIds == [sourceID] && combined.families == [familyID] && bulk.mergeSelection.isEmpty &&
            bulk.detail?.profile.id == targetID && bulk.detail?.profile.label == "Combined destination",
            "One private mutation merges groups and run workloads, excludes the chosen destination, and follows its identity")
        bulk.toggleMergeItem(selectedGroup)
        bulk.toggleMergeItem(LearningMergeItem(identity: splitID, label: "Newer group", isGroup: true, sourceGroup: splitID, revision: 8))
        try check(bulk.mergeSelection.count == 1 && bulk.error?.contains("Learning changed") == true,
            "Selections from different learning revisions cannot be silently combined")
        bulk.clearMergeSelection()
        try check(bulk.mergeSelection.isEmpty, "Clearing the bulk selection removes stale items")

        store.query = "tests"
        await store.loadPage(offset: 50)?.value
        let pagedCalls = await fixture.calls
        try check(pagedCalls.contains { $0.arguments == ["profiles", "list", "--offset", "50", "--limit", "50", "--query", "tests", "--json"] } &&
            store.hasPrevious, "Search and previous/next request explicit bounded offsets")
        store.draftLabel = "Renamed tests"
        store.draftMemory = "3"
        store.draftCPU = "2.5"
        await fixture.holdNextCall()
        let saving = store.save()
        try check(saving != nil && store.isMutating && store.save() == nil && store.reset() == nil && store.loadPage() == nil,
            "Mutation reserves synchronously and blocks duplicate writes or list reloads")
        await fixture.waitUntilHeld()
        await fixture.release()
        await saving?.value
        let savedCalls = await fixture.calls
        guard let editCall = savedCalls.last(where: { $0.arguments[1] == "edit" }), let editData = editCall.input else {
            throw ClientError.message("Missing edit call")
        }
        let edit = try JSONDecoder().decode(EditInput.self, from: editData)
        try check(editCall.arguments == ["profiles", "edit", sourceID, "--json"] && edit.revision == 7 &&
            edit.label == "Renamed tests" && edit.memoryFloorBytes == 3_221_225_472 && edit.cpuFloorCores == 2.5,
            "Rename and numeric floors use reviewed revision and private JSON rather than argv")
        try check(store.detail?.revision == 8 && store.notice?.contains("saved") == true && !store.isMutating,
            "Successful correction refreshes detail and revision before reporting success")

        store.draftMemory = ""
        store.draftCPU = ""
        await store.save()?.value
        let clearedCalls = await fixture.calls
        let clearedData = clearedCalls.last(where: { $0.arguments[1] == "edit" })?.input ?? Data()
        let cleared = try JSONDecoder().decode(EditInput.self, from: clearedData)
        try check(cleared.explicitMemoryNull && cleared.explicitCPUNull && store.detail?.profile.memoryFloorBytes == nil,
            "Clearing fields sends explicit null minimums to restore automatic prediction")

        store.draftMemory = "NaN"
        try check(store.save() == nil && store.error?.contains("positive number") == true,
            "Nonfinite floors are rejected before I/O")
        store.draftMemory = "-1"
        try check(store.save() == nil, "Negative minimum reservations cannot be submitted")
        store.draftMemory = "0.000000000001"
        try check(store.save() == nil, "Sub-byte memory floors cannot silently round down to zero")
        store.draftMemory = "1048577"
        try check(store.save() == nil && store.error?.contains("1 PiB") == true, "Oversized RAM floor is rejected")
        store.draftMemory = ""
        store.draftCPU = "1025"
        try check(store.save() == nil && store.error?.contains("1024") == true, "Oversized CPU floor is rejected")
        store.draftCPU = ""
        store.draftLabel = "bad\nname"
        try check(store.save() == nil && store.error?.contains("printable") == true, "Multiline labels are rejected")
        store.draftLabel = String(repeating: "a", count: 121)
        try check(store.save() == nil, "Labels are bounded before private input encoding")
        store.draftLabel = "Unsaved correction"
        store.draftMemory = "4"
        await fixture.failNextMutation(true)
        await store.save()?.value
        try check(store.error?.contains("changed") == true && store.notice == nil && store.draftLabel == "Unsaved correction" &&
            store.draftMemory == "4" && store.detail?.revision == 9,
            "Stale or failed correction preserves unsaved drafts and never reports success")
        await fixture.failNextMutation(false)
        await store.reloadSelection()?.value
        try check(store.draftLabel == "Web tests" && store.error == nil,
            "Explicit reload obtains the reviewed current version and replaces stale drafts")

        await store.merge(into: targetID)?.value
        let mergedCalls = await fixture.calls
        guard let mergeCall = mergedCalls.last(where: { $0.arguments[1] == "merge" }), let mergeData = mergeCall.input else {
            throw ClientError.message("Missing merge call")
        }
        let merge = try JSONDecoder().decode(MergeInput.self, from: mergeData)
        try check(mergeCall.arguments == ["profiles", "merge", sourceID, "--json"] && merge.targetId == targetID && merge.revision == 9 &&
            store.selectedID == targetID && store.detail?.profile.id == targetID,
            "Sharing learning sends source revision and selects the returned target profile")
        try check(store.merge(into: targetID) == nil, "A workload cannot share learning with itself")

        await store.split(family: familyID)?.value
        let splitCalls = await fixture.calls
        let splitData = splitCalls.last(where: { $0.arguments[1] == "split" })?.input ?? Data()
        let split = try JSONDecoder().decode(SplitInput.self, from: splitData)
        try check(split.family == familyID && split.revision == 10 && store.selectedID == splitID &&
            store.detail?.profile.id == splitID && store.ungroupedFrom?.id == targetID, "Separating a member sends its family identity and follows the newly returned profile")
        try check(store.split(family: String(repeating: "9", count: 64)) == nil,
            "A family outside the selected profile cannot be split")
        await store.select(sourceID)?.value
        await store.reset()?.value
        let resetCalls = await fixture.calls
        let resetData = resetCalls.last(where: { $0.arguments[1] == "reset" })?.input ?? Data()
        let reset = try JSONDecoder().decode(ResetInput.self, from: resetData)
        try check(reset.confirm && reset.revision == 11 && store.notice?.contains("preserved") == true,
            "Confirmed reset carries the reviewed revision and preserves correction meaning")

        await fixture.holdNextCall()
        store.draftLabel = "Finishing earlier selection"
        let olderSave = store.save()
        await fixture.waitUntilHeld()
        await store.select(targetID)?.value
        store.draftLabel = "Draft for new selection"
        await fixture.release()
        await olderSave?.value
        try check(store.selectedID == targetID && store.detail?.profile.id == targetID && store.draftLabel == "Draft for new selection" &&
            store.notice == nil, "Changing selection during a mutation cannot overwrite another workload's detail or draft")

        var confirmedAction = false
        store.confirm(sourceID: sourceID, revision: 7) { confirmedAction = true }
        try check(!confirmedAction && store.error?.contains("selected workload changed") == true,
            "A confirmation from another selection or revision cannot mutate the current workload")
        await fixture.failRead(true)
        await store.select(sourceID)?.value
        try check(!store.isDetailLoading && store.detail == nil && store.error?.contains("no longer available") == true,
            "Failed detail loading stops progress and exposes an actionable reload error")
        await fixture.failRead(false)
        await store.reloadSelection()?.value
        try check(store.detail?.profile.id == sourceID && store.error == nil,
            "Reload recovers a failed detail read without inventing a cached profile")

        let beforePreview = await fixture.calls
        let preview = LearningStore(fixtureMode: true, command: { try await fixture.load($0, $1) })
        await preview.loadPage()?.value
        await preview.select(sourceID)?.value
        preview.draftLabel = "Attempted fixture edit"
        try check(preview.save() == nil && preview.merge(into: targetID) == nil && preview.split(family: familyID) == nil &&
            preview.reset() == nil && preview.reloadSelection() == nil,
            "Fixture controls are inert for every read and mutation path")
        let afterPreview = await fixture.calls
        try check(beforePreview.count == afterPreview.count && preview.detail?.profile.label == "Web test suite" &&
            preview.controllerMessage?.contains("has applied") == true,
            "Snapshot uses artificial profiles and never invokes the installed queue")
        let unavailable = testDetail(prediction: nil)
        let decoded = try JSONDecoder().decode(LearningDetail.self, from: JSONEncoder().encode(unavailable))
        try check(decoded.prediction == nil && decoded.predictionMachine == nil && decoded.runs.first?.id.count == 64,
            "Unavailable prediction and opaque run identities decode without fabricating telemetry")
        let learning = JobLearningReference(id: sourceID, label: "Web tests", reason: "Member job family shares this profile")
        guard let prediction = LearningStore.previewDetail().prediction else { throw ClientError.message("Missing artificial prediction") }
        let jobResources = JobResources(prediction: prediction, observed: nil, decision: nil, learning: learning)
        let decodedResources = try JSONDecoder().decode(JobResources.self, from: JSONEncoder().encode(jobResources))
        try check(decodedResources.learning == learning, "Job resources retain the profile lookup reason for native detail links")
    }
}
