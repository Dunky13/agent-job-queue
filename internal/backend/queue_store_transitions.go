package backend

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// Job state lives twice: the phase column the daemon schedules on, and the
// Pueue-shaped task["status"] callers read. Every write to a job row goes
// through this file, so legal edges, matching writes and compare-and-swap guards
// have one owner. Reads never validate: snapshots must still show a corrupt row.

var (
	errQueueChanged           = errors.New("Queue job changed concurrently")
	errQueueIllegalTransition = errors.New("Illegal queue job transition")
	errQueueStateDisagreement = errors.New("Queue job phase and status disagree")
	// Callers branch on errQueueChanged to retry or skip a job, so these two
	// stale results stay distinct: neither is retried.
	errQueueLaunchChanged  = errors.New("Job changed before launch")
	errQueueAttemptRevoked = errors.New("Execution attempt is no longer authorized")
)

var (
	queueCommandEdges = map[string][]string{
		"queued":   {"starting", "outcome"},
		"starting": {"running", "outcome"},
		"running":  {"outcome"},
		"outcome":  {"done"},
	}
	// A lease has no process, so it skips claiming and cleanup.
	queueLeaseEdges = map[string][]string{
		"queued":  {"running", "done"},
		"running": {"done"},
	}
)

// queueJobAgreement reports whether a row's phase and status describe one state.
// It covers only that pair: attempt and worker evidence are handover's stricter
// concern, and a held lease legitimately has neither.
func queueJobAgreement(j queueJob) error {
	name, details, e := TaskStatus(j.task)
	if e != nil {
		return e
	}
	agrees := false
	switch j.phase {
	case "queued":
		agrees = name == "Queued" || name == "Stashed"
	case "starting", "running":
		// Pausing a job before its worker claims it leaves starting and Paused.
		agrees = name == "Running" || name == "Paused"
	case "outcome", "done":
		code, resultErr := ResultCode(details["result"])
		agrees = name == "Done" && resultErr == nil
		if j.outcome.Valid {
			agrees = agrees && int64(code) == j.outcome.Int64
		} else {
			// importLegacy retains a completed result without the outcome column.
			agrees = agrees && j.phase == "done"
		}
	}
	if !agrees {
		return fmt.Errorf("%w: job %d is %s with status %s", errQueueStateDisagreement, j.id, j.phase, name)
	}
	return nil
}

// checkQueueEdge takes the kind from the method, not the row: the two lifecycles
// share edges, and a command at outcome must not pass as a lease ending.
func checkQueueEdge(j queueJob, lease bool, to string) error {
	edges := queueCommandEdges
	if lease {
		edges = queueLeaseEdges
	}
	if taskIsLease(j.task) != lease || !slices.Contains(edges[j.phase], to) {
		return fmt.Errorf("%w: %s %d cannot move from %s to %s", errQueueIllegalTransition, taskKind(j.task), j.id, j.phase, to)
	}
	return queueJobAgreement(j)
}

// swapJob commits one guarded UPDATE that leaves the row as after describes. The
// query keeps its guards in place, as they differ by edge; stale reports a miss.
func (s *queueStore) swapJob(after queueJob, stale error, query string, args ...any) error {
	if e := queueJobAgreement(after); e != nil {
		return e
	}
	result, e := s.db.Exec(query, args...)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return stale
	}
	return nil
}

func (s *queueStore) jobIDByToken(value string) (int, error) {
	var id int
	e := s.db.QueryRow("SELECT id FROM queue_jobs WHERE token=?", value).Scan(&id)
	return id, e
}

