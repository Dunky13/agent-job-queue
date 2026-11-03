package backend

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Dunky13/agent-job-queue/internal/wire"
)

var updateStatusContract = flag.Bool("update", false, "rewrite the status snapshot golden files")

const (
	statusGoldenPath   = "../../native/tests/fixtures/queue.json"
	statusVariantsPath = "../../native/tests/fixtures/status-variants.json"
	statusRunningToken = "11111111111111111111111111111111"
	statusQueuedToken  = "22222222222222222222222222222222"
	statusSubmittedAt  = "2026-10-18T21:06:21.570177+02:00"
)

var (
	statusSampledAt = regexp.MustCompile(`"sampledAt":[0-9.]+`)
	statusUpdatedAt = regexp.MustCompile(`"updatedAt":"[^"]*"}$`)
)

// statusScenario is one private daemon state. Consumer scenarios are valid for
// every consumer and are checked in; the others are Go-only edge states.
type statusScenario struct {
	name      string
	consumer  bool
	snapshot  Object
	mode      string
	resources Object
	receipts  map[int]Object
	drain     Object // the contents of drain.json, when the scenario has one
}

func statusGroup(status string) Object {
	return Object{"default": Object{"status": status, "parallel_tasks": 1}}
}

func statusCommand(token string, status Object, priority int) Object {
	task := Object{"command": "echo SECRET", "path": "/example/project", "priority": priority, "created_at": statusSubmittedAt, "status": status}
	if token != "" {
		task["token"] = token
	}
	return task
}

func statusLease(state, lane string, status, fields Object) Object {
	lease := Object{"state": state, "meta": Object{"ticket": "WM-123"}, "owner": Object{"pid": 4242}, "ownerAlive": true, "queuedAt": "2026-10-29T09:59:00Z", "grantedAt": nil, "renewedAt": nil, "expiresAt": nil, "reason": nil}
	for key, value := range fields {
		lease[key] = value
	}
	return Object{"kind": "lease", "lane": lane, "label": "QA session", "priority": 0, "created_at": "2026-10-29T09:59:00Z", "status": status, "lease": lease}
}

func statusLaneRow(name, label string, plugin any, priority int, shares bool, running, held, waiting int) Object {
	return Object{"name": name, "label": label, "plugin": plugin, "concurrency": 1, "priority": priority, "sharesLimit": shares, "reservation": Object{"memoryMiB": 4096, "cpuPercent": 100}, "ttlSeconds": 900, "removed": false, "running": running, "held": held, "waiting": waiting}
}

func statusPrediction() Object {
	return Object{"memoryBytes": 2147483648, "cpuCores": 2, "durationSeconds": 60, "samples": 3, "familySamples": 3, "confidence": "low", "source": "family"}
}

func statusResources(mode string, jobs, leases Object) Object {
	return Object{"mode": mode, "available": true, "admissionReady": true, "learningRevision": 0, "reason": "Fixture resource controller", "updatedAt": "2026-10-19T10:00:00Z", "host": Object{"totalMemoryBytes": 17179869184, "availableMemoryBytes": 4294967296, "cpuCores": 8, "cpuBusyCores": 2.5, "memoryPressure": "warning", "swapUsedBytes": 1073741824, "swapInBytesPerSecond": 0, "swapOutBytesPerSecond": 0, "diskFreeBytes": 21474836480, "machine": "arm64", "complete": false}, "jobs": jobs, "leases": leases}
}

