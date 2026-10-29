package backend

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var errQueueLaneFull = errors.New("Lane is full")
var errQueueUnknownLane = errors.New("Unknown lane")
var errQueueLeaseGone = errors.New("Lease is no longer active")
var errQueueLeaseOwner = errors.New("Lease belongs to another owner")

const leaseOwnerCheckInterval = time.Second

func taskIsLease(task Object) bool { return text(task["kind"]) == "lease" }
func taskKind(task Object) string {
	if taskIsLease(task) {
		return "lease"
	}
	return "command"
}
func taskLane(task Object) string {
	if lane := text(task["lane"]); lane != "" {
		return lane
	}
	return defaultLaneName
}

type laneCount struct{ running, held, waiting int }

func (c *laneCount) occupied() int { return c.running + c.held }

func defaultLane(limit int) queueLane {
	return queueLane{Name: defaultLaneName, Label: "Default", Concurrency: limit, SharesLimit: true}
}

// lane resolves a lane for scheduling. Work in a lane that disappeared while
// the daemon was stopped keeps a conservative definition until it finishes.
func (d *queueDaemon) lane(name string, limit int) (queueLane, bool) {
	if name == defaultLaneName {
		return defaultLane(limit), true
	}
	if lane, ok := d.plugins.lanes[name]; ok {
		return lane, true
	}
	if lane, ok := d.retired[name]; ok {
		return lane, false
	}
	return queueLane{Name: name, Label: name, Concurrency: 1, SharesLimit: true, Removed: true}, false
}

// laneAdmissible is the scheduling rule shared by dispatch and status: a lane's
// own concurrency, then the global limit for limit-sharing lanes.
func laneAdmissible(lane queueLane, count *laneCount, sharing, limit int) bool {
	if lane.Name != defaultLaneName && count.occupied() >= lane.Concurrency {
		return false
	}
	return !lane.SharesLimit || sharing < limit
}

func laneUsage(jobs []queueJob) map[string]*laneCount {
	usage := map[string]*laneCount{}
	for _, j := range jobs {
		lane := taskLane(j.task)
		if usage[lane] == nil {
			usage[lane] = &laneCount{}
		}
		switch {
		case taskIsLease(j.task) && j.phase == "running":
			usage[lane].held++
		case !taskIsLease(j.task) && (j.phase == "starting" || j.phase == "running" || j.phase == "outcome"):
			usage[lane].running++
		case j.phase == "queued":
			if name, _, err := TaskStatus(j.task); err == nil && (name == "Queued" || name == "Stashed") {
				usage[lane].waiting++
			}
		}
	}
	return usage
}

// laneRoom recounts durable state immediately before a transition, so every
// entry point (dispatch, Start now, controller admission, acquire) enforces the
// lane's concurrency even when its caller's view was stale.
func (d *queueDaemon) laneRoom(name string) error {
	if name == defaultLaneName {
		return nil
	}
	jobs, err := d.store.unfinishedJobs()
	if err != nil {
		return err
	}
	lane, _ := d.lane(name, MaxParallel)
	count := laneUsage(jobs)[name]
	if count != nil && count.occupied() >= lane.Concurrency {
		return fmt.Errorf("%w (%s: %d of %d in use); Start now cannot exceed a lane's concurrency", errQueueLaneFull, name, count.occupied(), lane.Concurrency)
	}
	return nil
}

func (d *queueDaemon) reloadPlugins() error {
	next := loadPluginRegistry(d.g)
	jobs, err := d.store.unfinishedJobs()
	if err != nil {
		return err
	}
	usage := laneUsage(jobs)
	// Validate every transition before publishing any of the new registry.
	// Existing work retains its slots and plugin owner after a refused reload.
	for name, lane := range next.lanes {
		count := usage[name]
		if count == nil || count.occupied()+count.waiting == 0 {
			continue
		}
		previous, _ := d.lane(name, MaxParallel)
		if previous.Removed || previous.Plugin != lane.Plugin {
			return fmt.Errorf("Lane %q still has unfinished work owned by its previous plugin; drain it before reusing its name", name)
		}
		if count.occupied() > lane.Concurrency {
			return fmt.Errorf("Lane %q has %d occupied slots; release or finish work before lowering concurrency to %d", name, count.occupied(), lane.Concurrency)
		}
	}
	previous := d.plugins
	d.plugins = next
	if d.retired == nil {
		d.retired = map[string]queueLane{}
	}
	for name, lane := range previous.lanes {
		if _, ok := d.plugins.lanes[name]; !ok {
			lane.Removed = true
			d.retired[name] = lane
		}
	}
	for name := range d.plugins.lanes {
		delete(d.retired, name)
	}
	d.pruneRetiredLanes()
	return nil
}

