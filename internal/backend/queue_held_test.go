package backend

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Rows here are corrupted with raw SQL on purpose: no store method can write
// them, and the daemon must hold only that row.

func heldDaemon(t *testing.T) (*queueDaemon, *queueStore, *laneClock, *bytes.Buffer) {
	t.Helper()
	d, clock := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":4}`)})
	output := &bytes.Buffer{}
	d.logOutput = output
	return d, d.store, clock, output
}

func corruptStatus(t *testing.T, s *queueStore, id int, status Object) queueJob {
	t.Helper()
	j := transitionRow(t, s, id)
	j.task["status"] = status
	data, err := queueJSON(j.task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE queue_jobs SET task=? WHERE id=?", data, id); err != nil {
		t.Fatal(err)
	}
	return transitionRow(t, s, id)
}

// exitedWorker is the marker of a process group that has already gone.
func exitedWorker(t *testing.T) Object {
	t.Helper()
	command := exec.Command("/usr/bin/true")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	boot, err := BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return Object{"pgid": command.Process.Pid, "pid": command.Process.Pid, "boot_uuid": boot, "members": []Object{}}
}

func runningWithWorker(t *testing.T, s *queueStore, id int, worker Object) queueJob {
	t.Helper()
	j := transitionCommand(t, s, id, "starting")
	if err := s.claimWorker(j.id, j.token, j.attempt, worker); err != nil {
		t.Fatal(err)
	}
	return transitionRow(t, s, id)
}

func heldTick(t *testing.T, d *queueDaemon) {
	t.Helper()
	d.mu.Lock()
	err := d.tick()
	d.mu.Unlock()
	if err != nil {
		t.Fatalf("a held row stopped the tick: %v", err)
	}
	d.cleanup.Wait()
}

func heldProblems(t *testing.T, d *queueDaemon) map[int]string {
	t.Helper()
	snapshot, err := d.control("snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	problems := map[int]string{}
	for _, raw := range list(snapshot["problems"]) {
		problem := obj(raw)
		id, _ := integer(problem["id"])
		problems[id] = text(problem["kind"])
	}
	return problems
}

// The reported reproduction: base settled all three rows, and the first head
// left all three stuck.
func TestHeldRowDoesNotBlockHigherIDs(t *testing.T) {
	d, s, clock, _ := heldDaemon(t)
	runningWithWorker(t, s, 0, exitedWorker(t))
	before := corruptStatus(t, s, 0, Object{"Queued": Object{}})
	transitionCommand(t, s, 1, "outcome")
	expiring := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 60})
	waiting := acquireTestLease(t, d, "qa", nil)
	requireLeaseState(t, d, waiting, "held")
	for range 3 {
		heldTick(t, d)
	}
	if j := transitionRow(t, s, 1); j.phase != "done" {
		t.Fatalf("a valid outcome behind the held row was not retired: %+v", j)
	}
	clock.advance(61 * time.Second)
	heldTick(t, d)
	requireLeaseState(t, d, expiring, "expired")
	requireLeaseState(t, d, waiting, "held")
	requireRowUnchanged(t, s, before)
}

func TestHeldRowKeepsItsSlot(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	if _, err := s.db.Exec("UPDATE queue_settings SET parallel_limit=1"); err != nil {
		t.Fatal(err)
	}
	runningWithWorker(t, s, 0, exitedWorker(t))
	corruptStatus(t, s, 0, Object{"Stashed": Object{}})
	jobs, err := s.unfinishedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if count := laneUsage(jobs)[defaultLaneName]; count == nil || count.occupied() != 1 {
		t.Fatalf("held row gave up its slot: %+v", count)
	}
	queued := transitionCommand(t, s, 1, "queued")
	heldTick(t, d)
	requireRowUnchanged(t, s, queued)
}

func TestHeldRowIsNotSelfHealed(t *testing.T) {
	for name, status := range map[string]Object{"Queued": {"Queued": Object{}}, "Done": {"Done": Object{"result": "Success"}}} {
		t.Run(name, func(t *testing.T) {
			d, s, clock, _ := heldDaemon(t)
			runningWithWorker(t, s, 0, exitedWorker(t))
			before := corruptStatus(t, s, 0, status)
			for range 3 {
				heldTick(t, d)
				clock.advance(time.Hour)
			}
			requireRowUnchanged(t, s, before)
		})
	}
}

