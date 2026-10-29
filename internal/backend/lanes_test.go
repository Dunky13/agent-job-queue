package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func (f *daemonFixture) writePlugin(name, manifest string) {
	f.t.Helper()
	directory := filepath.Join(pluginRoot(f.gate), name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plugin.json"), []byte(manifest), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *daemonFixture) reloadPlugins() Object {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	if code := PluginsMain(f.gate, []string{"reload", "--json"}, &stdout, &stderr); code != 0 {
		f.t.Fatalf("plugins reload: %d %s %s", code, stdout.String(), stderr.String())
	}
	var result Object
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *daemonFixture) launchLane(lane string, args ...string) *queueClient {
	f.t.Helper()
	executable, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	client := &queueClient{done: make(chan error, 1)}
	client.command = exec.Command(executable, append([]string{"run", "--lane", lane, "--"}, args...)...)
	client.command.Env = f.environment
	client.command.Dir = f.root
	client.command.Stdout = &client.out
	client.command.Stderr = &client.errors
	client.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = client.command.Start(); err != nil {
		f.t.Fatal(err)
	}
	go func() { client.done <- client.command.Wait(); close(client.done) }()
	f.clients = append(f.clients, client)
	return client
}

func (f *daemonFixture) lane(args ...string) (int, Object) {
	f.t.Helper()
	code, result, err := f.laneQuiet(args...)
	if err != nil {
		f.t.Fatal(err)
	}
	return code, result
}

// laneQuiet is safe to call from goroutines other than the test's own.
func (f *daemonFixture) laneQuiet(args ...string) (int, Object, error) {
	var stdout, stderr bytes.Buffer
	gate := *f.gate
	code := LaneMain(&gate, append(args, "--json"), &stdout, &stderr)
	var result Object
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return code, nil, fmt.Errorf("lane %v: %d %q %q", args, code, stdout.String(), stderr.String())
	}
	return code, result, nil
}

func (f *daemonFixture) latestTask() int {
	f.t.Helper()
	latest := -1
	for _, id := range f.taskIDs() {
		latest = max(latest, id)
	}
	return latest
}

// watchLane fails the test if a lane ever reports more running jobs plus held
// leases than its concurrency.
func (f *daemonFixture) watchLane(name string) func() {
	f.t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	var violation atomic.Value
	// A Gate is not safe for concurrent use; the watcher polls with its own.
	gate := *f.gate
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			snapshot, err := gate.Snapshot()
			if err != nil {
				continue
			}
			lane := obj(obj(snapshot["lanes"])[name])
			running, _ := integer(lane["running"])
			held, _ := integer(lane["held"])
			concurrency, _ := integer(lane["concurrency"])
			if lane != nil && running+held > concurrency {
				violation.Store(fmt.Sprintf("lane %s had %d running and %d held with concurrency %d", name, running, held, concurrency))
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		if message, ok := violation.Load().(string); ok {
			f.t.Fatal(message)
		}
	}
}

func TestRealDaemonLaneConcurrencyUnderConcurrentAcquireRunAndStartNow(t *testing.T) {
	f := daemonAcceptance(t)
	f.writePlugin("qa-plugin", `{"schemaVersion":1,"name":"qa-plugin","lanes":[{"name":"qa","label":"QA","concurrency":1,"priority":50,"sharesLimit":false}]}`)
	f.reloadPlugins()
	stopWatching := f.watchLane("qa")
	exclusive := filepath.Join(f.root, "qa-exclusive")
	holder := f.launch("/bin/sh", "-c", "echo started > default-started; while [ ! -f release-default ]; do sleep 0.1; done")
	fixtureUntil(t, 8*time.Second, func() bool { return acceptanceFileExists(filepath.Join(f.root, "default-started")) })
	// A lane outside the global limit runs beside the default job at limit 1.
	script := fmt.Sprintf("mkdir %q || exit 99; echo $$ > \"$1\"; while [ ! -f \"$2\" ]; do sleep 0.05; done; rmdir %q", exclusive, exclusive)
	first := f.launchLane("qa", "/bin/sh", "-c", script, "lane", "first-started", "release-first")
	fixtureUntil(t, 8*time.Second, func() bool { return acceptanceFileExists(filepath.Join(f.root, "first-started")) })
	second := f.launchLane("qa", "/bin/sh", "-c", script, "lane", "second-started", "release-second")
	f.waitJobs(3)
	secondID := f.latestTask()
	if err := StartJob(f.gate, secondID); !errors.Is(err, errQueueLaneFull) {
		t.Fatalf("Start now exceeded the lane's concurrency: %v", err)
	}
	// Agents acquire leases concurrently while commands queue in the same lane.
	var wg sync.WaitGroup
	failures := make(chan string, 8)
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, lease, err := f.laneQuiet("acquire", "qa", "--owner-pid", strconv.Itoa(os.Getpid()), "--label", fmt.Sprintf("agent %d", i), "--timeout", "40")
			if err != nil || code != 0 {
				failures <- fmt.Sprintf("acquire %d: %d %v %v", i, code, lease, err)
				return
			}
			if err := os.Mkdir(exclusive, 0o700); err != nil {
				failures <- fmt.Sprintf("lease %v overlapped other lane work", lease["id"])
			} else {
				time.Sleep(150 * time.Millisecond)
				_ = os.Remove(exclusive)
			}
			if code, result, err := f.laneQuiet("release", strconv.Itoa(mustInteger(lease["id"])), "--owner-pid", strconv.Itoa(os.Getpid())); err != nil || code != 0 {
				failures <- fmt.Sprintf("release %d: %d %v %v", i, code, result, err)
			}
		}(i)
	}
	time.Sleep(300 * time.Millisecond)
	if acceptanceFileExists(filepath.Join(f.root, "second-started")) {
		t.Fatal("the second lane job started while the first held the lane")
	}
	acceptanceRelease(t, filepath.Join(f.root, "release-first"))
	first.wait(t, 0)
	fixtureUntil(t, 20*time.Second, func() bool { return acceptanceFileExists(filepath.Join(f.root, "second-started")) })
	acceptanceRelease(t, filepath.Join(f.root, "release-second"))
	second.wait(t, 0)
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	acceptanceRelease(t, filepath.Join(f.root, "release-default"))
	holder.wait(t, 0)
	stopWatching()
}

