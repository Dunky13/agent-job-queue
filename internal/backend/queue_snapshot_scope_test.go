package backend

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/wire"
)

// scopedDaemon holds finished jobs whose end times run against their IDs: job 1
// waited longest and ended last. Job 4 is still queued.
func scopedDaemon(t *testing.T) *queueDaemon {
	t.Helper()
	d, _ := laneDaemon(t, 2, nil)
	d.g.queueControl = func(op string, payload Object) (Object, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.control(op, payload)
	}
	for id, end := range map[int]string{1: "2026-10-01T10:00:03Z", 2: "2026-10-01T10:00:01Z", 3: "2026-10-01T10:00:02Z"} {
		scopedRow(t, d, id, "done", Object{"Done": Object{"start": "2026-10-01T10:00:00Z", "end": end, "result": "Success"}})
	}
	scopedRow(t, d, 4, "queued", Object{"Queued": Object{"enqueued_at": "2026-10-01T10:00:00Z"}})
	return d
}

func scopedRow(t *testing.T, d *queueDaemon, id int, phase string, status Object) {
	t.Helper()
	scopedTask(t, d, id, phase, Object{"status": status})
}

func scopedTask(t *testing.T, d *queueDaemon, id int, phase string, fields Object) {
	t.Helper()
	value := fmt.Sprintf("%032d", id)
	fields["label"], fields["token"], fields["created_at"], fields["priority"] = "scoped", value, "2026-10-01T10:00:00Z", 0
	task, err := queueJSON(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.store.db.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(?,?,?,?)", id, value, task, phase); err != nil {
		t.Fatal(err)
	}
}

func snapshotIDs(t *testing.T, snapshot Object) []int {
	t.Helper()
	ids := []int{}
	for key := range obj(snapshot["tasks"]) {
		id, err := strconv.Atoi(key)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestSnapshotScopeSelectsRows(t *testing.T) {
	d := scopedDaemon(t)
	for _, entry := range []struct {
		name    string
		payload Object
		want    []int
	}{
		{"unscoped reads all history", nil, []int{1, 2, 3, 4}},
		{"active leaves out finished jobs", Object{"recentDone": 0}, []int{4}},
		{"recent follows end time, not ID", Object{"recentDone": 2}, []int{1, 3, 4}},
		{"one job outside the recent window", Object{"id": 2}, []int{2}},
		{"unknown job", Object{"id": 99}, []int{}},
		{"negative job", Object{"id": -1}, []int{}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			snapshot, err := d.g.QueueControl("snapshot", entry.payload)
			if err != nil {
				t.Fatal(err)
			}
			if got := snapshotIDs(t, snapshot); !reflect.DeepEqual(got, entry.want) {
				t.Fatalf("tasks %v, want %v", got, entry.want)
			}
			// Every scope keeps what callers read besides tasks.
			if defaultGroup(snapshot)["status"] == nil || obj(snapshot["lanes"])[defaultLaneName] == nil || snapshot["plugins"] == nil {
				t.Fatalf("scoped snapshot lost settings, lanes or plugins: %v", snapshot)
			}
		})
	}
}

func TestSnapshotScopeRefusesInvalidValues(t *testing.T) {
	d := scopedDaemon(t)
	for _, payload := range []Object{{"id": "one"}, {"id": 1.5}, {"recentDone": "many"}, {"recentDone": -1}} {
		if snapshot, err := d.g.QueueControl("snapshot", payload); err == nil {
			t.Fatalf("%v answered %v", payload, snapshot)
		}
	}
}

func TestQueueMutationReplyLeavesOutHistory(t *testing.T) {
	d := scopedDaemon(t)
	snapshot, err := d.g.QueueControl("hold", Object{"id": 4})
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshotIDs(t, snapshot); !reflect.DeepEqual(got, []int{4}) {
		t.Fatalf("reply listed %v", got)
	}
	if snapshot, err = d.g.QueueControl("plugins-reload", Object{}); err != nil {
		t.Fatal(err)
	}
	if got := snapshotIDs(t, snapshot); !reflect.DeepEqual(got, []int{4}) {
		t.Fatalf("reload reply listed %v", got)
	}
}

// Every waiting run client polls Task, and a full snapshot there cost a whole core.
func TestPollingReadsOnlyWhatItNeeds(t *testing.T) {
	d := scopedDaemon(t)
	control := d.g.queueControl
	listed := []int{}
	d.g.queueControl = func(op string, payload Object) (Object, error) {
		reply, err := control(op, payload)
		listed = snapshotIDs(t, reply)
		return reply, err
	}
	if _, err := d.g.Task(2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listed, []int{2}) {
		t.Fatalf("Task read %v", listed)
	}
	if _, err := LeaseCapacity(d.g); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listed, []int{4}) {
		t.Fatalf("LeaseCapacity read %v", listed)
	}
}

