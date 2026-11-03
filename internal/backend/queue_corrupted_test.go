package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Rows here are corrupted with raw SQL on purpose: no strict store method can
// write them. The daemon settles such a row and logs it once.

func corruptedDaemon(t *testing.T) (*queueDaemon, *queueStore, *laneClock, *bytes.Buffer) {
	t.Helper()
	d, clock := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":4}`)})
	output := &bytes.Buffer{}
	d.logOutput = output
	d.g.queueControl = func(op string, payload Object) (Object, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.control(op, payload)
	}
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

func corruptTask(t *testing.T, s *queueStore, id int, task string) queueJob {
	t.Helper()
	if _, err := s.db.Exec("UPDATE queue_jobs SET task=? WHERE id=?", task, id); err != nil {
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

// liveWorker runs a verified process group that ignores SIGINT, so only SIGKILL
// ends it.
func liveWorker(t *testing.T) (Object, int, <-chan struct{}) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	process := exec.Command("/bin/sh", "-c", `trap '' INT; touch "$1"; while :; do sleep 1; done`, "fixture", ready)
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := process.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	exited := make(chan struct{})
	go func() { _ = process.Wait(); close(exited) }()
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(ready); return err == nil })
	boot, err := BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	members, err := ProcessGroupMembers(pgid)
	if err != nil {
		t.Fatal(err)
	}
	return Object{"pgid": pgid, "pid": pgid, "boot_uuid": boot, "members": members}, pgid, exited
}

func runningWithWorker(t *testing.T, s *queueStore, id int, worker Object) queueJob {
	t.Helper()
	j := transitionCommand(t, s, id, "starting")
	if err := s.claimWorker(j.id, j.token, j.attempt, worker); err != nil {
		t.Fatal(err)
	}
	return transitionRow(t, s, id)
}

func corruptedTick(t *testing.T, d *queueDaemon) {
	t.Helper()
	d.mu.Lock()
	err := d.tick()
	d.mu.Unlock()
	if err != nil {
		t.Fatalf("a corrupted row stopped the tick: %v", err)
	}
	d.cleanup.Wait()
}

// tickingForClient keeps the daemon working while Cancel() polls it from outside.
func tickingForClient(t *testing.T, d *queueDaemon) func() {
	t.Helper()
	start := make(chan struct{})
	close(start)
	return ticksFrom(d, start)
}

// tickingAfter holds ticks until the daemon has replied to op, or to anything when
// op is empty. A tick that came first would settle a row whose worker has exited
// with 125, before Cancel has read it or sent a cancel.
func tickingAfter(op string) func(*testing.T, *queueDaemon) func() {
	return func(t *testing.T, d *queueDaemon) func() {
		t.Helper()
		replied := make(chan struct{})
		var once sync.Once
		control := d.g.queueControl
		d.g.queueControl = func(name string, payload Object) (Object, error) {
			reply, err := control(name, payload)
			if op == "" || name == op {
				once.Do(func() { close(replied) })
			}
			return reply, err
		}
		return ticksFrom(d, replied)
	}
}

func ticksFrom(d *queueDaemon, start <-chan struct{}) func() {
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-stop:
			return
		case <-start:
		}
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				d.mu.Lock()
				_ = d.tick()
				d.mu.Unlock()
			}
		}
	}()
	return func() {
		close(stop)
		<-stopped
		d.cleanup.Wait()
	}
}

func cancelledExit(t *testing.T, d *queueDaemon, id int) int {
	t.Helper()
	raw, err := ReadPrivate(filepath.Join(d.g.Cancelled, fmt.Sprintf("%d.json", id)))
	if err != nil {
		t.Fatal(err)
	}
	code, _ := integer(raw["exit_code"])
	return code
}

// The reported reproduction: main settles all three rows, and so must this.
func TestCorruptedRowScenarioSettlesAllThree(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
	runningWithWorker(t, s, 1, exitedWorker(t))
	corruptStatus(t, s, 1, Object{"Queued": Object{}})
	transitionCommand(t, s, 2, "outcome")
	lease := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 60})
	for range 3 {
		corruptedTick(t, d)
	}
	first := transitionRow(t, s, 1)
	if first.phase != "done" || first.outcome.Int64 != 125 {
		t.Fatalf("corrupted row: %+v", first)
	}
	if err := queueJobAgreement(first); err != nil {
		t.Fatal(err)
	}
	if j := transitionRow(t, s, 2); j.phase != "done" {
		t.Fatalf("valid outcome: %+v", j)
	}
	requireLeaseState(t, d, lease, "held")
}

