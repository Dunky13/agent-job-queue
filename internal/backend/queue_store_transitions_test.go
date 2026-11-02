package backend

import (
	"errors"
	"os/exec"
	"testing"
	"time"
)

// transitionCommand reaches phase only through real transitions.
func transitionCommand(t *testing.T, s *queueStore, id int, phase string) queueJob {
	t.Helper()
	value := token()
	if err := s.enqueue(id, value, Object{"token": value, "priority": 0, "status": Object{"Queued": Object{}}}, nil); err != nil {
		t.Fatal(err)
	}
	j := transitionRow(t, s, id)
	step := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		j = transitionRow(t, s, id)
	}
	switch phase {
	case "queued":
	case "starting", "running":
		claimed, err := s.claimAttempt(j)
		step(err)
		if phase == "running" {
			step(s.claimWorker(id, value, claimed.attempt, Object{"pgid": 1}))
		}
	case "outcome", "done":
		step(s.recordOutcome(j, 7))
		if phase == "done" {
			step(s.retire(j))
		}
	default:
		t.Fatalf("no route to %s", phase)
	}
	return j
}

func transitionLease(t *testing.T, s *queueStore, id int, phase string) queueJob {
	t.Helper()
	value := token()
	task := Object{"kind": "lease", "lane": "fixture", "priority": 0, "status": Object{"Queued": Object{}}, "lease": Object{"state": "queued"}}
	if err := s.enqueue(id, value, task, nil); err != nil {
		t.Fatal(err)
	}
	j := transitionRow(t, s, id)
	switch phase {
	case "queued":
		return j
	case "running":
		if err := s.grantLease(j, "2026-11-02T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	case "done":
		if err := s.endLease(j, "2026-11-02T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("no route to %s", phase)
	}
	return transitionRow(t, s, id)
}

func transitionRow(t *testing.T, s *queueStore, id int) queueJob {
	t.Helper()
	j, err := s.job(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func transitionStatus(t *testing.T, j queueJob) string {
	t.Helper()
	name, _, err := TaskStatus(j.task)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func requireRowUnchanged(t *testing.T, s *queueStore, before queueJob) {
	t.Helper()
	after := transitionRow(t, s, before.id)
	if after.phase != before.phase || after.attempt != before.attempt || after.outcome != before.outcome || !jsonEqual(after.task, before.task) || !jsonEqual(after.worker, before.worker) {
		t.Fatalf("rejected transition changed the row: before=%+v after=%+v", before, after)
	}
}

func TestQueueTransitionCommandLifecycle(t *testing.T) {
	_, s := edgeQueueStore(t)
	value := token()
	if err := s.enqueue(3, value, Object{"token": value, "priority": 0, "status": Object{"Stashed": Object{}}}, nil); err != nil {
		t.Fatal(err)
	}
	settings, err := s.settings()
	if err != nil || settings["next_id"] != 4 {
		t.Fatalf("enqueue did not reserve the next ID: %v %v", settings, err)
	}
	if id, err := s.jobIDByToken(value); err != nil || id != 3 {
		t.Fatalf("token lookup: %d %v", id, err)
	}
	j := transitionRow(t, s, 3)
	if j.phase != "queued" || transitionStatus(t, j) != "Stashed" || j.attempt != "" {
		t.Fatalf("queued row: %+v", j)
	}

	claimed, err := s.claimAttempt(j)
	if err != nil {
		t.Fatal(err)
	}
	j = transitionRow(t, s, 3)
	if claimed.phase != "starting" || claimed.attempt == "" || j.phase != "starting" || j.attempt != claimed.attempt || transitionStatus(t, j) != "Running" || j.worker != nil {
		t.Fatalf("claimed row: returned=%+v durable=%+v", claimed, j)
	}

	if err = s.claimWorker(3, value, claimed.attempt, Object{"pgid": 9}); err != nil {
		t.Fatal(err)
	}
	j = transitionRow(t, s, 3)
	if j.phase != "running" || transitionStatus(t, j) != "Running" || !jsonEqual(j.worker, Object{"pgid": 9}) {
		t.Fatalf("worker row: %+v", j)
	}

	if err = s.recordOutcome(j, 41); err != nil {
		t.Fatal(err)
	}
	j = transitionRow(t, s, 3)
	_, details, err := TaskStatus(j.task)
	if err != nil {
		t.Fatal(err)
	}
	code, err := ResultCode(details["result"])
	if err != nil || code != 41 || j.phase != "outcome" || transitionStatus(t, j) != "Done" || !j.outcome.Valid || j.outcome.Int64 != 41 || details["start"] == nil || details["end"] == nil {
		t.Fatalf("outcome row: %+v %v", j, err)
	}

	before := j.task
	if err = s.retire(j); err != nil {
		t.Fatal(err)
	}
	j = transitionRow(t, s, 3)
	if j.phase != "done" || !jsonEqual(j.task, before) || j.outcome.Int64 != 41 {
		t.Fatalf("retired row: %+v", j)
	}
}

// Cancellation settles a job that never had a process, with no attempt.
func TestQueueTransitionCancelledBeforeClaim(t *testing.T) {
	_, s := edgeQueueStore(t)
	j := transitionCommand(t, s, 1, "queued")
	if err := s.recordOutcome(j, 130); err != nil {
		t.Fatal(err)
	}
	j = transitionRow(t, s, 1)
	if j.phase != "outcome" || j.attempt != "" || j.outcome.Int64 != 130 {
		t.Fatalf("cancelled row: %+v", j)
	}
	if err := s.retire(j); err != nil {
		t.Fatal(err)
	}
}

// Pausing between launch and the worker's claim is a real sequence.
func TestQueueTransitionPauseDuringClaimStaysLegal(t *testing.T) {
	_, s := edgeQueueStore(t)
	j := transitionCommand(t, s, 1, "starting")
	_, details, err := TaskStatus(j.task)
	if err != nil {
		t.Fatal(err)
	}
	if details["start"] == nil {
		t.Fatalf("fixture has no start: %v", details)
	}
	if err = s.setPaused(j, true); err != nil {
		t.Fatal(err)
	}
	if err = s.claimWorker(j.id, j.token, j.attempt, Object{"pgid": 9}); err != nil {
		t.Fatal(err)
	}
	j = transitionRow(t, s, 1)
	if _, kept, err := TaskStatus(j.task); j.phase != "running" || transitionStatus(t, j) != "Paused" || err != nil || kept["start"] != details["start"] {
		t.Fatalf("worker claim lost the pause or its start: %+v %v", j, err)
	}
	if err = s.recordOutcome(j, 130); err != nil {
		t.Fatal(err)
	}
}

func TestQueueTransitionLeaseLifecycle(t *testing.T) {
	_, s := edgeQueueStore(t)
	held := transitionLease(t, s, 1, "running")
	_, details, err := TaskStatus(held.task)
	if err != nil || held.phase != "running" || held.attempt != "" || held.worker != nil || details["start"] != "2026-11-02T00:00:00Z" {
		t.Fatalf("held lease: %+v %v", held, err)
	}
	if err = s.endLease(held, "2026-11-02T01:00:00Z"); err != nil {
		t.Fatal(err)
	}
	for id, j := range map[int]queueJob{1: transitionRow(t, s, 1), 2: transitionLease(t, s, 2, "done")} {
		_, details, err := TaskStatus(j.task)
		if err != nil || j.phase != "done" || details["result"] != "Success" || !j.outcome.Valid || j.outcome.Int64 != 0 {
			t.Fatalf("ended lease %d: %+v %v", id, j, err)
		}
	}
	if _, details, _ = TaskStatus(transitionRow(t, s, 1).task); details["end"] != "2026-11-02T01:00:00Z" {
		t.Fatalf("lease end did not use the caller's clock: %v", details)
	}
}

// Each edge outside the lifecycle is refused before it reaches SQL, whichever
// kind of row and method are combined.
func TestQueueTransitionIllegalEdgesLeaveRowUnchanged(t *testing.T) {
	operations := map[string]func(*queueStore, queueJob) error{
		"claimAttempt":  func(s *queueStore, j queueJob) error { _, e := s.claimAttempt(j); return e },
		"recordOutcome": func(s *queueStore, j queueJob) error { return s.recordOutcome(j, 1) },
		"retire":        func(s *queueStore, j queueJob) error { return s.retire(j) },
		"grantLease":    func(s *queueStore, j queueJob) error { return s.grantLease(j, "2026-11-02T00:00:00Z") },
		"endLease":      func(s *queueStore, j queueJob) error { return s.endLease(j, "2026-11-02T00:00:00Z") },
	}
	legal := map[string]bool{
		"command/queued/claimAttempt":    true,
		"command/queued/recordOutcome":   true,
		"command/starting/recordOutcome": true,
		"command/running/recordOutcome":  true,
		"command/outcome/retire":         true,
		"lease/queued/grantLease":        true,
		"lease/queued/endLease":          true,
		"lease/running/endLease":         true,
	}
	reach := map[string]func(*testing.T, *queueStore, int, string) queueJob{"command": transitionCommand, "lease": transitionLease}
	phases := map[string][]string{"command": {"queued", "starting", "running", "outcome", "done"}, "lease": {"queued", "running", "done"}}
	for kind, fixture := range reach {
		for _, phase := range phases[kind] {
			for name, operation := range operations {
				key := kind + "/" + phase + "/" + name
				t.Run(key, func(t *testing.T) {
					_, s := edgeQueueStore(t)
					j := fixture(t, s, 1, phase)
					before := transitionRow(t, s, 1)
					err := operation(s, j)
					if legal[key] {
						if err != nil {
							t.Fatalf("legal edge refused: %v", err)
						}
						return
					}
					if !errors.Is(err, errQueueIllegalTransition) {
						t.Fatalf("error=%v; want illegal transition", err)
					}
					requireRowUnchanged(t, s, before)
				})
			}
		}
	}
	for _, phase := range []string{"queued", "running", "done"} {
		t.Run("lease/"+phase+"/claimWorker", func(t *testing.T) {
			_, s := edgeQueueStore(t)
			j := transitionLease(t, s, 1, phase)
			if err := s.claimWorker(j.id, j.token, j.attempt, Object{"pgid": 9}); !errors.Is(err, errQueueAttemptRevoked) {
				t.Fatalf("error=%v", err)
			}
			requireRowUnchanged(t, s, j)
		})
	}
}

// A stale caller holds a legal view of a row that has since moved on. Each edge
// keeps the exact error its callers branch on.
func TestQueueTransitionStaleViewLeavesRowUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		advance func(*testing.T, *queueStore, queueJob)
		stale   func(*queueStore, queueJob) error
		want    error
		reach   func(*testing.T, *queueStore, int, string) queueJob
		from    string
	}{
		{"claimAttempt", func(t *testing.T, s *queueStore, j queueJob) {
			if _, e := s.claimAttempt(j); e != nil {
				t.Fatal(e)
			}
		}, func(s *queueStore, j queueJob) error { _, e := s.claimAttempt(j); return e }, errQueueLaunchChanged, transitionCommand, "queued"},
		{"recordOutcome", func(t *testing.T, s *queueStore, j queueJob) {
			if e := s.claimWorker(j.id, j.token, j.attempt, Object{"pgid": 1}); e != nil {
				t.Fatal(e)
			}
		}, func(s *queueStore, j queueJob) error { return s.recordOutcome(j, 1) }, errQueueChanged, transitionCommand, "starting"},
		{"retire", func(t *testing.T, s *queueStore, j queueJob) {
			if e := s.retire(j); e != nil {
				t.Fatal(e)
			}
		}, func(s *queueStore, j queueJob) error { return s.retire(j) }, errQueueChanged, transitionCommand, "outcome"},
		{"saveTask", func(t *testing.T, s *queueStore, j queueJob) {
			if e := s.recordOutcome(j, 1); e != nil {
				t.Fatal(e)
			}
		}, func(s *queueStore, j queueJob) error { return s.saveTask(j) }, errQueueChanged, transitionCommand, "running"},
		{"grantLease", func(t *testing.T, s *queueStore, j queueJob) {
			if e := s.grantLease(j, "2026-11-02T00:00:00Z"); e != nil {
				t.Fatal(e)
			}
		}, func(s *queueStore, j queueJob) error { return s.grantLease(j, "2026-11-02T01:00:00Z") }, errQueueChanged, transitionLease, "queued"},
		{"endLease", func(t *testing.T, s *queueStore, j queueJob) {
			if e := s.endLease(j, "2026-11-02T00:00:00Z"); e != nil {
				t.Fatal(e)
			}
		}, func(s *queueStore, j queueJob) error { return s.endLease(j, "2026-11-02T01:00:00Z") }, errQueueChanged, transitionLease, "queued"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, s := edgeQueueStore(t)
			test.reach(t, s, 1, test.from)
			view := transitionRow(t, s, 1)
			test.advance(t, s, transitionRow(t, s, 1))
			durable := transitionRow(t, s, 1)
			err := test.stale(s, view)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v; want %v", err, test.want)
			}
			if test.want != errQueueChanged && errors.Is(err, errQueueChanged) {
				t.Fatal("a retried sentinel leaked from an edge that never retried")
			}
			requireRowUnchanged(t, s, durable)
		})
	}
}

func TestQueueTransitionWorkerClaimRequiresExactAttempt(t *testing.T) {
	_, s := edgeQueueStore(t)
	j := transitionCommand(t, s, 1, "starting")
	for name, claim := range map[string][3]any{
		"id":      {2, j.token, j.attempt},
		"token":   {1, token(), j.attempt},
		"attempt": {1, j.token, token()},
	} {
		if err := s.claimWorker(claim[0].(int), claim[1].(string), claim[2].(string), Object{"pgid": 9}); !errors.Is(err, errQueueAttemptRevoked) {
			t.Fatalf("%s: error=%v", name, err)
		}
		requireRowUnchanged(t, s, j)
	}
	if err := s.claimWorker(1, j.token, j.attempt, Object{"pgid": 9}); err != nil {
		t.Fatal(err)
	}
	running := transitionRow(t, s, 1)
	if err := s.claimWorker(1, j.token, j.attempt, Object{"pgid": 10}); !errors.Is(err, errQueueAttemptRevoked) {
		t.Fatalf("duplicate worker: %v", err)
	}
	requireRowUnchanged(t, s, running)
}

// Rows here are deliberately corrupted with raw SQL: no store method can write
// them, which is the point of the check.
func TestQueueTransitionDisagreeingRowIsRefused(t *testing.T) {
	done := `{"status":{"Done":{"result":{"Failed":7}}}}`
	cases := []struct {
		name, task string
		outcome    any
		reach      func(*testing.T, *queueStore, int, string) queueJob
		from       string
		operation  func(*queueStore, queueJob) error
	}{
		{"queued as Running", `{"status":{"Running":{}}}`, nil, transitionCommand, "queued", func(s *queueStore, j queueJob) error { _, e := s.claimAttempt(j); return e }},
		{"queued as Done", `{"status":{"Done":{"result":"Success"}}}`, nil, transitionCommand, "queued", func(s *queueStore, j queueJob) error { return s.recordOutcome(j, 1) }},
		{"starting as Queued", `{"status":{"Queued":{}}}`, nil, transitionCommand, "starting", func(s *queueStore, j queueJob) error { return s.recordOutcome(j, 125) }},
		{"worker claim on starting as Done", done, nil, transitionCommand, "starting", func(s *queueStore, j queueJob) error {
			return s.claimWorker(j.id, j.token, j.attempt, Object{"pgid": 9})
		}},
		{"running as Stashed", `{"status":{"Stashed":{}}}`, nil, transitionCommand, "running", func(s *queueStore, j queueJob) error { return s.recordOutcome(j, 125) }},
		{"outcome as Queued", `{"status":{"Queued":{}}}`, 7, transitionCommand, "outcome", func(s *queueStore, j queueJob) error { return s.retire(j) }},
		{"outcome missing column", done, nil, transitionCommand, "outcome", func(s *queueStore, j queueJob) error { return s.retire(j) }},
		{"outcome mismatched column", done, 8, transitionCommand, "outcome", func(s *queueStore, j queueJob) error { return s.retire(j) }},
		{"outcome with unknown result", `{"status":{"Done":{"result":"Mystery"}}}`, 7, transitionCommand, "outcome", func(s *queueStore, j queueJob) error { return s.retire(j) }},
		{"queued lease as Running", `{"kind":"lease","status":{"Running":{}}}`, nil, transitionLease, "queued", func(s *queueStore, j queueJob) error { return s.grantLease(j, "2026-11-02T00:00:00Z") }},
		{"held lease as Queued", `{"kind":"lease","status":{"Queued":{}}}`, nil, transitionLease, "running", func(s *queueStore, j queueJob) error { return s.endLease(j, "2026-11-02T00:00:00Z") }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, s := edgeQueueStore(t)
			test.reach(t, s, 1, test.from)
			if _, err := s.db.Exec("UPDATE queue_jobs SET task=?,outcome=? WHERE id=1", test.task, test.outcome); err != nil {
				t.Fatal(err)
			}
			j := transitionRow(t, s, 1)
			before := transitionRow(t, s, 1)
			if err := test.operation(s, j); !errors.Is(err, errQueueStateDisagreement) {
				t.Fatalf("error=%v; want disagreement", err)
			}
			requireRowUnchanged(t, s, before)
		})
	}
}

