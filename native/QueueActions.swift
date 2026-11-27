import Foundation

/// An operator decision on the queue or one job. The action owns its rules; callers ask it
/// instead of reading job flags, so menus, confirmations and the store cannot disagree.
enum QueueAction: Equatable {
    case next(QueueJob), last(QueueJob), start(QueueJob), cancel(QueueJob)
    case resume
    /// Closes intake for a while; the daemon reopens it on its own when the time is up.
    case drain(minutes: Int)
    case undrain
    case limit(Int)
    case scheduling(SchedulingMode)

    /// The job a job action targets.
    var job: QueueJob? {
        switch self {
        case .next(let job), .last(let job), .start(let job), .cancel(let job): job
        case .resume, .drain, .undrain, .limit, .scheduling: nil
        }
    }

    /// The controller verb; for confirmations it is also the command-line spelling.
    var name: String {
        switch self {
        case .next: "next"
        case .last: "last"
        case .start: "start"
        case .cancel: "cancel"
        case .resume: "resume"
        case .drain: "drain"
        case .undrain: "undrain"
        case .limit: "limit"
        case .scheduling: "scheduling"
        }
    }

    /// The one list of actions that ask for confirmation.
    var confirmation: QueueConfirmationKind? {
        switch self {
        case .start: .start
        case .cancel: .cancel
        case .next, .last, .resume, .drain, .undrain, .limit, .scheduling: nil
        }
    }

    /// Whether the job and queue state permit this action. Store-level busy and read-only
    /// states are separate and apply on top.
    func isAllowed(in snapshot: QueueSnapshot?) -> Bool {
        switch self {
        case .next(let job), .last(let job): job.isWaiting && job.canReorder
        case .start(let job): job.isWaiting && (job.canStart ?? job.canReorder)
        case .cancel(let job): job.canCancel
        case .resume: snapshot?.queue.status.lowercased() == "paused"
        // A new drain replaces the end time of one already active.
        case .drain(let minutes): snapshot != nil && Self.drainMinutes.contains(minutes)
        // Undrain also removes a drain file the daemon cannot trust.
        case .undrain: snapshot.map { $0.queue.drainUntil != nil || $0.queue.drainError != nil } ?? false
        case .limit(let limit): Self.limits.contains(limit) && limit != snapshot?.queue.limit
        case .scheduling(let mode): snapshot?.scheduler.map { $0.mode != mode } ?? false
        }
    }

    static let limits = 1...8
    static let drainMinutes = [30, 60, 120, 240]

    static func drainLabel(_ minutes: Int) -> String {
        minutes < 60 ? "\(minutes) minutes" : minutes == 60 ? "1 hour" : "\(minutes / 60) hours"
    }

    var command: QueueClient.Command {
        switch self {
        case .resume: .resume
        case .drain(let minutes): .drain(minutes: minutes)
        // Ending a drain leaves a pause alone; Resume is its own control.
        case .undrain: .undrainKeepPaused
        case .limit(let limit): .limit(limit)
        case .scheduling(let mode): .scheduling(mode)
        case .next(let job), .last(let job), .start(let job), .cancel(let job): .jobAction(name, id: job.id)
        }
    }

    var notice: String {
        switch self {
        case .next(let job): "Job #\(job.id) will run next."
        case .last(let job): "Job #\(job.id) moved to the end."
        case .start(let job): "Job #\(job.id) requested to start outside the parallel limit."
        case .cancel(let job): "Cancellation requested for job #\(job.id)."
        case .resume: "Queue resumed."
        case .drain(let minutes): "Draining for \(Self.drainLabel(minutes)). New jobs wait; queued and running jobs continue."
        case .undrain: "Draining stopped. New jobs are accepted."
        case .limit(let limit): "Parallel job limit changed to \(limit)."
        case .scheduling(let mode): "Scheduling changed to \(mode.label)."
        }
    }
}

/// The actions that need a second step before they are sent.
enum QueueConfirmationKind: String {
    case start, cancel

    func action(for job: QueueJob) -> QueueAction {
        switch self {
        case .start: .start(job)
        case .cancel: .cancel(job)
        }
    }

    /// Why the confirmation can no longer proceed.
    var unavailableMessage: String {
        switch self {
        case .start: "This job cannot be started from its current state. Return to the queue to see its latest status."
        case .cancel: "This job is no longer available to cancel."
        }
    }
}
