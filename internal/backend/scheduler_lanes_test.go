package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// laneControllerFixture is an adaptive controller over a fake daemon snapshot
// that publishes the given lanes. A primed sampler already has a complete
// interval; an unprimed one has taken no host sample yet.
func laneControllerFixture(t *testing.T, lanes Object, primed bool) (*Gate, Object, *Controller) {
	t.Helper()
	g, snapshot := schedulerFixtureGate(t)
	snapshot["lanes"] = lanes
	s, b, now, _ := resourceFixture()
	s.wallClock = resourceNow
	epoch := time.Now()
	s.clock = func() float64 { return time.Since(epoch).Seconds() }
	if primed {
		mustHost(t, s)
		*now += 2
		b.raw["cpuTicks"] = []any{110, 100, 290, 100}
	}
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
	g, snapshot, controller := laneControllerFixture(t, Object{"qa": fixtureLane("qa", 1, 1, 0, true, 0)}, true)
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

func fixtureLeaseTask(lane string) Object {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return Object{"kind": "lease", "lane": lane, "label": "session", "priority": 0, "created_at": now, "status": Object{"Queued": Object{}}, "lease": Object{"state": "queued", "queuedAt": now, "ttlSeconds": 0, "meta": Object{}}}
}

// leaseLanesSnapshot queues a zero-reservation lease (1), a reserved lease (2)
// and an ordinary command (3) in an adaptive queue.
func leaseLanesSnapshot(t *testing.T, g *Gate, snapshot Object) string {
	t.Helper()
	value := strings.Repeat("3", 32)
	if e := WritePrivate(filepath.Join(g.State, "workloads", value+".json"), fixtureIdentity()); e != nil {
		t.Fatal(e)
	}
	tasks := obj(snapshot["tasks"])
	tasks["1"] = fixtureLeaseTask("mutex")
	tasks["2"] = fixtureLeaseTask("heavy")
	tasks["3"] = Object{"command": "agent-heavy execute " + value, "status": Object{"Queued": Object{}}, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "priority": 0}
	writeSchedulerSnapshot(t, g, snapshot)
	return value
}

func leaseLanes() Object {
	return Object{"mutex": fixtureLane("mutex", 1, 0, 0, false, 0), "heavy": fixtureLane("heavy", 1, 0, 0, true, 2048)}
}

func requireOnlyLeaseStarted(t *testing.T, g *Gate, leases Object, reserved string) {
	t.Helper()
	if decision := obj(obj(leases["1"])["decision"]); !boolean(decision["allowed"]) {
		t.Fatalf("zero-reservation lease waited for resource samples: %v", leases)
	}
	if decision := obj(obj(leases["2"])["decision"]); boolean(decision["allowed"]) || decision["reason"] != reserved {
		t.Fatalf("reserved lease was admitted without a fresh sample: %v", leases)
	}
	calls := schedulerCalls(t, g)
	if !strings.Contains(calls, "start 1\n") || strings.Contains(calls, "start 2\n") || strings.Contains(calls, "start 3\n") {
		t.Fatalf("only the zero-reservation lease may start: %q", calls)
	}
}

// Before the controller has a complete interval, a zero-reservation lease is
// admitted and started; reserved leases and commands keep waiting.
func TestControllerStartsUnreservedLeaseBeforeHostInterval(t *testing.T) {
	g, snapshot, controller := laneControllerFixture(t, leaseLanes(), false)
	value := leaseLanesSnapshot(t, g, snapshot)
	state, e := controller.Tick()
	if e != nil {
		t.Fatal(e)
	}
	if boolean(state["available"]) {
		t.Fatal("the first host sample reported a complete interval")
	}
	requireOnlyLeaseStarted(t, g, obj(state["leases"]), "Collecting a complete resource interval")
	if decision := obj(obj(obj(state["jobs"])[value])["decision"]); boolean(decision["allowed"]) {
		t.Fatalf("a command was admitted without a complete interval: %v", decision)
	}
}

// A failed host sample still admits a zero-reservation lease. The tick still
// fails, so the published status keeps admissions held, and it reports the
// lease decisions without granting any command.
func TestControllerAdmitsUnreservedLeaseWhenHostSampleFails(t *testing.T) {
	g, snapshot := schedulerFixtureGate(t)
	snapshot["lanes"] = leaseLanes()
	value := leaseLanesSnapshot(t, g, snapshot)
	s := NewResourceSampler(".", &failingResourceBackend{})
	if e := serveScheduler(g, s, true); e == nil {
		t.Fatal("a failed host sample returned success")
	}
	status, e := ReadPrivate(filepath.Join(g.State, "resource-status.json"))
	if e != nil {
		t.Fatal(e)
	}
	if boolean(status["available"]) || status["host"] != nil {
		t.Fatalf("failed sample published usable telemetry: %v", status)
	}
	requireOnlyLeaseStarted(t, g, obj(status["leases"]), "Waiting for a fresh host resource sample")
	admissions, e := ReadPrivate(filepath.Join(g.State, "resource-admissions.json"))
	if e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if grant := obj(admissions["grants"])[value]; grant != nil {
		t.Fatalf("a command was granted without host telemetry: %v", fmt.Sprint(grant))
	}
}

// Job and lease admission share one headroom computation: for the same host,
// active work and held reservations, a demand that fits or misses for a job
// fits or misses the same way as a lease reservation.
func TestJobAndLeaseAdmissionShareHeadroom(t *testing.T) {
	active := []Object{resourceActive(Object{"memoryBytes": 3 * GiB, "cpuCores": 2.0, "durationSeconds": 10.0, "samples": 12, "confidence": "high", "source": "variant"}, GiB, 1, true)}
	held := leaseReservations{memoryBytes: float64(GiB), cpuCores: 0.5}
	for _, entry := range []struct {
		name                 string
		available, busy      float64
		memoryBytes, cpu     float64
		jobReason, leaseNote string
	}{
		{"fits", float64(8 * GiB), 1, float64(GiB), 1, "fits the current headroom", "fits the current headroom"},
		{"memory", float64(5 * GiB), 1, float64(GiB), 1, "peak memory headroom", "memory headroom for the lane reservation"},
		{"cpu", float64(8 * GiB), 4.5, float64(GiB), 1, "predicted CPU headroom", "CPU headroom for the lane reservation"},
		{"pressure", float64(8 * GiB), 1, float64(GiB), 1, "memory pressure", "memory pressure"},
		{"budget", float64(8 * GiB), 1, float64(15 * GiB), 1, "exceeds this machine's budget", "exceeds this machine's budget"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			host := fixtureResourceHost()
			host["availableMemoryBytes"] = int64(entry.available)
			host["cpuBusyCores"] = entry.busy
			if entry.name == "pressure" {
				host["memoryPressure"] = "warning"
			}
			prediction := fixturePrediction()
			prediction["memoryBytes"], prediction["cpuCores"] = int64(entry.memoryBytes), entry.cpu
			jobAllowed, jobReason := resourceDecision(host, prediction, active, 0, 4, held)
			leaseAllowed, leaseReason := leaseDecision(host, entry.memoryBytes, entry.cpu, active, held)
			if jobAllowed != leaseAllowed || jobAllowed != (entry.name == "fits") {
				t.Fatalf("job %v %q, lease %v %q", jobAllowed, jobReason, leaseAllowed, leaseReason)
			}
			if !strings.Contains(jobReason, entry.jobReason) || !strings.Contains(leaseReason, entry.leaseNote) {
				t.Fatalf("job %q, lease %q", jobReason, leaseReason)
			}
		})
	}
}