func TestHeldRowDoesNotStopLeaseOwnerChecks(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	owner := exec.Command("/bin/sleep", "60")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill() })
	bad := acquireTestLease(t, d, "qa", Object{"ownerPid": owner.Process.Pid})
	good := acquireTestLease(t, d, "qa", Object{"ownerPid": owner.Process.Pid})
	badID, _ := integer(bad["id"])
	before := corruptStatus(t, s, badID, Object{"Queued": Object{}})
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	if err := d.checkLeaseOwners(); err != nil {
		t.Fatalf("a held lease stopped owner checks: %v", err)
	}
	requireLeaseState(t, d, good, "expired")
	requireRowUnchanged(t, s, before)
}

func TestHeldRowRecoverStartsAndSettlesOthers(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	transitionCommand(t, s, 0, "starting")
	queuedStatus := corruptStatus(t, s, 0, Object{"Queued": Object{}})
	transitionCommand(t, s, 1, "starting")
	doneStatus := corruptStatus(t, s, 1, Object{"Done": Object{"result": "Success"}})
	transitionCommand(t, s, 2, "starting")
	if err := d.recover(); err != nil {
		t.Fatalf("a held row refused startup: %v", err)
	}
	requireRowUnchanged(t, s, queuedStatus)
	requireRowUnchanged(t, s, doneStatus)
	if j := transitionRow(t, s, 2); j.phase != "outcome" || j.outcome.Int64 != 125 {
		t.Fatalf("a claim after the held rows was not failed: %+v", j)
	}
	if problems := heldProblems(t, d); problems[0] != "held" || problems[1] != "held" || len(problems) != 2 {
		t.Fatalf("snapshot problems: %v", problems)
	}
}

func TestHeldRowCancelAndKillSettle(t *testing.T) {
	failed := Object{"Done": Object{"result": Object{"Failed": 7}}}
	queued := Object{"Queued": Object{}}
	running := Object{"Running": Object{}}
	command := func(phase string, status Object) func(*testing.T, *queueStore) {
		return func(t *testing.T, s *queueStore) {
			if phase == "running" {
				runningWithWorker(t, s, 0, exitedWorker(t))
			} else {
				transitionCommand(t, s, 0, phase)
			}
			corruptStatus(t, s, 0, status)
		}
	}
	lease := func(phase string, status Object) func(*testing.T, *queueStore) {
		return func(t *testing.T, s *queueStore) {
			transitionLease(t, s, 0, phase)
			corruptStatus(t, s, 0, status)
		}
	}
	cases := []struct {
		name  string
		setup func(*testing.T, *queueStore)
		want  int64
	}{
		{"queued as Running", command("queued", running), 130},
		{"starting as Queued", command("starting", queued), 130},
		{"starting as Done", command("starting", failed), 130},
		{"running as Queued", command("running", queued), 130},
		{"running as Done", command("running", failed), 130},
		{"outcome as Queued keeps its exit status", command("outcome", queued), 7},
		{"running without a worker", func(t *testing.T, s *queueStore) {
			runningWithWorker(t, s, 0, exitedWorker(t))
			corruptStatus(t, s, 0, queued)
			if _, err := s.db.Exec("UPDATE queue_jobs SET worker=NULL WHERE id=0"); err != nil {
				t.Fatal(err)
			}
		}, 130},
		{"unreadable status", func(t *testing.T, s *queueStore) {
			runningWithWorker(t, s, 0, exitedWorker(t))
			if _, err := s.db.Exec("UPDATE queue_jobs SET task='{}' WHERE id=0"); err != nil {
				t.Fatal(err)
			}
		}, 130},
		{"held lease as Queued", lease("running", queued), 0},
		{"queued lease as Running", lease("queued", running), 0},
		{"lease with no lease object", func(t *testing.T, s *queueStore) {
			transitionLease(t, s, 0, "running")
			if _, err := s.db.Exec(`UPDATE queue_jobs SET task='{"kind":"lease","status":{"Queued":{}}}' WHERE id=0`); err != nil {
				t.Fatal(err)
			}
		}, 0},
	}
	for _, op := range []string{"cancel", "kill"} {
		for _, test := range cases {
			t.Run(op+"/"+test.name, func(t *testing.T) {
				d, s, _, _ := heldDaemon(t)
				test.setup(t, s)
				if heldProblems(t, d)[0] != "held" {
					t.Fatal("fixture is not held")
				}
				if _, err := d.control(op, Object{"id": 0}); err != nil {
					t.Fatalf("%s did not settle: %v", op, err)
				}
				heldTick(t, d)
				heldTick(t, d)
				j := transitionRow(t, s, 0)
				if j.phase != "done" || !j.outcome.Valid || j.outcome.Int64 != test.want {
					t.Fatalf("%s reported success without settling: %+v", op, j)
				}
				if err := queueJobAgreement(j); err != nil {
					t.Fatalf("settled row still disagrees: %v", err)
				}
				if len(heldProblems(t, d)) != 0 {
					t.Fatal("settled row is still reported")
				}
			})
		}
	}
}

