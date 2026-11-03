package backend

import (
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// orphanedWorker is a worker record whose only recorded member, the group
// leader, has exited while a descendant it never recorded still holds the group.
func orphanedWorker(t *testing.T) (Object, int) {
	t.Helper()
	process := exec.Command("/bin/sh", "-c", `sleep 60 & echo ready; read line; exit 0`)
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := process.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if _, err = io.ReadFull(stdout, make([]byte, len("ready\n"))); err != nil {
		t.Fatal(err)
	}
	boot, err := BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	table, err := identityCensus(processIdentityWording)
	if err != nil {
		t.Fatal(err)
	}
	leader := Object{"pid": pgid, "birth": table[pgid].Birth}
	_ = stdin.Close()
	if err = process.Wait(); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("the descendant should hold the group: %v", err)
	}
	return Object{"pgid": pgid, "pid": pgid, "boot_uuid": boot, "members": []Object{leader}}, pgid
}

// A worker publishes its outcome before it exits. A signal built from a row read
// before that finds the group alive with no recorded member; once the row has
// settled there is nothing to signal, and while it has not the refusal stands.
// Neither case signals the group.
func TestSignalFromAStaleRowAfterTheWorkerExited(t *testing.T) {
	d, s, _, _ := corruptedDaemon(t)
	worker, pgid := orphanedWorker(t)
	stale := runningWithWorker(t, s, 0, worker)

	err := d.signal(stale, syscall.SIGTERM)
	if !errors.Is(err, errGroupOwnershipUnverified) {
		t.Fatalf("a running row with no recorded member left must refuse, got %v", err)
	}
	for _, want := range []string{"group " + strconv.Itoa(pgid), "recorded members [" + strconv.Itoa(pgid) + "@", "current members ["} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must say what it compared (%q): %v", want, err)
		}
	}
	if err = syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("the refusal signalled the group: %v", err)
	}

	if err = s.recordOutcome(stale, 130); err != nil {
		t.Fatal(err)
	}
	if err = d.signal(stale, syscall.SIGTERM); err != nil {
		t.Fatalf("a settled row has nothing to signal: %v", err)
	}
	if err = syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("an unverified group was signalled: %v", err)
	}
}
