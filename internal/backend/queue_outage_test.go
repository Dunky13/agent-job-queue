package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fastOutage(t *testing.T, bound time.Duration) {
	t.Helper()
	oldBound, oldBackoff, oldWindow := queueOutageBound, queueOutageBackoff, queueCleanupWindow
	queueOutageBound, queueOutageBackoff, queueCleanupWindow = bound, func(int) time.Duration { return time.Millisecond }, 20*time.Millisecond
	t.Cleanup(func() { queueOutageBound, queueOutageBackoff, queueCleanupWindow = oldBound, oldBackoff, oldWindow })
}

// captureStreams redirects the process streams the follow loop writes to.
func captureStreams(t *testing.T) func() (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.Create(filepath.Join(dir, "err"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = out, errFile
	restore := func() { os.Stdout, os.Stderr = oldOut, oldErr }
	t.Cleanup(restore)
	return func() (string, string) {
		restore()
		a, _ := os.ReadFile(out.Name())
		b, _ := os.ReadFile(errFile.Name())
		return string(a), string(b)
	}
}

func transportTimeout(t *testing.T) error {
	t.Helper()
	err := fmt.Errorf("Invalid queue daemon response: %w", queueTransportError(os.ErrDeadlineExceeded))
	if !errors.Is(err, errQueueTransportUnavailable) {
		t.Fatal("fixture is not a transport failure", err)
	}
	return err
}

func jobToken(n int) string { return fmt.Sprintf("%032x", n) }

func taskWithToken(token, status string, details Object) Object {
	return Object{"token": token, "status": Object{status: details}}
}

func taskWith(status string, details Object) Object {
	return taskWithToken(jobToken(8), status, details)
}

func running() Object { return taskWith("Running", Object{}) }
func done(result any) Object {
	return taskWith("Done", Object{"result": result})
}

func writeJobLog(t *testing.T, g *Gate, id int, content string) {
	t.Helper()
	path := filepath.Join(g.State, "logs", fmt.Sprint(id)+".log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// step is one scripted snapshot answer: a lost reply, or the job's state.
type step struct {
	task  Object
	fail  bool
	pause bool // report the default group as paused
}

// scriptSnapshots answers each snapshot with the next step (the last repeats).
// Mutations go to mutate, or fail the test when it is nil.
func scriptSnapshots(t *testing.T, g *Gate, steps []step, mutate func(string) (Object, error)) *[]string {
	t.Helper()
	operations := &[]string{}
	n := 0
	g.queueControl = func(operation string, _ Object) (Object, error) {
		*operations = append(*operations, operation)
		if operation != "snapshot" {
			if mutate == nil {
				t.Errorf("unexpected operation %s", operation)
				return nil, errors.New("unexpected operation")
			}
			return mutate(operation)
		}
		current := steps[min(n, len(steps)-1)]
		n++
		if current.fail {
			return nil, transportTimeout(t)
		}
		snapshot := testSnapshot(Object{"7": current.task})
		if current.pause {
			snapshot["groups"] = Object{"default": Object{"status": "Paused", "parallel_tasks": 1}}
		}
		return snapshot, nil
	}
	return operations
}

func signals(n int) chan os.Signal {
	c := make(chan os.Signal, 8)
	for i := 0; i < n; i++ {
		c <- syscall.SIGINT
	}
	return c
}

func mutations(operations []string) []string {
	out := []string{}
	for _, operation := range operations {
		if operation != "snapshot" {
			out = append(out, operation)
		}
	}
	return out
}

func TestFollowRidesOutTransientQueueTransportFailure(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	writeJobLog(t, g, 7, "job output\n")
	scriptSnapshots(t, g, []step{{fail: true}, {fail: true}, {fail: true}, {task: done(Object{"Failed": 41})}}, nil)
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	stdout, stderr := read()
	if err != nil || code != 41 {
		t.Fatal("transient transport failure ended the follow", code, err)
	}
	if stdout != "job output\n" {
		t.Fatalf("job output not delivered: %q", stdout)
	}
	if strings.Count(stderr, "not responding") != 1 || strings.Count(stderr, "answering again") != 1 {
		t.Fatalf("expected one loss and one recovery notice: %q", stderr)
	}
}

func TestFollowEndsVisiblyWhenDaemonStaysUnreachable(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	_, stderr := read()
	if code != 125 || err == nil {
		t.Fatal("sustained outage did not end the follow", code, err)
	}
	for _, want := range []string{"job 7 may still be running", "agent-heavy logs 7"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	if !errors.Is(err, errQueueTransportUnavailable) {
		t.Fatal("cause was hidden", err)
	}
	if strings.Count(stderr, "not responding") != 1 || strings.Contains(stderr, "answering again") {
		t.Fatalf("unexpected notices: %q", stderr)
	}
}

func TestFollowStillFailsAtOnceOnNonTransportError(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	boom := errors.New("Queue returned an invalid task")
	calls := 0
	g.queueControl = func(string, Object) (Object, error) { calls++; return nil, boom }
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	_, stderr := read()
	if code != 125 || !errors.Is(err, boom) {
		t.Fatal("non-transport error was not returned", code, err)
	}
	if calls != 2 || stderr != "" { // one for the log path, one in the loop
		t.Fatalf("non-transport error was retried or announced: calls=%d stderr=%q", calls, stderr)
	}
}

func TestFollowRefusesAnotherJobUnderTheSameID(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	other := taskWithToken(jobToken(99), "Done", Object{"result": "Success"})
	g.queueControl = func(string, Object) (Object, error) { return testSnapshot(Object{"7": other}), nil }
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), jobToken(8))
	read()
	if code != 125 || err == nil || !strings.Contains(err.Error(), "no longer the command this client submitted") {
		t.Fatal("another job's result was used", code, err)
	}
}

// A replaced row discovered after a lost read must stop the held cancellation
// before it sends anything.
func TestFollowDoesNotCancelAReplacedJobAfterALostRead(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	replaced := taskWithToken(jobToken(99), "Running", Object{})
	operations := scriptSnapshots(t, g, []step{{task: running()}, {fail: true}, {task: replaced}}, nil)
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(1), jobToken(8))
	read()
	if code != 125 || err == nil || !strings.Contains(err.Error(), "no longer the command this client submitted") || len(mutations(*operations)) != 0 {
		t.Fatal("a different job was cancelled or reported", code, err, *operations)
	}
}

func TestCancelRefusesAJobReplacedBetweenReads(t *testing.T) {
	g := testGate(t)
	replaced := taskWithToken(jobToken(99), "Running", Object{})
	operations := scriptSnapshots(t, g, []step{{task: running()}, {task: replaced}}, func(string) (Object, error) { return Object{}, nil })
	read := captureStreams(t)
	err := Cancel(g, 7) // no token supplied: the first read fixes it
	read()
	if err == nil || !strings.Contains(err.Error(), "no longer the job this cancellation targeted") {
		t.Fatal("replaced job was cancelled", err)
	}
	if strings.Join(*operations, ",") != "snapshot,cancel,snapshot" {
		t.Fatal("unexpected operations", *operations)
	}
}

func TestCancelLeavesAnotherJobsReceiptWhenTheRowIsMissing(t *testing.T) {
	g := testGate(t)
	if err := g.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(g.ReceiptPath(7), Object{"token": jobToken(99)}); err != nil {
		t.Fatal(err)
	}
	g.queueControl = func(string, Object) (Object, error) { return testSnapshot(Object{}), nil }
	read := captureStreams(t)
	_, err := cancelJob(g, 7, jobToken(8), nil)
	read()
	if err == nil || !strings.Contains(err.Error(), "no longer the job this cancellation targeted") {
		t.Fatal("a missing row with another job's receipt was accepted", err)
	}
	if receipt, err := g.Receipt(7); err != nil || receipt == nil {
		t.Fatal("the other job's receipt was removed", receipt, err)
	}
	if _, err := os.Stat(filepath.Join(g.Cancelled, "7.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cancellation marker was written for another job", err)
	}
}

func TestCancelRefusesAMismatchedExpectedTokenBeforeAnyMutation(t *testing.T) {
	g := testGate(t)
	operations := scriptSnapshots(t, g, []step{{task: taskWithToken(jobToken(99), "Running", Object{})}}, nil)
	read := captureStreams(t)
	_, err := cancelJob(g, 7, jobToken(8), nil)
	read()
	if err == nil || len(mutations(*operations)) != 0 {
		t.Fatal("a different job was cancelled", err, *operations)
	}
}

func TestCancelReportsAnUnconfirmedCancellationAtOnce(t *testing.T) {
	for _, test := range []struct {
		name  string
		steps []step
		want  string
	}{
		{"lost read", []step{{fail: true}}, "snapshot"},
		{"lost mutation", []step{{task: running()}}, "snapshot,cancel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := testGate(t)
			operations := scriptSnapshots(t, g, test.steps, func(string) (Object, error) { return nil, transportTimeout(t) })
			read := captureStreams(t)
			err := Cancel(g, 7)
			_, stderr := read()
			if err == nil || !strings.Contains(err.Error(), "could not be confirmed") || !strings.Contains(err.Error(), "agent-heavy logs 7") || !errors.Is(err, errQueueTransportUnavailable) {
				t.Fatal("unconfirmed cancellation was not reported", err)
			}
			if strings.Contains(err.Error(), "not delivered") || strings.Join(*operations, ",") != test.want || stderr != "" {
				t.Fatalf("unexpected claim, retry or notice: %v %v %q", err, *operations, stderr)
			}
		})
	}
}

func TestCancelStillFailsAtOnceOnNonTransportError(t *testing.T) {
	g := testGate(t)
	boom := errors.New("Queue returned an invalid task")
	calls := 0
	g.queueControl = func(string, Object) (Object, error) { calls++; return nil, boom }
	read := captureStreams(t)
	err := Cancel(g, 7)
	read()
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatal("non-transport error was retried", err, calls)
	}
}