func TestCorruptedStartingRowsLetRecoverFinish(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
	rows := []func(int){
		func(id int) {
			transitionCommand(t, s, id, "starting")
			corruptStatus(t, s, id, Object{"Queued": Object{}})
		},
		func(id int) {
			transitionCommand(t, s, id, "starting")
			corruptStatus(t, s, id, Object{"Done": Object{"result": "Success"}})
		},
		func(id int) { transitionCommand(t, s, id, "starting"); corruptTask(t, s, id, "{}") },
		func(id int) { transitionCommand(t, s, id, "starting") }, // Healthy, as a crash leaves it.
	}
	for id, row := range rows {
		row(id)
	}
	for id, name := range map[int]string{4: "Running", 5: "Paused"} {
		transitionLease(t, s, id, "queued")
		corruptTask(t, s, id, fmt.Sprintf(`{"kind":"lease","lane":"qa","status":{%q:{}},"lease":{"state":"held"}}`, name))
		if _, err := s.db.Exec("UPDATE queue_jobs SET phase='starting' WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.recover(); err != nil {
		t.Fatalf("a corrupted row refused startup: %v", err)
	}
	for id := range 6 {
		j := transitionRow(t, s, id)
		if j.phase != "outcome" || !j.outcome.Valid || j.outcome.Int64 != 125 {
			t.Fatalf("row %d was not recorded as main records it: %+v", id, j)
		}
		if err := queueJobAgreement(j); err != nil {
			t.Fatalf("row %d: %v", id, err)
		}
	}
}

func TestCorruptedRowsDoNotDelayLeaseExpiry(t *testing.T) {
	d, s, clock, _ := corruptedDaemon(t)
	runningWithWorker(t, s, 0, exitedWorker(t))
	corruptStatus(t, s, 0, Object{"Queued": Object{}})
	transitionCommand(t, s, 1, "running") // Unverifiable, so it stays and must not stop the tick.
	corruptStatus(t, s, 1, Object{"Queued": Object{}})
	healthy := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 60})
	corrupted := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 60})
	id, _ := integer(corrupted["id"])
	corruptStatus(t, s, id, Object{"Queued": Object{}})
	clock.advance(61 * time.Second)
	corruptedTick(t, d)
	requireLeaseState(t, d, healthy, "expired")
	requireLeaseState(t, d, corrupted, "expired")
}

// Tick and the client's Cancel both leave a verified live process alone until it
// has really gone, and neither reports cancelled before then.
func TestCorruptedRowWithLiveProcessIsNotSettledAway(t *testing.T) {
	t.Run("tick", func(t *testing.T) {
		d, s, _, _ := corruptedDaemon(t)
		worker, pgid, _ := liveWorker(t)
		runningWithWorker(t, s, 0, worker)
		before := corruptStatus(t, s, 0, Object{"Queued": Object{}})
		for range 3 {
			corruptedTick(t, d)
		}
		requireRowUnchanged(t, s, before)
		if err := syscall.Kill(-pgid, 0); err != nil {
			t.Fatalf("tick touched a live process: %v", err)
		}
	})
	t.Run("Cancel escalates and settles after exit", func(t *testing.T) {
		d, s, _, _ := corruptedDaemon(t)
		worker, pgid, exited := liveWorker(t)
		j := runningWithWorker(t, s, 0, worker)
		corruptStatus(t, s, 0, Object{"Queued": Object{}})
		mustWrite(t, d.g.ReceiptPath(0), Object{"task_id": 0, "token": j.token})
		stop := tickingForClient(t, d)
		defer stop()
		result := make(chan error, 1)
		go func() { result <- Cancel(d.g, 0) }()
		// SIGINT is ignored, so the row must still be running before the escalation.
		time.Sleep(time.Second)
		if current := transitionRow(t, s, 0); current.phase != "running" || !boolean(current.task["cancelled"]) {
			t.Fatalf("settled while its process lived, or lost the request: %+v", current)
		}
		if err := syscall.Kill(-pgid, 0); err != nil {
			t.Fatalf("process should ignore SIGINT: %v", err)
		}
		if err := <-result; err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		<-exited
		current := transitionRow(t, s, 0)
		if current.phase != "done" || current.outcome.Int64 != 130 || cancelledExit(t, d, 0) != 130 {
			t.Fatalf("settled row: %+v", current)
		}
	})
	t.Run("Cancel on a Done reading row reports no false cancel", func(t *testing.T) {
		d, s, _, _ := corruptedDaemon(t)
		worker, pgid, _ := liveWorker(t)
		j := runningWithWorker(t, s, 0, worker)
		before := corruptStatus(t, s, 0, Object{"Done": Object{"result": "Success"}})
		mustWrite(t, d.g.ReceiptPath(0), Object{"task_id": 0, "token": j.token})
		stop := tickingForClient(t, d)
		defer stop()
		if err := Cancel(d.g, 0); err == nil || !strings.Contains(err.Error(), "cleanup is not confirmed") {
			t.Fatalf("a live process was reported cancelled: %v", err)
		}
		requireRowUnchanged(t, s, before)
		if err := syscall.Kill(-pgid, 0); err != nil {
			t.Fatalf("process was signalled: %v", err)
		}
	})
}

