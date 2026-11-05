//go:build darwin && cgo

package backend

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestResourceDarwinCPUTimeIsNanoseconds(t *testing.T) {
	backend, e := newPlatformResourceBackend(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	read := func() (reported, kernel float64) {
		t.Helper()
		process, e := backend.Process(os.Getpid(), syscall.Getpgrp())
		if e != nil {
			t.Fatal(e)
		}
		var usage syscall.Rusage
		if e = syscall.Getrusage(syscall.RUSAGE_SELF, &usage); e != nil {
			t.Fatal(e)
		}
		return metric(process["cpuNanoseconds"]) / 1e9, float64(usage.Utime.Nano()+usage.Stime.Nano()) / 1e9
	}
	reportedBefore, kernelBefore := read()
	// Burn CPU until the kernel's own accounting has moved measurably.
	spin := uint64(1)
	for _, kernel := read(); kernel-kernelBefore < .25; _, kernel = read() {
		for i := range 2000000 {
			spin = spin*31 + uint64(i)
		}
	}
	reportedAfter, kernelAfter := read()
	if ratio := (reportedAfter - reportedBefore) / (kernelAfter - kernelBefore); ratio < .8 || ratio > 1.25 {
		t.Fatalf("CPU time is in the wrong unit: reported %.3fs, kernel %.3fs (%d)", reportedAfter-reportedBefore, kernelAfter-kernelBefore, spin)
	}
}
func TestResourceDarwinProcessExitClassification(t *testing.T) {
	backend, e := newPlatformResourceBackend(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	start := func(arguments ...string) *exec.Cmd {
		t.Helper()
		command := exec.Command(arguments[0], arguments[1:]...)
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if e := command.Start(); e != nil {
			t.Fatal(e)
		}
		return command
	}
	live := start("/bin/sleep", "30")
	defer func() { live.Process.Kill(); live.Wait() }()
	if _, e = backend.Process(live.Process.Pid, live.Process.Pid); e != nil {
		t.Fatalf("Live process was unreadable: %v", e)
	}
	if _, e = backend.Process(live.Process.Pid, live.Process.Pid+1); e == nil || errors.Is(e, errResourceProcessExited) {
		t.Fatalf("A process in another group was classified as exited: %v", e)
	}
	if os.Geteuid() != 0 {
		if _, e = backend.Process(1, 1); e == nil || errors.Is(e, errResourceProcessExited) {
			t.Fatalf("An unreadable live process was classified as exited: %v", e)
		}
	}
	unreaped := start("/usr/bin/true")
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, e = backend.Process(unreaped.Process.Pid, unreaped.Process.Pid)
		if errors.Is(e, errResourceProcessExited) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Unreaped process was not classified as exited: %v", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	unreaped.Wait()
	if _, e = backend.Process(unreaped.Process.Pid, unreaped.Process.Pid); !errors.Is(e, errResourceProcessExited) {
		t.Fatalf("Reaped process was not classified as exited: %v", e)
	}
}
func TestResourceDarwinNativeMetrics(t *testing.T) {
	backend, e := newPlatformResourceBackend(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	host, e := backend.HostCounters()
	if e != nil {
		t.Fatal(e)
	}
	if metric(host["totalMemoryBytes"]) <= 0 || metric(host["availableMemoryBytes"]) > metric(host["totalMemoryBytes"]) || metric(host["cpuCores"]) <= 0 || metric(host["diskFreeBytes"]) <= 0 || len(text(host["machine"])) != 24 || len(list(host["cpuTicks"])) != 4 {
		t.Fatalf("Invalid native capacity/counters: %v", host)
	}
	process, e := backend.Process(os.Getpid(), syscall.Getpgrp())
	if e != nil {
		t.Fatal(e)
	}
	if metric(process["memoryBytes"]) <= 0 || metric(process["start"]) <= 0 {
		t.Fatalf("Invalid libproc footprint/birth counters: %v", process)
	}
	members, e := backend.Members(syscall.Getpgrp())
	if e != nil {
		t.Fatal(e)
	}
	seen := false
	for _, m := range members {
		pid, _ := integer(m["pid"])
		seen = seen || pid == os.Getpid()
	}
	if !seen {
		t.Fatal("Native resource census omitted observer")
	}
}