func TestCancelNeverKillsWithoutADeliveredCancel(t *testing.T) {
	oldGrace := queueCancelGrace
	queueCancelGrace = 30 * time.Millisecond
	t.Cleanup(func() { queueCancelGrace = oldGrace })
	g := testGate(t)
	killed, delivered := false, 0
	var operations []string
	g.queueControl = func(operation string, _ Object) (Object, error) {
		operations = append(operations, operation)
		switch operation {
		case "snapshot":
			if killed {
				return testSnapshot(Object{"7": done("Killed")}), nil
			}
			return testSnapshot(Object{"7": running()}), nil
		case "cancel":
			if delivered == 0 && len(operations) == 2 { // a slow cancel whose reply is lost
				time.Sleep(60 * time.Millisecond)
				return nil, transportTimeout(t)
			}
			delivered++
			return Object{}, nil
		case "kill":
			if delivered == 0 {
				t.Error("SIGKILL without a delivered cancel", operations)
			}
			killed = true
			return Object{}, nil
		}
		t.Fatalf("unexpected operation %s", operation)
		return nil, nil
	}
	read := captureStreams(t)
	defer read()
	if err := Cancel(g, 7); err == nil {
		t.Fatal("a lost reply must be reported, not turned into a kill")
	}
	if strings.Contains(strings.Join(operations, ","), "kill") {
		t.Fatal("kill without evidence of delivery", operations)
	}
	if err := Cancel(g, 7); err != nil {
		t.Fatal(err, operations)
	}
	if !strings.Contains(strings.Join(operations, ","), "cancel,snapshot,kill") {
		t.Fatal("kill did not follow a delivered cancel and its grace period", operations)
	}
}