// pgid 1 fails identity verification. Nothing is signalled or settled, and the
// row does not stop other jobs.
func TestCorruptedRowWithUnverifiableWorker(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
	transitionCommand(t, s, 0, "running")
	before := corruptStatus(t, s, 0, Object{"Queued": Object{}})
	transitionCommand(t, s, 1, "outcome")
	for range 3 {
		corruptedTick(t, d)
	}
	requireRowUnchanged(t, s, before)
	if j := transitionRow(t, s, 1); j.phase != "done" {
		t.Fatalf("a job behind the row stalled: %+v", j)
	}
	if err := Cancel(d.g, 0); err == nil || !strings.Contains(err.Error(), "Unsafe process group") {
		t.Fatalf("Cancel reported success on an unverifiable worker: %v", err)
	}
	if j := transitionRow(t, s, 0); j.phase != "running" {
		t.Fatalf("Cancel settled an unverifiable row: %+v", j)
	}
}

func TestCorruptedRowWithoutWorkerSettles(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
	runningWithWorker(t, s, 0, exitedWorker(t))
	corruptStatus(t, s, 0, Object{"Queued": Object{}})
	if _, err := s.db.Exec("UPDATE queue_jobs SET worker=NULL WHERE id=0"); err != nil {
		t.Fatal(err)
	}
	corruptedTick(t, d)
	if j := transitionRow(t, s, 0); j.phase != "outcome" || j.outcome.Int64 != 125 {
		t.Fatalf("row: %+v", j)
	}
}

func TestCorruptedRowsCancelThroughClient(t *testing.T) {
	queued, running := Object{"Queued": Object{}}, Object{"Running": Object{}}
	failed := Object{"Done": Object{"result": Object{"Failed": 7}}}
	for _, test := range []struct {
		name       string
		setup      func(*testing.T, *queueStore) queueJob
		code, file int
		ticking    func(*testing.T, *queueDaemon) func()
	}{
		{"queued reading Running", func(t *testing.T, s *queueStore) queueJob {
			transitionCommand(t, s, 0, "queued")
			return corruptStatus(t, s, 0, running)
		}, 130, 130, tickingForClient},
		{"starting reading Queued", func(t *testing.T, s *queueStore) queueJob {
			transitionCommand(t, s, 0, "starting")
			return corruptStatus(t, s, 0, queued)
		}, 130, 130, tickingForClient},
		{"running reading Queued", func(t *testing.T, s *queueStore) queueJob {
			runningWithWorker(t, s, 0, exitedWorker(t))
			return corruptStatus(t, s, 0, queued)
		}, 130, 130, tickingAfter("cancel")},
		{"running reading Done keeps main's reported code", func(t *testing.T, s *queueStore) queueJob {
			runningWithWorker(t, s, 0, exitedWorker(t))
			return corruptStatus(t, s, 0, failed)
		}, 125, 7, tickingAfter("")},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, s, _, _ := corruptedDaemon(t)
			j := test.setup(t, s)
			mustWrite(t, d.g.ReceiptPath(0), Object{"task_id": 0, "token": j.token})
			stop := test.ticking(t, d)
			defer stop()
			if err := Cancel(d.g, 0); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if receipt, err := d.g.Receipt(0); err != nil || receipt != nil {
				t.Fatalf("Cancel returned before cleanup: %v %v", receipt, err)
			}
			current := transitionRow(t, s, 0)
			if current.phase != "done" || current.outcome.Int64 != int64(test.code) || cancelledExit(t, d, 0) != test.file {
				t.Fatalf("row %+v, cancelled file %d", current, cancelledExit(t, d, 0))
			}
			if err := queueJobAgreement(current); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, test := range []struct {
		name, phase string
		status      Object
	}{{"granted lease reading Queued", "running", queued}, {"queued lease reading Running", "queued", running}} {
		t.Run(test.name, func(t *testing.T) {
			d, s, _, _ := corruptedDaemon(t)
			transitionLease(t, s, 0, test.phase)
			corruptStatus(t, s, 0, test.status)
			stop := tickingForClient(t, d)
			defer stop()
			if err := Cancel(d.g, 0); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			current := transitionRow(t, s, 0)
			if current.phase != "done" || current.outcome.Int64 != 0 {
				t.Fatalf("lease row: %+v", current)
			}
		})
	}
}

// Main panicked here, on an assignment into the missing lease object.
func TestCorruptedLeaseWithoutLeaseObjectDoesNotPanic(t *testing.T) {
	t.Run("granted", func(t *testing.T) {
		d, s, _, _ := corruptedDaemon(t)
		transitionLease(t, s, 0, "queued")
		corruptTask(t, s, 0, `{"kind":"lease","lane":"qa","priority":0,"status":{"Queued":{}}}`)
		corruptedTick(t, d)
		if j := transitionRow(t, s, 0); j.phase != "running" {
			t.Fatalf("row: %+v", j)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		d, s, _, _ := corruptedDaemon(t)
		transitionLease(t, s, 0, "running")
		corruptTask(t, s, 0, `{"kind":"lease","lane":"qa","status":{"Queued":{}}}`)
		if err := Cancel(d.g, 0); err != nil {
			t.Fatal(err)
		}
		if j := transitionRow(t, s, 0); j.phase != "done" {
			t.Fatalf("row: %+v", j)
		}
	})
}

// Each row reports once per daemon run, and no row hides another.
func TestCorruptedRowLogsOnce(t *testing.T) {
	d, s, _, output := corruptedDaemon(t)
	transitionCommand(t, s, 0, "running") // Unverifiable: seen on every tick.
	corruptStatus(t, s, 0, Object{"Queued": Object{}})
	transitionCommand(t, s, 1, "running")
	corruptStatus(t, s, 1, Object{"Stashed": Object{}})
	runningWithWorker(t, s, 2, exitedWorker(t))
	corruptStatus(t, s, 2, Object{"Done": Object{"result": "Success"}})
	for range 5 {
		corruptedTick(t, d)
	}
	text := output.String()
	for _, want := range []string{"corrupted job 0: phase running, status Queued", "corrupted job 1: phase running, status Stashed", "corrupted job 2: phase running, status Done"} {
		if count := strings.Count(text, want); count != 1 {
			t.Fatalf("%q appears %d times in %q", want, count, text)
		}
	}
	if lines := strings.Count(text, "\n"); lines != 3 {
		t.Fatalf("%d lines: %q", lines, text)
	}
	next := newQueueDaemon(d.g, s)
	buffer := &bytes.Buffer{}
	next.logOutput = buffer
	next.mu.Lock()
	err := next.tick()
	next.mu.Unlock()
	next.hooks.wait()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), "corrupted job 0:") {
		t.Fatalf("a new daemon run should report again: %q", buffer.String())
	}
}