func (d *queueDaemon) pruneRetiredLanes() {
	if len(d.retired) == 0 {
		return
	}
	jobs, err := d.store.unfinishedJobs()
	if err != nil {
		return
	}
	usage := laneUsage(jobs)
	for name := range d.retired {
		if count := usage[name]; count == nil || count.occupied()+count.waiting == 0 {
			delete(d.retired, name)
		}
	}
}

func (d *queueDaemon) laneOwner(name string) (queuePlugin, bool) {
	lane, ok := d.plugins.lanes[name]
	if !ok {
		lane, ok = d.retired[name]
	}
	if !ok || lane.Plugin == "" {
		return queuePlugin{}, false
	}
	for _, plugin := range d.plugins.plugins {
		if plugin.Name == lane.Plugin && plugin.Loaded {
			return plugin, true
		}
	}
	return queuePlugin{}, false
}

// snapshot adds the lane registry and live lane usage to the private task view.
func (d *queueDaemon) snapshot() (Object, error) {
	snapshot, err := d.store.snapshot()
	if err != nil {
		return nil, err
	}
	jobs, err := d.store.unfinishedJobs()
	if err != nil {
		return nil, err
	}
	limit, _ := integer(obj(obj(snapshot["groups"])["default"])["parallel_tasks"])
	usage := laneUsage(jobs)
	lanes := Object{}
	add := func(lane queueLane) {
		value := laneDefinition(lane)
		count := usage[lane.Name]
		if count == nil {
			count = &laneCount{}
		}
		value["running"], value["held"], value["waiting"] = count.running, count.held, count.waiting
		lanes[lane.Name] = value
	}
	add(defaultLane(limit))
	for _, lane := range d.plugins.lanes {
		add(lane)
	}
	for name := range usage {
		if lanes[name] == nil {
			lane, _ := d.lane(name, limit)
			add(lane)
		}
	}
	plugins := []Object{}
	for _, plugin := range d.plugins.plugins {
		value := pluginStatus(plugin)
		if dropped := d.hooks.dropped(plugin.Name); dropped > 0 {
			notice := fmt.Sprintf("%d hook events were dropped because %d hooks were already running", dropped, hookConcurrency)
			if value["warning"] != nil {
				notice = text(value["warning"]) + "; " + notice
			}
			value["warning"] = notice
		}
		plugins = append(plugins, value)
	}
	tasks := obj(snapshot["tasks"])
	for _, j := range jobs {
		lease := obj(obj(tasks[strconv.Itoa(j.id)])["lease"])
		if taskIsLease(j.task) && lease != nil && lease["owner"] != nil {
			alive, checked := d.ownerAlive[j.id]
			lease["ownerAlive"] = alive || !checked
		}
	}
	snapshot["lanes"] = lanes
	snapshot["plugins"] = plugins
	return snapshot, nil
}

func validLeaseLabel(value string) error {
	if len([]rune(value)) > 120 {
		return errors.New("Label must be at most 120 printable characters")
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return errors.New("Label must be at most 120 printable characters")
		}
	}
	return nil
}

func validLeaseMeta(raw any) (Object, error) {
	if raw == nil {
		return Object{}, nil
	}
	values := obj(raw)
	if values == nil || len(values) > 8 {
		return nil, errors.New("Lease metadata accepts at most 8 key=value pairs")
	}
	for key, value := range values {
		s, ok := value.(string)
		if !leaseMetaKeyPattern.MatchString(key) || !ok || len(s) > 512 || strings.ContainsAny(s, "\x00\n\r") {
			return nil, fmt.Errorf("Invalid lease metadata %q: keys match ^[a-z][a-z0-9_]{0,31}$ and values are at most 512 bytes without NUL or newline", key)
		}
	}
	return values, nil
}