func TestFollowHeldInterruptEndsAtTheOutageBoundWhenOnlyMutationsFail(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	operations := scriptSnapshots(t, g, []step{{task: running()}}, func(string) (Object, error) { return nil, transportTimeout(t) })
	read := captureStreams(t)
	start := time.Now()
	code, err := followWithToken(g, 7, signals(1), "")
	_, stderr := read()
	if code != 125 || err == nil || !strings.Contains(err.Error(), "could not be confirmed") || !strings.Contains(err.Error(), "agent-heavy logs 7") {
		t.Fatal("failing mutations did not end at the bound", code, err)
	}
	if time.Since(start) > 5*time.Second || len(*operations) > 2000 {
		t.Fatal("kept retrying past the bound", time.Since(start), len(*operations))
	}
	if strings.Count(stderr, "not responding") != 1 {
		t.Fatalf("one outage must give one loss notice: %q", stderr)
	}
}

func TestFollowSecondSignalDuringOutageEndsTheWait(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	var mutated []string
	g.queueControl = func(operation string, _ Object) (Object, error) {
		if operation != "snapshot" {
			mutated = append(mutated, operation)
		}
		return nil, transportTimeout(t)
	}
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(2), "")
	_, stderr := read()
	if code != 125 || err == nil || !strings.Contains(err.Error(), "could not be confirmed") || !strings.Contains(err.Error(), "agent-heavy logs 7") {
		t.Fatal("second interrupt did not end the wait", code, err)
	}
	if len(mutated) != 0 || strings.Count(stderr, "interrupt received") != 1 {
		t.Fatalf("unexpected mutation or notice: %v %q", mutated, stderr)
	}
}

