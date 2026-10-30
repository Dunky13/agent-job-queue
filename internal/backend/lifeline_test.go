package backend

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/lifeline"
)

// A helper the menu bar app started must end when the app dies. The app holds
// the write end of a pipe whose read end is the helper's descriptor 3, so the
// helper sees EOF and takes its ordinary SIGTERM shutdown path. These tests run
// the real daemon and scheduler entry points against temporary private state.
func lifelineHelper(t *testing.T, command string, ready func(g *Gate) bool) (*exec.Cmd, *os.File, chan error, *bytes.Buffer) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("queue runtime uses macOS ownership APIs")
	}
	root, err := os.MkdirTemp("/tmp", "queue-lifeline-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	g := testGate(t)
	g.State = filepath.Join(root, "state")
	g.Config = filepath.Join(root, "config.yml")
	g.Slot = filepath.Join(root, "slot")
	g.Jobs = filepath.Join(g.State, "jobs")
	g.Receipts = filepath.Join(g.State, "receipts")
	g.Metadata = filepath.Join(g.State, "metadata")
	g.Cancelled = filepath.Join(g.State, "cancelled")
	if err = g.Prepare(); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{}
	for key, value := range envMap() {
		if !strings.HasPrefix(key, "AGENT_HEAVY_") && !strings.HasPrefix(key, "PUEUE_") {
			environment[key] = value
		}
	}
	environment["AGENT_HEAVY_CONFIG"] = g.Config
	environment["AGENT_HEAVY_STATE"] = g.State
	environment["AGENT_HEAVY_LEGACY_SLOT"] = g.Slot
	environment["PATH"] = environment["PATH"] + ":" + ToolPath
	environment[lifeline.Env] = "3"
	g.ControlEnv = environment
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { write.Close() })
	var output bytes.Buffer
	process := exec.Command(executable, command)
	process.Env = envList(environment)
	process.Stdout, process.Stderr = &output, &output
	process.ExtraFiles = []*os.File{read}
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	read.Close()
	done := make(chan error, 1)
	var exited atomic.Bool
	go func() {
		err := process.Wait()
		exited.Store(true)
		done <- err
	}()
	t.Cleanup(func() {
		if !exited.Load() {
			_ = process.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("private %s output: %.5000s", command, output.String())
		}
	})
	fixtureUntil(t, 10*time.Second, func() bool { return ready(g) })
	return process, write, done, &output
}

func requireLifelineExit(t *testing.T, write *os.File, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the helper ended while the app still held its lifeline: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	begin := time.Now()
	write.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("closing the lifeline did not end the helper through its orderly SIGTERM shutdown: %v", err)
		}
		if elapsed := time.Since(begin); elapsed > 8*time.Second {
			t.Fatalf("shutdown took %v", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the helper outlived the app that supervised it")
	}
}

func TestRealDaemonEndsWhenItsLifelineCloses(t *testing.T) {
	_, write, done, _ := lifelineHelper(t, "daemon", func(g *Gate) bool { _, err := g.Snapshot(); return err == nil })
	requireLifelineExit(t, write, done)
}

func TestRealSchedulerEndsWhenItsLifelineCloses(t *testing.T) {
	var state string
	_, write, done, _ := lifelineHelper(t, "scheduler", func(g *Gate) bool {
		state = g.State
		busy, err := installInspectLock(g.State, "resource-controller.lock")
		return err == nil && busy
	})
	requireLifelineExit(t, write, done)
	if busy, err := installInspectLock(state, "resource-controller.lock"); err != nil || busy {
		t.Fatal("the scheduler kept its lifetime lock after exiting", err)
	}
}

func TestLifelineIsNeverForwardedToControlEnvironments(t *testing.T) {
	t.Setenv(lifeline.Env, "3")
	t.Setenv("AGENT_HEAVY_STATE", filepath.Join(t.TempDir(), "state"))
	g, err := NewGate()
	if err != nil {
		t.Fatal(err)
	}
	if _, forwarded := g.ControlEnv[lifeline.Env]; forwarded {
		t.Fatal("the gate forwards the lifeline to the helpers it starts")
	}
	for _, entry := range envList(g.ControlEnv) {
		if strings.HasPrefix(entry, lifeline.Env+"=") {
			t.Fatal("a control environment carries the lifeline", entry)
		}
	}
}