func TestGateTaskFindsAnyRetainedJob(t *testing.T) {
	d := scopedDaemon(t)
	task, err := d.g.Task(2)
	if err != nil {
		t.Fatal(err)
	}
	if name, _, err := TaskStatus(task); err != nil || name != "Done" {
		t.Fatalf("finished job read as %q: %v", name, err)
	}
	if task, err = d.g.Task(99); err != nil || task != nil {
		t.Fatalf("unknown job read as %v: %v", task, err)
	}
}

// A daemon that predates scopes ignores them and answers with all history.
func TestGateReadsAnUnscopedDaemon(t *testing.T) {
	d := scopedDaemon(t)
	d.g.queueControl = func(op string, _ Object) (Object, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.control(op, nil)
	}
	task, err := d.g.Task(4)
	if err != nil {
		t.Fatal(err)
	}
	if name, _, err := TaskStatus(task); err != nil || name != "Queued" {
		t.Fatalf("job read as %q: %v", name, err)
	}
	if task, err = d.g.Task(99); err != nil || task != nil {
		t.Fatalf("unknown job read as %v: %v", task, err)
	}
}

func TestStatusListsJobsThatFinishedLastEvenWithOldIDs(t *testing.T) {
	for _, scoped := range []bool{true, false} {
		t.Run(fmt.Sprintf("scoped=%v", scoped), func(t *testing.T) {
			statusListsJobsThatFinishedLast(t, scoped)
		})
	}
}

func statusListsJobsThatFinishedLast(t *testing.T, scoped bool) {
	t.Helper()
	d, _ := laneDaemon(t, 2, nil)
	d.g.queueControl = func(op string, payload Object) (Object, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if !scoped {
			payload = nil
		}
		return d.control(op, payload)
	}
	// Job 1 waited through every later job and ended after them all.
	scopedRow(t, d, 1, "done", Object{"Done": Object{"start": "2026-10-01T10:00:00Z", "end": "2026-10-02T10:00:00Z", "result": "Success"}})
	total := recentDoneJobs + 10
	for id := 2; id <= total; id++ {
		end := fmt.Sprintf("2026-10-01T10:%02d:00Z", id)
		scopedRow(t, d, id, "done", Object{"Done": Object{"start": "2026-10-01T10:00:00Z", "end": end, "result": "Success"}})
	}
	scopedRow(t, d, total+1, "queued", Object{"Queued": Object{"enqueued_at": "2026-10-01T10:00:00Z"}})
	status, err := StatusData(d.g)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int{}
	for _, job := range status.Jobs {
		ids = append(ids, job.ID)
	}
	want := []int{total + 1, 1}
	for id := total; len(want) < recentDoneJobs+1; id-- {
		want = append(want, id)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("status listed %v, want %v", ids, want)
	}
}