func TestFollowSaysHeldInterruptWasNotConfirmedAtBound(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(1), "")
	read()
	if code != 125 || err == nil || !strings.Contains(err.Error(), "cancellation requested by your interrupt could not be confirmed") || !strings.Contains(err.Error(), "agent-heavy logs 7") {
		t.Fatal("held interrupt was dropped silently", code, err)
	}
}

// With no cancel acknowledged, a job seen finished reports its own result.
func TestFollowReportsAFinishedJobsResultOverAHeldInterrupt(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	scriptSnapshots(t, g, []step{{task: running()}, {fail: true}, {task: done(Object{"Failed": 41})}}, nil)
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(1), jobToken(8))
	_, stderr := read()
	if code != 41 || err != nil || !strings.Contains(stderr, "finished with exit status 41; no cancellation was confirmed") {
		t.Fatal("the finished job's result was replaced", code, err, stderr)
	}
}

// After an acknowledged cancel the interrupt status is reported as ever, so a
// blip while its cleanup is confirmed cannot decide the exit status.
func TestFollowReportsTheInterruptStatusWhenCleanupFailsAfterAnAcknowledgedCancel(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": jobToken(8)})
	n := 0
	g.queueControl = func(operation string, _ Object) (Object, error) {
		if operation != "snapshot" {
			return Object{}, nil
		}
		n++
		switch {
		case n == 2: // the interrupt is taken during this lost read
			return nil, transportTimeout(t)
		case n <= 4: // the log path, the follow read, then the cancellation's first read
			return testSnapshot(Object{"7": running()}), nil
		case n == 5: // the cancel took effect and the job finished
			return testSnapshot(Object{"7": done("Success")}), nil
		}
		return nil, transportTimeout(t) // cleanup cannot be confirmed
	}
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(1), jobToken(8))
	_, stderr := read()
	if code != 128+int(syscall.SIGINT) || err != nil || !strings.Contains(stderr, "after its cancellation was acknowledged") {
		t.Fatal("a cleanup blip decided the exit status", code, err, stderr)
	}
}

