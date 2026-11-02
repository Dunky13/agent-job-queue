package backend

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const resourceFreshSeconds = 10.0
const resourceGrantSeconds = 10.0
const resourceRecoverySeconds = 10.0
const resourceStarvationSeconds = 60.0

func schedulingNumber(v any, positive bool) bool {
	n, ok := number(v)
	return ok && n >= 0 && n < 9223372036854775808 && (!positive || n > 0)
}
func schedulingInteger(v any) (int, bool) {
	switch v.(type) {
	case float64, float32:
		return 0, false
	}
	return integer(v)
}
func validPrediction(v Object) bool {
	if v == nil {
		return false
	}
	memory, ok := schedulingInteger(v["memoryBytes"])
	samples, sok := schedulingInteger(v["samples"])
	if value, exists := v["familySamples"]; exists {
		if count, valid := schedulingInteger(value); !valid || count < 0 {
			return false
		}
	}
	return ok && memory > 0 && schedulingNumber(v["cpuCores"], true) && sok && samples >= 0 && containsString([]string{"variant", "family", "prior"}, text(v["source"])) && containsString([]string{"unknown", "low", "medium", "high"}, text(v["confidence"])) && (v["durationSeconds"] == nil || schedulingNumber(v["durationSeconds"], false))
}
func validHost(v Object) bool {
	if v == nil {
		return false
	}
	for _, key := range []string{"totalMemoryBytes", "availableMemoryBytes", "cpuCores", "swapUsedBytes", "diskFreeBytes"} {
		n, ok := schedulingInteger(v[key])
		if !ok || n < 0 {
			return false
		}
	}
	for _, key := range []string{"sampledAt", "cpuBusyCores", "swapInBytesPerSecond", "swapOutBytesPerSecond"} {
		if !schedulingNumber(v[key], false) {
			return false
		}
	}
	_, completeOK := v["complete"].(bool)
	return metric(v["totalMemoryBytes"]) > 0 && metric(v["availableMemoryBytes"]) <= metric(v["totalMemoryBytes"]) && metric(v["cpuCores"]) > 0 && metric(v["cpuBusyCores"]) <= metric(v["cpuCores"]) && completeOK && containsString([]string{"normal", "warning", "critical"}, text(v["memoryPressure"])) && text(v["machine"]) != ""
}
func SchedulingSettings(g *Gate) (Object, error) {
	v, e := ReadPrivate(filepath.Join(g.State, "scheduling.json"))
	if os.IsNotExist(e) {
		return Object{"mode": "fixed", "generation": "initial"}, nil
	}
	if e != nil {
		return nil, e
	}
	if len(v) != 2 || !containsString([]string{"fixed", "shadow", "adaptive"}, text(v["mode"])) || text(v["generation"]) == "" {
		return nil, fmt.Errorf("Invalid scheduling settings; new admissions are held")
	}
	return v, nil
}
func SchedulingIdentity(g *Gate, value string) (Object, error) {
	if !validToken(value) {
		return nil, fmt.Errorf("Invalid private job token")
	}
	v, e := ReadPrivate(filepath.Join(g.State, "workloads", value+".json"))
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if len(v) != 3 || !validProfileIdentity(v) {
		return nil, fmt.Errorf("Invalid workload identity; new admissions are held")
	}
	return v, nil
}
func EnsureSchedulingIdentity(g *Gate, value string, id int) (Object, error) {
	v, e := SchedulingIdentity(g, value)
	if e != nil || v != nil {
		return v, e
	}
	path, e := g.PayloadPath(value)
	if e != nil {
		return nil, e
	}
	payload, e := ReadJobPayload(path)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	taskID, ok := integer(payload["task_id"])
	if text(payload["token"]) != value || !ok || taskID != id {
		return nil, fmt.Errorf("Workload payload does not match its acknowledged job")
	}
	argv, e := stringsFrom(payload["argv"])
	if e != nil {
		return nil, e
	}
	environment := map[string]string{}
	rawEnvironment := obj(payload["env"])
	if raw, present := payload["env"]; present && (raw == nil || rawEnvironment == nil) {
		return nil, fmt.Errorf("Invalid workload environment")
	}
	for k, raw := range rawEnvironment {
		v, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("Invalid workload environment")
		}
		environment[k] = v
	}
	v, e = IdentifyWorkload(argv, text(payload["cwd"]), environment)
	if e != nil {
		return nil, e
	}
	if e = WritePrivate(filepath.Join(g.State, "workloads", value+".json"), v); e != nil {
		return nil, e
	}
	return v, nil
}
func SchedulingView(g *Gate) (Object, error) {
	configuration, e := SchedulingSettings(g)
	if e != nil {
		return nil, e
	}
	unavailable := Object{"mode": configuration["mode"], "available": false, "reason": "Resource controller is unavailable; adaptive jobs remain waiting", "updatedAt": nil, "host": nil, "jobs": Object{}}
	value, e := ReadPrivate(filepath.Join(g.State, "resource-status.json"))
	if os.IsNotExist(e) {
		return unavailable, nil
	}
	if e != nil {
		return nil, e
	}
	if !schedulingNumber(value["sampledAt"], false) {
		return nil, fmt.Errorf("Invalid resource heartbeat timestamp")
	}
	now := resourceNow()
	age := now - metric(value["sampledAt"])
	host := obj(value["host"])
	if value["host"] != nil && !validHost(host) {
		return nil, fmt.Errorf("Invalid host resource observations")
	}
	hostAge := 0.0
	if host != nil {
		hostAge = now - metric(host["sampledAt"])
	}
	boot, e := BootIdentity()
	if e != nil {
		return nil, e
	}
	if value["generation"] != configuration["generation"] || value["boot"] != boot || age < 0 || age > resourceFreshSeconds || hostAge < 0 || hostAge > resourceFreshSeconds {
		return unavailable, nil
	}
	_, availableOK := value["available"].(bool)
	if obj(value["jobs"]) == nil || !availableOK || (boolean(value["available"]) && host == nil) {
		return nil, fmt.Errorf("Invalid resource controller status")
	}
	store, e := NewProfileStore(g.State, true)
	if e != nil {
		return nil, e
	}
	revision, e := store.Revision()
	if e != nil {
		return nil, e
	}
	applied := value["learningRevision"]
	appliedRevision, appliedOK := schedulingInteger(applied)
	if applied != nil && (!appliedOK || appliedRevision < 0) {
		return nil, fmt.Errorf("Invalid applied learning revision")
	}
	if revision != 0 && (!appliedOK || appliedRevision != revision) {
		value["mode"] = configuration["mode"]
		value["available"] = false
		value["admissionReady"] = false
		value["jobs"] = Object{}
		value["reason"] = "Saved learning is waiting for a fresh controller pass; automatic admissions are held"
		return value, nil
	}
	value["mode"] = configuration["mode"]
	return value, nil
}
func SchedulingLedger(g *Gate, configuration Object) (Object, error) {
	boot, e := BootIdentity()
	if e != nil {
		return nil, e
	}
	empty := Object{"boot": boot, "generation": configuration["generation"], "grants": Object{}}
	value, e := ReadPrivate(filepath.Join(g.State, "resource-admissions.json"))
	if os.IsNotExist(e) {
		return empty, nil
	}
	if e != nil {
		return nil, e
	}
	if value["boot"] != boot || value["generation"] != configuration["generation"] {
		return empty, nil
	}
	grants := obj(value["grants"])
	if grants == nil {
		return nil, fmt.Errorf("Invalid resource admission ledger")
	}
	for token, raw := range grants {
		grant := obj(raw)
		task, ok := schedulingInteger(grant["task_id"])
		_, manualOK := grant["manual"].(bool)
		if !validToken(token) || grant == nil || !ok || task < 0 || !manualOK || !schedulingNumber(grant["expires"], false) || obj(grant["prediction"]) == nil || (!boolean(grant["manual"]) && !validPrediction(obj(grant["prediction"]))) {
			return nil, fmt.Errorf("Invalid resource admission grant")
		}
	}
	return value, nil
}
func ResourceDecision(host, prediction Object, active []Object, limit int) (bool, string) {
	return resourceDecision(host, prediction, active, len(active), limit, leaseReservations{})
}