// pgid 1 fails identity verification, so nothing may be signalled or settled.
func TestHeldRowWithUnverifiableProcessFailsVisibly(t *testing.T) {
	for _, op := range []string{"cancel", "kill"} {
		t.Run(op, func(t *testing.T) {
			d, s, _, _ := heldDaemon(t)
			transitionCommand(t, s, 0, "running")
			before := corruptStatus(t, s, 0, Object{"Queued": Object{}})
			_, err := d.control(op, Object{"id": 0})
			if err == nil || !strings.Contains(err.Error(), "could not be verified") {
				t.Fatalf("error=%v", err)
			}
			requireRowUnchanged(t, s, before)
		})
	}
}

// Cancel asks a live owned process to stop. The row settles only after it goes.
func TestHeldRowWithLiveProcessSettlesAfterItExits(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	ready := filepath.Join(t.TempDir(), "ready")
	process := exec.Command("/bin/sh", "-c", `trap '' INT; touch "$1"; while :; do sleep 1; done`, "fixture", ready)
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := process.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	exited := make(chan error, 1)
	go func() { exited <- process.Wait() }()
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(ready); return err == nil })
	boot, err := BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	members, err := ProcessGroupMembers(pgid)
	if err != nil {
		t.Fatal(err)
	}
	runningWithWorker(t, s, 0, Object{"pgid": pgid, "pid": pgid, "boot_uuid": boot, "members": members})
	corruptStatus(t, s, 0, Object{"Queued": Object{}})

	if _, err = d.control("cancel", Object{"id": 0}); err != nil {
		t.Fatal(err)
	}
	heldTick(t, d)
	j := transitionRow(t, s, 0)
	if j.phase != "running" || !boolean(j.task["cancelled"]) {
		t.Fatalf("a row with a live process was settled or lost the request: %+v", j)
	}
	if err = syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("fixture process should ignore SIGINT: %v", err)
	}
	if _, err = d.control("kill", Object{"id": 0}); err != nil {
		t.Fatal(err)
	}
	<-exited
	fixtureUntil(t, 8*time.Second, func() bool {
		heldTick(t, d)
		return transitionRow(t, s, 0).phase != "running"
	})
	if j = transitionRow(t, s, 0); !j.outcome.Valid || j.outcome.Int64 != 130 {
		t.Fatalf("settled row: %+v", j)
	}
}

func TestHeldRowRefusesOtherOperationsClearly(t *testing.T) {
	statuses := map[string]Object{"running/Queued": {"Queued": Object{}}, "running/Done": {"Done": Object{"result": "Success"}}}
	for name, status := range statuses {
		for _, op := range []string{"start", "hold", "enqueue", "priority", "next", "last", "pause-job", "resume-job"} {
			t.Run(name+"/"+op, func(t *testing.T) {
				d, s, _, _ := heldDaemon(t)
				runningWithWorker(t, s, 0, exitedWorker(t))
				before := corruptStatus(t, s, 0, status)
				_, err := d.control(op, Object{"id": 0, "priority": 3})
				if err == nil || !strings.Contains(err.Error(), "only cancel or kill can settle it") {
					t.Fatalf("error=%v", err)
				}
				requireRowUnchanged(t, s, before)
			})
		}
	}
}

// Only cancel and kill settle a hold. A lease owner's release keeps refusing.
func TestHeldLeaseReleaseAndRenewRefuse(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	lease := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 60})
	id, _ := integer(lease["id"])
	before := corruptStatus(t, s, id, Object{"Queued": Object{}})
	for _, op := range []string{"lease-release", "lease-renew"} {
		if _, err := d.control(op, Object{"id": id, "force": true}); !errors.Is(err, errQueueStateDisagreement) {
			t.Fatalf("%s: %v", op, err)
		}
		requireRowUnchanged(t, s, before)
	}
}