// With no cancel acknowledged, the status cancelJob read must survive a lost
// cleanup probe instead of waiting out the budget and exiting 125.
func TestFollowKeepsTheResultReadByCancelWhenItsCleanupProbeIsLost(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": jobToken(8)})
	scriptSnapshots(t, g, []step{{task: running()}, {fail: true}, {task: running()}, {task: done("Success")}, {fail: true}}, nil)
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(1), jobToken(8))
	_, stderr := read()
	if code != 0 || err != nil || !strings.Contains(stderr, "no cancellation was confirmed") || !strings.Contains(stderr, "cleanup could not be confirmed") {
		t.Fatal("the job's result was lost", code, err, stderr)
	}
}

// A cancel whose reply was lost is not an acknowledged cancel, and the note must
// not claim that the interrupt did nothing.
func TestFollowDoesNotClaimTheInterruptDidNothingAfterALostCancelReply(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	lost := false
	g.queueControl = func(operation string, _ Object) (Object, error) {
		if operation == "cancel" {
			lost = true
			return nil, transportTimeout(t)
		}
		if lost {
			return testSnapshot(Object{"7": done(Object{"Failed": 41})}), nil
		}
		return testSnapshot(Object{"7": running()}), nil
	}
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(1), jobToken(8))
	_, stderr := read()
	if code != 41 || err != nil || strings.Contains(stderr, "did nothing") || strings.Contains(stderr, "being confirmed") || !strings.Contains(stderr, "no cancellation was confirmed") {
		t.Fatal(code, err, stderr)
	}
}

// A responsive daemon that still holds the receipt does not turn a finished job
// into 125: its own status is returned with a note.
func TestFollowReportsTheJobStatusWhenAnAnsweringDaemonKeepsTheReceipt(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": jobToken(8)})
	scriptSnapshots(t, g, []step{{task: done("Success")}}, nil)
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	_, stderr := read()
	if code != 0 || err != nil || !strings.Contains(stderr, "cleanup is not confirmed") || !strings.Contains(stderr, "the job's own exit status is 0") {
		t.Fatal(code, err, stderr)
	}
}

// A job that ignores SIGINT must still be killed once a cancel was acknowledged
// and the grace period passed, even when a read is lost in between.
func TestFollowKillsAJobIgnoringInterruptAcrossALostRead(t *testing.T) {
	fastOutage(t, time.Minute)
	oldGrace := queueCancelGrace
	queueCancelGrace = 30 * time.Millisecond
	t.Cleanup(func() { queueCancelGrace = oldGrace })
	g := testGate(t)
	var operations []string
	acked, lostRead, killed := false, false, false
	g.queueControl = func(operation string, _ Object) (Object, error) {
		operations = append(operations, operation)
		switch operation {
		case "cancel":
			acked = true
			return Object{}, nil
		case "kill":
			killed = true
			return Object{}, nil
		}
		if killed {
			return testSnapshot(Object{"7": done("Killed")}), nil
		}
		if acked && !lostRead { // the read after the acknowledged cancel is lost
			lostRead = true
			return nil, transportTimeout(t)
		}
		return testSnapshot(Object{"7": running()}), nil
	}
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(1), jobToken(8))
	_, stderr := read()
	if code != 128+int(syscall.SIGINT) || err != nil || !killed {
		t.Fatal("an acknowledged cancel was never followed by a kill", code, err, killed, operations)
	}
	if strings.Count(stderr, "may still be running") != 0 {
		t.Fatal(stderr)
	}
}

// A held cancellation that meets alternating lost replies and a busy handover
// is bounded by the outage budget, not restarted by every busy answer.
func TestFollowBoundsAHeldCancellationAlternatingLostRepliesAndBusyHandovers(t *testing.T) {
	fastOutage(t, 100*time.Millisecond)
	g := testGate(t)
	flip := false
	scriptSnapshots(t, g, []step{{task: running()}}, func(string) (Object, error) {
		flip = !flip
		if flip {
			return nil, transportTimeout(t)
		}
		return nil, errQueueHandoverBusy
	})
	read := captureStreams(t)
	start := time.Now()
	code, err := followWithToken(g, 7, signals(1), "")
	read()
	if code != 125 || err == nil || time.Since(start) > 5*time.Second {
		t.Fatal("the held cancellation was not bounded", code, err, time.Since(start))
	}
	if strings.Count(err.Error(), "may still be running") != 1 {
		t.Fatalf("the message repeats itself: %v", err)
	}
}

