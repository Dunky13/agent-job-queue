package backend

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if strings.Count(stderr, "not responding") != 2 || strings.Count(stderr, "responding again") != 2 {
		t.Fatalf("expected a loss and recovery notice per outage: %q", stderr)
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
	if strings.Count(stderr.String(), "not responding") != 1 || strings.Count(stderr.String(), "responding again") != 1 || !strings.Contains(stdout.String(), "lease 5 held") {
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