func TestCorruptedQueuedRowWithUnreadableStatusSettles(t *testing.T) {
	d, s, _, output := corruptedDaemon(t)
	transitionCommand(t, s, 0, "queued")
	corruptTask(t, s, 0, "{}")
	transitionCommand(t, s, 1, "outcome")
	corruptedTick(t, d)
	if j := transitionRow(t, s, 0); j.phase != "outcome" || j.outcome.Int64 != 125 {
		t.Fatalf("row: %+v", j)
	}
	if !strings.Contains(output.String(), "corrupted job 0: phase queued, status unreadable") {
		t.Fatalf("log: %q", output.String())
	}
}

// A corrupted outcome keeps the exit status it published, or records one.
func TestCorruptedOutcomeRowRetires(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
	transitionCommand(t, s, 0, "outcome")
	corruptStatus(t, s, 0, Object{"Queued": Object{}})
	transitionCommand(t, s, 1, "outcome")
	corruptStatus(t, s, 1, Object{"Running": Object{}})
	if _, err := s.db.Exec("UPDATE queue_jobs SET outcome=NULL WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	corruptedTick(t, d)
	corruptedTick(t, d)
	for id, code := range map[int]int64{0: 7, 1: 125} {
		j := transitionRow(t, s, id)
		if j.phase != "done" || !j.outcome.Valid || j.outcome.Int64 != code {
			t.Fatalf("row %d: %+v", id, j)
		}
		if err := queueJobAgreement(j); err != nil {
			t.Fatalf("row %d: %v", id, err)
		}
	}
}

// A grant or launch the store refuses gives callers the result main's guard gave,
// which dispatch already moves past.
func TestCorruptedRowsKeepMainsDispatchResults(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
	transitionLease(t, s, 0, "queued")
	if _, err := s.db.Exec("UPDATE queue_jobs SET phase='starting' WHERE id=0"); err != nil {
		t.Fatal(err)
	}
	waiting := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 60})
	corruptedTick(t, d)
	requireLeaseState(t, d, waiting, "held")
	runningWithWorker(t, s, 10, exitedWorker(t))
	transitionCommand(t, s, 11, "running")
	before := corruptStatus(t, s, 11, Object{"Queued": Object{}})
	if _, err := d.control("start", Object{"id": 11}); err == nil || err.Error() != "Job changed before launch" {
		t.Fatalf("start on a running row: %v", err)
	}
	if _, err := d.control("hold", Object{"id": 11}); err != nil {
		t.Fatalf("hold should stay a no-op: %v", err)
	}
	requireRowUnchanged(t, s, before)
}

// The settling methods cannot become a way around the strict ones.
func TestQueueStoreOverwriteRefusesHealthyRowsAndKeepsGuards(t *testing.T) {
	_, s := edgeQueueStore(t)
	const at = "2026-11-02T00:00:00Z"
	command := transitionCommand(t, s, 0, "running")
	lease := transitionLease(t, s, 1, "running")
	published := transitionCommand(t, s, 2, "outcome")
	finished := transitionCommand(t, s, 3, "done")
	for name, attempt := range map[string]error{
		"outcome on a healthy command":  s.overwriteOutcome(command, 1),
		"outcome on a healthy lease":    s.overwriteOutcome(lease, 1),
		"outcome on a published result": s.overwriteOutcome(published, 1),
		"outcome on a finished row":     s.overwriteOutcome(finished, 1),
		"lease end on a healthy lease":  s.overwriteLeaseEnd(lease, at),
		"lease end on a command":        s.overwriteLeaseEnd(command, at),
		"cancel on a healthy command":   s.overwriteCancelled(command),
		"cancel on a finished row":      s.overwriteCancelled(finished),
	} {
		if !errors.Is(attempt, errQueueIllegalTransition) {
			t.Fatalf("%s: %v", name, attempt)
		}
	}
	for _, row := range []queueJob{command, lease, published, finished} {
		requireRowUnchanged(t, s, row)
	}
}