type leaseProcess struct {
	UID          int
	Birth, State string
}
type leaseProcesses struct {
	Boot      string
	Processes map[int]leaseProcess
}

// leaseProcessSnapshot reads the complete process table once so that a missing
// PID can be told apart from a failed listing; failures keep leases held.
func leaseProcessSnapshot(boot string) (leaseProcesses, error) {
	result := leaseProcesses{Boot: boot, Processes: map[int]leaseProcess{}}
	output, err := identityCommand("/bin/ps", "-axo", "pid=,uid=,stat=,lstart=")
	if err != nil {
		return result, err
	}
	observer := false
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			return result, errors.New("Incomplete lease owner process listing")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		uid, uidErr := strconv.Atoi(fields[1])
		if pidErr != nil || uidErr != nil || pid <= 0 {
			return result, errors.New("Invalid lease owner process listing")
		}
		if _, duplicate := result.Processes[pid]; duplicate {
			return result, errors.New("Ambiguous lease owner process listing")
		}
		result.Processes[pid] = leaseProcess{UID: uid, Birth: strings.Join(fields[3:], " "), State: fields[2]}
		observer = observer || pid == os.Getpid()
	}
	if !observer {
		return result, errors.New("Incomplete lease owner process listing")
	}
	return result, nil
}

// alive treats a missing PID, a zombie, a different start time or boot as the
// recorded owner having exited: a reused PID must never keep a lease alive.
func (p leaseProcesses) alive(owner Object) bool {
	pid, ok := integer(owner["pid"])
	if !ok || text(owner["boot"]) != p.Boot {
		return false
	}
	state, present := p.Processes[pid]
	return present && state.UID == os.Getuid() && state.Birth == text(owner["birth"]) && !strings.HasPrefix(state.State, "Z")
}

func (d *queueDaemon) bootIdentity() (string, error) {
	if d.boot == "" {
		boot, err := BootIdentity()
		if err != nil {
			return "", err
		}
		d.boot = boot
	}
	return d.boot, nil
}

func (d *queueDaemon) leaseOwner(pid int) (Object, error) {
	if pid <= 1 || pid > math.MaxInt32 {
		return nil, errors.New("Owner PID must name a running process")
	}
	boot, err := d.bootIdentity()
	if err != nil {
		return nil, err
	}
	processes, err := leaseProcessSnapshot(boot)
	if err != nil {
		return nil, err
	}
	state, present := processes.Processes[pid]
	if !present || strings.HasPrefix(state.State, "Z") {
		return nil, fmt.Errorf("Owner process %d is not running", pid)
	}
	if state.UID != os.Getuid() {
		return nil, fmt.Errorf("Owner process %d belongs to another user", pid)
	}
	return Object{"pid": pid, "birth": state.Birth, "boot": boot}, nil
}

func (d *queueDaemon) markOwner(id int, alive bool) {
	if d.ownerAlive == nil {
		d.ownerAlive = map[int]bool{}
	}
	d.ownerAlive[id] = alive
}

func (d *queueDaemon) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}
func (d *queueDaemon) stamp() string { return d.clock().UTC().Format(time.RFC3339Nano) }