func TestCancelWritesNoLocalMarkerForAReplacedJob(t *testing.T) {
	g := testGate(t)
	scriptSnapshots(t, g, []step{{task: taskWithToken(jobToken(99), "Running", Object{})}}, nil)
	read := captureStreams(t)
	_, err := cancelJob(g, 7, jobToken(8), nil)
	read()
	if err == nil {
		t.Fatal("a replaced job was accepted")
	}
	if _, statErr := os.Stat(filepath.Join(g.Cancelled, "7.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("a cancellation marker was written for a job that was not targeted", statErr)
	}
}

func TestFollowReportsAFinishedJobFromTheStoreWhenDaemonStaysGone(t *testing.T) {
	token := jobToken(8)
	for _, test := range []struct {
		name, phase, jobToken string
		task                  Object
		outcome               any
		code                  int
	}{
		{"done", "done", token, done(Object{"Failed": 41}), 41, 41},
		{"outcome", "outcome", token, running(), 23, 23},
		{"running", "running", token, running(), nil, -1},
		{"other token", "done", jobToken(99), done("Success"), 0, -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fastOutage(t, 25*time.Millisecond)
			g := testGate(t)
			writeJobLog(t, g, 7, "ran\n")
			store, err := openQueueStore(g.State, true)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(test.task)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.db.Exec("INSERT INTO queue_jobs(id,token,task,phase,outcome) VALUES(7,?,?,?,?)", test.jobToken, string(data), test.phase, test.outcome); err != nil {
				t.Fatal(err)
			}
			store.close()
			before, _ := os.ReadFile(QueueDatabasePath(g.State))
			g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
			read := captureStreams(t)
			code, err := followWithToken(g, 7, make(chan os.Signal), token)
			stdout, stderr := read()
			if test.code < 0 {
				if code != 125 || err == nil || !strings.Contains(err.Error(), "may still be running") {
					t.Fatal("unproven job was reported finished", code, err)
				}
			} else if code != test.code || err != nil || stdout != "ran\n" || !strings.Contains(stderr, "read from the queue store") {
				t.Fatal("finished job was not reported", code, err, stdout, stderr)
			}
			if after, _ := os.ReadFile(QueueDatabasePath(g.State)); !bytes.Equal(before, after) {
				t.Fatal("the store was written")
			}
		})
	}
}

func TestFollowKeepsTheJobResultWhenOnlyCleanupLosesTheDaemon(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": jobToken(8)})
	scriptSnapshots(t, g, []step{{task: done(Object{"Failed": 41})}, {task: done(Object{"Failed": 41})}, {fail: true}}, nil)
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	_, stderr := read()
	if code != 41 || err != nil || !strings.Contains(stderr, "agent-heavy logs 7") {
		t.Fatal("a finished job's result was replaced", code, err, stderr)
	}
}

func TestWaitCleanupReportsUnconfirmedCleanupAfterOneProbe(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": jobToken(8)})
	calls := 0
	g.queueControl = func(string, Object) (Object, error) { calls++; return nil, transportTimeout(t) }
	read := captureStreams(t)
	err := WaitCleanup(g, 7)
	_, stderr := read()
	if err == nil || !errors.Is(err, errQueueTransportUnavailable) || !strings.Contains(err.Error(), "could not be confirmed") || !strings.Contains(err.Error(), "agent-heavy logs 7") {
		t.Fatal("unconfirmed cleanup was not reported", err)
	}
	if calls != 1 || stderr != "" {
		t.Fatalf("cleanup retried or announced: calls=%d %q", calls, stderr)
	}
}