func TestControllerReadsHistoryOnlyForARunOutsideTheWindow(t *testing.T) {
	g, _ := schedulerFixtureGate(t)
	sampler, _, _, _ := resourceFixture()
	controller, err := NewController(g, sampler, nil)
	if err != nil {
		t.Fatal(err)
	}
	recent, old := strings.Repeat("a", 32), strings.Repeat("b", 32)
	done := Object{"Done": Object{"start": "2026-10-01T10:00:00Z", "end": "2026-10-01T10:00:01Z", "result": "Success"}}
	settings := Object{"default": Object{"parallel_tasks": 2, "status": "Running"}}
	full := 0
	g.queueControl = func(operation string, payload Object) (Object, error) {
		tasks := Object{"1": Object{"token": recent, "status": done}}
		if payload["recentDone"] == nil {
			full++
			tasks["2"] = Object{"token": old, "status": done}
		}
		return Object{"tasks": tasks, "groups": settings}, nil
	}
	controller.runs[recent] = Object{}
	snapshot, err := controller.queueView()
	if err != nil {
		t.Fatal(err)
	}
	if full != 0 || len(obj(snapshot["tasks"])) != 1 {
		t.Fatalf("a run inside the window read history: %d reads, %v", full, snapshot)
	}
	controller.runs[old] = Object{}
	if snapshot, err = controller.queueView(); err != nil {
		t.Fatal(err)
	}
	if full != 1 || obj(snapshot["tasks"])["2"] == nil {
		t.Fatalf("a run outside the window was not found: %d reads, %v", full, snapshot)
	}
	// A finalized run owes nothing, so leaving the window costs no history read.
	controller.finalized[old] = true
	if _, err = controller.queueView(); err != nil {
		t.Fatal(err)
	}
	if full != 1 {
		t.Fatalf("a finalized run read history: %d reads", full)
	}
}

// Before the scheduler window left out leases, 30 finished leases hid a command
// from it: the run lost its final record, or every tick read all history.
func TestSchedulerWindowIgnoresFinishedLeases(t *testing.T) {
	d, _ := laneDaemon(t, 2, nil)
	d.g.queueControl = func(op string, payload Object) (Object, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.control(op, payload)
	}
	finished := func(minute int) Object {
		return Object{"Done": Object{"start": "2026-10-01T10:00:00Z", "end": fmt.Sprintf("2026-10-01T10:%02d:00Z", minute), "result": "Success"}}
	}
	scopedRow(t, d, 1, "done", finished(0))
	for id := 2; id <= recentDoneJobs+1; id++ {
		scopedTask(t, d, id, "done", Object{"kind": "lease", "status": finished(id)})
	}
	sampler, _, _, _ := resourceFixture()
	controller, err := NewController(d.g, sampler, nil)
	if err != nil {
		t.Fatal(err)
	}
	control, full := d.g.queueControl, 0
	d.g.queueControl = func(op string, payload Object) (Object, error) {
		if payload == nil {
			full++
		}
		return control(op, payload)
	}
	controller.runs[fmt.Sprintf("%032d", 1)] = Object{}
	snapshot, err := controller.queueView()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshotIDs(t, snapshot); !reflect.DeepEqual(got, []int{1}) || full != 0 {
		t.Fatalf("window listed %v after %d full reads", got, full)
	}
	// Status counts leases, as it always has.
	if snapshot, err = d.g.RecentSnapshot(recentDoneJobs); err != nil {
		t.Fatal(err)
	}
	if got := snapshotIDs(t, snapshot); slices.Contains(got, 1) || len(got) != recentDoneJobs {
		t.Fatalf("status window listed %v", got)
	}
}