func statusScenarios() []statusScenario {
	running := Object{"Running": Object{"start": "2026-10-18T21:07:00.000001+02:00"}}
	queued := Object{"Queued": Object{}}
	held := statusLease("held", "qa", Object{"Running": Object{"start": "2026-10-29T09:59:01Z"}}, Object{"grantedAt": "2026-10-29T09:59:01Z"})
	failed := Object{"Done": Object{"result": Object{"Failed": 7}, "start": "2026-10-18T21:08:00Z", "end": "2026-10-18T21:09:00Z"}}
	lanes := Object{"qa": statusLaneRow("qa", "QA", "qa-plugin", 50, false, 0, 1, 0), defaultLaneName: statusLaneRow(defaultLaneName, "Default", nil, 0, true, 1, 0, 1)}
	plugins := []any{Object{"name": "qa-plugin", "loaded": true, "error": nil, "warning": nil, "lanes": []any{"qa"}}, Object{"name": "broken", "loaded": false, "error": "Invalid plugin.json: unknown field \"concurency\"", "warning": nil, "lanes": []any{}}}
	current := statusResources("adaptive", Object{
		statusRunningToken: Object{"prediction": statusPrediction(), "observed": Object{"memoryBytes": 1073741824, "peakMemoryBytes": 1610612736, "cpuSeconds": 20, "cpuCores": 1.5, "durationSeconds": 15, "samples": 5, "complete": false}, "decision": nil, "learning": Object{"id": strings.Repeat("a", 64), "label": "Browser tests", "reason": "Operator shared these job families"}},
		statusQueuedToken:  Object{"prediction": statusPrediction(), "observed": nil, "decision": Object{"allowed": false, "reason": "Waiting for memory headroom"}},
	}, Object{})
	return []statusScenario{
		{name: "current", consumer: true, mode: "adaptive", resources: current, snapshot: Object{"groups": statusGroup("Running"), "lanes": lanes, "plugins": plugins, "tasks": Object{
			"1": statusCommand(statusRunningToken, running, 0),
			"2": statusCommand(statusQueuedToken, queued, 0),
			"3": statusCommand("", failed, 0),
			"4": held,
		}}},
		{name: "empty", consumer: true, snapshot: Object{"groups": statusGroup("Running"), "tasks": Object{}}},
		{name: "pausedWithPublishedLanes", consumer: true, snapshot: Object{"groups": statusGroup("Paused"), "lanes": Object{defaultLaneName: statusLaneRow(defaultLaneName, "Default", nil, 0, true, 0, 0, 0)}, "plugins": []any{}, "tasks": Object{}}},
		{name: "daemonWithoutLanes", consumer: true, snapshot: Object{"groups": statusGroup("Running"), "tasks": Object{
			"1": statusCommand(statusRunningToken, running, 0),
			"2": statusCommand(statusQueuedToken, queued, 5),
			"3": statusCommand("", queued, 2),
		}}},
		{name: "shadowLearning", consumer: true, mode: "shadow", resources: statusResources("shadow", Object{statusQueuedToken: Object{"prediction": Object{"memoryBytes": 1073741824, "cpuCores": 2.0, "durationSeconds": nil, "samples": 0, "familySamples": 0, "confidence": "unknown", "source": "prior"}, "observed": nil, "decision": nil}}, Object{}), snapshot: Object{"groups": statusGroup("Running"), "tasks": Object{"2": statusCommand(statusQueuedToken, queued, 0)}}},
		{name: "olderController", consumer: true, mode: "adaptive", resources: func() Object {
			value := statusResources("adaptive", Object{statusQueuedToken: Object{"prediction": Object{"memoryBytes": 1073741824, "cpuCores": 1.25, "durationSeconds": 10.5, "samples": 12, "confidence": "high", "source": "variant"}, "observed": nil, "decision": nil}}, Object{})
			obj(value["host"])["cpuBusyCores"] = 0
			return value
		}(), snapshot: Object{"groups": statusGroup("Running"), "tasks": Object{"2": statusCommand(statusQueuedToken, Object{"Stashed": Object{}}, 0)}}},
		{name: "receiptPhases", consumer: true, receipts: map[int]Object{1: {"token": statusRunningToken}, 3: {"token": statusQueuedToken}}, snapshot: Object{"groups": statusGroup("Running"), "tasks": Object{
			"1": statusCommand(statusRunningToken, running, 0),
			"3": statusCommand(statusQueuedToken, Object{"Done": Object{"result": "Success", "end": "2026-10-18T21:09:00Z"}}, 0),
		}}},
		{name: "leaseStates", consumer: true, mode: "adaptive", resources: statusResources("adaptive", Object{}, Object{"6": Object{"decision": Object{"allowed": false, "reason": "Lane reservation exceeds this machine's budget"}}}), snapshot: Object{"groups": statusGroup("Running"),
			"lanes": Object{"qa": statusLaneRow("qa", "QA", "qa-plugin", 50, false, 0, 1, 2), "gone": func() Object {
				lane := statusLaneRow("gone", "gone", nil, 0, false, 0, 0, 0)
				lane["removed"] = true
				return lane
			}(), defaultLaneName: statusLaneRow(defaultLaneName, "Default", nil, 0, true, 0, 0, 0)},
			"plugins": []any{Object{"name": "qa-plugin", "loaded": true, "error": nil, "warning": "2 hook events were dropped because 4 hooks were already running", "lanes": []any{"qa"}}},
			"tasks": Object{
				"5":  statusLease("held", "qa", Object{"Running": Object{"start": "2026-10-29T09:59:01Z"}}, Object{"grantedAt": "2026-10-29T09:59:01Z", "renewedAt": "2026-10-29T10:00:00Z", "expiresAt": "2026-10-29T10:15:00Z", "meta": nil}),
				"6":  statusLease("queued", "qa", Object{"Queued": Object{}}, Object{"ownerAlive": nil}),
				"7":  statusLease("queued", "qa", Object{"Queued": Object{}}, Object{"owner": nil}),
				"8":  statusLease("released", "qa", Object{"Done": Object{"result": "Success", "end": "2026-10-29T10:03:00Z"}}, Object{"reason": "released"}),
				"9":  statusLease("expired", "qa", Object{"Done": Object{"result": "Success", "end": "2026-10-29T10:02:00Z"}}, Object{"reason": "ttl-expired"}),
				"10": statusLease("dropped", "qa", Object{"Done": Object{"result": "Success", "end": "2026-10-29T10:02:00Z"}}, Object{"reason": "owner-exited"}),
			}}},
		{name: "doneResults", consumer: true, snapshot: Object{"groups": statusGroup("Running"), "tasks": Object{
			"1": statusCommand("", Object{"Done": Object{"result": "Killed", "end": "2026-10-18T21:09:00Z"}}, 0),
			"2": statusCommand("", Object{"Done": Object{"result": "Errored", "end": "2026-10-18T21:09:00Z"}}, 0),
			"3": func() Object {
				task := statusCommand("", Object{"Done": Object{"result": "Success"}}, 0)
				delete(task, "created_at")
				task["label"] = "Line\nbreak <b>&</b>"
				return task
			}(),
			"4": statusCommand("", Object{"Paused": Object{}}, -3),
			"5": statusCommand("", Object{"Stashed": Object{}}, 0),
		}}},
		// Intake closed by a drain, and by a drain file that cannot be trusted. Both
		// add an optional key to queue; older consumers ignore it.
		{name: "draining", consumer: true, drain: Object{"until": "2099-01-01T00:00:00Z"}, snapshot: Object{"groups": statusGroup("Running"), "tasks": Object{"1": statusCommand(statusRunningToken, running, 0)}}},
		{name: "invalidDrainFile", consumer: true, drain: Object{"until": "tomorrow"}, snapshot: Object{"groups": statusGroup("Running"), "tasks": Object{}}},
		{name: "missingDefaultGroup", snapshot: Object{"groups": Object{}, "tasks": Object{"2": statusCommand(statusQueuedToken, queued, 0)}}},
	}
}