func TestQueueStoreOverwriteKeepsStaleGuards(t *testing.T) {
	_, s := edgeQueueStore(t)
	transitionCommand(t, s, 0, "starting")
	view := corruptStatus(t, s, 0, Object{"Queued": Object{}})
	if err := s.overwriteOutcome(view, 125); err != nil {
		t.Fatal(err)
	}
	settled := transitionRow(t, s, 0)
	if err := s.overwriteOutcome(view, 130); !errors.Is(err, errQueueChanged) {
		t.Fatalf("stale overwrite: %v", err)
	}
	if err := s.overwriteCancelled(view); !errors.Is(err, errQueueChanged) {
		t.Fatalf("stale cancel: %v", err)
	}
	requireRowUnchanged(t, s, settled)
	transitionLease(t, s, 1, "running")
	lease := corruptStatus(t, s, 1, Object{"Queued": Object{}})
	if err := s.overwriteLeaseEnd(lease, "2026-11-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := s.overwriteLeaseEnd(lease, "2026-11-02T01:00:00Z"); !errors.Is(err, errQueueChanged) {
		t.Fatalf("stale lease end: %v", err)
	}
}

// Ordinary rows keep their no-op and their refusals through the store's rule.
func TestQueueControlHoldAndPauseKeepTheirBehaviour(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
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
	for _, row := range []queueJob{lease, starting, queued, done} {
		requireRowUnchanged(t, s, row)
	}
	stale := transitionRow(t, s, 2)
	if err := s.recordOutcome(transitionRow(t, s, 2), 130); err != nil {
		t.Fatal(err)
	}
	if err := s.markCancelled(stale); !errors.Is(err, errQueueChanged) {
		t.Fatalf("a stale cancel request: %v", err)
	}
}

// Legacy history has no outcome column. It is terminal, and nothing touches it.
func TestLegacyHistoryStaysUntouched(t *testing.T) {
	d, s, _, output := corruptedDaemon(t)
	mustWrite(t, filepath.Join(d.g.State, "legacy-queue-import.json"), Object{
		"tasks":  Object{"4": Object{"priority": 0, "status": Object{"Done": Object{"result": Object{"Failed": 7}}}}},
		"groups": Object{"default": Object{"parallel_tasks": 1, "status": "Running"}},
	})
	if err := s.importLegacy(); err != nil {
		t.Fatal(err)
	}
	before := transitionRow(t, s, 4)
	corruptedTick(t, d)
	if _, err := d.control("cancel", Object{"id": 4}); err != nil {
		t.Fatalf("cancelling finished history should stay a no-op: %v", err)
	}
	requireRowUnchanged(t, s, before)
	if output.Len() != 0 {
		t.Fatalf("legacy history was reported: %q", output.String())
	}
}

// Captured from main at 7b4b27c on this same fixture, so a healthy snapshot stays
// byte-identical.
const healthySnapshotOnMain = `{"groups":{"default":{"dirty":0,"imported":0,"next_id":5,"parallel_tasks":8,"status":"Running"}},"lanes":{"default":{"concurrency":8,"held":0,"label":"Default","name":"default","plugin":null,"priority":0,"removed":false,"reservation":{"cpuPercent":0,"memoryMiB":0},"running":1,"sharesLimit":true,"ttlSeconds":0,"waiting":1},"qa":{"concurrency":4,"held":0,"label":"qa","name":"qa","plugin":"qa","priority":0,"removed":false,"reservation":{"cpuPercent":0,"memoryMiB":0},"running":0,"sharesLimit":true,"ttlSeconds":0,"waiting":1}},"plugins":[{"error":null,"lanes":["qa"],"loaded":true,"name":"qa","warning":null}],"tasks":{"1":{"priority":0,"status":{"Queued":{}},"token":"00000000000000000000000000000001"},"2":{"priority":0,"status":{"Running":{"start":"2026-11-02T00:00:00Z"}},"token":"00000000000000000000000000000002"},"3":{"priority":0,"status":{"Done":{"result":"Success"}}},"4":{"kind":"lease","lane":"qa","lease":{"state":"queued"},"priority":0,"status":{"Queued":{}}}}}`

