package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSchedulerShutdownWhileInstallerHoldsSchedulingLock(t *testing.T) {
	const childState = "AGENT_QUEUE_TEST_SCHEDULER_SHUTDOWN_STATE"
	state := os.Getenv(childState)
	if state != "" {
		g := &Gate{State: state, Slot: filepath.Join(state, "slot"), Config: filepath.Join(state, "queue.json"),
			Jobs: filepath.Join(state, "jobs"), Receipts: filepath.Join(state, "receipts"),
			Metadata: filepath.Join(state, "metadata"), Cancelled: filepath.Join(state, "cancelled")}
		g.queueControl = func(string, Object) (Object, error) {
			if err := os.WriteFile(filepath.Join(state, "snapshot-ready"), []byte("ready"), 0600); err != nil {
				return nil, err
			}
			return Object{"tasks": Object{}, "groups": Object{"default": Object{"parallel_tasks": 1, "status": "Running"}}}, nil
		}
		sampler := NewResourceSampler(state, &admissionFixtureBackend{pressure: "normal", processCount: map[int]uint64{}})
		if err := serveScheduler(g, sampler, false); err != nil {
			t.Fatal(err)
		}
		return
	}
	physical, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state = physical
	g := &Gate{State: state}
	if err := WritePrivate(filepath.Join(state, "scheduling.json"), Object{"mode": "fixed", "generation": "shutdown-fixture"}); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockPrivate(g, "scheduling.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestSchedulerShutdownWhileInstallerHoldsSchedulingLock$")
	command.Env = append(os.Environ(), childState+"="+state)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitError error
	go func() { waitError = command.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = command.Process.Kill()
			<-done
		}
	})
	fixtureUntil(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(state, "snapshot-ready"))
		return err == nil
	})
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if waitError != nil {
			t.Fatalf("scheduler did not shut down gracefully: %v: %s", waitError, output.String())
		}
		if busy, err := installInspectLock(state, "resource-controller.lock"); err != nil || busy {
			t.Fatal("scheduler retained its lifetime lock", busy, err)
		}
		if busy, err := installInspectLock(state, "scheduling.lock"); err != nil || !busy {
			t.Fatal("scheduler shutdown changed installer lock ownership", busy, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler ignored termination while waiting for installer-owned scheduling lock")
	}
}

func TestQueueControlCancellationClosesStalledPrivateConnection(t *testing.T) {
	state, err := os.MkdirTemp("/tmp", "aq-rpc-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	if err := os.Mkdir(filepath.Join(state, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &Gate{State: state, controlContext: ctx}
	listener, err := net.Listen("unix", queueSocket(g))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(queueSocket(g), 0600); err != nil {
		t.Fatal(err)
	}
	received, serverDone := make(chan struct{}), make(chan struct{})
	var serverError error
	go func() {
		defer close(serverDone)
		connection, err := listener.Accept()
		if err != nil {
			serverError = err
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		decoder := json.NewDecoder(connection)
		var request queueRequest
		if err := decoder.Decode(&request); err != nil {
			serverError = err
			return
		}
		close(received)
		var next queueRequest
		serverError = decoder.Decode(&next)
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case <-serverDone:
		case <-time.After(6 * time.Second):
			t.Error("private RPC fixture did not stop")
		}
	})
	done := make(chan error, 1)
	go func() { _, err := g.QueueControl("snapshot", nil); done <- err }()
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("private RPC did not reach its controlled stalled response")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled RPC returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled private RPC waited for its normal deadline")
	}
	select {
	case <-serverDone:
		if !errors.Is(serverError, io.EOF) {
			t.Fatal("cancellation did not close its owned socket", serverError)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled RPC retained its private connection")
	}
	if _, err := g.QueueControl("snapshot", nil); !errors.Is(err, context.Canceled) {
		t.Fatal("already cancelled control reached I/O", err)
	}
}
