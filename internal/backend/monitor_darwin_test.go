//go:build darwin && cgo

package backend

import (
	"os"
	"syscall"
	"testing"
)

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
