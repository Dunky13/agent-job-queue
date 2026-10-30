package remote

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/lifeline"
)

// The menu bar app holds the write end of the pipe whose read end is serve's
// descriptor 3. When the app dies, serve must take its orderly SIGTERM
// shutdown: stop its owned children, retire its generation record and exit 0.
// This runs the real controller entry point against temporary private state.
func TestRemoteServeEndsWhenItsLifelineCloses(t *testing.T) {
	if testing.Short() {
		t.Skip("real private embedded gateway acceptance")
	}
	c := fixtureController(t)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	t.Setenv("AGENT_REMOTE_TEST_HELPER", "1")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	controllerPath := filepath.Join(c.p.state, "controller")
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then printf '%s' '{\"jobs\":[],\"queue\":{\"limit\":1}}'; else printf '%s' '{\"ok\":true}'; fi\n"
	if err := os.WriteFile(controllerPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_REMOTE_TEST_CONTROLLER", controllerPath)
	cli := func(input string, args ...string) {
		t.Helper()
		command, err := selfArguments(args...)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Env = os.Environ()
		cmd.Stdin = strings.NewReader(input)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("remote %s: %v %s", args[0], err, output)
		}
	}
	input, _ := json.Marshal(map[string]any{"password": "2468", "port": port})
	cli(string(input), "configure")
	cli("", "enable")

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer write.Close()
	command, _ := selfArguments("serve")
	process := exec.Command(command[0], command[1:]...)
	process.Env = append(os.Environ(), lifeline.Env+"=3")
	log, err := os.OpenFile(filepath.Join(c.p.state, "lifeline-test.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	process.Stdout, process.Stderr = log, log
	process.ExtraFiles = []*os.File{read}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	read.Close()
	exited := make(chan error, 1)
	var ended atomic.Bool
	go func() {
		err := process.Wait()
		ended.Store(true)
		exited <- err
	}()
	t.Cleanup(func() {
		_ = c.disable()
		if !ended.Load() {
			_ = process.Process.Kill()
			<-exited
		}
	})
	deadline := time.Now().Add(18 * time.Second)
	for !c.isRunning() {
		select {
		case err := <-exited:
			body, _ := os.ReadFile(filepath.Join(c.p.state, "lifeline-test.log"))
			t.Fatalf("serve exited before readiness: %v %s", err, body)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("embedded gateway did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case err := <-exited:
		t.Fatalf("serve ended while the app still held its lifeline: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	write.Close()
	select {
	case err := <-exited:
		if err != nil {
			body, _ := os.ReadFile(filepath.Join(c.p.state, "lifeline-test.log"))
			t.Fatalf("closing the lifeline did not end serve through its orderly shutdown: %v %s", err, body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve outlived the app that supervised it")
	}
	if c.isRunning() {
		t.Fatal("serve left an active generation behind")
	}
	if paths, err := c.childRecords(); err != nil || len(paths) != 0 {
		t.Fatal("serve left owned children behind", paths, err)
	}
}

func TestRemoteControlEnvironmentNeverForwardsTheLifeline(t *testing.T) {
	t.Setenv(lifeline.Env, "3")
	for _, testing := range []bool{false, true} {
		for _, entry := range controlEnvironment(testing) {
			if strings.HasPrefix(entry, lifeline.Env+"=") {
				t.Fatal("a child environment carries the lifeline", testing)
			}
		}
	}
}