// leaseReservations totals the declared reservations of held leases. Leases
// have no measured workers, so these stand in for their resource use.
type leaseReservations struct{ memoryBytes, cpuCores float64 }

// hostHeadroom is the headroom computation shared by job and lease admission:
// the host budgets and the growth active work may still claim.
type hostHeadroom struct {
	host                          Object
	reserve, cpuBudget            float64
	memoryGrowth, cpuGrowth       float64
	heldMemoryBytes, heldCPUCores float64
}

// hostSettled reports why the host cannot take new work right now, or "".
func hostSettled(host Object) string {
	if !boolean(host["complete"]) {
		return "Collecting a complete resource interval"
	}
	if host["memoryPressure"] != "normal" {
		return "Waiting for memory pressure to recover"
	}
	if metric(host["swapOutBytesPerSecond"]) > 1024*1024 {
		return "Waiting for active swapping to settle"
	}
	return ""
}

func newHostHeadroom(host Object, held leaseReservations) hostHeadroom {
	return hostHeadroom{host: host, reserve: max(float64(GiB), math.Trunc(metric(host["totalMemoryBytes"])*.15)), cpuBudget: max(1.0, metric(host["cpuCores"])*.8), heldMemoryBytes: held.memoryBytes, heldCPUCores: held.cpuCores}
}

