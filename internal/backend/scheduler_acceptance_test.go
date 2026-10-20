package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The queue, helper, lease and libproc identity census remain real. Only demand
// counters are controlled, so admission tests do not depend on host contention.
type admissionFixtureBackend struct {
	mu           sync.Mutex
	pressure     string
	hostCount    uint64
	processCount map[int]uint64
}

func (b *admissionFixtureBackend) HostCounters() (Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hostCount++
	n := b.hostCount
	return Object{"totalMemoryBytes": 16 * GiB, "availableMemoryBytes": 12 * GiB, "cpuCores": 12, "cpuTicks": []any{n * 100, 0, n * 900, 0}, "memoryPressure": b.pressure, "swapUsedBytes": 0, "swapInBytes": 0, "swapOutBytes": 0, "diskFreeBytes": 100 * GiB, "machine": "acceptance-machine"}, nil
}
func (b *admissionFixtureBackend) Members(pgid int) ([]Object, error) {
	return resourceProcessListing(pgid)
}
func (b *admissionFixtureBackend) Process(pid, pgid int) (Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.processCount[pid]++
	return Object{"start": uint64(pid), "memoryBytes": uint64(16 * 1024 * 1024), "cpuNanoseconds": b.processCount[pid] * 1e6, "readBytes": uint64(0), "writeBytes": uint64(0)}, nil
}
func (b *admissionFixtureBackend) setPressure(value string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pressure = value
}

type acceptanceController struct {
	backend    *admissionFixtureBackend
	controller *Controller
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	failure    error
}