// saveTask cannot change phase, so a status that no longer fits it is refused
// before the write, while a status that still fits passes.
func TestQueueTransitionSaveTaskKeepsStatusWithPhase(t *testing.T) {
	cases := []struct {
		phase, status string
		allowed       bool
	}{
		{"queued", "Stashed", true},
		{"queued", "Running", false},
		{"queued", "Done", false},
		{"starting", "Paused", true},
		{"starting", "Queued", false},
		{"running", "Paused", true},
		{"running", "Queued", false},
		{"running", "Done", false},
		{"outcome", "Running", false},
		{"done", "Queued", false},
	}
	for _, test := range cases {
		t.Run(test.phase+"/"+test.status, func(t *testing.T) {
			_, s := edgeQueueStore(t)
			j := transitionCommand(t, s, 1, test.phase)
			before := transitionRow(t, s, 1)
			j.task["status"] = Object{test.status: Object{}}
			err := s.saveTask(j)
			if test.allowed {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, errQueueStateDisagreement) {
				t.Fatalf("error=%v", err)
			}
			requireRowUnchanged(t, s, before)
		})
	}
}

func TestQueueTransitionInvalidStatusKeepsItsError(t *testing.T) {
	_, s := edgeQueueStore(t)
	transitionCommand(t, s, 1, "running")
	if _, err := s.db.Exec("UPDATE queue_jobs SET task='{}' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := s.recordOutcome(transitionRow(t, s, 1), 125); err == nil || err.Error() != "Queue returned invalid task status" {
		t.Fatalf("error=%v", err)
	}
}