func TestHealthySnapshotIsByteIdenticalToMain(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
	insert := func(id int, phase, attempt, task string, worker, outcome any) {
		if _, err := s.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase,worker,outcome) VALUES(?,?,?,?,?,?,?)", id, fmt.Sprintf("%032d", id), task, attempt, phase, worker, outcome); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE queue_settings SET next_id=max(next_id,?) WHERE id=1", id+1); err != nil {
			t.Fatal(err)
		}
	}
	insert(1, "queued", "", fmt.Sprintf(`{"token":"%032d","priority":0,"status":{"Queued":{}}}`, 1), nil, nil)
	insert(2, "running", fmt.Sprintf("%032d", 92), fmt.Sprintf(`{"token":"%032d","priority":0,"status":{"Running":{"start":"2026-11-02T00:00:00Z"}}}`, 2), `{"pgid":99999}`, nil)
	insert(3, "done", fmt.Sprintf("%032d", 93), `{"status":{"Done":{"result":"Success"}},"priority":0}`, nil, 0)
	insert(4, "queued", "", `{"kind":"lease","lane":"qa","priority":0,"status":{"Queued":{}},"lease":{"state":"queued"}}`, nil, nil)
	snapshot, err := d.control("snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != healthySnapshotOnMain {
		t.Fatalf("healthy snapshot changed:\n got %s\nwant %s", raw, healthySnapshotOnMain)
	}
}

// A real daemon starts with corrupted rows, runs another job and keeps serving.
func TestRealDaemonStartsWithCorruptedStartingRows(t *testing.T) {
	f := daemonAcceptance(t)
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
	for id, task := range map[int]string{
		40: `{"priority":0,"status":{"Queued":{}}}`,
		41: `{"priority":0,"status":{"Done":{"result":"Success"}}}`,
		42: `{"kind":"lease","lane":"gone","status":{"Running":{}},"lease":{"state":"held"}}`,
		43: `{"priority":0}`,
	} {
		if _, err = s.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase) VALUES(?,?,?,?,'starting')", id, strings.Repeat("c", 30)+fmt.Sprint(id), task, strings.Repeat("d", 32)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec("UPDATE queue_settings SET next_id=44 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	queueRestartFixture(t, f)
	if output := f.launch("/bin/sh", "-c", "printf ran").wait(t, 0); output != "ran" {
		t.Fatalf("another job did not run: %q", output)
	}
	for _, id := range []int{40, 41, 43} {
		id := id
		fixtureUntil(t, 8*time.Second, func() bool { return transitionRow(t, s, id).phase == "done" })
		if j := transitionRow(t, s, id); j.outcome.Int64 != 125 {
			t.Fatalf("row %d: %+v", id, j)
		}
	}
	if j := transitionRow(t, s, 42); j.phase != "outcome" || j.outcome.Int64 != 125 {
		t.Fatalf("lease row: %+v", j)
	}
}

func TestCorruptedLeaseWithUnreadableStatusEnds(t *testing.T) {
	for _, test := range []struct {
		name, want string
		phase      string
	}{{"granted", "released", "running"}, {"queued", "dropped", "queued"}} {
		t.Run(test.name, func(t *testing.T) {
			d, s, _, output := corruptedDaemon(t)
			transitionLease(t, s, 0, test.phase)
			corruptTask(t, s, 0, `{"kind":"lease","lane":"qa","priority":0,"lease":{"state":"held"}}`)
			corruptedTick(t, d)
			corruptedTick(t, d)
			j := transitionRow(t, s, 0)
			if j.phase != "done" || !j.outcome.Valid || j.outcome.Int64 != 0 {
				t.Fatalf("row: %+v", j)
			}
			lease := obj(j.task["lease"])
			if lease["state"] != test.want || lease["reason"] != "corrupted" {
				t.Fatalf("lease: %v", lease)
			}
			line := fmt.Sprintf("corrupted job 0: phase %s, status unreadable", test.phase)
			if count := strings.Count(output.String(), line); count != 1 {
				t.Fatalf("%q appears %d times in %q", line, count, output.String())
			}
		})
	}
	t.Run("gives up its lane slot", func(t *testing.T) {
		d, _ := laneDaemon(t, 8, map[string]string{"one": laneManifest("one", `{"name":"one","concurrency":1}`)})
		d.logOutput = &bytes.Buffer{}
		first := acquireTestLease(t, d, "one", Object{"ttlSeconds": 3600})
		second := acquireTestLease(t, d, "one", Object{"ttlSeconds": 3600})
		requireLeaseState(t, d, second, "queued")
		id, _ := integer(first["id"])
		corruptTask(t, d.store, id, `{"kind":"lease","lane":"one","priority":0,"lease":{"state":"held"}}`)
		corruptedTick(t, d)
		requireLeaseState(t, d, second, "held")
	})
}

func TestCorruptedStartingRowWithUnreadableStatus(t *testing.T) {
	d, s, _, output := corruptedDaemon(t)
	if _, err := s.db.Exec("UPDATE queue_settings SET parallel_limit=1"); err != nil {
		t.Fatal(err)
	}
	transitionCommand(t, s, 0, "starting")
	before := corruptTask(t, s, 0, `{"priority":0}`)
	queued := transitionCommand(t, s, 1, "queued")
	d.children[0] = &exec.Cmd{}
	corruptedTick(t, d)
	// A live child still owns it, and it keeps its slot until that child exits.
	requireRowUnchanged(t, s, before)
	requireRowUnchanged(t, s, queued)
	delete(d.children, 0)
	if err := s.setWaiting(queued, true); err != nil {
		t.Fatal(err)
	}
	corruptedTick(t, d)
	j := transitionRow(t, s, 0)
	if j.phase != "outcome" || !j.outcome.Valid || j.outcome.Int64 != 125 {
		t.Fatalf("row: %+v", j)
	}
	if count := strings.Count(output.String(), "corrupted job 0: phase starting, status unreadable"); count != 1 {
		t.Fatalf("log: %q", output.String())
	}
}

