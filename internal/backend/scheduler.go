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
	if !validHost(host) || !validPrediction(prediction) {
		return false, "Waiting for valid resource observations and predictions"
	}
	if len(active) >= limit {
		return false, "Waiting for the parallel job limit"
	}
	if !boolean(host["complete"]) {
		return false, "Collecting a complete resource interval"
	}
	if host["memoryPressure"] != "normal" {
		return false, "Waiting for memory pressure to recover"
	}
	if metric(host["swapOutBytesPerSecond"]) > 1024*1024 {
		return false, "Waiting for active swapping to settle"
	}
	reserve := max(float64(GiB), math.Trunc(metric(host["totalMemoryBytes"])*.15))
	if metric(prediction["memoryBytes"]) > metric(host["totalMemoryBytes"])-reserve {
		return false, "Predicted memory exceeds this machine's budget; reduce workers or inspect Start now"
	}
	if metric(host["diskFreeBytes"]) < max(float64(GiB), reserve) {
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
	memoryGrowth, cpuGrowth := 0.0, 0.0
	cpuBudget := max(1.0, metric(host["cpuCores"])*.8)
	for _, job := range active {
		pred, observed := obj(job["prediction"]), obj(job["observed"])
		if !validPrediction(pred) || !schedulingNumber(observed["memoryBytes"], false) || !schedulingNumber(observed["cpuCores"], false) {
			return false, "Waiting for valid active resource reservations"
		}
		memoryGrowth += max(0.0, metric(pred["memoryBytes"])-metric(observed["memoryBytes"]))
		cpuGrowth += max(0.0, min(cpuBudget, metric(pred["cpuCores"]))-metric(observed["cpuCores"]))
	}
	if metric(prediction["memoryBytes"])+reserve+memoryGrowth > metric(host["availableMemoryBytes"]) {
		return false, "Waiting for predicted peak memory headroom"
	}
	if len(active) == 0 && metric(host["cpuBusyCores"]) > metric(host["cpuCores"])*.6 {
		return false, "Waiting for background CPU activity to settle"
	}
	if metric(prediction["cpuCores"]) > cpuBudget {
		return false, "Predicted CPU demand exceeds this machine's budget; reduce workers or inspect Start now"
	}
	if metric(host["cpuBusyCores"])+cpuGrowth+metric(prediction["cpuCores"]) > cpuBudget {
		return false, "Waiting for predicted CPU headroom"
	}
	return true, "Predicted resource demand fits the current headroom"
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
			if !boolean(state["available"]) || !boolean(state["admissionReady"]) || memberCount >= limit || group["status"] != "Running" {
				return false, nil
			}
			for _, raw := range obj(queue["tasks"]) {
				task := obj(raw)
				status, _, e := TaskStatus(task)
				if e != nil {
					return false, e
				}
				if (status == "Running" || status == "Paused") && CommandToken(task) == "" && (task["group"] == nil || task["group"] == "default") {
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
			allowed, _ := ResourceDecision(obj(state["host"]), obj(grant["prediction"]), active, limit)
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
		return memberCount < limit, e
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