func prepareStatusScenario(t *testing.T, scenario statusScenario) *Gate {
	t.Helper()
	g := testGate(t)
	for _, value := range []string{statusRunningToken, statusQueuedToken} {
		if err := CacheDescription(g, value, Object{"project": "Example", "branch": "main", "directory": "/example/project", "title": "Fixture validation", "executable": "python3"}); err != nil {
			t.Fatal(err)
		}
	}
	for id, receipt := range scenario.receipts {
		mustWrite(t, g.ReceiptPath(id), receipt)
	}
	fakeQueue(t, g, scenario.snapshot)
	if scenario.drain != nil {
		mustWrite(t, drainPath(g), scenario.drain)
	}
	if scenario.mode != "" {
		mustWrite(t, filepath.Join(g.State, "scheduling.json"), Object{"mode": scenario.mode, "generation": "fixture-generation"})
	}
	if scenario.resources != nil {
		boot, err := BootIdentity()
		if err != nil {
			t.Fatal(err)
		}
		value := Object{"generation": "fixture-generation", "boot": boot, "sampledAt": resourceNow()}
		for key, field := range scenario.resources {
			value[key] = field
		}
		obj(value["host"])["sampledAt"] = resourceNow()
		mustWrite(t, filepath.Join(g.State, "resource-status.json"), value)
	}
	return g
}

