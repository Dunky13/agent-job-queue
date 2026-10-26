package backend

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

type Controller struct {
	gate          *Gate
	sampler       *ResourceSampler
	store         *ProfileStore
	boot          string
	recoveryUntil time.Time
	runs          map[string]Object
	finalized     map[string]bool
	groups        map[string]int
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
	return &Controller{gate: g, sampler: sampler, store: store, boot: boot, runs: map[string]Object{}, finalized: map[string]bool{}, groups: map[string]int{}}, nil
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
	if e != nil {
		return nil, e
	}
	if !validHost(host) {
		return nil, fmt.Errorf("Invalid host resource observations")
	}
	if host["memoryPressure"] != "normal" || metric(host["swapOutBytesPerSecond"]) > 1024*1024 {
		c.recoveryUntil = time.Now().Add(time.Duration(resourceRecoverySeconds * float64(time.Second)))
	}
	jobs := Object{}
	active := []Object{}
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
	_, limit, e := schedulingGroup(snapshot)
	if e != nil {
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
		if state == "Done" {
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
		if state == "Done" && !terminalTokens[value] && c.runs[value] == nil {
			continue
		}
		if value == "" {
			if (state == "Running" || state == "Paused") && (task["group"] == nil || task["group"] == "default") {
				active = append(active, unknownReservation())
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
				active = append(active, unknownReservation())
			}
			continue
		}
		description, e := ReadPrivate(filepath.Join(g.State, "metadata", value+".json"))
		if os.IsNotExist(e) {
			description = Object{}
		} else if e != nil {
			return nil, e
		}
		safe := Object{}
		for _, key := range []string{"title", "project", "executable"} {
			maxLength := 80
			if key == "title" {
				maxLength = 120
			}
			label := DisplayText(text(description[key]), maxLength)
			if label == "" {
				safe[key] = nil
			} else {
				safe[key] = label
			}
		}
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
				active = append(active, resources)
				sampledActive[value] = true
			}
		} else if state == "Running" || state == "Paused" {
			candidates = append(candidates, schedulerCandidate{id, value, task, resources})
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
			candidates = append(candidates, schedulerCandidate{id, value, task, resources})
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
	current, e := g.Snapshot()
	if e != nil {
		return nil, e
	}
	group, limit, e := schedulingGroup(current)
	if e != nil {
		return nil, e
	}
	hostAge := resourceNow() - metric(host["sampledAt"])
	hostFresh := hostAge >= 0 && hostAge <= resourceFreshSeconds
	admissionReady := hostFresh && boolean(host["complete"]) && host["memoryPressure"] == "normal" && metric(host["swapOutBytesPerSecond"]) <= 1024*1024 && !time.Now().Before(c.recoveryUntil)
	slotExists := false
	var manifest Object
	if _, e = os.Stat(g.Slot); e == nil {
		slotExists = true
		manifest, e = SharedManifest(g)
		if e != nil {
			return nil, e
		}
		if manifest != nil {
			members, e := os.ReadDir(filepath.Join(g.Slot, "agent-heavy-jobs"))
			if e != nil {
				return nil, e
			}
			for _, path := range members {
				member, e := ReadPrivate(filepath.Join(g.Slot, "agent-heavy-jobs", path.Name()))
				if e != nil {
					return nil, e
				}
				if !sampledActive[text(member["token"])] {
					active = append(active, unknownReservation())
				}
			}
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	grants := obj(admissions["grants"])
	currentTasks := obj(current["tasks"])
	for value, raw := range grants {
		grant := obj(raw)
		id, _ := integer(grant["task_id"])
		task := obj(currentTasks[fmt.Sprint(id)])
		receipt, e := g.Receipt(id)
		if e != nil {
			return nil, e
		}
		pending := receipt == nil || receipt["pgid"] == nil
		state, _, e := taskState(task)
		if e != nil {
			return nil, e
		}
		if state == "Missing" || state == "Done" || (pending && (metric(grant["expires"]) < resourceNow() || (!boolean(grant["manual"]) && !admissionReady))) {
			delete(grants, value)
		} else if pending {
			prediction := obj(grant["prediction"])
			if len(prediction) == 0 {
				prediction = Object{"memoryBytes": GiB, "cpuCores": 2.0}
			}
			active = append(active, Object{"prediction": prediction, "observed": Object{}})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if metric(a.task["priority"]) != metric(b.task["priority"]) {
			return metric(a.task["priority"]) > metric(b.task["priority"])
		}
		return a.id < b.id
	})
	selected := -1
	for index, entry := range candidates {
		allowed, why := ResourceDecision(host, obj(entry.resources["prediction"]), active, limit)
		if !hostFresh {
			allowed, why = false, "Waiting for a fresh host resource sample"
		}
		if time.Now().Before(c.recoveryUntil) {
			allowed, why = false, "Waiting for a stable recovery interval"
		}
		if group["status"] != "Running" {
			allowed, why = false, "Queue is paused"
		}
		if slotExists && manifest == nil {
			allowed, why = false, "Waiting for the external shared validation owner"
		}
		if index > 0 && metric(candidates[0].task["priority"]) > metric(entry.task["priority"]) {
			allowed, why = false, "Waiting behind a higher-priority job"
		}
		if index > 0 {
			created, ok := resourceTimestamp(candidates[0].task["created_at"])
			age := resourceStarvationSeconds
			if ok {
				age = resourceNow() - created
			}
			if age >= resourceStarvationSeconds {
				allowed, why = false, "Reserving the next opportunity for an older job"
			}
		}
		if grants[entry.token] != nil {
			allowed, why = false, "Admission is starting"
		}
		state, _, e := taskState(obj(currentTasks[fmt.Sprint(entry.id)]))
		if e != nil {
			return nil, e
		}
		if !containsString([]string{"Queued", "Stashed", "Running"}, state) {
			allowed, why = false, "Job changed while resources were sampled"
		}
		entry.resources["decision"] = Object{"allowed": allowed, "reason": why}
		if configuration["mode"] == "shadow" || configuration["mode"] == "adaptive" {
			audits = append(audits, schedulerAudit{entry.token, text(configuration["mode"]), obj(entry.resources["prediction"]), allowed, why})
		}
		if selected < 0 && allowed {
			selected = index
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
	if e = WritePrivate(filepath.Join(g.State, "resource-status.json"), value); e != nil {
		return nil, e
	}
	if configuration["mode"] == "adaptive" && selected >= 0 && available {
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
		if selected >= 0 && available {
			entry := candidates[selected]
			state, _, e := taskState(obj(currentTasks[fmt.Sprint(entry.id)]))
			if e != nil {
				return nil, e
			}
			if state == "Queued" || state == "Stashed" {
				if _, e = g.QueueControl("start", Object{"id": entry.id}); e != nil {
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
	for {
		started := time.Now()
		_, e = controller.Tick()
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
			if writeError := WritePrivate(filepath.Join(g.State, "resource-status.json"), value); writeError != nil {
				return writeError
			}
			if once {
				return e
			}
		}
		if once {
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