// A finished row with no end time disagrees with its phase. However old it is, the
// readers that recent scopes serve still see it instead of hiding the disagreement.
func TestRecentScopeKeepsAFinishedRowWithNoEndTime(t *testing.T) {
	for _, entry := range []struct {
		name   string
		fields Object
		fails  bool
	}{
		{"running status", Object{"status": Object{"Running": Object{"start": "2026-10-01T10:00:00Z"}}}, false},
		{"no status", Object{}, true},
	} {
		t.Run(entry.name, func(t *testing.T) {
			d, _ := laneDaemon(t, 2, nil)
			d.g.queueControl = func(op string, payload Object) (Object, error) {
				d.mu.Lock()
				defer d.mu.Unlock()
				return d.control(op, payload)
			}
			scopedTask(t, d, 1, "done", entry.fields)
			for id := 2; id <= recentDoneJobs+2; id++ {
				scopedRow(t, d, id, "done", Object{"Done": Object{"start": "2026-10-01T10:00:00Z", "end": fmt.Sprintf("2026-10-01T10:%02d:00Z", id), "result": "Success"}})
			}
			status, err := StatusData(d.g)
			if entry.fails {
				if err == nil {
					t.Fatal("status hid an unreadable row")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(status.Jobs, func(job wire.Job) bool { return job.ID == 1 }) {
				t.Fatalf("status hid job 1: %v", status.Jobs)
			}
		})
	}
}

// One tick owes a run outside the window its final record. No later tick reads
// history for it.
func TestTickRecordsARunOutsideTheWindowOnce(t *testing.T) {
	g, snapshot := schedulerFixtureGate(t)
	sampled, waiting := strings.Repeat("1", 32), strings.Repeat("2", 32)
	for _, value := range []string{sampled, waiting} {
		if err := WritePrivate(filepath.Join(g.State, "workloads", value+".json"), fixtureIdentity()); err != nil {
			t.Fatal(err)
		}
	}
	finished := time.Now().UTC().Add(-time.Second)
	done := func(at time.Time) Object {
		end := at.Format(time.RFC3339Nano)
		return Object{"Done": Object{"start": end, "end": end, "result": "Success"}}
	}
	created := finished.Format(time.RFC3339Nano)
	tasks := obj(snapshot["tasks"])
	tasks["1"] = Object{"command": "agent-heavy execute " + sampled, "status": done(finished), "created_at": created, "priority": 0}
	tasks["2"] = Object{"command": "agent-heavy execute " + waiting, "status": Object{"Stashed": Object{}}, "created_at": created, "priority": 0}
	// These commands finished after the sampled run and fill the window.
	for id := 3; id < 3+recentDoneJobs; id++ {
		tasks[strconv.Itoa(id)] = Object{"command": "agent-heavy execute " + fmt.Sprintf("%032d", id), "status": done(finished.Add(time.Duration(id) * time.Millisecond)), "created_at": created, "priority": 0}
	}
	writeSchedulerSnapshot(t, g, snapshot)
	control, full := g.queueControl, 0
	g.queueControl = func(operation string, payload Object) (Object, error) {
		reply, err := control(operation, payload)
		if operation == "snapshot" && err == nil {
			if payload["recentDone"] == nil {
				full++
			} else {
				delete(obj(reply["tasks"]), "1")
			}
		}
		return reply, err
	}
	s, b, now, _ := resourceFixture()
	s.wallClock = resourceNow
	epoch := time.Now()
	s.clock = func() float64 { return time.Since(epoch).Seconds() }
	mustHost(t, s)
	*now += 2
	b.raw["cpuTicks"] = []any{110, 100, 290, 100}
	controller, err := NewController(g, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	at := float64(finished.UnixNano()) / 1e9
	controller.runs[sampled] = Object{"started": at - 10, "sampledAt": at, "peakMemoryBytes": 32 * GiB, "peakCpuCores": 2.0, "cpuSeconds": 20.0, "durationSeconds": 10.0, "samples": 3, "complete": true, "boot": controller.boot, "machine": "opaque-capacity"}
	state, err := controller.Tick()
	if err != nil {
		t.Fatal(err)
	}
	// The waiting job shares the workload, so its prediction shows the run was recorded.
	if memory := metric(obj(obj(obj(state["jobs"])[waiting])["prediction"])["memoryBytes"]); full != 1 || memory < 32*GiB {
		t.Fatalf("first tick: %d full reads, predicted %v bytes", full, memory)
	}
	if controller.runs[sampled] != nil {
		t.Fatalf("first tick kept the run: %v", controller.runs[sampled])
	}
	*now += 2
	b.raw["cpuTicks"] = []any{120, 100, 480, 100}
	if _, err = controller.Tick(); err != nil {
		t.Fatal(err)
	}
	if full != 1 {
		t.Fatalf("second tick read history: %d full reads", full)
	}
}