// Ordinary rows keep their no-op and their refusals through the store's rule.
func TestQueueControlHoldAndPauseKeepTheirBehaviour(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	queued := transitionCommand(t, s, 0, "queued")
	for _, test := range []struct {
		op, want string
	}{{"hold", "Stashed"}, {"hold", "Stashed"}, {"enqueue", "Queued"}} {
		if _, err := d.control(test.op, Object{"id": queued.id}); err != nil {
			t.Fatal(err)
		}
		if got := transitionStatus(t, transitionRow(t, s, 0)); got != test.want {
			t.Fatalf("%s left %s", test.op, got)
		}
	}
	lease := transitionLease(t, s, 1, "queued")
	claimed := transitionCommand(t, s, 2, "starting")
	for _, row := range []queueJob{lease, claimed} {
		for _, op := range []string{"hold", "enqueue"} {
			if _, err := d.control(op, Object{"id": row.id}); err != nil {
				t.Fatalf("%s on job %d was not a no-op: %v", op, row.id, err)
			}
			requireRowUnchanged(t, s, row)
		}
	}
	if _, err := d.control("pause-job", Object{"id": lease.id}); err == nil || !strings.Contains(err.Error(), "A lease has no process") {
		t.Fatalf("pause on a lease: %v", err)
	}
	if _, err := d.control("pause-job", Object{"id": queued.id}); err == nil || !strings.Contains(err.Error(), "Only active jobs") {
		t.Fatalf("pause on a queued job: %v", err)
	}
	if _, err := d.control("pause-job", Object{"id": claimed.id}); err != nil {
		t.Fatal(err)
	}
	if got := transitionStatus(t, transitionRow(t, s, 2)); got != "Paused" {
		t.Fatalf("pause left %s", got)
	}
	if _, err := d.control("resume-job", Object{"id": claimed.id}); err != nil {
		t.Fatal(err)
	}
	_, details, err := TaskStatus(transitionRow(t, s, 2).task)
	if err != nil || details["start"] == nil {
		t.Fatalf("resume lost the attempt's start: %v %v", details, err)
	}
}

func TestQueueStoreStatusMethodsRefuseOutsideTheirPhases(t *testing.T) {
	_, s := edgeQueueStore(t)
	lease := transitionLease(t, s, 0, "queued")
	if err := s.setWaiting(lease, true); !errors.Is(err, errQueueIllegalTransition) {
		t.Fatalf("a lease was stashed: %v", err)
	}
	if err := s.setPaused(lease, true); !errors.Is(err, errQueueIllegalTransition) {
		t.Fatalf("a lease was paused: %v", err)
	}
	starting := transitionCommand(t, s, 1, "starting")
	if err := s.setWaiting(starting, true); !errors.Is(err, errQueueIllegalTransition) {
		t.Fatalf("a claimed job was stashed: %v", err)
	}
	queued := transitionCommand(t, s, 2, "queued")
	if err := s.setPaused(queued, true); !errors.Is(err, errQueueIllegalTransition) {
		t.Fatalf("a queued job was paused: %v", err)
	}
	done := transitionCommand(t, s, 3, "done")
	if err := s.markCancelled(done); !errors.Is(err, errQueueIllegalTransition) {
		t.Fatalf("a done job was cancelled: %v", err)
	}
	if err := s.settleHeld(queued, 130); !errors.Is(err, errQueueIllegalTransition) {
		t.Fatalf("a healthy job was settled as held: %v", err)
	}
	if err := s.settleHeldLease(lease, "2026-11-02T00:00:00Z"); !errors.Is(err, errQueueIllegalTransition) {
		t.Fatalf("a healthy lease was settled as held: %v", err)
	}
	for _, row := range []queueJob{lease, starting, queued, done} {
		requireRowUnchanged(t, s, row)
	}
	stale := transitionRow(t, s, 2)
	if err := s.setWaiting(transitionRow(t, s, 2), true); err != nil {
		t.Fatal(err)
	}
	if err := s.markCancelled(stale); err != nil {
		t.Fatalf("attempt and phase still match, so the request applies: %v", err)
	}
	advanced := transitionRow(t, s, 2)
	if err := s.recordOutcome(advanced, 130); err != nil {
		t.Fatal(err)
	}
	if err := s.markCancelled(stale); !errors.Is(err, errQueueChanged) {
		t.Fatalf("a stale cancel request: %v", err)
	}
}