// reserveGrowth charges each active job's predicted growth beyond its current
// measurement. It fails when an active reservation is not valid.
func (h *hostHeadroom) reserveGrowth(active []Object) bool {
	for _, job := range active {
		pred, observed := obj(job["prediction"]), obj(job["observed"])
		if !validPrediction(pred) || !schedulingNumber(observed["memoryBytes"], false) || !schedulingNumber(observed["cpuCores"], false) {
			return false
		}
		h.memoryGrowth += max(0.0, metric(pred["memoryBytes"])-metric(observed["memoryBytes"]))
		h.cpuGrowth += max(0.0, min(h.cpuBudget, metric(pred["cpuCores"]))-metric(observed["cpuCores"]))
	}
	return true
}

func (h hostHeadroom) exceedsMemoryBudget(memoryBytes float64) bool {
	return memoryBytes > metric(h.host["totalMemoryBytes"])-h.reserve
}

func (h hostHeadroom) memoryFits(memoryBytes float64) bool {
	return memoryBytes+h.reserve+h.memoryGrowth+h.heldMemoryBytes <= metric(h.host["availableMemoryBytes"])
}

func (h hostHeadroom) cpuFits(cpuCores float64) bool {
	return metric(h.host["cpuBusyCores"])+h.cpuGrowth+h.heldCPUCores+cpuCores <= h.cpuBudget
}

// resourceDecision is ResourceDecision with lane accounting: occupied counts
// only work that shares the global limit (negative when the candidate's lane
// does not share it), and held lease reservations reduce headroom.
func resourceDecision(host, prediction Object, active []Object, occupied, limit int, held leaseReservations) (bool, string) {
	if !validHost(host) || !validPrediction(prediction) {
		return false, "Waiting for valid resource observations and predictions"
	}
	if occupied >= 0 && occupied >= limit {
		return false, "Waiting for the parallel job limit"
	}
	if reason := hostSettled(host); reason != "" {
		return false, reason
	}
	headroom := newHostHeadroom(host, held)
	if headroom.exceedsMemoryBudget(metric(prediction["memoryBytes"])) {
		return false, "Predicted memory exceeds this machine's budget; reduce workers or inspect Start now"
	}
	if metric(host["diskFreeBytes"]) < max(float64(GiB), headroom.reserve) {
		return false, "Waiting for free disk space"
	}
	if len(active) > 0 {
		if metric(prediction["samples"]) < 3 {
			return false, "Learning this unfamiliar workload before overlapping it"
		}
		for _, job := range active {
			if metric(obj(job["prediction"])["samples"]) < 3 {
				return false, "Learning this unfamiliar workload before overlapping it"
			}
		}
	}
	for _, job := range active {
		if !boolean(obj(job["observed"])["complete"]) {
			return false, "Waiting for complete measurements of active jobs"
		}
	}
	if !headroom.reserveGrowth(active) {
		return false, "Waiting for valid active resource reservations"
	}
	if !headroom.memoryFits(metric(prediction["memoryBytes"])) {
		return false, "Waiting for predicted peak memory headroom"
	}
	if len(active) == 0 && metric(host["cpuBusyCores"]) > metric(host["cpuCores"])*.6 {
		return false, "Waiting for background CPU activity to settle"
	}
	if metric(prediction["cpuCores"]) > headroom.cpuBudget {
		return false, "Predicted CPU demand exceeds this machine's budget; reduce workers or inspect Start now"
	}
	if !headroom.cpuFits(metric(prediction["cpuCores"])) {
		return false, "Waiting for predicted CPU headroom"
	}
	return true, "Predicted resource demand fits the current headroom"
}