// Cleanup has no retry state to restart: the first answered probe with the
// receipt still present ends it, as before.
func TestWaitCleanupEndsAtTheFirstAnsweredProbeWithTheReceiptStillPresent(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": jobToken(8)})
	calls := 0
	g.queueControl = func(string, Object) (Object, error) { calls++; return testSnapshot(Object{}), nil }
	read := captureStreams(t)
	err := WaitCleanup(g, 7)
	read()
	var unconfirmed *queueCleanupUnconfirmed
	if !errors.As(err, &unconfirmed) || !strings.Contains(err.Error(), "cleanup is not confirmed") || calls != 1 {
		t.Fatal("an answered probe did not end cleanup", err, calls)
	}
}

func TestWaitCleanupStillFailsAtOnceOnNonTransportError(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": jobToken(8)})
	calls := 0
	g.queueControl = func(string, Object) (Object, error) { calls++; return nil, errors.New("boom") }
	read := captureStreams(t)
	err := WaitCleanup(g, 7)
	_, stderr := read()
	if err == nil || !strings.Contains(err.Error(), "cleanup did not complete") || calls != 1 || stderr != "" {
		t.Fatal("non-transport error was retried or announced", err, calls, stderr)
	}
}

func TestFollowExplainsAPausedQueueOnceAndWithoutGuessingWhy(t *testing.T) {
	queued := taskWith("Queued", Object{})
	for _, test := range []struct {
		name  string
		steps []step
	}{
		{"after one outage", []step{{fail: true}, {fail: true}, {task: queued, pause: true}, {task: queued, pause: true}, {task: done("Success")}}},
		{"across flapping outages", []step{{fail: true}, {fail: true}, {task: queued, pause: true}, {task: queued, pause: true}, {fail: true}, {task: queued, pause: true}, {fail: true}, {task: queued, pause: true}, {task: done("Success")}}},
		{"after a lost check", []step{{fail: true}, {fail: true}, {task: queued, pause: true}, {fail: true}, {task: queued, pause: true}, {task: queued, pause: true}, {task: done("Success")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fastOutage(t, time.Minute)
			g := testGate(t)
			scriptSnapshots(t, g, test.steps, nil)
			read := captureStreams(t)
			code, err := followWithToken(g, 7, make(chan os.Signal), "")
			_, stderr := read()
			if err != nil || code != 0 {
				t.Fatal(code, err)
			}
			if strings.Count(stderr, "the queue is paused; job 7 will not start until the queue is resumed") != 1 {
				t.Fatalf("pause was not explained exactly once: %q", stderr)
			}
			for _, claim := range []string{"unclean", "restart", "not affected", "agent-heavy resume"} {
				if strings.Contains(stderr, claim) {
					t.Fatalf("notice claims %q: %q", claim, stderr)
				}
			}
			if strings.Count(stderr, "not responding") != 1 {
				t.Fatalf("notices were not kept concise: %q", stderr)
			}
		})
	}
}

func TestFollowSaysNothingAboutAQueueThatIsNotPaused(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	queued := taskWith("Queued", Object{})
	scriptSnapshots(t, g, []step{{fail: true}, {fail: true}, {task: queued}, {task: queued}, {task: done("Success")}}, nil)
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	_, stderr := read()
	if err != nil || code != 0 || strings.Contains(stderr, "paused") {
		t.Fatal(code, err, stderr)
	}
}

func TestQueueOutageNoticesAreConciseAndSurviveAHandoverReset(t *testing.T) {
	var out bytes.Buffer
	o := newQueueOutage(3, &out)
	o.lose(nil)
	o.reset()
	if !o.recover() || strings.Count(out.String(), "answering again") != 1 {
		t.Fatal("recovery after a reset was silent", out.String())
	}
	for i := 0; i < 20; i++ { // flapping stays quiet
		o.lose(nil)
		o.recover()
	}
	if strings.Count(out.String(), "not responding") != 1 || strings.Count(out.String(), "answering again") != 1 {
		t.Fatal("flapping produced a notice per blip", out.String())
	}
	o.noticed = time.Now().Add(-time.Hour)
	o.lose(nil)
	if strings.Count(out.String(), "not responding") != 2 {
		t.Fatal("a later outage was not announced", out.String())
	}
}

func TestQueueOutageDoesNotClaimAnUnresponsiveDaemonDuringAHandover(t *testing.T) {
	var out bytes.Buffer
	newQueueOutage(3, &out).lose(errQueueHandoverBusy)
	if strings.Contains(out.String(), "not responding") || !strings.Contains(out.String(), "handover in progress") {
		t.Fatal(out.String())
	}
}

func TestOversizedQueueResponseIsNotATransportFailure(t *testing.T) {
	decoder := json.NewDecoder(&capReader{reader: strings.NewReader(`{"result":"` + strings.Repeat("x", 100) + `"}`), left: 10})
	var value Object
	err := decoder.Decode(&value)
	if !errors.Is(err, errQueueResponseTooLarge) || errors.Is(queueTransportError(err), errQueueTransportUnavailable) {
		t.Fatal("an oversized reply would be retried as a lost connection", err)
	}
}

func TestLaneWaitRidesOutTransientQueueTransportFailure(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	failures := 2
	g.queueControl = func(operation string, _ Object) (Object, error) {
		if failures > 0 {
			failures--
			return nil, transportTimeout(t)
		}
		return Object{"id": 5, "state": "held", "lane": "build"}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := laneWait(g, 5, nil, laneOutput{stdout: &stdout, stderr: &stderr}, nil); code != 0 {
		t.Fatal("transient outage ended the lane wait", code, stderr.String())
	}
	if strings.Count(stderr.String(), "still waiting for lease 5") != 1 || strings.Count(stderr.String(), "answering again (lease 5)") != 1 || !strings.Contains(stdout.String(), "lease 5 held") {
		t.Fatalf("unexpected output: %q %q", stdout.String(), stderr.String())
	}
}

func TestLaneWaitEndsVisiblyWhenDaemonStaysUnreachable(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
	var stderr bytes.Buffer
	code := laneWait(g, 5, nil, laneOutput{stdout: io.Discard, stderr: &stderr}, nil)
	if code != laneExitRefused || !strings.Contains(stderr.String(), "lease 5 may still be queued") {
		t.Fatal("sustained outage was not reported", code, stderr.String())
	}
}

func TestLaneWaitStillFailsAtOnceOnNonTransportError(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	calls := 0
	g.queueControl = func(string, Object) (Object, error) { calls++; return nil, errors.New("boom") }
	var stdout, stderr bytes.Buffer
	if code := laneWait(g, 5, nil, laneOutput{stdout: &stdout, stderr: &stderr}, nil); code != laneExitRefused || calls != 1 || strings.Contains(stderr.String(), "not responding") {
		t.Fatal("non-transport error was retried", code, calls, stderr.String())
	}
}

func TestLaneWaitReportsWhetherTheWithdrawalWorked(t *testing.T) {
	for _, test := range []struct {
		name    string
		release error
		want    string
	}{{"released", nil, "lease 5 was withdrawn"}, {"refused", errors.New("release refused"), "could not be withdrawn"}} {
		t.Run(test.name, func(t *testing.T) {
			g := testGate(t)
			g.queueControl = func(string, Object) (Object, error) { return Object{"id": 5, "state": "queued"}, nil }
			var stdout, stderr bytes.Buffer
			interrupt := signals(1) // a channel, not a process signal: nothing can end the test binary
			code := laneWaitSignals(g, 5, nil, laneOutput{stdout: &stdout, stderr: &stderr}, func() error { return test.release }, interrupt)
			if code != 130 || !strings.Contains(stderr.String(), test.want) {
				t.Fatal(code, stderr.String())
			}
			if test.release != nil && !strings.Contains(stderr.String(), "agent-heavy lane status 5") {
				t.Fatal("no way to inspect the lease", stderr.String())
			}
		})
	}
}
