package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

type laneClock struct{ now time.Time }

func (c *laneClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// laneDaemon drives the real daemon control and dispatch code in-process on a
// private store, with an injectable clock for TTL expiry.
func laneDaemon(t *testing.T, limit int, manifests map[string]string) (*queueDaemon, *laneClock) {
	t.Helper()
	return laneDaemonWith(t, limit, func(g *Gate) {
		for name, manifest := range manifests {
			pluginFixture(t, g, name, manifest, nil)
		}
	})
}

func laneDaemonWith(t *testing.T, limit int, setup func(*Gate)) (*queueDaemon, *laneClock) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("lease owners use macOS process identity")
	}
	g := testGate(t)
	setup(g)
	s, err := openQueueStore(g.State, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	if _, err = s.db.Exec("UPDATE queue_settings SET parallel_limit=? WHERE id=1", limit); err != nil {
		t.Fatal(err)
	}
	d := newQueueDaemon(g, s)
	clock := &laneClock{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	d.now = func() time.Time { return clock.now }
	t.Cleanup(d.hooks.wait)
	return d, clock
}

func acquireTestLease(t *testing.T, d *queueDaemon, lane string, extra Object) Object {
	t.Helper()
	payload := Object{"token": token(), "lane": lane}
	if _, ok := extra["ttlSeconds"]; !ok {
		payload["ownerPid"] = os.Getpid()
	}
	for key, value := range extra {
		payload[key] = value
	}
	lease, err := d.control("lease-acquire", payload)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func leaseState(t *testing.T, d *queueDaemon, lease Object) Object {
	t.Helper()
	id, _ := integer(lease["id"])
	view, err := d.control("lease-status", Object{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func requireLeaseState(t *testing.T, d *queueDaemon, lease Object, state string) Object {
	t.Helper()
	view := leaseState(t, d, lease)
	if view["state"] != state {
		t.Fatalf("lease %v is %v, want %s: %v", lease["id"], view["state"], state, view)
	}
	return view
}

func heldInLane(t *testing.T, d *queueDaemon, lane string) int {
	t.Helper()
	jobs, err := d.store.unfinishedJobs()
	if err != nil {
		t.Fatal(err)
	}
	count := laneUsage(jobs)[lane]
	if count == nil {
		return 0
	}
	return count.occupied()
}

func TestLeaseLaneConcurrencyAndQueueOrder(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	leases := []Object{}
	for range 4 {
		leases = append(leases, acquireTestLease(t, d, "qa", nil))
		if held := heldInLane(t, d, "qa"); held != 1 {
			t.Fatalf("lane concurrency exceeded: %d held", held)
		}
	}
	requireLeaseState(t, d, leases[0], "held")
	for index, lease := range leases[1:] {
		if view := requireLeaseState(t, d, lease, "queued"); view["position"] != index+1 {
			t.Fatalf("queue position %v, want %d", view["position"], index+1)
		}
	}
	// Start now never exceeds a lane's concurrency.
	id, _ := integer(leases[1]["id"])
	if _, err := d.control("start", Object{"id": id}); !errors.Is(err, errQueueLaneFull) {
		t.Fatalf("start now exceeded the lane: %v", err)
	}
	// next and last reorder within the lane only.
	other, err := d.control("lease-acquire", Object{"token": token(), "lane": "qa", "ownerPid": os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	last, _ := integer(leases[3]["id"])
	if _, err = d.control("next", Object{"id": last}); err != nil {
		t.Fatal(err)
	}
	if view := requireLeaseState(t, d, leases[3], "queued"); view["position"] != 1 {
		t.Fatalf("next did not move the lease to the front of its lane: %v", view)
	}
	first, _ := integer(leases[1]["id"])
	if _, err = d.control("last", Object{"id": first}); err != nil {
		t.Fatal(err)
	}
	if view := requireLeaseState(t, d, leases[1], "queued"); view["position"] != 4 {
		t.Fatalf("last did not move the lease to the back of its lane: %v", view)
	}
	held, _ := integer(leases[0]["id"])
	if _, err = d.control("lease-release", Object{"id": held, "ownerPid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err = d.tick(); err != nil {
		t.Fatal(err)
	}
	requireLeaseState(t, d, leases[3], "held")
	requireLeaseState(t, d, other, "queued")
	if held := heldInLane(t, d, "qa"); held != 1 {
		t.Fatalf("lane concurrency exceeded after release: %d held", held)
	}
}

func TestLeaseReorderIgnoresOtherLanes(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`, `{"name":"build","concurrency":1}`)})
	acquireTestLease(t, d, "qa", nil)
	acquireTestLease(t, d, "build", nil)
	queued := acquireTestLease(t, d, "qa", nil)
	busy := acquireTestLease(t, d, "build", nil)
	busyID, _ := integer(busy["id"])
	if _, err := d.control("priority", Object{"id": busyID, "priority": 500}); err != nil {
		t.Fatal(err)
	}
	id, _ := integer(queued["id"])
	result, err := d.control("next", Object{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	if priority, _ := integer(result["priority"]); priority != 1 {
		t.Fatalf("next used another lane's priority: %v", result)
	}
}

func TestLeasePriorityOrderingAcrossLanes(t *testing.T) {
	d, _ := laneDaemon(t, 1, map[string]string{"lanes": laneManifest("lanes", `{"name":"blocker","concurrency":1}`, `{"name":"high","concurrency":1,"priority":50}`, `{"name":"low","concurrency":1,"priority":-10}`)})
	blocker := acquireTestLease(t, d, "blocker", nil)
	low := acquireTestLease(t, d, "low", nil)
	high := acquireTestLease(t, d, "high", nil)
	requireLeaseState(t, d, blocker, "held")
	requireLeaseState(t, d, low, "queued")
	requireLeaseState(t, d, high, "queued")
	id, _ := integer(blocker["id"])
	if _, err := d.control("cancel", Object{"id": id}); err != nil {
		t.Fatal(err)
	}
	if view := requireLeaseState(t, d, blocker, "released"); view["reason"] != "cancelled" {
		t.Fatalf("cancel did not release the held lease: %v", view)
	}
	if err := d.tick(); err != nil {
		t.Fatal(err)
	}
	requireLeaseState(t, d, high, "held")
	requireLeaseState(t, d, low, "queued")
}

func TestLeaseSharesLimit(t *testing.T) {
	d, _ := laneDaemon(t, 1, map[string]string{"lanes": laneManifest("lanes", `{"name":"shared","concurrency":2}`, `{"name":"also-shared","concurrency":2}`, `{"name":"device","concurrency":1,"sharesLimit":false}`)})
	shared := acquireTestLease(t, d, "shared", nil)
	requireLeaseState(t, d, shared, "held")
	// The shared lane has room, but the global limit of 1 is used.
	requireLeaseState(t, d, acquireTestLease(t, d, "shared", nil), "queued")
	requireLeaseState(t, d, acquireTestLease(t, d, "also-shared", nil), "queued")
	device := acquireTestLease(t, d, "device", nil)
	requireLeaseState(t, d, device, "held")
	requireLeaseState(t, d, acquireTestLease(t, d, "device", nil), "queued")
	snapshot, err := d.control("snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	lanes := obj(snapshot["lanes"])
	if obj(lanes["device"])["held"] != 1 || obj(lanes["shared"])["held"] != 1 || obj(lanes["shared"])["waiting"] != 1 || obj(lanes[defaultLaneName])["concurrency"] != 1 {
		t.Fatalf("lane usage: %v", lanes)
	}
}

func TestLeaseOwnerDeathReleasesHeldAndDropsQueued(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	owner := exec.Command("/bin/sleep", "60")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	held := acquireTestLease(t, d, "qa", Object{"ownerPid": owner.Process.Pid})
	queued := acquireTestLease(t, d, "qa", Object{"ownerPid": owner.Process.Pid})
	survivor := acquireTestLease(t, d, "qa", nil)
	if err := d.checkLeaseOwners(); err != nil {
		t.Fatal(err)
	}
	if view := requireLeaseState(t, d, held, "held"); view["ownerAlive"] != true || !jsonEqual(view["ownerPid"], owner.Process.Pid) {
		t.Fatalf("live owner not reported: %v", view)
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	if err := d.checkLeaseOwners(); err != nil {
		t.Fatal(err)
	}
	if view := requireLeaseState(t, d, held, "expired"); view["reason"] != "owner-exited" || view["ownerAlive"] != false {
		t.Fatalf("held lease of a dead owner: %v", view)
	}
	if view := requireLeaseState(t, d, queued, "dropped"); view["reason"] != "owner-exited" {
		t.Fatalf("queued lease of a dead owner: %v", view)
	}
	if err := d.tick(); err != nil {
		t.Fatal(err)
	}
	requireLeaseState(t, d, survivor, "held")
}

func TestLeaseOwnerIdentityRejectsReusedPID(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("lease owners use macOS process identity")
	}
	boot, err := BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	processes, err := leaseProcessSnapshot(boot)
	if err != nil {
		t.Fatal(err)
	}
	self := processes.Processes[os.Getpid()]
	owner := Object{"pid": os.Getpid(), "birth": self.Birth, "boot": boot}
	if !processes.alive(owner) {
		t.Fatal("the live owner was not recognised")
	}
	for name, changed := range map[string]Object{
		"different start time": {"pid": os.Getpid(), "birth": "Thu Jan  1 00:00:00 1970", "boot": boot},
		"different boot":       {"pid": os.Getpid(), "birth": self.Birth, "boot": "00000000-0000-0000-0000-000000000000"},
		"missing process":      {"pid": 1 << 30, "birth": self.Birth, "boot": boot},
	} {
		if processes.alive(changed) {
			t.Errorf("%s kept a lease alive", name)
		}
	}
	zombie := processes
	zombie.Processes = map[int]leaseProcess{os.Getpid(): {UID: self.UID, Birth: self.Birth, State: "Z"}}
	if zombie.alive(owner) {
		t.Error("a zombie owner kept a lease alive")
	}
	foreign := processes
	foreign.Processes = map[int]leaseProcess{os.Getpid(): {UID: self.UID + 1, Birth: self.Birth, State: "S"}}
	if foreign.alive(owner) {
		t.Error("another user's process kept a lease alive")
	}
}

func TestLeaseTTLExpiryAndRenew(t *testing.T) {
	d, clock := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1,"lease":{"ttlSeconds":120}}`)})
	held := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 60})
	queued := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 60})
	defaulted := acquireTestLease(t, d, "qa", Object{"ownerPid": os.Getpid()})
	if view := requireLeaseState(t, d, defaulted, "queued"); view["expiresAt"] == nil {
		t.Fatal("lane default TTL was not applied")
	}
	clock.advance(40 * time.Second)
	id, _ := integer(held["id"])
	renewed, err := d.control("lease-renew", Object{"id": id})
	if err != nil || renewed["renewedAt"] == nil {
		t.Fatalf("renew: %v %v", renewed, err)
	}
	clock.advance(30 * time.Second)
	if err = d.tick(); err != nil {
		t.Fatal(err)
	}
	requireLeaseState(t, d, held, "held")
	if view := requireLeaseState(t, d, queued, "expired"); view["reason"] != "ttl" {
		t.Fatalf("queued lease without renewal: %v", view)
	}
	clock.advance(31 * time.Second)
	if err = d.tick(); err != nil {
		t.Fatal(err)
	}
	if view := requireLeaseState(t, d, held, "expired"); view["reason"] != "ttl" {
		t.Fatalf("held lease after its TTL: %v", view)
	}
	if _, err = d.control("lease-renew", Object{"id": id}); !errors.Is(err, errQueueLeaseGone) {
		t.Fatalf("renewed an expired lease: %v", err)
	}
	requireLeaseState(t, d, defaulted, "held")
}

func TestLeaseReleaseRequiresOwnerOrForce(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":2}`)})
	owned := acquireTestLease(t, d, "qa", nil)
	id, _ := integer(owned["id"])
	for _, payload := range []Object{{"id": id}, {"id": id, "ownerPid": os.Getpid() + 1}, {"id": id, "force": false}} {
		if _, err := d.control("lease-release", payload); !errors.Is(err, errQueueLeaseOwner) {
			t.Fatalf("released without the owner or force: %v %v", payload, err)
		}
	}
	if _, err := d.control("lease-release", Object{"id": id, "ownerPid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	requireLeaseState(t, d, owned, "released")
	if _, err := d.control("lease-release", Object{"id": id, "ownerPid": os.Getpid()}); !errors.Is(err, errQueueLeaseGone) {
		t.Fatalf("released twice: %v", err)
	}
	forced := acquireTestLease(t, d, "qa", nil)
	forcedID, _ := integer(forced["id"])
	if _, err := d.control("lease-release", Object{"id": forcedID, "force": true}); err != nil {
		t.Fatal(err)
	}
	requireLeaseState(t, d, forced, "released")
	unowned := acquireTestLease(t, d, "qa", Object{"ttlSeconds": 600})
	unownedID, _ := integer(unowned["id"])
	if _, err := d.control("lease-release", Object{"id": unownedID}); err != nil {
		t.Fatalf("a TTL-only lease needed force: %v", err)
	}
}

func TestLeaseAcquireValidation(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	for name, payload := range map[string]Object{
		"default lane":    {"lane": defaultLaneName, "ownerPid": os.Getpid()},
		"unknown lane":    {"lane": "missing", "ownerPid": os.Getpid()},
		"no owner or ttl": {"lane": "qa"},
		"short ttl":       {"lane": "qa", "ttlSeconds": 59},
		"dead owner":      {"lane": "qa", "ownerPid": 1 << 30},
		"long label":      {"lane": "qa", "ownerPid": os.Getpid(), "label": strings.Repeat("x", 121)},
		"too much meta":   {"lane": "qa", "ownerPid": os.Getpid(), "meta": Object{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6", "g": "7", "h": "8", "i": "9"}},
		"newline in meta": {"lane": "qa", "ownerPid": os.Getpid(), "meta": Object{"thread": "a\nb"}},
		"bad meta key":    {"lane": "qa", "ownerPid": os.Getpid(), "meta": Object{"Thread": "a"}},
		"long meta value": {"lane": "qa", "ownerPid": os.Getpid(), "meta": Object{"thread": strings.Repeat("x", 513)}},
	} {
		payload["token"] = token()
		if _, err := d.control("lease-acquire", payload); err == nil {
			t.Errorf("%s: accepted %v", name, payload)
		}
	}
	lease := acquireTestLease(t, d, "qa", Object{"label": "QA session", "meta": Object{"worktree": "/tmp/example", "ticket": "WM-123"}})
	again, err := d.control("lease-acquire", Object{"token": "", "lane": "qa"})
	if err == nil {
		t.Fatalf("accepted an invalid token: %v", again)
	}
	view := requireLeaseState(t, d, lease, "held")
	if view["label"] != "QA session" || obj(view["meta"])["ticket"] != "WM-123" || view["queuedAt"] == nil || view["grantedAt"] == nil {
		t.Fatalf("lease fields: %v", view)
	}
	// Retrying with the same token returns the original lease.
	payload := Object{"token": token(), "lane": "qa", "ownerPid": os.Getpid()}
	first, err := d.control("lease-acquire", payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.control("lease-acquire", payload)
	if err != nil || second["id"] != first["id"] {
		t.Fatalf("token retry created a second lease: %v %v %v", first, second, err)
	}
}

func TestLeaseRemovedLaneKeepsWorkUntilReleased(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	held := acquireTestLease(t, d, "qa", nil)
	queued := acquireTestLease(t, d, "qa", nil)
	if err := os.RemoveAll(pluginRoot(d.g)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.control("plugins-reload", Object{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.control("lease-acquire", Object{"token": token(), "lane": "qa", "ownerPid": os.Getpid()}); !errors.Is(err, errQueueUnknownLane) {
		t.Fatalf("accepted work into a removed lane: %v", err)
	}
	snapshot, err := d.control("snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lane := obj(obj(snapshot["lanes"])["qa"]); lane["removed"] != true || lane["held"] != 1 || lane["concurrency"] != 1 {
		t.Fatalf("removed lane not retained: %v", lane)
	}
	id, _ := integer(held["id"])
	if _, err = d.control("lease-release", Object{"id": id, "ownerPid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err = d.tick(); err != nil {
		t.Fatal(err)
	}
	requireLeaseState(t, d, queued, "held")
	queuedID, _ := integer(queued["id"])
	if _, err = d.control("lease-release", Object{"id": queuedID, "ownerPid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.control("plugins-reload", Object{}); err != nil {
		t.Fatal(err)
	}
	if snapshot, err = d.control("snapshot", nil); err != nil || obj(snapshot["lanes"])["qa"] != nil {
		t.Fatalf("finished removed lane still listed: %v %v", snapshot["lanes"], err)
	}
}

func TestLeaseBlocksLiveHandoverAndKeepsDrainPending(t *testing.T) {
	d, _ := laneDaemon(t, 8, map[string]string{"qa": laneManifest("qa", `{"name":"qa","concurrency":1}`)})
	acquireTestLease(t, d, "qa", nil)
	if _, err := queueHandoverAttempts(d.store); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("live handover accepted a held lease: %v", err)
	}
	snapshot, err := d.store.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var drain *installDrainPendingError
	if err = installRequireDrained(obj(snapshot["tasks"])); !errors.As(err, &drain) {
		t.Fatalf("drain did not wait for the held lease: %v", err)
	}
}

func TestLaneCommandsRejectDaemonWithoutLanes(t *testing.T) {
	g := testGate(t)
	fakeQueue(t, g, testSnapshot(Object{}))
	var stdout, stderr bytes.Buffer
	if code := LaneMain(g, []string{"list", "--json"}, &stdout, &stderr); code != laneExitRefused || !strings.Contains(stdout.String(), "does not support lanes") {
		t.Fatalf("lane list on an older daemon: %d %q %q", code, stdout.String(), stderr.String())
	}
	var decoded Object
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || decoded["schemaVersion"] != float64(1) {
		t.Fatalf("JSON error shape: %q %v", stdout.String(), err)
	}
	stdout.Reset()
	if code := PluginsMain(g, []string{"reload"}, &stdout, &stderr); code != laneExitRefused {
		t.Fatalf("plugins reload on an older daemon: %d", code)
	}
	code, err := RunInLane(g, []string{"/usr/bin/true"}, "", "qa")
	if err != nil || code != laneExitUsage {
		t.Fatalf("run --lane on an older daemon: %d %v", code, err)
	}
	entries, _ := os.ReadDir(g.Jobs)
	if len(entries) != 0 {
		t.Fatal("a payload was written for a daemon without lanes")
	}
}

func TestLaneCommandUsageErrors(t *testing.T) {
	g := testGate(t)
	fakeQueue(t, g, Object{"tasks": Object{}, "groups": Object{"default": Object{"status": "Running", "parallel_tasks": 1}}, "lanes": Object{defaultLaneName: laneDefinition(defaultLane(1))}, "plugins": []Object{}})
	t.Setenv("AGENT_QUEUE_OWNER_PID", "")
	for _, args := range [][]string{{}, {"bogus"}, {"acquire"}, {"acquire", "qa"}, {"acquire", "qa", "--ttl", "10"}, {"acquire", "qa", "--owner-pid"}, {"acquire", "qa", "--owner-pid", "1", "--meta", "novalue"}, {"wait"}, {"wait", "x"}, {"status", "1", "--force"}, {"release", "-1"}, {"acquire", "missing", "--owner-pid", "2"}} {
		var stdout, stderr bytes.Buffer
		if code := LaneMain(g, args, &stdout, &stderr); code != laneExitUsage {
			t.Errorf("%v: exit %d, want 2 (%s%s)", args, code, stdout.String(), stderr.String())
		}
	}
	if code, err := RunInLane(g, []string{"/usr/bin/true"}, "", "missing"); err != nil || code != laneExitUsage {
		t.Fatalf("run --lane with an unknown lane: %d %v", code, err)
	}
	for _, args := range [][]string{{"run", "--lane"}, {"run", "--lane", "Bad Lane", "--", "true"}} {
		if code := Main(args); code != laneExitUsage {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	argv, label, lane, err := runLaneArguments([]string{"--lane", "qa", "--label=Check", "--", "sh", "--lane"}, true)
	if err != nil || lane != "qa" || label != "Check" || strings.Join(argv, " ") != "sh --lane" {
		t.Fatalf("run --lane arguments: %v %q %q %v", argv, label, lane, err)
	}
	if !commandHelp([]string{"run", "--lane", "qa", "--help"}) || commandHelp([]string{"run", "--lane", "--help", "sh"}) {
		t.Fatal("run --lane help detection")
	}
	if argv, _, err := runArguments([]string{"--lane", "qa"}); err != nil || strings.Join(argv, " ") != "--lane qa" {
		t.Fatalf("plain run arguments changed: %v %v", argv, err)
	}
}

func TestStatusWithoutLanesReportsDefaultLaneOnly(t *testing.T) {
	g := testGate(t)
	fakeQueue(t, g, testSnapshot(Object{"3": Object{"status": Object{"Queued": Object{}}, "priority": 0, "path": g.State}}))
	data, err := StatusData(g)
	if err != nil {
		t.Fatal(err)
	}
	lanes := data["lanes"].([]Object)
	if len(lanes) != 1 || lanes[0]["name"] != defaultLaneName || lanes[0]["waiting"] != 1 || lanes[0]["plugin"] != nil || len(data["plugins"].([]Object)) != 0 {
		t.Fatalf("lanes for a daemon without lanes: %v %v", lanes, data["plugins"])
	}
	job := data["jobs"].([]Object)[0]
	if job["lane"] != defaultLaneName || job["kind"] != "command" || job["queuePosition"] != 1 {
		t.Fatalf("default job fields: %v", job)
	}
	if _, ok := job["position"]; ok {
		t.Fatal("ordinary jobs gained a lease position")
	}
}