// Counting a row raises the ceiling, so an unreadable one stays uncounted. A live
// worker still counts against the ceiling through its slot member.
func TestLeaseCapacityWithUnreadableRows(t *testing.T) {
	group := func(limit int) Object {
		return Object{"default": Object{"parallel_tasks": limit, "status": "Running"}}
	}
	running := Object{"priority": 0, "status": Object{"Running": Object{}}}
	for _, test := range []struct {
		name  string
		limit int
		tasks Object
		want  int
	}{
		{"unreadable rows leave the limit", 1, Object{"1": Object{"priority": 0}, "2": Object{"group": "default"}}, 1},
		{"unreadable rows add nothing to what is admitted", 1, Object{"1": running, "2": running, "3": Object{"priority": 0}}, 2},
		{"readable rows still count as on main", 1, Object{"1": running, "2": running}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := testGate(t)
			fakeQueue(t, g, Object{"tasks": test.tasks, "groups": group(test.limit)})
			capacity, err := LeaseCapacity(g)
			if err != nil {
				t.Fatalf("an unreadable row failed the worker: %v", err)
			}
			if capacity != test.want {
				t.Fatalf("capacity %d, want %d", capacity, test.want)
			}
		})
	}
	t.Run("a live member with an unreadable row still fills the ceiling", func(t *testing.T) {
		g := testGate(t)
		fakeQueue(t, g, Object{"tasks": Object{"1": Object{"priority": 0}, "2": running}, "groups": group(1)})
		boot, err := BootIdentity()
		if err != nil {
			t.Fatal(err)
		}
		member := marker(1)
		member["boot_uuid"] = boot
		if acquired, err := AcquireLease(g, member, 1); err != nil || !acquired {
			t.Fatalf("first member: %v %v", acquired, err)
		}
		capacity, err := LeaseCapacity(g)
		if err != nil {
			t.Fatal(err)
		}
		next := marker(2)
		next["boot_uuid"] = boot
		if acquired, err := AcquireLease(g, next, capacity); err != nil || acquired {
			t.Fatalf("a job joined a full slot at capacity %d: %v %v", capacity, acquired, err)
		}
	})
}