func TestHeldRowLogIsRateLimited(t *testing.T) {
	d, s, clock, output := heldDaemon(t)
	runningWithWorker(t, s, 0, exitedWorker(t))
	corruptStatus(t, s, 0, Object{"Queued": Object{}})
	transitionCommand(t, s, 1, "starting")
	corruptStatus(t, s, 1, Object{"Queued": Object{}})
	lines := func(id int) int { return strings.Count(output.String(), fmt.Sprintf("holding job %d,", id)) }
	for range 5 {
		heldTick(t, d)
		clock.advance(10 * time.Millisecond)
	}
	if lines(0) != 1 || lines(1) != 1 {
		t.Fatalf("each held row should log once: %q", output.String())
	}
	clock.advance(queueLogInterval)
	heldTick(t, d)
	if lines(0) != 2 || lines(1) != 2 {
		t.Fatalf("each held row should log again after the interval: %q", output.String())
	}
	d.logLimited("tick", "repeated failure")
	d.logLimited("tick", "repeated failure")
	if count := strings.Count(output.String(), "repeated failure"); count != 1 {
		t.Fatalf("tick errors were not limited: %d", count)
	}
}

func heldStatus(t *testing.T, d *queueDaemon) Object {
	t.Helper()
	d.g.queueControl = func(op string, payload Object) (Object, error) { return d.control(op, payload) }
	status, err := StatusData(d.g)
	if err != nil {
		t.Fatalf("a held row broke the status view: %v", err)
	}
	return status
}

