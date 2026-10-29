package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"
)

type Controller struct {
	gate           *Gate
	sampler        *ResourceSampler
	store          *ProfileStore
	boot           string
	recoveryUntil  time.Time
	runs           map[string]Object
	finalized      map[string]bool
	groups         map[string]int
	learningScopes map[string]string
}

func NewController(g *Gate, sampler *ResourceSampler, store *ProfileStore) (*Controller, error) {
	boot, e := BootIdentity()
	if e != nil {
		return nil, e
	}
	if sampler == nil {
		sampler = NewResourceSampler(".", nil)
	}
	if store == nil {
		store, e = NewProfileStore(g.State, false)
		if e != nil {
			return nil, e
		}
	}
	return &Controller{gate: g, sampler: sampler, store: store, boot: boot, runs: map[string]Object{}, finalized: map[string]bool{}, groups: map[string]int{}, learningScopes: map[string]string{}}, nil
}
func (c *Controller) SampleRun(id int, value string, marker, host, prediction Object) (Object, error) {
	run := c.runs[value]
	if run == nil {
		var e error
		run, e = ReadPrivate(filepath.Join(c.gate.State, "resource-runs", value+".json"))
		if os.IsNotExist(e) {
			observedSince := resourceNow()
			started := marker["executionStartedAt"]
			timely := schedulingNumber(started, false) && observedSince-metric(started) >= 0 && observedSince-metric(started) <= MaxSampleInterval
			start := observedSince
			if schedulingNumber(started, false) && metric(started) <= observedSince {
				start = metric(started)
			}
			run = Object{"started": start, "observedSince": observedSince, "boot": c.boot, "samples": 0, "attempts": 0, "complete": timely, "memoryBytes": 0, "peakMemoryBytes": 0, "cpuSeconds": 0.0, "cpuCores": 0.0, "peakCpuCores": 0.0, "durationSeconds": 0.0}
		} else if e != nil {
			return nil, e
		} else {
			run["complete"] = false
		}
		c.runs[value] = run
	}
	pgid, ok := integer(marker["pgid"])
	if !ok {
		return nil, fmt.Errorf("Invalid resource process group")
	}
	members := []Object{}
	for _, raw := range list(marker["members"]) {
		member := obj(raw)
		if member == nil {
			return nil, fmt.Errorf("Invalid resource ownership identity")
		}
		members = append(members, member)
	}
	measured, e := c.sampler.Group(pgid, members)
	if e != nil {
		run["complete"] = false
		run["latestComplete"] = false
	} else {
		run["latestComplete"] = boolean(measured["complete"])
		if !boolean(measured["complete"]) && (metric(run["attempts"]) > 0 || !boolean(measured["baseline"])) {
			run["complete"] = false
		}
		run["memoryBytes"] = measured["memoryBytes"]
		run["peakMemoryBytes"] = int64(max(metric(run["peakMemoryBytes"]), metric(measured["memoryBytes"])))
		run["cpuSeconds"] = max(metric(run["cpuSeconds"]), metric(measured["cpuSeconds"]))
		run["cpuCores"] = measured["cpuCores"]
		run["peakCpuCores"] = max(metric(run["peakCpuCores"]), metric(measured["cpuCores"]))
		if boolean(measured["complete"]) {
			n, _ := integer(run["samples"])
			run["samples"] = n + 1
		}
	}
	n, _ := integer(run["attempts"])
	run["attempts"] = n + 1
	run["durationSeconds"] = max(0.0, resourceNow()-metric(run["started"]))
	run["sampledAt"] = resourceNow()
	run["task_id"] = id
	run["machine"] = host["machine"]
	if e = WritePrivate(filepath.Join(c.gate.State, "resource-runs", value+".json"), run); e != nil {
		return nil, e
	}
	c.groups[value] = pgid
	observed := resourceObservation(run)
	observed["complete"] = boolean(run["latestComplete"])
	adjusted := Object{}
	for k, v := range prediction {
		adjusted[k] = v
	}
	adjusted["memoryBytes"] = int64(max(metric(prediction["memoryBytes"]), float64(int64(metric(run["peakMemoryBytes"])*1.2))))
	adjusted["cpuCores"] = max(metric(prediction["cpuCores"]), metric(run["peakCpuCores"]))
	return Object{"prediction": adjusted, "observed": observed, "decision": nil}, nil
}
func resourceObservation(run Object) Object {
	result := Object{}
	for _, key := range []string{"memoryBytes", "peakMemoryBytes", "cpuSeconds", "cpuCores", "durationSeconds", "samples", "complete"} {
		result[key] = run[key]
	}
	return result
}