// leaseDecision admits a lease when its lane reservation fits the headroom.
// A lease is never an unfamiliar workload, and a zero reservation always fits.
func leaseDecision(host Object, memoryBytes, cpuCores float64, active []Object, held leaseReservations) (bool, string) {
	if memoryBytes == 0 && cpuCores == 0 {
		return true, "Lane has capacity"
	}
	if !validHost(host) {
		return false, "Waiting for valid resource observations"
	}
	if reason := hostSettled(host); reason != "" {
		return false, reason
	}
	headroom := newHostHeadroom(host, held)
	if headroom.exceedsMemoryBudget(memoryBytes) || cpuCores > headroom.cpuBudget {
		return false, "Lane reservation exceeds this machine's budget"
	}
	if !headroom.reserveGrowth(active) {
		return false, "Waiting for valid active resource reservations"
	}
	if !headroom.memoryFits(memoryBytes) {
		return false, "Waiting for memory headroom for the lane reservation"
	}
	if !headroom.cpuFits(cpuCores) {
		return false, "Waiting for CPU headroom for the lane reservation"
	}
	return true, "Lane reservation fits the current headroom"
}

// snapshotLane reads the lane registry the daemon publishes in its snapshot.
// An older daemon publishes none; callers then treat work as default-lane.
func snapshotLane(snapshot Object, name string) Object {
	return obj(obj(snapshot["lanes"])[name])
}

// snapshotSharesLimit is fail-closed: an unknown lane counts against the
// global limit.
func snapshotSharesLimit(snapshot Object, name string) bool {
	lane := snapshotLane(snapshot, name)
	return lane == nil || lane["sharesLimit"] != false
}

func laneReservation(snapshot Object, name string) (float64, float64) {
	reservation := obj(snapshotLane(snapshot, name)["reservation"])
	memory, _ := integer(reservation["memoryMiB"])
	cpu, _ := integer(reservation["cpuPercent"])
	return float64(memory) * 1024 * 1024, float64(cpu) / 100
}

// heldLeases returns how many held leases share the global limit and the total
// reservation of all held leases, which applies whether or not they share it.
func heldLeases(snapshot Object) (int, leaseReservations, error) {
	sharing := 0
	held := leaseReservations{}
	for _, raw := range obj(snapshot["tasks"]) {
		task := obj(raw)
		if !taskIsLease(task) {
			continue
		}
		state, _, err := TaskStatus(task)
		if err != nil {
			return 0, held, err
		}
		if state != "Running" {
			continue
		}
		lane := taskLane(task)
		if snapshotSharesLimit(snapshot, lane) {
			sharing++
		}
		memory, cpu := laneReservation(snapshot, lane)
		held.memoryBytes += memory
		held.cpuCores += cpu
	}
	return sharing, held, nil
}

func snapshotTaskByID(snapshot Object, id int) Object {
	return obj(obj(snapshot["tasks"])[strconv.Itoa(id)])
}

func schedulingGroup(snapshot Object) (Object, int, error) {
	group := obj(obj(snapshot["groups"])["default"])
	limit, ok := schedulingInteger(group["parallel_tasks"])
	if !ok || limit < 1 || limit > MaxParallel {
		return nil, 0, fmt.Errorf("Invalid queue parallel job limit")
	}
	return group, limit, nil
}
func SchedulingPermit(g *Gate, marker Object, memberCount int) (bool, error) {
	return schedulingPermit(g, marker, func(Object) int { return memberCount })
}