func mustInteger(value any) int { n, _ := integer(value); return n }

func TestRealDaemonLanePriorityAgainstDefaultLane(t *testing.T) {
	f := daemonAcceptance(t)
	f.writePlugin("lanes", `{"schemaVersion":1,"name":"lanes","lanes":[{"name":"high","concurrency":1,"priority":50},{"name":"low","concurrency":1,"priority":-10}]}`)
	f.reloadPlugins()
	holder := f.launch("/bin/sh", "-c", "echo started > holder-started; while [ ! -f release ]; do sleep 0.1; done")
	fixtureUntil(t, 8*time.Second, func() bool { return acceptanceFileExists(filepath.Join(f.root, "holder-started")) })
	low := f.launchLane("low", "/bin/sh", "-c", "echo low >> order")
	f.waitJobs(2)
	plain := f.launch("/bin/sh", "-c", "echo default >> order")
	f.waitJobs(3)
	high := f.launchLane("high", "/bin/sh", "-c", "echo high >> order")
	f.waitJobs(4)
	status, err := StatusData(f.gate)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{}
	for _, raw := range status["jobs"].([]Object) {
		if raw["queuePosition"] != nil {
			order = append(order, text(raw["lane"]))
		}
	}
	if strings.Join(order, ",") != "high,default,low" {
		t.Fatalf("status queue order: %v", order)
	}
	lanes := status["lanes"].([]Object)
	if len(lanes) != 3 || lanes[0]["name"] != "high" || lanes[1]["name"] != defaultLaneName || lanes[2]["name"] != "low" {
		t.Fatalf("lanes are not in priority order: %v", lanes)
	}
	acceptanceRelease(t, filepath.Join(f.root, "release"))
	holder.wait(t, 0)
	high.wait(t, 0)
	plain.wait(t, 0)
	low.wait(t, 0)
	body, err := os.ReadFile(filepath.Join(f.root, "order"))
	if err != nil || strings.Join(strings.Fields(string(body)), ",") != "high,default,low" {
		t.Fatalf("dispatch order: %q %v", body, err)
	}
}

