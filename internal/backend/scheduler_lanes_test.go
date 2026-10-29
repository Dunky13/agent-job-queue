package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// laneControllerFixture is an adaptive controller over a fake daemon snapshot
// that publishes the given lanes. Its sampler already has a complete interval.
func laneControllerFixture(t *testing.T, lanes Object) (*Gate, Object, *Controller) {
	t.Helper()
	g, snapshot := schedulerFixtureGate(t)
	snapshot["lanes"] = lanes
	s, b, now, _ := resourceFixture()
	s.wallClock = resourceNow
	epoch := time.Now()
	s.clock = func() float64 { return time.Since(epoch).Seconds() }
	mustHost(t, s)
	*now += 2
	b.raw["cpuTicks"] = []any{110, 100, 290, 100}
	controller, e := NewController(g, s, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(controller.Close)
	return g, snapshot, controller
}

func fixtureLane(name string, concurrency, running, held int, sharesLimit bool, memoryMiB int) Object {
	lane := laneDefinition(queueLane{Name: name, Label: name, Plugin: "devices", Concurrency: concurrency, SharesLimit: sharesLimit, MemoryMiB: memoryMiB})
	lane["running"], lane["held"], lane["waiting"] = running, held, 0
	return lane
}

func schedulerCalls(t *testing.T, g *Gate) string {
	t.Helper()
	calls, e := os.ReadFile(g.Config + ".calls")
	if e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	return string(calls)
}

// A plugin-lane command the daemon already started counts in its lane's
// running total. When its pending grant is dropped before the worker gets the
// slot, the controller must grant it again rather than wait for lane capacity
// it already holds.
func TestControllerRegrantsStartedLaneCommandAfterGrantDrop(t *testing.T) {
	g, snapshot, controller := laneControllerFixture(t, Object{"qa": fixtureLane("qa", 1, 1, 0, true, 0)})
	value := strings.Repeat("1", 32)
	if e := WritePrivate(filepath.Join(g.State, "workloads", value+".json"), fixtureIdentity()); e != nil {
		t.Fatal(e)
	}
	obj(snapshot["tasks"])["1"] = Object{"command": "agent-heavy execute " + value, "lane": "qa", "status": Object{"Running": Object{"start": time.Now().UTC().Format(time.RFC3339Nano)}}, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "priority": 0}
	writeSchedulerSnapshot(t, g, snapshot)
	state, e := controller.Tick()
	if e != nil {
		t.Fatal(e)
	}
	decision := obj(obj(obj(state["jobs"])[value])["decision"])
	if !boolean(decision["allowed"]) {
		t.Fatalf("started lane command was not re-admitted: %v", decision)
	}
	admissions, e := ReadPrivate(filepath.Join(g.State, "resource-admissions.json"))
	if e != nil {
		t.Fatal(e)
	}
	if grant := obj(obj(admissions["grants"])[value]); grant == nil || mustInteger(grant["task_id"]) != 1 {
		t.Fatalf("no grant for the started lane command: %v", admissions)
	}
	if calls := schedulerCalls(t, g); strings.Contains(calls, "start") {
		t.Fatalf("a running command was started again: %s", calls)
	}
}