// schedulingPermit lets the worker recheck its admission. sharingMembers counts
// slot members whose lanes share the global limit in the given snapshot.
func schedulingPermit(g *Gate, marker Object, sharingMembers func(Object) int) (bool, error) {
	configuration, e := SchedulingSettings(g)
	if e != nil {
		return false, e
	}
	if configuration["mode"] != "adaptive" && !boolean(marker["resourceManaged"]) {
		return true, nil
	}
	admissions, e := SchedulingLedger(g, configuration)
	if e != nil {
		return false, e
	}
	grant := obj(obj(admissions["grants"])[text(marker["token"])])
	grantID, grantOK := integer(grant["task_id"])
	markerID, markerOK := integer(marker["task_id"])
	if grant != nil && grantOK && markerOK && grantID == markerID && resourceNow() <= metric(grant["expires"]) {
		if boolean(grant["manual"]) {
			return true, nil
		}
		if configuration["mode"] == "adaptive" {
			state, e := SchedulingView(g)
			if e != nil {
				return false, e
			}
			queue, e := g.Snapshot()
			if e != nil {
				return false, e
			}
			group, limit, e := schedulingGroup(queue)
			if e != nil {
				return false, e
			}
			sharing := snapshotSharesLimit(queue, taskLane(snapshotTaskByID(queue, markerID)))
			heldSharing, held, e := heldLeases(queue)
			if e != nil {
				return false, e
			}
			occupied := -1
			if sharing {
				occupied = sharingMembers(queue) + heldSharing
			}
			if !boolean(state["available"]) || !boolean(state["admissionReady"]) || sharing && occupied >= limit || group["status"] != "Running" {
				return false, nil
			}
			for _, raw := range obj(queue["tasks"]) {
				task := obj(raw)
				status, _, e := TaskStatus(task)
				if e != nil {
					return false, e
				}
				if (status == "Running" || status == "Paused") && CommandToken(task) == "" && !taskIsLease(task) && (task["group"] == nil || task["group"] == "default") {
					return false, nil
				}
			}
			active := []Object{}
			if _, e := os.Stat(g.Slot); e == nil {
				members, e := os.ReadDir(filepath.Join(g.Slot, "agent-heavy-jobs"))
				if e != nil {
					return false, e
				}
				for _, path := range members {
					member, e := ReadPrivate(filepath.Join(g.Slot, "agent-heavy-jobs", path.Name()))
					if e != nil {
						return false, e
					}
					measured := obj(obj(state["jobs"])[text(member["token"])])
					if measured == nil || obj(measured["observed"]) == nil {
						return false, nil
					}
					active = append(active, measured)
				}
			} else if !os.IsNotExist(e) {
				return false, e
			}
			allowed, _ := resourceDecision(obj(state["host"]), obj(grant["prediction"]), active, occupied, limit, held)
			now := resourceNow()
			return allowed && now <= metric(grant["expires"]) && now-metric(state["sampledAt"]) >= 0 && now-metric(state["sampledAt"]) <= resourceFreshSeconds && now-metric(obj(state["host"])["sampledAt"]) >= 0 && now-metric(obj(state["host"])["sampledAt"]) <= resourceFreshSeconds, nil
		}
	}
	if configuration["mode"] != "adaptive" {
		snapshot, e := g.Snapshot()
		if e != nil {
			return false, e
		}
		_, limit, e := schedulingGroup(snapshot)
		if e != nil {
			return false, e
		}
		// The daemon already enforced this lane's concurrency when it
		// dispatched the attempt; only limit-sharing lanes recheck the limit.
		if !snapshotSharesLimit(snapshot, taskLane(snapshotTaskByID(snapshot, markerID))) {
			return true, nil
		}
		return sharingMembers(snapshot) < limit, nil
	}
	return false, nil
}
func retainActiveGrants(g *Gate, admissions Object) error {
	grants := obj(admissions["grants"])
	for token, raw := range grants {
		grant := obj(raw)
		if boolean(grant["manual"]) {
			continue
		}
		id, _ := integer(grant["task_id"])
		receipt, e := g.Receipt(id)
		if e != nil {
			return e
		}
		if receipt["pgid"] == nil {
			delete(grants, token)
		}
	}
	return nil
}
func SetSchedulingMode(g *Gate, mode string) error {
	if !containsString([]string{"fixed", "shadow", "adaptive"}, mode) {
		return fmt.Errorf("Scheduling mode must be fixed, shadow, or adaptive")
	}
	if e := g.Prepare(); e != nil {
		return e
	}
	unlock, e := LockPrivate(g, "scheduling.lock")
	if e != nil {
		return e
	}
	defer unlock()
	previous, e := SchedulingSettings(g)
	if e != nil {
		return e
	}
	if previous["mode"] == mode {
		return nil
	}
	if mode == "adaptive" {
		state, e := SchedulingView(g)
		if e != nil {
			return e
		}
		if !boolean(state["available"]) {
			return fmt.Errorf("Start the resource controller and wait for fresh telemetry before enabling adaptive mode")
		}
	}
	lease, e := LockPrivate(g, "")
	if e != nil {
		return e
	}
	admissions, e := SchedulingLedger(g, previous)
	if e == nil {
		configuration := Object{"mode": mode, "generation": token()}
		admissions["generation"] = configuration["generation"]
		e = retainActiveGrants(g, admissions)
		if e == nil {
			e = WritePrivate(filepath.Join(g.State, "resource-admissions.json"), admissions)
		}
		if e == nil {
			e = WritePrivate(filepath.Join(g.State, "scheduling.json"), configuration)
		}
	}
	lease()
	if e != nil {
		return e
	}
	snapshot, e := g.Snapshot()
	if e != nil {
		return e
	}
	changed := false
	for id, raw := range obj(snapshot["tasks"]) {
		task := obj(raw)
		if CommandToken(task) == "" {
			continue
		}
		status, _, e := TaskStatus(task)
		if e != nil {
			return e
		}
		if mode == "adaptive" && status == "Queued" {
			_, e = g.QueueControl("hold", Object{"id": mustSchedulerID(id)})
			if e != nil {
				return e
			}
			changed = true
		} else if mode != "adaptive" && status == "Stashed" {
			if _, e = g.QueueControl("enqueue", Object{"id": mustSchedulerID(id)}); e != nil {
				return e
			}
			changed = true
		}
	}
	if changed {
		return nil
	}
	return nil
}
func ManualGrant(g *Gate, id int, value string) error {
	if id < 0 || !validToken(value) {
		return fmt.Errorf("Invalid manual resource admission")
	}
	unlock, e := LockPrivate(g, "")
	if e != nil {
		return e
	}
	defer unlock()
	configuration, e := SchedulingSettings(g)
	if e != nil {
		return e
	}
	admissions, e := SchedulingLedger(g, configuration)
	if e != nil {
		return e
	}
	obj(admissions["grants"])[value] = Object{"task_id": id, "manual": true, "expires": resourceNow() + 30, "prediction": Object{}}
	return WritePrivate(filepath.Join(g.State, "resource-admissions.json"), admissions)
}