func TestQueueTransitionOutOfRangeOutcomeIsRefused(t *testing.T) {
	_, s := edgeQueueStore(t)
	j := transitionCommand(t, s, 1, "running")
	before := transitionRow(t, s, 1)
	if err := s.recordOutcome(j, 300); err == nil {
		t.Fatal("outcome outside the exit status range was accepted")
	}
	requireRowUnchanged(t, s, before)
}

// importLegacy retains completed history with no outcome column. Reads never
// validate, and no edge leaves done.
func TestQueueTransitionLegacyHistoryStillAgrees(t *testing.T) {
	g, s := edgeQueueStore(t)
	mustWrite(t, g.State+"/legacy-queue-import.json", Object{
		"tasks":  Object{"4": Object{"priority": 0, "status": Object{"Done": Object{"result": Object{"Failed": 7}}}}},
		"groups": Object{"default": Object{"parallel_tasks": 1, "status": "Running"}},
	})
	if err := s.importLegacy(); err != nil {
		t.Fatal(err)
	}
	j := transitionRow(t, s, 4)
	if j.outcome.Valid {
		t.Fatalf("fixture no longer exercises a missing outcome: %+v", j)
	}
	if err := s.retire(j); !errors.Is(err, errQueueIllegalTransition) {
		t.Fatalf("done row moved: %v", err)
	}
}

