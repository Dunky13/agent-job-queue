package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureResourceHost() Object {
	v := fixtureHostCounters()
	delete(v, "cpuTicks")
	delete(v, "swapInBytes")
	delete(v, "swapOutBytes")
	v["sampledAt"] = resourceNow()
	v["cpuBusyCores"] = 1.0
	v["swapInBytesPerSecond"] = 0.0
	v["swapOutBytesPerSecond"] = 0.0
	v["complete"] = true
	return v
}
func fixturePrediction() Object {
	return Object{"memoryBytes": GiB, "cpuCores": 1.0, "durationSeconds": 10.0, "samples": 12, "confidence": "high", "source": "variant"}
}
func resourceActive(prediction Object, memory int, cpu float64, complete bool) Object {
	return Object{"prediction": prediction, "observed": Object{"memoryBytes": memory, "cpuCores": cpu, "complete": complete}}
}
func TestResourceDecisionCountsHostUseOnceAndReservesGrowth(t *testing.T) {
	host := fixtureResourceHost()
	host["availableMemoryBytes"] = 4 * GiB
	host["cpuBusyCores"] = 4.0
	prediction := fixturePrediction()
	active := []Object{resourceActive(prediction, GiB, 1, true)}
	if allowed, reason := ResourceDecision(host, prediction, active, 2); !allowed {
		t.Fatal("Current footprints double charged:", reason)
	}
	active[0] = resourceActive(Object{"memoryBytes": 4 * GiB, "cpuCores": 1.0, "durationSeconds": 10.0, "samples": 12, "confidence": "high", "source": "variant"}, GiB, 1, true)
	if allowed, reason := ResourceDecision(host, prediction, active, 2); allowed || !strings.Contains(reason, "memory headroom") {
		t.Fatal("Future memory growth not reserved:", reason)
	}
	host["availableMemoryBytes"] = 12 * GiB
	active[0] = resourceActive(Object{"memoryBytes": GiB, "cpuCores": 5.0, "durationSeconds": 10.0, "samples": 12, "confidence": "high", "source": "variant"}, GiB, 1, true)
	if allowed, reason := ResourceDecision(host, prediction, active, 2); allowed || !strings.Contains(reason, "CPU headroom") {
		t.Fatal("Future CPU growth not reserved:", reason)
	}
	active[0] = resourceActive(prediction, 3*GiB, 6, true)
	host["cpuBusyCores"] = 6.0
	if allowed, _ := ResourceDecision(host, prediction, active, 2); allowed {
		t.Fatal("Measured overprediction invented free CPU")
	}
}
func TestResourceDecisionSafetyGates(t *testing.T) {
	for _, entry := range []struct {
		name, key string
		value     any
		reason    string
	}{{"warning", "memoryPressure", "warning", "memory pressure"}, {"critical", "memoryPressure", "critical", "memory pressure"}, {"swapping", "swapOutBytesPerSecond", float64(1024*1024 + 1), "active swapping"}, {"disk", "diskFreeBytes", GiB - 1, "free disk"}, {"background", "cpuBusyCores", 6.0, "background CPU"}, {"incomplete", "complete", false, "complete resource interval"}} {
		t.Run(entry.name, func(t *testing.T) {
			host := fixtureResourceHost()
			host[entry.key] = entry.value
			if allowed, reason := ResourceDecision(host, fixturePrediction(), nil, 2); allowed || !strings.Contains(reason, entry.reason) {
				t.Fatalf("Safety gate missing: %v %s", allowed, reason)
			}
		})
	}
	host := fixtureResourceHost()
	host["swapUsedBytes"] = 10 * GiB
	if allowed, reason := ResourceDecision(host, fixturePrediction(), nil, 2); !allowed {
		t.Fatal("Historical allocated swap blocked work:", reason)
	}
	prediction := fixturePrediction()
	prediction["memoryBytes"] = 16 * GiB
	if allowed, reason := ResourceDecision(host, prediction, nil, 2); allowed || !strings.Contains(reason, "exceeds") {
		t.Fatal("Impossible memory not explained:", reason)
	}
	prediction = fixturePrediction()
	prediction["cpuCores"] = 8.0
	if allowed, reason := ResourceDecision(host, prediction, nil, 2); allowed || !strings.Contains(reason, "exceeds") {
		t.Fatal("Impossible CPU not explained:", reason)
	}
}
func TestResourceDecisionUnknownIncompleteAndLimit(t *testing.T) {
	host, prediction := fixtureResourceHost(), fixturePrediction()
	prediction["samples"] = 0
	if allowed, reason := ResourceDecision(host, prediction, nil, 2); !allowed {
		t.Fatal("Unknown solo workload prevented learning:", reason)
	}
	active := []Object{resourceActive(fixturePrediction(), GiB, 1, true)}
	if allowed, reason := ResourceDecision(host, prediction, active, 2); allowed || !strings.Contains(reason, "unfamiliar") {
		t.Fatal("Unknown workload overlapped:", reason)
	}
	prediction = fixturePrediction()
	active[0]["observed"] = Object{"complete": false}
	if allowed, reason := ResourceDecision(host, prediction, active, 2); allowed || !strings.Contains(reason, "complete measurements") {
		t.Fatal("Incomplete active interval granted:", reason)
	}
	if allowed, reason := ResourceDecision(host, prediction, active, 1); allowed || !strings.Contains(reason, "parallel job limit") {
		t.Fatal("Parallel limit bypassed:", reason)
	}
	for _, invalid := range []Object{{"memoryBytes": true}, Object{"memoryBytes": GiB, "cpuCores": 0, "samples": 0, "source": "prior", "confidence": "unknown"}, Object{"memoryBytes": GiB, "cpuCores": 1.0, "samples": true, "source": "prior", "confidence": "unknown"}} {
		if allowed, _ := ResourceDecision(host, invalid, nil, 2); allowed {
			t.Fatal("Malformed prediction authorized work")
		}
	}
}
func schedulerFixtureGate(t *testing.T) (*Gate, Object) {
	t.Helper()
	directory := t.TempDir()
	state := filepath.Join(directory, "state")
	g := &Gate{State: state, Slot: filepath.Join(directory, "slot"), Config: filepath.Join(directory, "queue.json"), Jobs: filepath.Join(state, "jobs"), Receipts: filepath.Join(state, "receipts"), Metadata: filepath.Join(state, "metadata"), Cancelled: filepath.Join(state, "cancelled"), ControlEnv: map[string]string{"PATH": ToolPath}}
	if e := g.Prepare(); e != nil {
		t.Fatal(e)
	}
	g.queueControl = func(operation string, payload Object) (Object, error) {
		if operation == "snapshot" {
			data, err := os.ReadFile(g.Config)
			if err != nil {
				return nil, err
			}
			return decodeObject(data)
		}
		name := operation
		if operation == "hold" {
			name = "stash"
		}
		file, err := os.OpenFile(g.Config+".calls", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		fmt.Fprintf(file, "%s %v\n", name, payload["id"])
		return Object{}, nil
	}
	snapshot := Object{"tasks": Object{}, "groups": Object{"default": Object{"parallel_tasks": 2, "status": "Running"}}}
	writeSchedulerSnapshot(t, g, snapshot)
	if e := WritePrivate(filepath.Join(state, "scheduling.json"), Object{"mode": "adaptive", "generation": "fixture-generation"}); e != nil {
		t.Fatal(e)
	}
	return g, snapshot
}
func writeSchedulerSnapshot(t *testing.T, g *Gate, snapshot Object) {
	t.Helper()
	data, e := json.Marshal(snapshot)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(g.Config, data, 0600); e != nil {
		t.Fatal(e)
	}
}
func publishSchedulerFixture(t *testing.T, g *Gate, host Object, revision any) {
	t.Helper()
	boot, e := BootIdentity()
	if e != nil {
		t.Fatal(e)
	}
	value := Object{"generation": "fixture-generation", "boot": boot, "sampledAt": resourceNow(), "updatedAt": "fixture", "host": host, "jobs": Object{}, "available": true, "admissionReady": true}
	if revision != nil {
		value["learningRevision"] = revision
	}
	if e = WritePrivate(filepath.Join(g.State, "resource-status.json"), value); e != nil {
		t.Fatal(e)
	}
}
func schedulerFixtureGrant(t *testing.T, g *Gate, manual bool, expires float64) {
	t.Helper()
	configuration, e := SchedulingSettings(g)
	if e != nil {
		t.Fatal(e)
	}
	ledger, e := SchedulingLedger(g, configuration)
	if e != nil {
		t.Fatal(e)
	}
	prediction := fixturePrediction()
	if manual {
		prediction = Object{}
	}
	obj(ledger["grants"])[strings.Repeat("1", 32)] = Object{"task_id": 1, "manual": manual, "expires": expires, "prediction": prediction}
	if e = WritePrivate(filepath.Join(g.State, "resource-admissions.json"), ledger); e != nil {
		t.Fatal(e)
	}
}
func TestSchedulingPermitRechecksHeartbeatExpiryCeilingAndManualOverride(t *testing.T) {
	g, snapshot := schedulerFixtureGate(t)
	marker := Object{"token": strings.Repeat("1", 32), "task_id": 1, "resourceManaged": true}
	publishSchedulerFixture(t, g, fixtureResourceHost(), 0)
	schedulerFixtureGrant(t, g, false, resourceNow()+5)
	if allowed, e := SchedulingPermit(g, marker, 0); e != nil || !allowed {
		t.Fatalf("Valid grant held: %v %v", allowed, e)
	}
	if allowed, e := SchedulingPermit(g, marker, 2); e != nil || allowed {
		t.Fatal("Limit ignored", e)
	}
	obj(obj(snapshot["groups"])["default"])["status"] = "Paused"
	writeSchedulerSnapshot(t, g, snapshot)
	if allowed, e := SchedulingPermit(g, marker, 0); e != nil || allowed {
		t.Fatal("Paused queue allowed", e)
	}
	schedulerFixtureGrant(t, g, true, resourceNow()+5)
	os.Remove(filepath.Join(g.State, "resource-status.json"))
	if allowed, e := SchedulingPermit(g, marker, 99); e != nil || !allowed {
		t.Fatal("Explicit manual override depended on heartbeat/ceiling", e)
	}
	schedulerFixtureGrant(t, g, true, resourceNow()-1)
	if allowed, e := SchedulingPermit(g, marker, 0); e != nil || allowed {
		t.Fatal("Expired manual grant authorized work", e)
	}
}
func TestSchedulingStaleGenerationBootAndCorruptState(t *testing.T) {
	g, _ := schedulerFixtureGate(t)
	for _, entry := range []struct {
		key   string
		value any
	}{{"sampledAt", resourceNow() - 11}, {"sampledAt", resourceNow() + 100}, {"generation", "different"}, {"boot", "different"}} {
		publishSchedulerFixture(t, g, fixtureResourceHost(), 0)
		value, e := ReadPrivate(filepath.Join(g.State, "resource-status.json"))
		if e != nil {
			t.Fatal(e)
		}
		value[entry.key] = entry.value
		WritePrivate(filepath.Join(g.State, "resource-status.json"), value)
		state, e := SchedulingView(g)
		if e != nil || boolean(state["available"]) {
			t.Fatalf("Stale %s authorized heartbeat: %v %v", entry.key, state, e)
		}
	}
	publishSchedulerFixture(t, g, fixtureResourceHost(), 0)
	host := fixtureResourceHost()
	host["sampledAt"] = resourceNow() - 11
	publishSchedulerFixture(t, g, host, 0)
	if state, e := SchedulingView(g); e != nil || boolean(state["available"]) {
		t.Fatal("Fresh wrapper revived stale host")
	}
	schedulerFixtureGrant(t, g, false, resourceNow()+5)
	ledger, e := ReadPrivate(filepath.Join(g.State, "resource-admissions.json"))
	if e != nil {
		t.Fatal(e)
	}
	obj(obj(obj(ledger["grants"])[strings.Repeat("1", 32)])["prediction"])["cpuCores"] = true
	WritePrivate(filepath.Join(g.State, "resource-admissions.json"), ledger)
	configuration, _ := SchedulingSettings(g)
	if _, e = SchedulingLedger(g, configuration); e == nil {
		t.Fatal("Malformed automatic prediction loaded")
	}
}
func TestLearningCorrectionInvalidatesGrantsAndRequiresFreshAppliedRevision(t *testing.T) {
	g, _ := schedulerFixtureGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	identity := fixtureIdentity()
	if e = store.Observe(identity, Object{"title": "Fixture workload"}); e != nil {
		t.Fatal(e)
	}
	publishSchedulerFixture(t, g, fixtureResourceHost(), 0)
	schedulerFixtureGrant(t, g, false, resourceNow()+5)
	request := Object{"revision": 0, "label": "Explicit minimum", "memoryFloorBytes": 4 * GiB, "cpuFloorCores": nil}
	result, e := LearningCorrect(g, "edit", text(identity["family"]), request)
	if e != nil {
		t.Fatal(e)
	}
	if metric(result["revision"]) != 1 || metric(obj(result["profile"])["memoryFloorBytes"]) != 4*GiB {
		t.Fatalf("Save did not return committed correction: %v", result)
	}
	settings, e := SchedulingSettings(g)
	if e != nil {
		t.Fatal(e)
	}
	admissions, e := SchedulingLedger(g, settings)
	if e != nil {
		t.Fatal(e)
	}
	if len(obj(admissions["grants"])) != 0 {
		t.Fatal("Automatic grant survived correction")
	}
	state, e := ReadPrivate(filepath.Join(g.State, "resource-status.json"))
	if e != nil {
		t.Fatal(e)
	}
	state["generation"] = settings["generation"]
	state["sampledAt"] = resourceNow()
	WritePrivate(filepath.Join(g.State, "resource-status.json"), state)
	view, e := SchedulingView(g)
	if e != nil || boolean(view["available"]) {
		t.Fatal("Old controller republished pre-correction estimates", e)
	}
	request["revision"] = 0
	if _, e = LearningCorrect(g, "edit", text(identity["family"]), request); e == nil {
		t.Fatal("Stale correction accepted")
	}
}
func TestLearningLegacyAdaptiveCorrectionRejectedAndPrivateInputValidated(t *testing.T) {
	g, _ := schedulerFixtureGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	identity := fixtureIdentity()
	store.Observe(identity, Object{})
	publishSchedulerFixture(t, g, fixtureResourceHost(), nil)
	before, _ := os.ReadFile(filepath.Join(g.State, "scheduling.json"))
	if _, e = LearningCorrect(g, "edit", text(identity["family"]), Object{"revision": 0, "label": "New", "memoryFloorBytes": nil, "cpuFloorCores": nil}); e == nil {
		t.Fatal("Legacy controller permitted correction")
	}
	after, _ := os.ReadFile(filepath.Join(g.State, "scheduling.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("Rejected correction mutated scheduling state")
	}
	for _, input := range []string{`{"revision":0,"revision":1}`, `{"nested":{"a":1,"a":2}}`, `[]`, `null`, `{} {}`, strings.Repeat("x", maxLearningInput+1)} {
		if _, e = ReadLearningInput(strings.NewReader(input)); e == nil {
			t.Fatalf("Invalid private input accepted: %.60s", input)
		}
	}
	for _, request := range []Object{{"revision": 0, "confirm": 1}, {"revision": -1, "confirm": true}, {"revision": 1.0, "confirm": true}, {"revision": 0, "confirm": true, "extra": true}} {
		if _, e = ValidateLearningRequest("reset", request); e == nil {
			t.Fatal("Malformed correction accepted")
		}
	}
	var output, stderr bytes.Buffer
	code := LearningCLI(g, []string{"reset", text(identity["family"]), "--json"}, strings.NewReader(`{"revision":true,"confirm":true}`), &output, &stderr)
	if code != 125 || !strings.Contains(output.String(), `"error"`) || stderr.Len() != 0 {
		t.Fatal("JSON CLI error contract changed")
	}
}
func TestControllerPublishesGrantAndReservesPendingStart(t *testing.T) {
	g, snapshot := schedulerFixtureGate(t)
	value := strings.Repeat("1", 32)
	identity := fixtureIdentity()
	WritePrivate(filepath.Join(g.State, "workloads", value+".json"), identity)
	obj(snapshot["tasks"])["1"] = Object{"command": "agent-heavy execute " + value, "status": Object{"Stashed": Object{}}, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "priority": 0}
	writeSchedulerSnapshot(t, g, snapshot)
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
	state, e := controller.Tick()
	if e != nil {
		t.Fatal(e)
	}
	if !boolean(state["available"]) || !boolean(obj(obj(obj(state["jobs"])[value])["decision"])["allowed"]) {
		t.Fatalf("Fresh safe candidate not selected: %v", state)
	}
	calls, _ := os.ReadFile(g.Config + ".calls")
	if strings.Count(string(calls), "start 1") != 1 {
		t.Fatalf("Expected one start: %s", calls)
	}
	*now += 2
	b.raw["cpuTicks"] = []any{120, 100, 480, 100}
	if _, e = controller.Tick(); e != nil {
		t.Fatal(e)
	}
	calls, _ = os.ReadFile(g.Config + ".calls")
	if strings.Count(string(calls), "start 1") != 1 {
		t.Fatal("Pending start capacity spent twice")
	}
}
func TestControllerRunBaselineRestartGapAndFinalization(t *testing.T) {
	g, _ := schedulerFixtureGate(t)
	sampler, backend, now, _ := resourceFixture()
	controller, e := NewController(g, sampler, nil)
	if e != nil {
		t.Fatal(e)
	}
	value := strings.Repeat("2", 32)
	marker := Object{"pgid": 400, "members": backend.members, "executionStartedAt": resourceNow()}
	host := fixtureResourceHost()
	first, e := controller.SampleRun(2, value, marker, host, fixturePrediction())
	if e != nil {
		t.Fatal(e)
	}
	if boolean(obj(first["observed"])["complete"]) || !boolean(controller.runs[value]["complete"]) {
		t.Fatal("Clean baseline marked entire run incomplete")
	}
	*now += 2
	backend.usages[101] = fixtureUsage(1, 2*GiB, 3e9)
	second, e := controller.SampleRun(2, value, marker, host, fixturePrediction())
	if e != nil {
		t.Fatal(e)
	}
	if !boolean(obj(second["observed"])["complete"]) || metric(obj(second["prediction"])["memoryBytes"]) < 2*GiB {
		t.Fatal("Observed peak did not immediately raise live reservation")
	}
	backend.fail[101] = true
	*now += 2
	controller.SampleRun(2, value, marker, host, fixturePrediction())
	delete(backend.fail, 101)
	*now += 2
	backend.usages[101] = fixtureUsage(1, 100, 5e9)
	controller.SampleRun(2, value, marker, host, fixturePrediction())
	if boolean(controller.runs[value]["complete"]) {
		t.Fatal("Later successful samples erased observation outage")
	}
	fresh, e := NewController(g, sampler, nil)
	if e != nil {
		t.Fatal(e)
	}
	*now += 2
	fresh.SampleRun(2, value, marker, host, fixturePrediction())
	if boolean(fresh.runs[value]["complete"]) {
		t.Fatal("Restart certified lost sampler history")
	}
	if e = controller.CensorRuns(); e != nil {
		t.Fatal(e)
	}
	retained, e := ReadPrivate(filepath.Join(g.State, "resource-runs", value+".json"))
	if e != nil || boolean(retained["complete"]) {
		t.Fatal("Outage censor did not survive restart")
	}
}
func TestSchedulingPayloadMigrationRequiresAcknowledgedIdentity(t *testing.T) {
	g, _ := schedulerFixtureGate(t)
	value := strings.Repeat("1", 32)
	path, _ := g.PayloadPath(value)
	WritePrivate(path, Object{"token": value, "task_id": 2, "argv": []string{"go", "test"}, "cwd": t.TempDir(), "env": Object{}})
	if _, e := EnsureSchedulingIdentity(g, value, 1); e == nil {
		t.Fatal("Unacknowledged payload migrated")
	}
	payload, _ := ReadPrivate(path)
	payload["task_id"] = 1
	WritePrivate(path, payload)
	if identity, e := EnsureSchedulingIdentity(g, value, 1); e != nil || !validProfileIdentity(identity) {
		t.Fatalf("Acknowledged payload did not migrate: %v %v", identity, e)
	}
}
func TestResourceServiceFailurePublishesUnavailable(t *testing.T) {
	g, _ := schedulerFixtureGate(t)
	s := NewResourceSampler(".", &failingResourceBackend{})
	if e := serveScheduler(g, s, true); e == nil {
		t.Fatal("One-shot collection failure returned success")
	}
	value, e := ReadPrivate(filepath.Join(g.State, "resource-status.json"))
	if e != nil || boolean(value["available"]) || value["host"] != nil {
		t.Fatalf("Failed service left usable heartbeat: %v %v", value, e)
	}
}

type failingResourceBackend struct{}

func (*failingResourceBackend) HostCounters() (Object, error) { return nil, fmt.Errorf("unavailable") }
func (*failingResourceBackend) Members(int) ([]Object, error) { return nil, fmt.Errorf("unavailable") }
func (*failingResourceBackend) Process(int, int) (Object, error) {
	return nil, fmt.Errorf("unavailable")
}
