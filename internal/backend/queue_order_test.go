package backend

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRealDaemonReorderingPreservesTiesAndRejectsOverflow(t *testing.T) {
	f := daemonAcceptance(t)
	first := f.launch("/bin/sh", "-c", "touch ready; while [ ! -f release ]; do sleep .05; done")
	fixtureUntil(t, 8*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "ready"))
		return err == nil
	})
	orderPath := filepath.Join(f.root, "order")
	queued := 1
	launch := func(label string) (*queueClient, int) {
		t.Helper()
		client := f.launch("/bin/sh", "-c", `printf '%s\n' "$1" >> "$2"`, "fixture", label, orderPath)
		queued++
		f.waitJobs(queued)
		id := -1
		for _, candidate := range f.taskIDs() {
			id = max(id, candidate)
		}
		return client, id
	}
	a, aID := launch("A")
	b, bID := launch("B")
	c, cID := launch("C")
	d, _ := launch("D")
	if priority, err := Reorder(f.gate, cID, "next", 0); err != nil || priority != 1 {
		t.Fatal("next did not move C ahead", priority, err)
	}
	if priority, err := Reorder(f.gate, bID, "last", 0); err != nil || priority != -1 {
		t.Fatal("last did not move B behind", priority, err)
	}
	if _, err := Reorder(f.gate, aID, "priority", math.MaxInt32); err != nil {
		t.Fatal(err)
	}
	if _, err := Reorder(f.gate, cID, "next", 0); err == nil {
		t.Fatal("next overflow changed the queue")
	}
	if _, err := Reorder(f.gate, aID, "priority", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Reorder(f.gate, bID, "priority", math.MinInt32); err != nil {
		t.Fatal(err)
	}
	if _, err := Reorder(f.gate, cID, "last", 0); err == nil {
		t.Fatal("last overflow changed the queue")
	}
	if _, err := Reorder(f.gate, bID, "priority", -1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	first.wait(t, 0)
	for _, client := range []*queueClient{c, a, d, b} {
		client.wait(t, 0)
	}
	order, err := os.ReadFile(orderPath)
	if err != nil || string(order) != "C\nA\nD\nB\n" {
		t.Fatalf("priority, tie order or overflow rejection changed execution: %q %v", order, err)
	}
}