func TestRealDaemonBrokenPluginsLeaveDefaultLaneWorking(t *testing.T) {
	f := daemonAcceptance(t)
	f.writePlugin("typo", `{"schemaVersion":1,"name":"typo","lanes":[{"name":"qa","concurency":1}]}`)
	f.writePlugin("duplicate", `{"schemaVersion":1,"name":"duplicate","name":"duplicate","lanes":[{"name":"dup","concurrency":1}]}`)
	f.writePlugin("reserved", `{"schemaVersion":1,"name":"reserved","lanes":[{"name":"default","concurrency":8}]}`)
	f.writePlugin("collide-a", `{"schemaVersion":1,"name":"collide-a","lanes":[{"name":"same","concurrency":1}]}`)
	f.writePlugin("collide-b", `{"schemaVersion":1,"name":"collide-b","lanes":[{"name":"same","concurrency":2}]}`)
	f.writePlugin("writable", `{"schemaVersion":1,"name":"writable","lanes":[{"name":"open","concurrency":1}]}`)
	if err := os.Chmod(filepath.Join(pluginRoot(f.gate), "writable", "plugin.json"), 0o666); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(f.root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "plugin.json"), []byte(`{"schemaVersion":1,"name":"linked","lanes":[{"name":"linked","concurrency":1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pluginRoot(f.gate), "linked")); err != nil {
		t.Fatal(err)
	}
	result := f.reloadPlugins()
	plugins := list(result["plugins"])
	if len(plugins) != 7 {
		t.Fatalf("plugins: %v", plugins)
	}
	for _, raw := range plugins {
		plugin := obj(raw)
		if plugin["error"] == nil {
			t.Errorf("plugin %v loaded without an error: %v", plugin["name"], plugin)
		}
	}
	lanes := list(result["lanes"])
	if len(lanes) != 1 || obj(lanes[0])["name"] != defaultLaneName {
		t.Fatalf("a broken plugin added a lane: %v", lanes)
	}
	client := f.launch("/bin/sh", "-c", "echo default works")
	if output := client.wait(t, 0); !strings.Contains(output, "default works") {
		t.Fatalf("default lane output: %q", output)
	}
	status, err := StatusData(f.gate)
	if err != nil {
		t.Fatal(err)
	}
	if len(status["plugins"].([]Object)) != 7 || len(status["lanes"].([]Object)) != 1 {
		t.Fatalf("status plugins and lanes: %v %v", status["plugins"], status["lanes"])
	}
	if code, _ := f.lane("acquire", "same", "--owner-pid", strconv.Itoa(os.Getpid())); code != laneExitUsage {
		t.Fatalf("acquired a disabled colliding lane: %d", code)
	}
	unknown := f.launchLane("open", "/usr/bin/true")
	unknown.wait(t, laneExitUsage)
	if !strings.Contains(unknown.errors.String(), "available lanes: default") {
		t.Fatalf("unknown lane message: %q", unknown.errors.String())
	}
}

func TestRealDaemonLeaseCLIWaitsOwnersAndStatus(t *testing.T) {
	f := daemonAcceptance(t)
	f.writePlugin("qa-plugin", `{"schemaVersion":1,"name":"qa-plugin","lanes":[{"name":"qa","label":"QA","concurrency":1,"priority":50,"sharesLimit":false,"reservation":{"memoryMiB":4096,"cpuPercent":100}}]}`)
	f.reloadPlugins()
	owners := []*exec.Cmd{}
	for range 2 {
		owner := exec.Command("/bin/sleep", "60")
		if err := owner.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
		owners = append(owners, owner)
	}
	firstOwner, secondOwner := strconv.Itoa(owners[0].Process.Pid), strconv.Itoa(owners[1].Process.Pid)
	code, first := f.lane("acquire", "qa", "--owner-pid", firstOwner, "--label", "QA session", "--meta", "ticket=WM-123", "--meta", "worktree=/tmp/example")
	if code != 0 || first["state"] != "held" || first["schemaVersion"] != float64(1) {
		t.Fatalf("first acquire: %d %v", code, first)
	}
	code, second := f.lane("acquire", "qa", "--owner-pid", secondOwner, "--timeout", "0")
	if code != laneExitTimedOut || second["state"] != "queued" || second["id"] == nil {
		t.Fatalf("bounded acquire: %d %v", code, second)
	}
	secondID := strconv.Itoa(mustInteger(second["id"]))
	code, third := f.lane("acquire", "qa", "--owner-pid", secondOwner, "--no-wait")
	if code != 0 || third["state"] != "queued" || third["position"] != float64(2) {
		t.Fatalf("no-wait acquire: %d %v", code, third)
	}
	for range 2 {
		if code, waited := f.lane("wait", secondID, "--timeout", "1"); code != laneExitTimedOut || waited["position"] != float64(1) {
			t.Fatalf("bounded wait lost its position: %d %v", code, waited)
		}
	}
	status, err := StatusData(f.gate)
	if err != nil {
		t.Fatal(err)
	}
	var held Object
	for _, job := range status["jobs"].([]Object) {
		if job["id"] == mustInteger(first["id"]) {
			held = job
		}
	}
	if held == nil || held["kind"] != "lease" || held["lane"] != "qa" || held["state"] != "held" || held["title"] != "QA session" || held["logPath"] != "" || held["exitCode"] != nil || held["ownerAlive"] != true || obj(held["meta"])["ticket"] != "WM-123" || held["canCancel"] != true || held["canStart"] != false {
		t.Fatalf("held lease in status: %v", held)
	}
	queue := obj(status["queue"])
	if queue["running"] != 1 || queue["waiting"] != 2 {
		t.Fatalf("queue summary: %v", queue)
	}
	qa := status["lanes"].([]Object)[0]
	if qa["name"] != "qa" || mustInteger(qa["held"]) != 1 || mustInteger(qa["waiting"]) != 2 || qa["plugin"] != "qa-plugin" || qa["sharesLimit"] != false {
		t.Fatalf("lane summary: %v", qa)
	}
	if code, refused := f.lane("release", secondID); code != laneExitRefused || !strings.Contains(text(refused["error"]), "--force") {
		t.Fatalf("release without owner: %d %v", code, refused)
	}
	if err := owners[0].Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owners[0].Wait()
	if code, granted := f.lane("wait", secondID, "--timeout", "15"); code != 0 || granted["state"] != "held" {
		t.Fatalf("lease after the holder exited: %d %v", code, granted)
	}
	firstID := strconv.Itoa(mustInteger(first["id"]))
	if code, gone := f.lane("status", firstID); code != 0 || gone["state"] != "expired" || gone["reason"] != "owner-exited" || gone["ownerAlive"] != false {
		t.Fatalf("lease of an exited owner: %d %v", code, gone)
	}
	if code, gone := f.lane("wait", firstID, "--timeout", "1"); code != laneExitRefused || gone["state"] != "expired" {
		t.Fatalf("wait on an expired lease: %d %v", code, gone)
	}
	if code, renewed := f.lane("renew", secondID); code != 0 || renewed["state"] != "held" {
		t.Fatalf("renew without a TTL: %d %v", code, renewed)
	}
	if code, released := f.lane("release", secondID, "--owner-pid", secondOwner); code != 0 || released["state"] != "released" {
		t.Fatalf("owner release: %d %v", code, released)
	}
	thirdID := strconv.Itoa(mustInteger(third["id"]))
	if code, _ := f.lane("wait", thirdID, "--timeout", "10"); code != 0 {
		t.Fatalf("third lease was not granted: %d", code)
	}
	if code, forced := f.lane("release", thirdID, "--force"); code != 0 || forced["state"] != "released" {
		t.Fatalf("forced release: %d %v", code, forced)
	}
	if code, lanes := f.lane("list"); code != 0 || len(list(lanes["lanes"])) != 2 {
		t.Fatalf("lane list: %d %v", code, lanes)
	}
}

func TestRealDaemonAdaptiveLeasesUseLaneReservations(t *testing.T) {
	f := daemonAcceptance(t)
	f.writePlugin("devices", `{"schemaVersion":1,"name":"devices","lanes":[
  {"name":"mutex","concurrency":1,"sharesLimit":false},
  {"name":"fits","concurrency":1,"sharesLimit":false,"reservation":{"memoryMiB":2048,"cpuPercent":100}},
  {"name":"heavy","concurrency":1,"reservation":{"memoryMiB":65536}}]}`)
	f.reloadPlugins()
	startAcceptanceController(t, f, "normal")
	if err := SetSchedulingMode(f.gate, "adaptive"); err != nil {
		t.Fatal(err)
	}
	owner := strconv.Itoa(os.Getpid())
	if code, lease := f.lane("acquire", "mutex", "--owner-pid", owner, "--timeout", "10"); code != 0 || lease["state"] != "held" {
		t.Fatalf("zero-reservation lease in adaptive mode: %d %v", code, lease)
	}
	if code, lease := f.lane("acquire", "fits", "--owner-pid", owner, "--timeout", "10"); code != 0 || lease["state"] != "held" {
		t.Fatalf("fitting reservation in adaptive mode: %d %v", code, lease)
	}
	code, heavy := f.lane("acquire", "heavy", "--owner-pid", owner, "--timeout", "1")
	if code != laneExitTimedOut {
		t.Fatalf("an oversized reservation was admitted: %d %v", code, heavy)
	}
	fixtureUntil(t, 5*time.Second, func() bool {
		status, err := StatusData(f.gate)
		if err != nil {
			return false
		}
		for _, job := range status["jobs"].([]Object) {
			if job["id"] == mustInteger(heavy["id"]) {
				return job["phase"] == "Lane reservation exceeds this machine's budget"
			}
		}
		return false
	})
	// Held leases are not unfamiliar workloads: an ordinary job still starts.
	started, release := filepath.Join(f.root, "job.started"), filepath.Join(f.root, "job.release")
	client := f.launch(acceptanceHoldingCommand(started, release)...)
	acceptanceWaitStarted(t, started)
	acceptanceRelease(t, release)
	client.wait(t, 0)
}