func (d *queueDaemon) acquireLease(p Object) (Object, error) {
	value := text(p["token"])
	if !validToken(value) {
		return nil, errors.New("Invalid lease token")
	}
	var existing int
	err := d.store.db.QueryRow("SELECT id FROM queue_jobs WHERE token=?", value).Scan(&existing)
	if err == nil {
		return d.leaseView(existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	name := text(p["lane"])
	lane, ok := d.plugins.lanes[name]
	if !ok {
		return nil, fmt.Errorf("%w %q; leases use a plugin lane", errQueueUnknownLane, name)
	}
	label := text(p["label"])
	if err = validLeaseLabel(label); err != nil {
		return nil, err
	}
	meta, err := validLeaseMeta(p["meta"])
	if err != nil {
		return nil, err
	}
	ttl := lane.TTLSeconds
	if raw, present := p["ttlSeconds"]; present && raw != nil {
		requested, valid := integer(raw)
		if !valid || validateLeaseTTL(requested, false) != nil {
			return nil, errors.New("TTL must be from 60 to 86400 seconds")
		}
		ttl = requested
	}
	lease := Object{"state": "queued", "ttlSeconds": ttl, "meta": meta}
	if raw, present := p["ownerPid"]; present && raw != nil {
		pid, valid := integer(raw)
		if !valid {
			return nil, errors.New("Owner PID must name a running process")
		}
		owner, err := d.leaseOwner(pid)
		if err != nil {
			return nil, err
		}
		lease["owner"] = owner
	} else if ttl == 0 {
		return nil, errors.New("A lease needs --owner-pid or a TTL")
	}
	settings, err := d.store.settings()
	if err != nil {
		return nil, err
	}
	next, validNext := integer(settings["next_id"])
	if !validNext || next < 0 || next == math.MaxInt {
		return nil, errors.New("Queue job ID space exhausted")
	}
	now := d.stamp()
	lease["queuedAt"] = now
	if ttl > 0 {
		lease["expiresAt"] = d.clock().Add(time.Duration(ttl) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	task := Object{"kind": "lease", "lane": name, "label": label, "priority": 0, "created_at": now, "status": Object{"Queued": Object{}}, "lease": lease}
	data, err := queueJSON(task)
	if err != nil {
		return nil, err
	}
	tx, err := d.store.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(?,?,?,'queued')", next, value, data); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("UPDATE queue_settings SET next_id=? WHERE id=1", next+1); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if lease["owner"] != nil {
		d.markOwner(next, true)
	}
	d.emitTask("lease.queued", next, task, nil)
	// Grant immediately when the lane has room, so --no-wait can report held.
	if err = d.tick(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-heavy daemon:", err)
	}
	return d.leaseView(next)
}

func (d *queueDaemon) grantLease(j queueJob) error {
	if d.handover != nil {
		return errors.New("Queue dispatch is frozen for live handover")
	}
	if err := d.laneRoom(taskLane(j.task)); err != nil {
		return err
	}
	lease := obj(j.task["lease"])
	now := d.stamp()
	lease["state"] = "held"
	lease["grantedAt"] = now
	if ttl, _ := integer(lease["ttlSeconds"]); ttl > 0 {
		lease["expiresAt"] = d.clock().Add(time.Duration(ttl) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	j.task["status"] = Object{"Running": Object{"start": now}}
	data, err := queueJSON(j.task)
	if err != nil {
		return err
	}
	result, err := d.store.db.Exec("UPDATE queue_jobs SET phase='running',task=? WHERE id=? AND token=? AND phase='queued'", data, j.id, j.token)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errQueueChanged
	}
	d.emitTask("lease.granted", j.id, j.task, nil)
	return nil
}

// endLease moves a lease straight to done: it has no process to clean up.
// Held leases are released or expire; queued ones are dropped or expire.
func (d *queueDaemon) endLease(j queueJob, reason string) error {
	lease := obj(j.task["lease"])
	held := j.phase == "running"
	state, event := "dropped", "lease.dropped"
	switch {
	case reason == "ttl":
		state, event = "expired", "lease.expired"
	case held && reason == "owner-exited":
		state, event = "expired", "lease.expired"
	case held:
		state, event = "released", "lease.released"
	}
	lease["state"] = state
	lease["reason"] = reason
	lease["endedAt"] = d.stamp()
	if reason == "owner-exited" {
		lease["ownerAlive"] = false
	}
	_, details, err := TaskStatus(j.task)
	if err != nil {
		return err
	}
	details["end"] = lease["endedAt"]
	details["result"] = "Success"
	j.task["status"] = Object{"Done": details}
	data, err := queueJSON(j.task)
	if err != nil {
		return err
	}
	result, err := d.store.db.Exec("UPDATE queue_jobs SET phase='done',task=?,outcome=0 WHERE id=? AND token=? AND phase=?", data, j.id, j.token, j.phase)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errQueueChanged
	}
	delete(d.ownerAlive, j.id)
	d.emitTask(event, j.id, j.task, Object{"reason": reason})
	return nil
}

func leaseExpired(task Object, now time.Time) bool {
	expires, err := time.Parse(time.RFC3339Nano, text(obj(task["lease"])["expiresAt"]))
	return err == nil && !now.Before(expires)
}

func (d *queueDaemon) activeLease(id int) (queueJob, error) {
	j, err := d.store.job(id)
	if errors.Is(err, sql.ErrNoRows) {
		return j, errors.New("Lease does not exist")
	}
	if err != nil {
		return j, err
	}
	if !taskIsLease(j.task) {
		return j, fmt.Errorf("Job %d is not a lease", id)
	}
	if j.phase != "queued" && j.phase != "running" {
		return j, fmt.Errorf("%w: lease %d is %s", errQueueLeaseGone, id, text(obj(j.task["lease"])["state"]))
	}
	return j, nil
}

func (d *queueDaemon) renewLease(p Object) (Object, error) {
	id, ok := integer(p["id"])
	if !ok || id < 0 {
		return nil, errors.New("Invalid lease ID")
	}
	j, err := d.activeLease(id)
	if err != nil {
		return nil, err
	}
	lease := obj(j.task["lease"])
	if ttl, _ := integer(lease["ttlSeconds"]); ttl > 0 {
		lease["renewedAt"] = d.stamp()
		lease["expiresAt"] = d.clock().Add(time.Duration(ttl) * time.Second).UTC().Format(time.RFC3339Nano)
		if err = d.store.saveTask(j); err != nil {
			return nil, err
		}
	}
	return d.leaseView(id)
}

// releaseLease is accepted from any process of this user. Without force it
// must name the recorded owner, so an agent cannot release another's session
// by accident; plugins pass force for an approved takeover.
func (d *queueDaemon) releaseLease(p Object) (Object, error) {
	id, ok := integer(p["id"])
	if !ok || id < 0 {
		return nil, errors.New("Invalid lease ID")
	}
	j, err := d.activeLease(id)
	if err != nil {
		return nil, err
	}
	owner := obj(obj(j.task["lease"])["owner"])
	if owner != nil && !boolean(p["force"]) {
		recorded, _ := integer(owner["pid"])
		caller, present := integer(p["ownerPid"])
		if p["ownerPid"] == nil || !present || caller != recorded {
			return nil, fmt.Errorf("%w: lease %d is owned by process %d; pass that --owner-pid or use --force", errQueueLeaseOwner, id, recorded)
		}
	}
	if err = d.endLease(j, "released"); err != nil {
		return nil, err
	}
	return d.leaseView(id)
}

// checkLeaseOwners runs outside the dispatch lock: the process listing can take
// seconds. Each verdict is re-validated against durable state before it acts.
func (d *queueDaemon) checkLeaseOwners() error {
	d.mu.Lock()
	jobs, err := d.store.unfinishedJobs()
	boot, bootErr := d.bootIdentity()
	d.mu.Unlock()
	if err != nil {
		return err
	}
	owners := map[int]Object{}
	for _, j := range jobs {
		if owner := obj(obj(j.task["lease"])["owner"]); taskIsLease(j.task) && owner != nil {
			owners[j.id] = owner
		}
	}
	if len(owners) == 0 {
		return nil
	}
	if bootErr != nil {
		return bootErr
	}
	processes, err := leaseProcessSnapshot(boot)
	if err != nil {
		return fmt.Errorf("Lease owners could not be verified; leases stay held: %w", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, owner := range owners {
		if processes.alive(owner) {
			d.markOwner(id, true)
			continue
		}
		j, err := d.store.job(id)
		if err != nil {
			return err
		}
		if !taskIsLease(j.task) || j.phase != "queued" && j.phase != "running" || !jsonEqual(obj(obj(j.task["lease"])["owner"]), owner) {
			continue
		}
		if err = d.endLease(j, "owner-exited"); err != nil && !errors.Is(err, errQueueChanged) {
			return err
		}
	}
	return nil
}

// leasePosition counts queued items ahead in the same lane, in dispatch order.
func leasePosition(jobs []queueJob, target queueJob) int {
	lane := taskLane(target.task)
	position := 1
	priority, _ := integer(target.task["priority"])
	for _, j := range jobs {
		if j.id == target.id || j.phase != "queued" || taskLane(j.task) != lane {
			continue
		}
		if name, _, err := TaskStatus(j.task); err != nil || name != "Queued" && name != "Stashed" {
			continue
		}
		other, _ := integer(j.task["priority"])
		if other > priority || other == priority && j.id < target.id {
			position++
		}
	}
	return position
}

func leaseFields(id int, task Object, position any) Object {
	lease := obj(task["lease"])
	owner := obj(lease["owner"])
	var ownerPid, ownerAlive any
	if owner != nil {
		ownerPid = owner["pid"]
		ownerAlive = lease["ownerAlive"]
		if ownerAlive == nil {
			ownerAlive = false
		}
	}
	meta := obj(lease["meta"])
	if meta == nil {
		meta = Object{}
	}
	result := Object{"id": id, "lane": taskLane(task), "state": text(lease["state"]), "position": position, "label": text(task["label"]), "meta": meta, "ownerPid": ownerPid, "ownerAlive": ownerAlive, "queuedAt": lease["queuedAt"], "grantedAt": lease["grantedAt"], "renewedAt": lease["renewedAt"], "expiresAt": lease["expiresAt"], "reason": lease["reason"]}
	if text(lease["state"]) == "held" || text(lease["state"]) == "queued" {
		result["reason"] = nil
	}
	return result
}

func (d *queueDaemon) leaseView(id int) (Object, error) {
	j, err := d.store.job(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("Lease does not exist")
	}
	if err != nil {
		return nil, err
	}
	if !taskIsLease(j.task) {
		return nil, fmt.Errorf("Job %d is not a lease", id)
	}
	lease := obj(j.task["lease"])
	if lease["owner"] != nil && (j.phase == "queued" || j.phase == "running") {
		alive, checked := d.ownerAlive[id]
		lease["ownerAlive"] = alive || !checked
	}
	var position any
	if j.phase == "queued" {
		jobs, err := d.store.unfinishedJobs()
		if err != nil {
			return nil, err
		}
		position = leasePosition(jobs, j)
	}
	return leaseFields(id, j.task, position), nil
}

func (d *queueDaemon) laneList() []Object {
	snapshot, err := d.snapshot()
	if err != nil {
		return []Object{}
	}
	return sortedLanes(obj(snapshot["lanes"]))
}

func sortedLanes(lanes Object) []Object {
	result := []Object{}
	for _, raw := range lanes {
		if lane := obj(raw); lane != nil {
			result = append(result, lane)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := integer(result[i]["priority"])
		b, _ := integer(result[j]["priority"])
		if a != b {
			return a > b
		}
		if (result[i]["name"] == defaultLaneName) != (result[j]["name"] == defaultLaneName) {
			return result[i]["name"] == defaultLaneName
		}
		return text(result[i]["name"]) < text(result[j]["name"])
	})
	return result
}

// emitTask hands an event to the plugin owning the task's lane, if any. The
// default lane has no plugin and therefore never starts hooks.
func (d *queueDaemon) emitTask(event string, id int, task Object, extra Object) {
	plugin, ok := d.laneOwner(taskLane(task))
	if !ok || len(plugin.Hooks[event]) == 0 {
		return
	}
	payload := Object{"schemaVersion": 1, "event": event, "time": d.stamp(), "lane": taskLane(task), "id": id, "kind": taskKind(task)}
	if label := text(task["label"]); label != "" {
		payload["label"] = label
	}
	if lease := obj(task["lease"]); lease != nil {
		if meta := obj(lease["meta"]); len(meta) > 0 {
			payload["meta"] = meta
		}
		if owner := obj(lease["owner"]); owner != nil {
			payload["ownerPid"] = owner["pid"]
		}
	}
	for key, value := range extra {
		payload[key] = value
	}
	d.hooks.dispatch(plugin, event, payload)
}