func TestQueueTransitionEnqueueIsAtomic(t *testing.T) {
	_, s := edgeQueueStore(t)
	value := token()
	if err := s.enqueue(1, value, Object{"status": Object{"Running": Object{}}}, nil); !errors.Is(err, errQueueStateDisagreement) {
		t.Fatalf("error=%v", err)
	}
	failure := errors.New("publication failed")
	if err := s.enqueue(1, value, Object{"status": Object{"Queued": Object{}}}, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	jobs, err := s.jobs()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("failed enqueue left a job: %v %v", jobs, err)
	}
	settings, err := s.settings()
	if err != nil || settings["next_id"] != 0 {
		t.Fatalf("failed enqueue reserved an ID: %v %v", settings, err)
	}
}

// Phase, status and outcome stay together; handover adds its stricter attempt
// and worker evidence and keeps its own error text.
func TestQueueTransitionHandoverValidatorKeepsItsRules(t *testing.T) {
	_, s := edgeQueueStore(t)
	for id, phase := range []string{"queued", "running", "outcome", "done"} {
		if err := validateQueueHandoverJobState(transitionCommand(t, s, id, phase)); err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
	}
	running := transitionRow(t, s, 1)
	queued := transitionRow(t, s, 0)
	done := transitionRow(t, s, 3)
	for _, test := range []struct {
		name string
		job  func() queueJob
		want string
	}{
		{"queued with attempt", func() queueJob { j := queued; j.attempt = token(); return j }, "Queued handover state is inconsistent"},
		{"queued as Running", func() queueJob { j := queued; j.task = Object{"status": Object{"Running": Object{}}}; return j }, "Queued handover state is inconsistent"},
		{"running without worker", func() queueJob { j := running; j.worker = nil; return j }, "Running handover state is inconsistent"},
		{"lease row without evidence", func() queueJob { j := running; j.attempt = ""; return j }, "Running handover state is inconsistent"},
		{"done without outcome", func() queueJob { j := done; j.outcome.Valid = false; return j }, "Completed handover state is inconsistent"},
		{"done with other outcome", func() queueJob { j := done; j.outcome.Int64 = 8; return j }, "Completed handover state is inconsistent"},
		{"starting", func() queueJob { j := running; j.phase = "starting"; return j }, "Unsupported handover execution state"},
		{"invalid status", func() queueJob { j := running; j.task = Object{}; return j }, "Queue returned invalid task status"},
	} {
		if err := validateQueueHandoverJobState(test.job()); err == nil || err.Error() != test.want {
			t.Fatalf("%s: error=%v; want %s", test.name, err, test.want)
		}
	}
}

