import Foundation

/// An operator decision on the queue or one job. The action owns its rules; callers ask it
/// instead of reading job flags, so menus, confirmations and the store cannot disagree.
enum QueueAction: Equatable {
    case next(QueueJob), last(QueueJob), start(QueueJob), cancel(QueueJob)
    case resume
    case limit(Int)
    case scheduling(SchedulingMode)

    /// The job a job action targets.
    var job: QueueJob? {
        switch self {
        case .next(let job), .last(let job), .start(let job), .cancel(let job): job
        case .resume, .limit, .scheduling: nil
        }
    }

    /// The same action aimed at a fresher copy of its job.
    func refreshed(with job: QueueJob) -> QueueAction {
        switch self {
        case .next: .next(job)
        case .last: .last(job)
        case .start: .start(job)
        case .cancel: .cancel(job)
        case .resume, .limit, .scheduling: self
        }
    }

    /// The controller verb for job actions; also the command-line spelling of confirmations.
    var name: String? {
        switch self {
        case .next: "next"
        case .last: "last"
        case .start: "start"
        case .cancel: "cancel"
        case .resume, .limit, .scheduling: nil
        }
    }

    /// The job action with this name that asks for confirmation, if any.
    static func confirming(named name: String) -> ((QueueJob) -> QueueAction)? {
        let make: ((QueueJob) -> QueueAction)? = switch name {
        case "start": { .start($0) }
        case "cancel": { .cancel($0) }
        default: nil
        }
        return make
    }

    var needsConfirmation: Bool {
        switch self {
        case .start, .cancel: true
        case .next, .last, .resume, .limit, .scheduling: false
        }
    }

    /// Whether the job and queue state permit this action. Store-level busy and read-only
    /// states are separate and apply on top.
    func isAllowed(in snapshot: QueueSnapshot?) -> Bool {
        switch self {
        case .next(let job), .last(let job): job.isWaiting && job.canReorder
        case .start(let job): job.permitsStart
        case .cancel(let job): job.canCancel
        case .resume: snapshot?.queue.status.lowercased() == "paused"
        case .limit(let limit): (1...8).contains(limit)
        case .scheduling(let mode): snapshot?.scheduler.map { $0.mode != mode } ?? false
        }
    }

    /// Why a confirmation can no longer proceed.
    var unavailableMessage: String {
        switch self {
        case .start: "This job cannot be started from its current state. Return to the queue to see its latest status."
        case .cancel: "This job is no longer available to cancel."
        default: "This action is not available."
        }
    }

    var command: QueueClient.Command {
        switch self {
        case .next(let job): .jobAction("next", id: job.id)
        case .last(let job): .jobAction("last", id: job.id)
        case .start(let job): .jobAction("start", id: job.id)
        case .cancel(let job): .jobAction("cancel", id: job.id)
        case .resume: .resume
        case .limit(let limit): .limit(limit)
        case .scheduling(let mode): .scheduling(mode)
        }
    }

    var notice: String {
        switch self {
        case .next(let job): "Job #\(job.id) will run next."
        case .last(let job): "Job #\(job.id) moved to the end."
        case .start(let job): "Job #\(job.id) requested to start outside the parallel limit."
        case .cancel(let job): "Cancellation requested for job #\(job.id)."
        case .resume: "Queue resumed."
        case .limit(let limit): "Parallel job limit changed to \(limit)."
        case .scheduling(let mode): "Scheduling changed to \(mode.label)."
        }
    }
}