func normalizeStatusBytes(g *Gate, body []byte) []byte {
	// json.Marshal escapes characters such as & in a temporary directory's path.
	escaped, err := json.Marshal(g.State)
	if err != nil {
		panic(err)
	}
	body = bytes.ReplaceAll(body, escaped[1:len(escaped)-1], []byte("/example/state"))
	body = bytes.ReplaceAll(body, []byte(g.State), []byte("/example/state"))
	body = statusSampledAt.ReplaceAll(body, []byte(`"sampledAt":1792404000`))
	return statusUpdatedAt.ReplaceAll(body, []byte(`"updatedAt":"2026-10-18T19:44:39.127731+00:00"}`))
}

// renderStatusScenario returns the producer's exact bytes with only the clock
// and the private temporary root replaced.
func renderStatusScenario(t *testing.T, scenario statusScenario) []byte {
	t.Helper()
	g := prepareStatusScenario(t, scenario)
	data, err := StatusData(g)
	if err != nil {
		t.Fatalf("%s: %v", scenario.name, err)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return normalizeStatusBytes(g, body)
}

func statusGoldenBytes(t *testing.T, value []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := json.Indent(&output, value, "", "  "); err != nil {
		t.Fatal(err)
	}
	output.WriteByte('\n')
	return output.Bytes()
}

// A Go-only edge state: every consumer rejects it, so it is not a shared fixture.
func TestStatusWithoutDefaultGroupReportsNullLimit(t *testing.T) {
	for _, scenario := range statusScenarios() {
		if scenario.name != "missingDefaultGroup" {
			continue
		}
		var status struct {
			Lanes []Object `json:"lanes"`
			Queue Object   `json:"queue"`
		}
		if err := json.Unmarshal(renderStatusScenario(t, scenario), &status); err != nil {
			t.Fatal(err)
		}
		if limit, present := status.Queue["limit"]; !present || limit != nil || status.Queue["status"] != "unknown" {
			t.Fatalf("queue without a default group: %v", status.Queue)
		}
		if concurrency, present := status.Lanes[0]["concurrency"]; !present || concurrency != nil {
			t.Fatalf("lane without a default group: %v", status.Lanes[0])
		}
		return
	}
	t.Fatal("missing scenario")
}

// The wire types must round-trip the golden exactly, or a consumer of the types
// would see less than `status --json` publishes.
func TestStatusContractRoundTripsThroughWireTypes(t *testing.T) {
	for _, scenario := range statusScenarios() {
		body := renderStatusScenario(t, scenario)
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var snapshot wire.Snapshot
		if err := decoder.Decode(&snapshot); err != nil {
			t.Fatalf("%s: %v", scenario.name, err)
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil || !bytes.Equal(encoded, body) {
			t.Fatalf("%s did not round-trip (%v):\nbefore %s\nafter  %s", scenario.name, err, body, encoded)
		}
	}
}

func TestStatusContractGolden(t *testing.T) {
	files := map[string][]byte{}
	variants := map[string]json.RawMessage{}
	for _, scenario := range statusScenarios() {
		body := renderStatusScenario(t, scenario)
		switch {
		case scenario.name == "current":
			files[statusGoldenPath] = statusGoldenBytes(t, body)
		case scenario.consumer:
			variants[scenario.name] = body
		}
	}
	encoded, err := json.Marshal(variants)
	if err != nil {
		t.Fatal(err)
	}
	files[statusVariantsPath] = statusGoldenBytes(t, encoded)
	for path, want := range files {
		if *updateStatusContract {
			if err := os.WriteFile(path, want, 0644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is stale (%v). Regenerate it with: go test -p 2 ./internal/backend -run '^TestStatusContractGolden$' -update", path, err)
		}
	}
}