// Cleanup that finds its row already moved on has nothing to report; a stale
// retire must not leave a permanent cleanup error behind.
func TestQueueCleanupToleratesRowThatMovedOn(t *testing.T) {
	g, s := edgeQueueStore(t)
	j := transitionCommand(t, s, 0, "outcome")
	mustWrite(t, g.ReceiptPath(0), Object{"task_id": 0, "token": j.token})
	d := &queueDaemon{g: g, store: s, children: map[int]*exec.Cmd{}, errors: map[int]string{}, cleaning: map[int]bool{}, finished: map[int]bool{}}
	release, err := LockPrivate(g, "")
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	err = d.tick()
	d.mu.Unlock()
	if err != nil {
		release()
		t.Fatal(err)
	}
	if err = s.retire(j); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	finished := make(chan struct{})
	go func() { d.cleanup.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not finish")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.errors) != 0 || d.finished[0] {
		t.Fatalf("stale retire left cleanup state: errors=%v finished=%v", d.errors, d.finished)
	}
}

func TestQueueTransitionErrorsKeepTheirText(t *testing.T) {
	for err, want := range map[error]string{
		errQueueChanged:        "Queue job changed concurrently",
		errQueueLaunchChanged:  "Job changed before launch",
		errQueueAttemptRevoked: "Execution attempt is no longer authorized",
	} {
		if err.Error() != want {
			t.Fatalf("error text changed: %v", err)
		}
	}
}