func TestCorruptedOutcomeRowUsesAReadableResult(t *testing.T) {
	for _, test := range []struct {
		name, task string
		cancelled  bool
		want       int64
		result     any
	}{
		{"Done with Success stays Success", `{"status":{"Done":{"result":"Success"}},"priority":0}`, false, 0, "Success"},
		{"Done with a failure keeps its code", `{"status":{"Done":{"result":{"Failed":7}}},"priority":0}`, false, 7, Object{"Failed": 7}},
		{"Done with an unknown result is unknown", `{"status":{"Done":{"result":"Mystery"}},"priority":0}`, false, 125, Object{"Failed": 125}},
		{"Running has no result", `{"status":{"Running":{}},"priority":0}`, false, 125, Object{"Failed": 125}},
		{"unreadable has no result", `{"priority":0}`, false, 125, Object{"Failed": 125}},
		{"a cancelled row with no result", `{"cancelled":true,"priority":0}`, true, 130, Object{"Failed": 130}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, s, _, _ := corruptedDaemon(t)
			transitionCommand(t, s, 0, "outcome")
			corruptTask(t, s, 0, test.task)
			if _, err := s.db.Exec("UPDATE queue_jobs SET outcome=NULL WHERE id=0"); err != nil {
				t.Fatal(err)
			}
			corruptedTick(t, d)
			corruptedTick(t, d)
			j := transitionRow(t, s, 0)
			_, details, err := TaskStatus(j.task)
			if err != nil || j.phase != "done" || !j.outcome.Valid || j.outcome.Int64 != test.want || !jsonEqual(details["result"], test.result) {
				t.Fatalf("row %+v details %v err %v", j, details, err)
			}
			if err = queueJobAgreement(j); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func processStopped(t *testing.T, pid int) bool {
	t.Helper()
	raw, err := exec.Command("/bin/ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.HasPrefix(strings.TrimSpace(string(raw)), "T")
}

func TestPauseRefusalSendsNoSignal(t *testing.T) {
	for _, phase := range []string{"queued", "outcome", "done"} {
		t.Run(phase+" reading Running", func(t *testing.T) {
			d, s, _, _ := corruptedDaemon(t)
			worker, pgid, _ := liveWorker(t)
			runningWithWorker(t, s, 0, worker)
			if _, err := s.db.Exec("UPDATE queue_jobs SET phase=? WHERE id=0", phase); err != nil {
				t.Fatal(err)
			}
			before := transitionRow(t, s, 0)
			if _, err := d.control("pause-job", Object{"id": 0}); !errors.Is(err, errQueueIllegalTransition) {
				t.Fatalf("pause: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			if processStopped(t, pgid) {
				t.Fatal("a refused pause stopped the process")
			}
			if err := syscall.Kill(-pgid, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			fixtureUntil(t, 4*time.Second, func() bool { return processStopped(t, pgid) })
			if _, err := d.control("resume-job", Object{"id": 0}); !errors.Is(err, errQueueIllegalTransition) {
				t.Fatalf("resume: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			if !processStopped(t, pgid) {
				t.Fatal("a refused resume continued the process")
			}
			requireRowUnchanged(t, s, before)
		})
	}
	t.Run("a healthy row still pauses and resumes", func(t *testing.T) {
		d, s, _, _ := corruptedDaemon(t)
		worker, pgid, _ := liveWorker(t)
		runningWithWorker(t, s, 0, worker)
		if _, err := d.control("pause-job", Object{"id": 0}); err != nil {
			t.Fatal(err)
		}
		fixtureUntil(t, 4*time.Second, func() bool { return processStopped(t, pgid) })
		if got := transitionStatus(t, transitionRow(t, s, 0)); got != "Paused" {
			t.Fatalf("status %s", got)
		}
		if _, err := d.control("resume-job", Object{"id": 0}); err != nil {
			t.Fatal(err)
		}
		fixtureUntil(t, 4*time.Second, func() bool { return !processStopped(t, pgid) })
		if got := transitionStatus(t, transitionRow(t, s, 0)); got != "Running" {
			t.Fatalf("status %s", got)
		}
	})
}

// A real daemon at limit 1 starts through recover() with one unreadable row, and a
// job submitted afterwards must still run: on main, LeaseCapacity failed it with 125.
func TestRealDaemonJobRunsPastUnreadableRowAtLimitOne(t *testing.T) {
	stoppedAtLimitOne := func(t *testing.T) (*daemonFixture, *queueStore) {
		t.Helper()
		f := daemonAcceptance(t)
		if _, err := f.gate.QueueControl("limit", Object{"limit": 1}); err != nil {
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
		t.Cleanup(s.close)
		return f, s
	}
	insert := func(t *testing.T, s *queueStore, phase, task string, worker, outcome any) {
		t.Helper()
		if _, err := s.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase,worker,outcome) VALUES(40,?,?,?,?,?,?)", strings.Repeat("c", 32), task, strings.Repeat("d", 32), phase, worker, outcome); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE queue_settings SET next_id=41 WHERE id=1"); err != nil {
			t.Fatal(err)
		}
	}
	const command, lease = `{"priority":0}`, `{"kind":"lease","lane":"gone","priority":0,"lease":{"state":"held"}}`
	for _, test := range []struct {
		name, phase, task string
		exited            bool
		outcome           any
	}{
		{"queued", "queued", command, false, nil},
		{"starting", "starting", command, false, nil},
		{"running with an exited worker", "running", command, true, nil},
		{"outcome", "outcome", command, false, 7},
		{"done", "done", command, false, 7},
		{"queued lease", "queued", lease, false, nil},
		{"granted lease", "running", lease, false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, s := stoppedAtLimitOne(t)
			var worker any
			if test.exited {
				data, err := queueJSON(exitedWorker(t))
				if err != nil {
					t.Fatal(err)
				}
				worker = data
			}
			insert(t, s, test.phase, test.task, worker, test.outcome)
			queueRestartFixture(t, f)
			if output := f.launch("/bin/sh", "-c", "printf ran").wait(t, 0); output != "ran" {
				t.Fatalf("a job after the row did not run: %q", output)
			}
			fixtureUntil(t, 8*time.Second, func() bool { return transitionRow(t, s, 40).phase == "done" })
		})
	}
	t.Run("running with a verified live worker keeps its slot", func(t *testing.T) {
		f, s := stoppedAtLimitOne(t)
		worker, pgid, exited := liveWorker(t)
		data, err := queueJSON(worker)
		if err != nil {
			t.Fatal(err)
		}
		insert(t, s, "running", command, data, nil)
		before := transitionRow(t, s, 40)
		queueRestartFixture(t, f)
		started := filepath.Join(f.root, "started")
		client := f.launch("/bin/sh", "-c", `touch "$1"; printf ran`, "fixture", started)
		time.Sleep(time.Second)
		requireRowUnchanged(t, s, before)
		if _, err = os.Stat(started); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a job took the slot of a live worker: %v", err)
		}
		if err = syscall.Kill(-pgid, 0); err != nil {
			t.Fatalf("the live worker was signalled: %v", err)
		}
		if err = syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		<-exited
		if output := client.wait(t, 0); output != "ran" {
			t.Fatalf("the waiting job did not run: %q", output)
		}
	})
}
