package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
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

func taskWith(status string, details Object) Object {
	return Object{"token": fmt.Sprintf("%032x", 8), "status": Object{status: details}}
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

func TestFollowRidesOutTransientQueueTransportFailure(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	writeJobLog(t, g, 7, "job output\n")
	failures := 3
	snapshots := 0
	g.queueControl = func(operation string, _ Object) (Object, error) {
		snapshots++
		if failures > 0 {
			failures--
			return nil, transportTimeout(t)
		}
		return testSnapshot(Object{"7": taskWith("Done", Object{"result": Object{"Failed": 41}})}), nil
	}
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	stdout, stderr := read()
	if err != nil || code != 41 {
		t.Fatal("transient transport failure ended the follow", code, err)
	}
	if stdout != "job output\n" {
		t.Fatalf("job output not delivered: %q", stdout)
	}
	if strings.Count(stderr, "not responding") != 1 || strings.Count(stderr, "responding again") != 1 {
		t.Fatalf("expected one loss and one recovery notice: %q", stderr)
	}
	if snapshots < 4 {
		t.Fatal("follow did not retry", snapshots)
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
	if strings.Count(stderr, "not responding") != 1 || strings.Contains(stderr, "responding again") {
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

func TestWaitCleanupRidesOutTransientQueueTransportFailure(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": fmt.Sprintf("%032x", 8)})
	failures := 2
	g.queueControl = func(string, Object) (Object, error) {
		if failures > 0 {
			failures--
			return nil, transportTimeout(t)
		}
		// The recovered daemon completes the cleanup it had not finished.
		_ = os.Remove(g.ReceiptPath(7))
		return testSnapshot(Object{}), nil
	}
	read := captureStreams(t)
	err := WaitCleanup(g, 7)
	_, stderr := read()
	if err != nil {
		t.Fatal("transient outage failed a finished job's cleanup", err)
	}
	if strings.Count(stderr, "not responding") != 1 || strings.Count(stderr, "responding again") != 1 {
		t.Fatalf("expected one loss and one recovery notice: %q", stderr)
	}
}

func TestWaitCleanupEndsVisiblyWhenDaemonStaysUnreachable(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": fmt.Sprintf("%032x", 8)})
	g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
	read := captureStreams(t)
	err := WaitCleanup(g, 7)
	read()
	if err == nil || !strings.Contains(err.Error(), "Job 7 finished but cleanup did not complete") || !errors.Is(err, errQueueTransportUnavailable) {
		t.Fatal("sustained outage was not reported", err)
	}
}

func TestCancelRidesOutTransientQueueTransportFailure(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	var operations []string
	failCancel := true
	g.queueControl = func(operation string, _ Object) (Object, error) {
		operations = append(operations, operation)
		switch operation {
		case "snapshot":
			if len(operations) == 1 { // the first look is lost to a timeout
				return nil, transportTimeout(t)
			}
			if failCancel {
				return testSnapshot(Object{"7": taskWith("Running", Object{})}), nil
			}
			return testSnapshot(Object{"7": taskWith("Done", Object{"result": "Killed"})}), nil
		case "cancel":
			if failCancel {
				failCancel = false
				return nil, transportTimeout(t)
			}
			return Object{}, nil
		}
		t.Fatalf("unexpected operation %s", operation)
		return nil, nil
	}
	read := captureStreams(t)
	err := Cancel(g, 7)
	_, stderr := read()
	if err != nil {
		t.Fatal("transient outage failed the cancellation", err, operations)
	}
	// The lost read and the lost mutation are one continuous outage.
	if strings.Count(stderr, "not responding") != 1 || strings.Count(stderr, "responding again") != 1 {
		t.Fatalf("expected one loss and one recovery notice: %q", stderr)
	}
	if strings.Join(operations, ",") != "snapshot,snapshot,cancel,snapshot" {
		t.Fatal("cancel was not retried after the lost response", operations)
	}
}

func TestCancelEndsVisiblyWhenDaemonStaysUnreachable(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
	read := captureStreams(t)
	err := Cancel(g, 7)
	read()
	if err == nil || !strings.Contains(err.Error(), "may still be running") || !strings.Contains(err.Error(), "agent-heavy logs 7") || !errors.Is(err, errQueueTransportUnavailable) {
		t.Fatal("sustained outage was not reported", err)
	}
}

func TestCancelStillFailsAtOnceOnNonTransportError(t *testing.T) {
	fastOutage(t, time.Minute)
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
	if strings.Count(stderr.String(), "still waiting for lease 5") != 1 || strings.Count(stderr.String(), "responding again (lease 5)") != 1 || !strings.Contains(stdout.String(), "lease 5 held") {
		t.Fatalf("unexpected output: %q %q", stdout.String(), stderr.String())
	}
}

func TestLaneWaitEndsVisiblyWhenDaemonStaysUnreachable(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
	var stdout, stderr bytes.Buffer
	code := laneWait(g, 5, nil, laneOutput{stdout: io.Discard, stderr: &stderr}, nil)
	if code != laneExitRefused || !strings.Contains(stderr.String(), "lease 5 may still be queued") {
		t.Fatal("sustained outage was not reported", code, stdout.String(), stderr.String())
	}
}

func signals(n int) chan os.Signal {
	c := make(chan os.Signal, 8)
	for i := 0; i < n; i++ {
		c <- syscall.SIGINT
	}
	return c
}

func TestCancelEndsAtBoundWhenOnlyMutationsFail(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	calls := 0
	g.queueControl = func(operation string, _ Object) (Object, error) {
		calls++
		if operation == "snapshot" {
			return testSnapshot(Object{"7": taskWith("Running", Object{})}), nil
		}
		return nil, transportTimeout(t)
	}
	read := captureStreams(t)
	start := time.Now()
	err := Cancel(g, 7)
	_, stderr := read()
	if err == nil || !strings.Contains(err.Error(), "was not delivered") || !strings.Contains(err.Error(), "agent-heavy logs 7") || !errors.Is(err, errQueueTransportUnavailable) {
		t.Fatal("failing mutations did not end at the bound", err)
	}
	if time.Since(start) > 5*time.Second || calls > 500 {
		t.Fatal("cancellation kept retrying past its bound", time.Since(start), calls)
	}
	if strings.Count(stderr, "not responding") != 1 || strings.Contains(stderr, "responding again") {
		t.Fatalf("one outage must give one loss notice: %q", stderr)
	}
}

func TestCancelNeverKillsWithoutADeliveredCancel(t *testing.T) {
	fastOutage(t, time.Minute)
	oldGrace := queueCancelGrace
	queueCancelGrace = 30 * time.Millisecond
	t.Cleanup(func() { queueCancelGrace = oldGrace })
	g := testGate(t)
	var operations []string
	delivered := 0
	killed := false
	g.queueControl = func(operation string, _ Object) (Object, error) {
		operations = append(operations, operation)
		switch operation {
		case "snapshot":
			if killed {
				return testSnapshot(Object{"7": taskWith("Done", Object{"result": "Killed"})}), nil
			}
			return testSnapshot(Object{"7": taskWith("Running", Object{})}), nil
		case "cancel":
			if len(operations) == 2 { // a slow cancel outlives the whole grace period, then its reply is lost
				time.Sleep(60 * time.Millisecond)
				return nil, transportTimeout(t)
			}
			delivered++
			return Object{}, nil
		case "kill":
			if delivered == 0 {
				t.Error("SIGKILL without evidence that the cancel was delivered", operations)
			}
			killed = true
			return Object{}, nil
		}
		t.Fatalf("unexpected operation %s", operation)
		return nil, nil
	}
	read := captureStreams(t)
	err := Cancel(g, 7)
	read()
	if err != nil {
		t.Fatal(err, operations)
	}
	first := strings.Index(strings.Join(operations, ","), "kill")
	if first < 0 || !strings.Contains(strings.Join(operations, ",")[:first], "cancel,snapshot,cancel") {
		t.Fatal("kill did not follow a delivered cancel and its grace period", operations)
	}
}

func TestFollowSecondSignalDuringOutageEndsTheWait(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	var mutations []string
	g.queueControl = func(operation string, _ Object) (Object, error) {
		if operation != "snapshot" {
			mutations = append(mutations, operation)
		}
		return nil, transportTimeout(t)
	}
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(2), "")
	_, stderr := read()
	if code != 125 || err == nil || !strings.Contains(err.Error(), "cancellation was not delivered") || !strings.Contains(err.Error(), "agent-heavy logs 7") {
		t.Fatal("second interrupt did not end the wait", code, err)
	}
	if len(mutations) != 0 || strings.Count(stderr, "interrupt received") != 1 {
		t.Fatalf("unexpected mutation or notice: %v %q", mutations, stderr)
	}
}

func TestFollowSaysHeldInterruptWasNotDeliveredAtBound(t *testing.T) {
	fastOutage(t, 25*time.Millisecond)
	g := testGate(t)
	g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
	read := captureStreams(t)
	code, err := followWithToken(g, 7, signals(1), "")
	read()
	if code != 125 || err == nil || !strings.Contains(err.Error(), "interrupt was not delivered") || !strings.Contains(err.Error(), "agent-heavy logs 7") {
		t.Fatal("held interrupt was dropped silently", code, err)
	}
}

func TestCancelOutageWaitIsInterruptible(t *testing.T) {
	fastOutage(t, time.Minute)
	queueOutageBackoff = func(int) time.Duration { return time.Hour }
	g := testGate(t)
	g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
	read := captureStreams(t)
	done := make(chan error, 1)
	go func() { done <- cancelWithSignals(g, 7, signals(1)) }()
	select {
	case err := <-done:
		read()
		if err == nil || !strings.Contains(err.Error(), "was not delivered") || !strings.Contains(err.Error(), "agent-heavy logs 7") {
			t.Fatal("interrupt did not explain the undelivered cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt did not end the backoff wait")
	}
}

func TestFollowExplainsAPausedQueueAfterAnOutage(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	calls := 0
	g.queueControl = func(string, Object) (Object, error) {
		calls++
		switch {
		case calls <= 2:
			return nil, transportTimeout(t)
		case calls <= 4: // the poll and the pause check
			snapshot := testSnapshot(Object{"7": taskWith("Queued", Object{})})
			snapshot["groups"] = Object{"default": Object{"status": "Paused", "parallel_tasks": 1}}
			return snapshot, nil
		}
		return testSnapshot(Object{"7": taskWith("Done", Object{"result": "Success"})}), nil
	}
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	_, stderr := read()
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if strings.Count(stderr, "agent-heavy resume") != 1 || strings.Contains(stderr, "not affected") {
		t.Fatalf("pause was not explained once: %q", stderr)
	}
}

func TestFollowRefusesAnotherJobUnderTheSameID(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	other := taskWith("Done", Object{"result": "Success"})
	other["token"] = fmt.Sprintf("%032x", 99)
	g.queueControl = func(string, Object) (Object, error) { return testSnapshot(Object{"7": other}), nil }
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), fmt.Sprintf("%032x", 8))
	read()
	if code != 125 || err == nil || !strings.Contains(err.Error(), "no longer the command this client submitted") {
		t.Fatal("another job's result was used", code, err)
	}
}

func TestFollowReportsAFinishedJobFromTheStoreWhenDaemonStaysGone(t *testing.T) {
	token := fmt.Sprintf("%032x", 8)
	for _, test := range []struct {
		name, phase, jobToken string
		task                  Object
		outcome               any
		code                  int
	}{
		{"done", "done", token, taskWith("Done", Object{"result": Object{"Failed": 41}}), 41, 41},
		{"outcome", "outcome", token, taskWith("Running", Object{}), 23, 23},
		{"running", "running", token, taskWith("Running", Object{}), nil, -1},
		{"other token", "done", fmt.Sprintf("%032x", 99), taskWith("Done", Object{"result": "Success"}), 0, -1},
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
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": fmt.Sprintf("%032x", 8)})
	calls := 0
	g.queueControl = func(string, Object) (Object, error) {
		calls++
		if calls <= 2 {
			return testSnapshot(Object{"7": taskWith("Done", Object{"result": Object{"Failed": 41}})}), nil
		}
		return nil, transportTimeout(t)
	}
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), "")
	_, stderr := read()
	if code != 41 || err != nil || !strings.Contains(stderr, "agent-heavy logs 7") {
		t.Fatal("a finished job's result was replaced", code, err, stderr)
	}
}

func TestQueueOutageNoticesSurviveAHandoverReset(t *testing.T) {
	var out bytes.Buffer
	o := newQueueOutage(3, &out)
	o.lose()
	o.reset()
	if !o.recover() || strings.Count(out.String(), "responding again") != 1 {
		t.Fatal("recovery after a reset was silent", out.String())
	}
	o.lose()
	if strings.Count(out.String(), "not responding") != 2 {
		t.Fatal("the next outage was not announced", out.String())
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

func TestWaitCleanupStillFailsAtOnceOnNonTransportError(t *testing.T) {
	fastOutage(t, time.Minute)
	g := testGate(t)
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": fmt.Sprintf("%032x", 8)})
	calls := 0
	g.queueControl = func(string, Object) (Object, error) { calls++; return nil, errors.New("boom") }
	read := captureStreams(t)
	err := WaitCleanup(g, 7)
	_, stderr := read()
	if err == nil || !strings.Contains(err.Error(), "cleanup did not complete") || calls != 1 || stderr != "" {
		t.Fatal("non-transport error was retried or announced", err, calls, stderr)
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
			// The guard keeps an early signal from ending the test binary; the sender
			// repeats until laneWait returns so a late registration still gets one.
			guard := make(chan os.Signal, 64)
			signal.Notify(guard, syscall.SIGINT)
			defer signal.Stop(guard)
			stop := make(chan struct{})
			go func() {
				for {
					select {
					case <-stop:
						return
					case <-time.After(50 * time.Millisecond):
						_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
					}
				}
			}()
			defer close(stop)
			code := laneWait(g, 5, nil, laneOutput{stdout: &stdout, stderr: &stderr}, func() error { return test.release })
			if code != 130 || !strings.Contains(stderr.String(), test.want) {
				t.Fatal(code, stderr.String())
			}
			if test.release != nil && !strings.Contains(stderr.String(), "agent-heavy lane status 5") {
				t.Fatal("no way to inspect the lease", stderr.String())
			}
		})
	}
}