type schedulerCandidate struct {
	id              int
	token           string
	task, resources Object
	lease           bool
}
type schedulerAudit struct {
	token, mode string
	prediction  Object
	allowed     bool
	reason      string
}

func taskState(task Object) (string, Object, error) { return TaskStatus(task) }
func (c *Controller) Tick() (Object, error) {
	g := c.gate
	host, e := c.sampler.Host()
	if e == nil && !validHost(host) {
		e = fmt.Errorf("Invalid host resource observations")
	}
	if e != nil {
		// A zero-reservation lease never waits for resource samples.
		leases, leaseError := c.admitLeasesWithoutHost()
		if leaseError != nil {
			return nil, errors.Join(e, leaseError)
		}
		return Object{"leases": leases}, e
	}
	if host["memoryPressure"] != "normal" || metric(host["swapOutBytesPerSecond"]) > 1024*1024 {
		c.recoveryUntil = time.Now().Add(time.Duration(resourceRecoverySeconds * float64(time.Second)))
	}
	jobs := Object{}
	active := []Object{}
	// activeLanes parallels active so the limit counts only lanes sharing it.
	activeLanes := []string{}
	addActive := func(entry Object, lane string) {
		active = append(active, entry)
		activeLanes = append(activeLanes, lane)
	}
	candidates := []schedulerCandidate{}
	audits := []schedulerAudit{}
	sampledActive := map[string]bool{}
	requests := []profileRequest{}
	collected := []struct {
		token     string
		resources Object
	}{}
	snapshot, e := g.Snapshot()
	if e != nil {
		return nil, e
	}
	if _, _, e = schedulingGroup(snapshot); e != nil {
		return nil, e
	}
	tasks := obj(snapshot["tasks"])
	terminal := []schedulerCandidate{}
	for key, raw := range tasks {
		task := obj(raw)
		state, _, e := taskState(task)
		if e != nil {
			return nil, e
		}
		if state == "Done" && !taskIsLease(task) {
			id, e := schedulerTaskID(key)
			if e != nil {
				return nil, e
			}
			terminal = append(terminal, schedulerCandidate{id: id, token: CommandToken(task), task: task})
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		_, a, _ := taskState(terminal[i].task)
		_, b, _ := taskState(terminal[j].task)
		if text(a["end"]) != text(b["end"]) {
			return text(a["end"]) > text(b["end"])
		}
		return terminal[i].id > terminal[j].id
	})
	terminal = terminal[:min(30, len(terminal))]
	terminalTokens := map[string]bool{}
	for _, entry := range terminal {
		terminalTokens[entry.token] = true
	}
	for key, raw := range tasks {
		task := obj(raw)
		value := CommandToken(task)
		state, details, e := taskState(task)
		if e != nil {
			return nil, e
		}
		// Leases have no worker to sample and no workload to learn; their lane
		// reservations are accounted for with the current snapshot below.
		if taskIsLease(task) {
			continue
		}
		if state == "Done" && !terminalTokens[value] && c.runs[value] == nil {
			continue
		}
		if value == "" {
			if (state == "Running" || state == "Paused") && (task["group"] == nil || task["group"] == "default") {
				addActive(unknownReservation(), taskLane(task))
			}
			continue
		}
		id, e := schedulerTaskID(key)
		if e != nil {
			return nil, e
		}
		workload, e := EnsureSchedulingIdentity(g, value, id)
		if e != nil {
			return nil, e
		}
		if workload == nil {
			if state == "Running" || state == "Paused" {
				addActive(unknownReservation(), taskLane(task))
			}
			continue
		}
		description, e := ReadPrivate(filepath.Join(g.State, "metadata", value+".json"))
		if os.IsNotExist(e) {
			description = Object{}
		} else if e != nil {
			return nil, e
		}
		if c.learningScopes == nil {
			c.learningScopes = map[string]string{}
		}
		safe := profileSimilarityDescription(description, c.learningScopes)
		// Predictions are read together after every completion has been recorded.
		prediction := Object{}
		resources := Object{"prediction": prediction, "observed": nil, "decision": nil}
		receipt, e := g.Receipt(id)
		if e != nil {
			return nil, e
		}
		if receipt != nil && receipt["pgid"] != nil {
			unlock, e := LockPrivate(g, "")
			if e != nil {
				return nil, e
			}
			marker, e := ValidateMarker(g, id)
			unlock()
			if e != nil {
				return nil, e
			}
			if marker != nil {
				resources, e = c.SampleRun(id, value, marker, host, prediction)
				if e != nil {
					return nil, e
				}
				addActive(resources, taskLane(task))
				sampledActive[value] = true
			}
		} else if state == "Running" || state == "Paused" {
			candidates = append(candidates, schedulerCandidate{id: id, token: value, task: task, resources: resources})
		}
		if state == "Done" && receipt != nil {
			if c.runs[value] != nil {
				c.runs[value]["complete"] = false
			}
		} else if state == "Done" {
			run := c.runs[value]
			observedHere := run != nil
			if run == nil {
				run, e = ReadPrivate(filepath.Join(g.State, "resource-runs", value+".json"))
				if e != nil && !os.IsNotExist(e) {
					return nil, e
				}
			}
			if run != nil {
				if !observedHere {
					run["complete"] = false
				}
				ended, endOK := resourceTimestamp(details["end"])
				observedAt := metric(run["sampledAt"])
				if run["sampledAt"] == nil {
					observedAt = metric(run["started"]) + metric(run["durationSeconds"])
				}
				if !endOK || ended-observedAt < 0 || ended-observedAt > MaxSampleInterval || ended > resourceNow() {
					run["complete"] = false
				}
				if endOK && metric(run["started"]) <= ended && ended <= resourceNow() {
					run["durationSeconds"] = max(metric(run["durationSeconds"]), ended-metric(run["started"]))
				}
				metrics := Object{"memoryBytes": run["peakMemoryBytes"], "cpuSeconds": run["cpuSeconds"], "cpuCores": run["peakCpuCores"], "durationSeconds": run["durationSeconds"], "sampledAt": observedAt}
				if !c.finalized[value] {
					code, codeError := ResultCode(details["result"])
					var codePtr *int
					if codeError == nil {
						codePtr = &code
					}
					complete := boolean(run["complete"]) && metric(run["samples"]) >= 2 && run["boot"] == c.boot
					if e = c.store.Record(value, workload, text(run["machine"]), metrics, complete, codePtr); e != nil {
						return nil, e
					}
				}
				resources["observed"] = resourceObservation(run)
			}
			c.finalized[value] = true
		} else if state == "Queued" || state == "Stashed" {
			candidates = append(candidates, schedulerCandidate{id: id, token: value, task: task, resources: resources})
		}
		requests = append(requests, profileRequest{workload, safe})
		collected = append(collected, struct {
			token     string
			resources Object
		}{value, resources})
		jobs[value] = resources
	}
	collection, e := c.store.collectProfiles(requests, text(host["machine"]))
	if e != nil {
		return nil, e
	}
	defer collection.Close()
	learningRevision := collection.revision
	for i, entry := range collected {
		prediction := collection.predictions[i]
		if sampledActive[entry.token] {
			run := c.runs[entry.token]
			prediction["memoryBytes"] = int64(max(metric(prediction["memoryBytes"]), float64(int64(metric(run["peakMemoryBytes"])*1.2))))
			prediction["cpuCores"] = max(metric(prediction["cpuCores"]), metric(run["peakCpuCores"]))
		}
		entry.resources["prediction"] = prediction
		entry.resources["learning"] = collection.matches[i]
	}
	configuration, e := SchedulingSettings(g)
	if e != nil {
		return nil, e
	}
	reason := "Resource predictions are ready"
	if configuration["mode"] == "fixed" {
		reason = "Collecting workload history"
	}
	unlockScheduling, e := LockPrivate(g, "scheduling.lock")
	if e != nil {
		return nil, e
	}
	schedulingLocked := true
	defer func() {
		if schedulingLocked {
			unlockScheduling()
		}
	}()
	unlockLease, e := LockPrivate(g, "")
	if e != nil {
		return nil, e
	}
	locked := true
	defer func() {
		if locked {
			unlockLease()
		}
	}()
	configuration, e = SchedulingSettings(g)
	if e != nil {
		return nil, e
	}
	admissions, e := SchedulingLedger(g, configuration)
	if e != nil {
		return nil, e
	}
	previousStatus, e := ReadPrivate(filepath.Join(g.State, "resource-status.json"))
	if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	if previous, ok := schedulingInteger(previousStatus["learningRevision"]); !ok || previous != learningRevision {
		// Automatic observation can join a family before this tick acquires the
		// scheduling locks. Retire old pending predictions before publishing the
		// new revision, just as explicit corrections retire pending grants. A
		// missing revision after startup or collection failure is stale as well.
		if e = retainActiveGrants(g, admissions); e != nil {
			return nil, e
		}
	}
	current, e := g.Snapshot()
	if e != nil {
		return nil, e
	}
	hostAge := resourceNow() - metric(host["sampledAt"])
	hostFresh := hostAge >= 0 && hostAge <= resourceFreshSeconds
	admissionReady := hostFresh && boolean(host["complete"]) && host["memoryPressure"] == "normal" && metric(host["swapOutBytesPerSecond"]) <= 1024*1024 && !time.Now().Before(c.recoveryUntil)
	pass := &admissionPass{current: current, tasks: tasks, host: host, hostFresh: hostFresh, admissionReady: admissionReady, active: active, activeLanes: activeLanes, measured: sampledActive, candidates: candidates, grants: obj(admissions["grants"])}
	if e = c.admit(pass); e != nil {
		return nil, e
	}
	candidates, selected, leaseDecisions := pass.candidates, pass.selected, pass.leases
	grants, currentTasks := pass.grants, obj(current["tasks"])
	if configuration["mode"] == "shadow" || configuration["mode"] == "adaptive" {
		for _, entry := range candidates {
			if !entry.lease {
				decision := obj(entry.resources["decision"])
				audits = append(audits, schedulerAudit{entry.token, text(configuration["mode"]), obj(entry.resources["prediction"]), boolean(decision["allowed"]), text(decision["reason"])})
			}
		}
	}
	// Serialize snapshot validation with the durable grant write. This read lock
	// is released before queue RPC so a slow daemon cannot block history writers.
	valid, releaseProfiles, e := collection.Validate()
	if e != nil {
		return nil, e
	}
	defer releaseProfiles()
	if !valid {
		releaseProfiles()
		if e = retainActiveGrants(g, admissions); e != nil {
			return nil, e
		}
		if e = WritePrivate(filepath.Join(g.State, "resource-admissions.json"), admissions); e != nil {
			return nil, e
		}
		value := c.status(configuration, host, Object{}, learningRevision, false, false, "Learning changed during collection; waiting for fresh predictions")
		if e = WritePrivate(filepath.Join(g.State, "resource-status.json"), value); e != nil {
			return nil, e
		}
		return value, nil
	}
	available := boolean(host["complete"]) && resourceNow()-metric(host["sampledAt"]) >= 0 && resourceNow()-metric(host["sampledAt"]) <= resourceFreshSeconds
	if !available {
		reason = "Collecting a complete resource interval"
	}
	value := c.status(configuration, host, jobs, learningRevision, available, admissionReady, reason)
	value["leases"] = leaseDecisions
	if e = WritePrivate(filepath.Join(g.State, "resource-status.json"), value); e != nil {
		return nil, e
	}
	if configuration["mode"] == "adaptive" && selected >= 0 && available && !candidates[selected].lease {
		entry := candidates[selected]
		grants[entry.token] = Object{"task_id": entry.id, "manual": false, "expires": resourceNow() + resourceGrantSeconds, "prediction": entry.resources["prediction"]}
	}
	if e = WritePrivate(filepath.Join(g.State, "resource-admissions.json"), admissions); e != nil {
		return nil, e
	}
	releaseProfiles()
	if configuration["mode"] == "adaptive" {
		for id, raw := range currentTasks {
			task := obj(raw)
			state, _, e := taskState(task)
			if e != nil {
				return nil, e
			}
			if CommandToken(task) != "" && state == "Queued" {
				if _, e = g.QueueControl("hold", Object{"id": mustSchedulerID(id)}); e != nil {
					return nil, e
				}
			}
		}
		// Admitted leases already met their own sample requirements: a reserved
		// lease needs a fresh sample and a zero-reservation lease needs none.
		if selected >= 0 && (available || candidates[selected].lease) {
			entry := candidates[selected]
			state, _, e := taskState(obj(currentTasks[fmt.Sprint(entry.id)]))
			if e != nil {
				return nil, e
			}
			if state == "Queued" || state == "Stashed" {
				// The daemon recounts the lane; a stale view is refused, not fatal.
				if _, e = g.QueueControl("start", Object{"id": entry.id}); e != nil && !errors.Is(e, errQueueLaneFull) {
					return nil, e
				}
			}
		}
	}
	unlockLease()
	locked = false
	unlockScheduling()
	schedulingLocked = false
	for _, audit := range audits {
		if e = c.store.RecordDecision(audit.token, audit.mode, audit.prediction, audit.allowed, audit.reason, metric(host["sampledAt"])); e != nil {
			return nil, e
		}
	}
	retained := map[string]bool{}
	for _, raw := range tasks {
		task := obj(raw)
		state, _, e := taskState(task)
		if e != nil {
			return nil, e
		}
		if state != "Done" {
			retained[CommandToken(task)] = true
		}
	}
	receipts, e := filepath.Glob(filepath.Join(g.Receipts, "*.json"))
	if e != nil {
		return nil, e
	}
	for _, path := range receipts {
		receipt, e := ReadPrivate(path)
		if e != nil {
			return nil, e
		}
		retained[text(receipt["token"])] = true
	}
	for value := range c.runs {
		if !retained[value] && !terminalTokens[value] {
			delete(c.runs, value)
		}
	}
	for value, pgid := range c.groups {
		if !retained[value] {
			c.sampler.ForgetGroup(pgid)
			delete(c.groups, value)
		}
	}
	for value := range c.finalized {
		if !terminalTokens[value] {
			delete(c.finalized, value)
		}
	}
	for _, directory := range []string{"resource-runs", "workloads"} {
		paths, e := filepath.Glob(filepath.Join(g.State, directory, "*.json"))
		if e != nil {
			return nil, e
		}
		type dated struct {
			path     string
			modified time.Time
		}
		files := []dated{}
		for _, path := range paths {
			info, e := os.Stat(path)
			if e != nil {
				return nil, e
			}
			files = append(files, dated{path, info.ModTime()})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
		for _, file := range files[:max(0, len(files)-1000)] {
			value := filepath.Base(file.path)
			value = value[:len(value)-5]
			if !retained[value] && !terminalTokens[value] {
				if e = os.Remove(file.path); e != nil {
					return nil, e
				}
			}
		}
	}
	return value, nil
}

// admissionPass is one admission decision over a daemon snapshot taken under
// the scheduling locks. active holds work already using capacity, activeLanes
// parallels it, and measured names the tokens whose workers are in active.
type admissionPass struct {
	current, tasks, host      Object
	hostFresh, admissionReady bool
	active                    []Object
	activeLanes               []string
	measured                  map[string]bool
	candidates                []schedulerCandidate
	grants                    Object
	selected                  int
	leases                    Object
}

// admit orders the candidates, adds queued leases, and records a decision for
// each. It selects the first admitted candidate, or -1. Grants it no longer
// counts are removed from pass.grants; persisting them is the caller's choice.
func (c *Controller) admit(pass *admissionPass) error {
	g, current, host, hostFresh := c.gate, pass.current, pass.host, pass.hostFresh
	group, limit, e := schedulingGroup(current)
	if e != nil {
		return e
	}
	active, activeLanes, candidates := pass.active, pass.activeLanes, pass.candidates
	addActive := func(entry Object, lane string) {
		active = append(active, entry)
		activeLanes = append(activeLanes, lane)
	}
	admissionReady := pass.admissionReady
	slotExists := false
	var manifest Object
	if _, e = os.Stat(g.Slot); e == nil {
		slotExists = true
		manifest, e = SharedManifest(g)
		if e != nil {
			return e
		}
		if manifest != nil {
			members, e := os.ReadDir(filepath.Join(g.Slot, "agent-heavy-jobs"))
			if e != nil {
				return e
			}
			for _, path := range members {
				member, e := ReadPrivate(filepath.Join(g.Slot, "agent-heavy-jobs", path.Name()))
				if e != nil {
					return e
				}
				if !pass.measured[text(member["token"])] {
					id, _ := integer(member["task_id"])
					addActive(unknownReservation(), taskLane(obj(pass.tasks[strconv.Itoa(id)])))
				}
			}
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	grants := pass.grants
	currentTasks := obj(current["tasks"])
	for value, raw := range grants {
		grant := obj(raw)
		id, _ := integer(grant["task_id"])
		task := obj(currentTasks[fmt.Sprint(id)])
		receipt, e := g.Receipt(id)
		if e != nil {
			return e
		}
		pending := receipt == nil || receipt["pgid"] == nil
		state, _, e := taskState(task)
		if e != nil {
			return e
		}
		if state == "Missing" || state == "Done" || (pending && (metric(grant["expires"]) < resourceNow() || (!boolean(grant["manual"]) && !admissionReady))) {
			delete(grants, value)
		} else if pending {
			prediction := obj(grant["prediction"])
			if len(prediction) == 0 {
				prediction = Object{"memoryBytes": GiB, "cpuCores": 2.0}
			}
			addActive(Object{"prediction": prediction, "observed": Object{}}, taskLane(task))
		}
	}
	heldSharing, held, e := heldLeases(current)
	if e != nil {
		return e
	}
	occupied := heldSharing
	for _, lane := range activeLanes {
		if snapshotSharesLimit(current, lane) {
			occupied++
		}
	}
	// The daemon's lane counts include started work; a grant still counts
	// against its lane until the daemon has started it.
	pendingByLane := map[string]int{}
	for _, raw := range grants {
		id, _ := integer(obj(raw)["task_id"])
		task := obj(currentTasks[fmt.Sprint(id)])
		if state, _, e := taskState(task); e == nil && (state == "Queued" || state == "Stashed") {
			pendingByLane[taskLane(task)]++
		}
	}
	for key, raw := range currentTasks {
		task := obj(raw)
		if state, _, e := taskState(task); e != nil {
			return e
		} else if taskIsLease(task) && state == "Queued" {
			id, e := schedulerTaskID(key)
			if e != nil {
				return e
			}
			candidates = append(candidates, schedulerCandidate{id: id, task: task, lease: true})
		}
	}
	lanePriority := func(task Object) int {
		priority, _ := integer(snapshotLane(current, taskLane(task))["priority"])
		return priority
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if lanePriority(a.task) != lanePriority(b.task) {
			return lanePriority(a.task) > lanePriority(b.task)
		}
		if metric(a.task["priority"]) != metric(b.task["priority"]) {
			return metric(a.task["priority"]) > metric(b.task["priority"])
		}
		return a.id < b.id
	})
	// A lane at its own concurrency cannot start anything, so it never holds
	// back other lanes. Limit-sharing work keeps today's head-of-line fairness
	// among itself; a lane outside the limit only queues behind its own work.
	// A command the daemon already started holds its lane slot and is counted
	// in the lane's running total, so only its admission grant is pending.
	laneFull := func(task Object) bool {
		name := taskLane(task)
		lane := snapshotLane(current, name)
		if name == defaultLaneName || lane == nil {
			return false
		}
		if state, _, e := taskState(task); e == nil && !taskIsLease(task) && (state == "Running" || state == "Paused") {
			return false
		}
		concurrency, _ := integer(lane["concurrency"])
		running, _ := integer(lane["running"])
		holding, _ := integer(lane["held"])
		return running+holding+pendingByLane[name] >= concurrency
	}
	head := func(index int) int {
		sharing := snapshotSharesLimit(current, taskLane(candidates[index].task))
		for i := range candidates[:index] {
			if laneFull(candidates[i].task) {
				continue
			}
			if sharing && snapshotSharesLimit(current, taskLane(candidates[i].task)) || !sharing && taskLane(candidates[i].task) == taskLane(candidates[index].task) {
				return i
			}
		}
		return index
	}
	leaseDecisions := Object{}
	selected := -1
	for index, entry := range candidates {
		lane := taskLane(entry.task)
		laneOccupied := -1
		if snapshotSharesLimit(current, lane) {
			laneOccupied = occupied
		}
		var allowed bool
		var why string
		if entry.lease {
			memory, cpu := laneReservation(current, lane)
			allowed, why = leaseDecision(host, memory, cpu, active, held)
			if laneOccupied >= limit {
				allowed, why = false, "Waiting for the parallel job limit"
			}
			if memory+cpu > 0 && !hostFresh {
				allowed, why = false, "Waiting for a fresh host resource sample"
			}
			if memory+cpu > 0 && time.Now().Before(c.recoveryUntil) {
				allowed, why = false, "Waiting for a stable recovery interval"
			}
		} else {
			allowed, why = resourceDecision(host, obj(entry.resources["prediction"]), active, laneOccupied, limit, held)
			if !hostFresh {
				allowed, why = false, "Waiting for a fresh host resource sample"
			}
			if time.Now().Before(c.recoveryUntil) {
				allowed, why = false, "Waiting for a stable recovery interval"
			}
		}
		if group["status"] != "Running" {
			allowed, why = false, "Queue is paused"
		}
		if slotExists && manifest == nil && !entry.lease {
			allowed, why = false, "Waiting for the external shared validation owner"
		}
		first := head(index)
		if index > first && (lanePriority(candidates[first].task) > lanePriority(entry.task) || lanePriority(candidates[first].task) == lanePriority(entry.task) && metric(candidates[first].task["priority"]) > metric(entry.task["priority"])) {
			allowed, why = false, "Waiting behind a higher-priority job"
		}
		if index > first {
			created, ok := resourceTimestamp(candidates[first].task["created_at"])
			age := resourceStarvationSeconds
			if ok {
				age = resourceNow() - created
			}
			if age >= resourceStarvationSeconds {
				allowed, why = false, "Reserving the next opportunity for an older job"
			}
		}
		if laneFull(entry.task) {
			allowed, why = false, "Waiting for lane capacity"
		}
		if !entry.lease && grants[entry.token] != nil {
			allowed, why = false, "Admission is starting"
		}
		state, _, e := taskState(obj(currentTasks[fmt.Sprint(entry.id)]))
		if e != nil {
			return e
		}
		if !containsString([]string{"Queued", "Stashed", "Running"}, state) || entry.lease && state != "Queued" {
			allowed, why = false, "Job changed while resources were sampled"
		}
		if entry.lease {
			leaseDecisions[strconv.Itoa(entry.id)] = Object{"decision": Object{"allowed": allowed, "reason": why}}
		} else {
			entry.resources["decision"] = Object{"allowed": allowed, "reason": why}
		}
		if selected < 0 && allowed {
			selected = index
		}
	}
	pass.candidates, pass.selected, pass.leases = candidates, selected, leaseDecisions
	return nil
}

// admitLeasesWithoutHost runs when the host sample failed. Commands and
// reserved leases keep waiting for a fresh sample; a zero-reservation lease is
// admitted by the same lane, limit and ordering rules from the daemon snapshot.
// Running work counts at the unknown reservation, and the admission ledger is
// read but never rewritten.
func (c *Controller) admitLeasesWithoutHost() (Object, error) {
	g := c.gate
	if configuration, e := SchedulingSettings(g); e != nil || configuration["mode"] != "adaptive" {
		return nil, e
	}
	unlockScheduling, e := LockPrivate(g, "scheduling.lock")
	if e != nil {
		return nil, e
	}
	defer unlockScheduling()
	unlockLease, e := LockPrivate(g, "")
	if e != nil {
		return nil, e
	}
	defer unlockLease()
	configuration, e := SchedulingSettings(g)
	if e != nil || configuration["mode"] != "adaptive" {
		return nil, e
	}
	admissions, e := SchedulingLedger(g, configuration)
	if e != nil {
		return nil, e
	}
	current, e := g.Snapshot()
	if e != nil {
		return nil, e
	}
	pass := &admissionPass{current: current, tasks: obj(current["tasks"]), measured: map[string]bool{}, grants: obj(admissions["grants"])}
	for key, raw := range pass.tasks {
		task := obj(raw)
		state, _, e := taskState(task)
		if e != nil {
			return nil, e
		}
		if taskIsLease(task) || !containsString([]string{"Queued", "Stashed", "Running", "Paused"}, state) {
			continue
		}
		started := state == "Running" || state == "Paused"
		value := CommandToken(task)
		if value == "" {
			if started && (task["group"] == nil || task["group"] == "default") {
				pass.active = append(pass.active, unknownReservation())
				pass.activeLanes = append(pass.activeLanes, taskLane(task))
			}
			continue
		}
		id, e := schedulerTaskID(key)
		if e != nil {
			return nil, e
		}
		receipt, e := g.Receipt(id)
		if e != nil {
			return nil, e
		}
		if receipt != nil && receipt["pgid"] != nil {
			pass.active = append(pass.active, unknownReservation())
			pass.activeLanes = append(pass.activeLanes, taskLane(task))
			pass.measured[value] = true
			continue
		}
		pass.candidates = append(pass.candidates, schedulerCandidate{id: id, token: value, task: task, resources: Object{"prediction": Object{}, "observed": nil, "decision": nil}})
	}
	if e = c.admit(pass); e != nil {
		return nil, e
	}
	if pass.selected >= 0 && pass.candidates[pass.selected].lease {
		entry := pass.candidates[pass.selected]
		if state, _, e := taskState(snapshotTaskByID(current, entry.id)); e != nil {
			return nil, e
		} else if state == "Queued" {
			if _, e = g.QueueControl("start", Object{"id": entry.id}); e != nil && !errors.Is(e, errQueueLaneFull) {
				return nil, e
			}
		}
	}
	return pass.leases, nil
}
func unknownReservation() Object {
	return Object{"prediction": Object{"memoryBytes": GiB, "cpuCores": 2.0}, "observed": Object{}}
}
func (c *Controller) status(configuration, host, jobs Object, revision int, available, ready bool, reason string) Object {
	return Object{"mode": configuration["mode"], "generation": configuration["generation"], "boot": c.boot, "sampledAt": resourceNow(), "updatedAt": time.Now().UTC().Format(time.RFC3339Nano), "learningRevision": revision, "available": available, "admissionReady": ready, "reason": reason, "host": host, "jobs": jobs}
}
func (c *Controller) CensorRuns() error {
	for token, run := range c.runs {
		run["complete"] = false
		run["latestComplete"] = false
		if e := WritePrivate(filepath.Join(c.gate.State, "resource-runs", token+".json"), run); e != nil {
			return e
		}
	}
	return nil
}
func (c *Controller) Close()                  { c.store.Close() }
func ServeScheduler(g *Gate, once bool) error { return serveScheduler(g, nil, once) }
func serveScheduler(g *Gate, sampler *ResourceSampler, once bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// The installer retains scheduling.lock until helper shutdown completes.
	// Interrupt this controller's waits without weakening that shared lock.
	controlledGate := *g
	controlledGate.controlContext = ctx
	g = &controlledGate
	if e := g.Prepare(); e != nil {
		return e
	}
	unlock, e := LockPrivate(g, "resource-controller.lock")
	if e != nil {
		return e
	}
	defer unlock()
	controller, e := NewController(g, sampler, nil)
	if e != nil {
		return e
	}
	defer controller.Close()
	backfilled := false
	for {
		started := time.Now()
		state, tickError := controller.Tick()
		e = tickError
		regrouped := false
		// Publish this controller's revision support before the first migration.
		// The migration rotates pending grants; a fresh tick adopts its grouping.
		if e == nil && !backfilled {
			e = controller.store.backfillSimilarity(g)
			if e == nil {
				backfilled = true
				var revision int
				revision, e = controller.store.Revision()
				regrouped = metric(state["learningRevision"]) != float64(revision)
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if e != nil {
			if censorError := controller.CensorRuns(); censorError != nil {
				return censorError
			}
			configuration, configError := SchedulingSettings(g)
			if configError != nil {
				return configError
			}
			value := Object{"mode": configuration["mode"], "generation": configuration["generation"], "boot": controller.boot, "sampledAt": resourceNow(), "updatedAt": time.Now().UTC().Format(time.RFC3339Nano), "available": false, "reason": "Resource collection failed; admissions held", "host": nil, "jobs": Object{}}
			if leases := obj(state["leases"]); leases != nil {
				value["leases"] = leases
			}
			if writeError := WritePrivate(filepath.Join(g.State, "resource-status.json"), value); writeError != nil {
				return writeError
			}
			if once {
				return e
			}
		}
		if once && !regrouped {
			return nil
		}
		delay := 2*time.Second - time.Since(started)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