// revokeManualGrant withdraws a Start now grant the daemon refused, so it
// cannot admit the job later outside its lane's order.
func revokeManualGrant(g *Gate, id int, value string) error {
	unlock, e := LockPrivate(g, "")
	if e != nil {
		return e
	}
	defer unlock()
	configuration, e := SchedulingSettings(g)
	if e != nil {
		return e
	}
	admissions, e := SchedulingLedger(g, configuration)
	if e != nil {
		return e
	}
	grants := obj(admissions["grants"])
	grant := obj(grants[value])
	if grantID, ok := integer(grant["task_id"]); !ok || grantID != id || !boolean(grant["manual"]) {
		return nil
	}
	delete(grants, value)
	return WritePrivate(filepath.Join(g.State, "resource-admissions.json"), admissions)
}
func schedulerTaskID(key string) (int, error) {
	id, e := strconv.Atoi(key)
	if e != nil || id < 0 {
		return 0, fmt.Errorf("Invalid queue task identity")
	}
	return id, nil
}
func resourceTimestamp(value any) (float64, bool) {
	parsed, e := time.Parse(time.RFC3339Nano, text(value))
	if e != nil {
		return 0, false
	}
	return float64(parsed.UnixNano()) / 1e9, true
}

func mustSchedulerID(value string) int { id, _ := strconv.Atoi(value); return id }