// enqueue atomically creates a queued job and reserves the next ID. publish runs
// inside the transaction, so a failed side effect leaves no job behind.
func (s *queueStore) enqueue(id int, value string, task Object, publish func() error) error {
	if e := queueJobAgreement(queueJob{id: id, phase: "queued", task: task}); e != nil {
		return e
	}
	data, e := queueJSON(task)
	if e != nil {
		return e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(?,?,?,'queued')", id, value, data); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE queue_settings SET next_id=? WHERE id=1", id+1); e != nil {
		return e
	}
	if publish != nil {
		if e = publish(); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// saveTask writes a task without moving its phase, such as a priority change or
// Running to Paused. The new status must still fit the phase the caller read.
func (s *queueStore) saveTask(j queueJob) error {
	data, e := queueJSON(j.task)
	if e != nil {
		return e
	}
	return s.swapJob(j, errQueueChanged, "UPDATE queue_jobs SET task=? WHERE id=? AND token=? AND attempt=? AND phase=?", data, j.id, j.token, j.attempt, j.phase)
}

// claimAttempt durably records a new attempt before any process exists, and
// returns the job as the caller must continue to use it.
func (s *queueStore) claimAttempt(j queueJob) (queueJob, error) {
	if e := checkQueueEdge(j, false, "starting"); e != nil {
		return j, e
	}
	j.attempt = token()
	j.phase = "starting"
	j.task["status"] = Object{"Running": Object{"start": queueNow()}}
	data, e := queueJSON(j.task)
	if e != nil {
		return j, e
	}
	return j, s.swapJob(j, errQueueLaunchChanged, "UPDATE queue_jobs SET attempt=?,phase='starting',task=? WHERE id=? AND phase='queued'", j.attempt, data, j.id)
}

// claimWorker fences a worker to its attempt before any command side effect. It
// writes only phase and worker, so a concurrent cancellation flag on the task
// survives; the worker holds no task of its own, so the row is read here.
func (s *queueStore) claimWorker(id int, value, attempt string, worker Object) error {
	j, e := scanQueueJob(s.db.QueryRow("SELECT "+queueColumns+" FROM queue_jobs WHERE id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return errQueueAttemptRevoked
	}
	if e != nil {
		return e
	}
	if j.token != value || j.attempt != attempt || j.phase != "starting" {
		return errQueueAttemptRevoked
	}
	if e = checkQueueEdge(j, false, "running"); e != nil {
		return e
	}
	data, e := queueJSON(worker)
	if e != nil {
		return e
	}
	j.phase, j.worker = "running", worker
	return s.swapJob(j, errQueueAttemptRevoked, "UPDATE queue_jobs SET phase='running',worker=? WHERE id=? AND token=? AND attempt=? AND phase='starting'", data, id, value, attempt)
}

// recordOutcome publishes a command's result. A queued job gets here through
// cancellation, with no attempt.
func (s *queueStore) recordOutcome(j queueJob, code int) error {
	if e := checkQueueEdge(j, false, "outcome"); e != nil {
		return e
	}
	_, details, e := TaskStatus(j.task)
	if e != nil {
		return e
	}
	details["end"] = queueNow()
	details["result"] = queueResult(code)
	j.task["status"] = Object{"Done": details}
	data, e := queueJSON(j.task)
	if e != nil {
		return e
	}
	from := j.phase
	j.phase, j.outcome = "outcome", sql.NullInt64{Int64: int64(code), Valid: true}
	return s.swapJob(j, errQueueChanged, "UPDATE queue_jobs SET phase='outcome',task=?,outcome=? WHERE id=? AND token=? AND attempt=? AND phase=?", data, code, j.id, j.token, j.attempt, from)
}

// retire ends a command after ownership-checked cleanup. Its task already holds
// the published result, so only the phase moves.
func (s *queueStore) retire(j queueJob) error {
	if e := checkQueueEdge(j, false, "done"); e != nil {
		return e
	}
	j.phase = "done"
	return s.swapJob(j, errQueueChanged, "UPDATE queue_jobs SET phase='done' WHERE id=? AND token=? AND attempt=? AND phase='outcome'", j.id, j.token, j.attempt)
}

// grantLease and endLease take their time from the daemon's injectable clock.
// The caller shapes task["lease"]; the store owns phase, status and outcome.
func (s *queueStore) grantLease(j queueJob, at string) error {
	if e := checkQueueEdge(j, true, "running"); e != nil {
		return e
	}
	j.task["status"] = Object{"Running": Object{"start": at}}
	data, e := queueJSON(j.task)
	if e != nil {
		return e
	}
	j.phase = "running"
	return s.swapJob(j, errQueueChanged, "UPDATE queue_jobs SET phase='running',task=? WHERE id=? AND token=? AND phase='queued'", data, j.id, j.token)
}

func (s *queueStore) endLease(j queueJob, at string) error {
	if e := checkQueueEdge(j, true, "done"); e != nil {
		return e
	}
	_, details, e := TaskStatus(j.task)
	if e != nil {
		return e
	}
	details["end"] = at
	details["result"] = "Success"
	j.task["status"] = Object{"Done": details}
	data, e := queueJSON(j.task)
	if e != nil {
		return e
	}
	from := j.phase
	j.phase, j.outcome = "done", sql.NullInt64{Int64: 0, Valid: true}
	return s.swapJob(j, errQueueChanged, "UPDATE queue_jobs SET phase='done',task=?,outcome=0 WHERE id=? AND token=? AND phase=?", data, j.id, j.token, from)
}