func startAcceptanceController(t *testing.T, f *daemonFixture, pressure string) *acceptanceController {
	t.Helper()
	backend := &admissionFixtureBackend{pressure: pressure, processCount: map[int]uint64{}}
	controller, e := NewController(f.gate, NewResourceSampler(f.root, backend), nil)
	if e != nil {
		t.Fatal(e)
	}
	run := &acceptanceController{backend: backend, controller: controller, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(run.done)
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-run.stop:
				return
			case <-ticker.C:
				if _, e := controller.Tick(); e != nil {
					run.mu.Lock()
					run.failure = e
					run.mu.Unlock()
					_ = controller.CensorRuns()
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		run.close()
		run.mu.Lock()
		defer run.mu.Unlock()
		if run.failure != nil {
			t.Errorf("private deterministic resource controller failed: %v", run.failure)
		}
	})
	fixtureUntil(t, 8*time.Second, func() bool { state, e := SchedulingView(f.gate); return e == nil && boolean(state["available"]) })
	return run
}
func (c *acceptanceController) close() {
	c.once.Do(func() { close(c.stop); <-c.done; c.controller.Close() })
}
func acceptanceHoldingCommand(start, release string) []string {
	return []string{"/bin/sh", "-c", `printf started > "$1"; i=0; while [ ! -f "$2" ]; do i=$((i+1)); [ "$i" -lt 600 ] || exit 99; sleep .05; done`, "queue-fixture", start, release}
}
func acceptanceFileExists(path string) bool { _, e := os.Stat(path); return e == nil }
func acceptanceRelease(t *testing.T, path string) {
	t.Helper()
	if e := os.WriteFile(path, []byte("release"), 0600); e != nil {
		t.Fatal(e)
	}
}
func acceptanceEnvironment(values []string) map[string]string {
	result := map[string]string{}
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}
func seedAcceptanceDemand(t *testing.T, f *daemonFixture, commands ...[]string) string {
	t.Helper()
	environment := acceptanceEnvironment(f.environment)
	environment["AGENT_HEAVY_PROFILE"] = "adaptive-acceptance"
	f.environment = envList(environment)
	store, e := NewProfileStore(f.gate.State, false)
	if e != nil {
		t.Fatal(e)
	}
	family := ""
	for index, command := range commands {
		identity, e := IdentifyWorkload(command, f.root, environment)
		if e != nil {
			t.Fatal(e)
		}
		family = text(identity["family"])
		for sample := 0; sample < 5; sample++ {
			e = store.Record(fmt.Sprintf("acceptance-%d-%d", index, sample), identity, "acceptance-machine", Object{"memoryBytes": 256 * 1024 * 1024, "cpuSeconds": .1, "cpuCores": .2, "durationSeconds": 1}, true, profileZero())
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	return family
}
func acceptanceTaskState(t *testing.T, f *daemonFixture, id int) string {
	t.Helper()
	task, e := f.gate.Task(id)
	if e != nil {
		t.Fatal(e)
	}
	state, _, e := TaskStatus(task)
	if e != nil {
		t.Fatal(e)
	}
	return state
}
func acceptanceWaitStarted(t *testing.T, path string) {
	t.Helper()
	fixtureUntil(t, 10*time.Second, func() bool { return acceptanceFileExists(path) })
}
func TestRealDaemonAdaptiveKnownOverlapLoweredCapAndLearningCorrection(t *testing.T) {
	f := daemonAcceptance(t)
	starts, releases := []string{}, []string{}
	commands := [][]string{}
	for i := 0; i < 3; i++ {
		start, release := filepath.Join(f.root, fmt.Sprintf("adaptive-%d.started", i)), filepath.Join(f.root, fmt.Sprintf("adaptive-%d.release", i))
		starts = append(starts, start)
		releases = append(releases, release)
		commands = append(commands, acceptanceHoldingCommand(start, release))
	}
	family := seedAcceptanceDemand(t, f, commands...)
	startAcceptanceController(t, f, "normal")
	if _, e := f.gate.QueueControl("limit", Object{"limit": 2}); e != nil {
		t.Fatal(e)
	}
	if e := SetSchedulingMode(f.gate, "adaptive"); e != nil {
		t.Fatal(e)
	}
	first := f.launch(commands[0]...)
	acceptanceWaitStarted(t, starts[0])
	second := f.launch(commands[1]...)
	acceptanceWaitStarted(t, starts[1])
	if _, e := f.gate.QueueControl("limit", Object{"limit": 1}); e != nil {
		t.Fatal(e)
	}
	third := f.launch(commands[2]...)
	f.waitJobs(3)
	ids := f.taskIDs()
	thirdID := 0
	for _, id := range ids {
		thirdID = max(thirdID, id)
	}
	fixtureUntil(t, 8*time.Second, func() bool { return acceptanceTaskState(t, f, thirdID) == "Stashed" })
	acceptanceRelease(t, releases[0])
	first.wait(t, 0)
	if acceptanceFileExists(starts[2]) {
		t.Fatal("Lowered cap overlapped a waiting adaptive job")
	}
	detail, e := LearningInspect(f.gate, family, 0, 100, "")
	if e != nil {
		t.Fatal(e)
	}
	revision, _ := integer(detail["revision"])
	saved, e := LearningCorrect(f.gate, "edit", family, Object{"revision": revision, "label": "Operator corrected tests", "memoryFloorBytes": 20 * GiB, "cpuFloorCores": nil})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.gate.QueueControl("limit", Object{"limit": 2}); e != nil {
		t.Fatal(e)
	}
	fixtureUntil(t, 8*time.Second, func() bool {
		state, e := SchedulingView(f.gate)
		if e != nil {
			return false
		}
		applied, _ := integer(state["learningRevision"])
		savedRevision, _ := integer(saved["revision"])
		if applied != savedRevision {
			return false
		}
		for _, raw := range obj(state["jobs"]) {
			decision := obj(obj(raw)["decision"])
			if strings.Contains(text(decision["reason"]), "exceeds") {
				return true
			}
		}
		return false
	})
	if acceptanceFileExists(starts[2]) {
		t.Fatal("Corrected operator floor did not hold waiting work")
	}
	select {
	case <-second.done:
		t.Fatal("Learning correction stopped active work")
	default:
	}
	savedRevision, _ := integer(saved["revision"])
	if _, e = LearningCorrect(f.gate, "edit", family, Object{"revision": savedRevision, "label": "Operator corrected tests", "memoryFloorBytes": nil, "cpuFloorCores": nil}); e != nil {
		t.Fatal(e)
	}
	acceptanceWaitStarted(t, starts[2])
	acceptanceRelease(t, releases[1])
	acceptanceRelease(t, releases[2])
	second.wait(t, 0)
	third.wait(t, 0)
}
func TestRealDaemonResourceControllerLossRequiresExplicitManualStart(t *testing.T) {
	f := daemonAcceptance(t)
	controller := startAcceptanceController(t, f, "normal")
	if e := SetSchedulingMode(f.gate, "adaptive"); e != nil {
		t.Fatal(e)
	}
	controller.close()
	started := filepath.Join(f.root, "manual.started")
	client := f.launch("/bin/sh", "-c", `printf started > "$1"`, "fixture", started)
	f.waitJobs(1)
	id := f.taskIDs()[0]
	fixtureUntil(t, 5*time.Second, func() bool { return acceptanceTaskState(t, f, id) == "Stashed" })
	if acceptanceFileExists(started) {
		t.Fatal("Controller loss authorized a new admission")
	}
	if e := StartJob(f.gate, id); e != nil {
		t.Fatal(e)
	}
	acceptanceWaitStarted(t, started)
	client.wait(t, 0)
}
func TestRealDaemonResourcePressureRecoveryAndShadowMode(t *testing.T) {
	f := daemonAcceptance(t)
	controller := startAcceptanceController(t, f, "warning")
	if e := SetSchedulingMode(f.gate, "shadow"); e != nil {
		t.Fatal(e)
	}
	shadow := f.launch("/bin/sh", "-c", "printf SHADOW")
	if got := shadow.wait(t, 0); got != "SHADOW" {
		t.Fatalf("Shadow affected execution: %q", got)
	}
	if e := SetSchedulingMode(f.gate, "adaptive"); e != nil {
		t.Fatal(e)
	}
	started := filepath.Join(f.root, "pressure.started")
	client := f.launch("/bin/sh", "-c", `printf started > "$1"`, "fixture", started)
	f.waitJobs(2)
	id := 0
	for _, taskID := range f.taskIDs() {
		id = max(id, taskID)
	}
	fixtureUntil(t, 8*time.Second, func() bool { return acceptanceTaskState(t, f, id) == "Stashed" })
	if acceptanceFileExists(started) {
		t.Fatal("Memory pressure authorized start")
	}
	controller.backend.setPressure("normal")
	fixtureUntil(t, 20*time.Second, func() bool { return acceptanceFileExists(started) })
	client.wait(t, 0)
	snapshot, e := f.gate.Snapshot()
	if e != nil {
		t.Fatal(e)
	}
	_, limit, e := schedulingGroup(snapshot)
	if e != nil || limit != 1 {
		t.Fatal("Pressure controller changed operator parallel limit")
	}
}