func TestHeldRowAppearsInStatusSnapshot(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	healthy := transitionCommand(t, s, 2, "queued")
	if status := heldStatus(t, d); status["problems"] != nil {
		t.Fatalf("a healthy queue reports problems: %v", status["problems"])
	}
	if snapshot, err := d.control("snapshot", nil); err != nil || snapshot["problems"] != nil {
		t.Fatalf("a healthy snapshot changed: %v %v", snapshot["problems"], err)
	}
	runningWithWorker(t, s, 0, exitedWorker(t))
	corruptStatus(t, s, 0, Object{"Queued": Object{}})
	runningWithWorker(t, s, 1, exitedWorker(t))
	if _, err := s.db.Exec("UPDATE queue_jobs SET task='{}' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	status := heldStatus(t, d)
	problems := map[int]Object{}
	for _, raw := range list(status["problems"]) {
		id, _ := integer(obj(raw)["id"])
		problems[id] = obj(raw)
	}
	if len(problems) != 2 || problems[0]["kind"] != "held" || !strings.Contains(text(problems[0]["message"]), "running with status Queued") {
		t.Fatalf("problems: %v", status["problems"])
	}
	seen := map[int]Object{}
	for _, raw := range list(status["jobs"]) {
		id, _ := integer(obj(raw)["id"])
		seen[id] = obj(raw)
	}
	job := seen[0]
	if job["state"] != "held" || job["canCancel"] != true || job["canStart"] != false || job["canReorder"] != false || job["queuePosition"] != nil {
		t.Fatalf("held job offers the wrong actions: %v", job)
	}
	if seen[healthy.id]["state"] != "queued" {
		t.Fatalf("other jobs must stay visible: %v", seen)
	}
}

// Cleanup failing for good used to stall in silence, with its message written
// to a field nothing read.
func TestCleanupFailureAppearsInStatusSnapshot(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	j := transitionCommand(t, s, 0, "outcome")
	mustWrite(t, d.g.ReceiptPath(0), Object{"task_id": 0, "token": strings.Repeat("0", 32), "pgid": "invalid"})
	heldTick(t, d)
	if transitionRow(t, s, 0).phase != "outcome" {
		t.Fatal("fixture cleanup did not fail")
	}
	status := heldStatus(t, d)
	problems := list(status["problems"])
	if len(problems) != 1 || obj(problems[0])["kind"] != "cleanup" || text(obj(problems[0])["message"]) == "" {
		t.Fatalf("cleanup failure stayed silent: %v", status["problems"])
	}
	if err := os.Remove(d.g.ReceiptPath(0)); err != nil {
		t.Fatal(err)
	}
	heldTick(t, d)
	heldTick(t, d)
	if transitionRow(t, s, 0).phase != "done" || heldStatus(t, d)["problems"] != nil {
		t.Fatalf("cleanup recovery left a stale problem: %+v", j)
	}
}

// Hold, enqueue and start refuse a held row that reads as waiting. A scheduling
// pass ends on a refusal, which would stall every other job.
func TestHeldRowDoesNotStopSchedulingModeChange(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	runningWithWorker(t, s, 0, exitedWorker(t))
	before := corruptStatus(t, s, 0, Object{"Stashed": Object{}})
	if err := s.setWaiting(transitionCommand(t, s, 1, "queued"), true); err != nil {
		t.Fatal(err)
	}
	d.g.queueControl = func(op string, payload Object) (Object, error) { return d.control(op, payload) }
	if err := SetSchedulingMode(d.g, "shadow"); err != nil {
		t.Fatalf("a held row stopped the mode change: %v", err)
	}
	if got := transitionStatus(t, transitionRow(t, s, 1)); got != "Queued" {
		t.Fatalf("the healthy job was not enqueued: %s", got)
	}
	requireRowUnchanged(t, s, before)
}

// Legacy history has no outcome column. It is terminal, not held.
func TestLegacyHistoryIsNotHeld(t *testing.T) {
	d, s, _, _ := heldDaemon(t)
	mustWrite(t, filepath.Join(d.g.State, "legacy-queue-import.json"), Object{
		"tasks":  Object{"4": Object{"priority": 0, "status": Object{"Done": Object{"result": Object{"Failed": 7}}}}},
		"groups": Object{"default": Object{"parallel_tasks": 1, "status": "Running"}},
	})
	if err := s.importLegacy(); err != nil {
		t.Fatal(err)
	}
	before := transitionRow(t, s, 4)
	heldTick(t, d)
	if problems := heldProblems(t, d); len(problems) != 0 {
		t.Fatalf("legacy history reported: %v", problems)
	}
	if _, err := d.control("cancel", Object{"id": 4}); err != nil {
		t.Fatalf("cancelling finished history should stay a no-op: %v", err)
	}
	requireRowUnchanged(t, s, before)
}

// The full path: a real daemon starts with a held row, takes commands, runs other
// jobs and settles the row through its socket.
func TestRealDaemonStartsWithHeldRowAndRunsOtherJobs(t *testing.T) {
	f := daemonAcceptance(t)
	if _, err := f.gate.QueueControl("limit", Object{"limit": 2}); err != nil {
		t.Fatal(err)
	}
	if err := f.daemon.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := f.daemon.Wait(); err != nil {
		t.Fatal(err)
	}
	s, err := openQueueStore(f.gate.State, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	id := 40
	value, attempt := strings.Repeat("c", 32), strings.Repeat("d", 32)
	task := Object{"token": value, "priority": 0, "created_at": queueNow(), "status": Object{"Queued": Object{}}, "logPath": filepath.Join(f.gate.State, "logs", "40.log")}
	data, err := queueJSON(task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase) VALUES(?,?,?,?,'starting')", id, value, data, attempt); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE queue_settings SET next_id=41 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	path, _ := f.gate.PayloadPath(value)
	mustWrite(t, path, Object{"token": value, "task_id": id, "argv": []string{"/bin/sh", "-c", "echo forbidden"}, "cwd": f.root, "env": Object{}})
	mustWrite(t, f.gate.ReceiptPath(id), Object{"task_id": id, "token": value})

	queueRestartFixture(t, f)
	if output := f.launch("/bin/sh", "-c", "printf ran").wait(t, 0); output != "ran" {
		t.Fatalf("another job did not run past the held row: %q", output)
	}
	status, err := StatusData(f.gate)
	if err != nil {
		t.Fatal(err)
	}
	if problems := list(status["problems"]); len(problems) != 1 || obj(problems[0])["kind"] != "held" {
		t.Fatalf("status problems: %v", status["problems"])
	}
	if j := transitionRow(t, s, id); j.phase != "starting" {
		t.Fatalf("the held row was self-healed: %+v", j)
	}
	if _, err = f.gate.QueueControl("cancel", Object{"id": id}); err != nil {
		t.Fatal(err)
	}
	fixtureUntil(t, 8*time.Second, func() bool { return transitionRow(t, s, id).phase == "done" })
	if j := transitionRow(t, s, id); !j.outcome.Valid || j.outcome.Int64 != 130 {
		t.Fatalf("settled row: %+v", j)
	}
}
