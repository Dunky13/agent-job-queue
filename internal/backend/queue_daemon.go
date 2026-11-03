package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type queueRequest struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	Payload   Object `json:"payload"`
}
type queueResponse struct {
	Version   int    `json:"version"`
	Result    Object `json:"result"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
}

func queueSocket(g *Gate) string { return filepath.Join(g.State, "run", "queue.sock") }
func safeQueueSocket(path string) error {
	for _, directory := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		info, e := os.Lstat(directory)
		if e != nil {
			return e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
			return errors.New("Unsafe queue socket directory")
		}
	}
	info, e := os.Lstat(path)
	if e != nil {
		return e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || int(st.Uid) != os.Getuid() || info.Mode().Perm() != 0600 || st.Nlink != 1 {
		return errors.New("Unsafe queue socket; work is held")
	}
	return nil
}
func (g *Gate) QueueControl(operation string, payload Object) (Object, error) {
	ctx := g.operationContext()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g.queueControl != nil {
		return g.queueControl(operation, payload)
	}
	if e := safeQueueSocket(queueSocket(g)); e != nil {
		return nil, fmt.Errorf("Queue daemon unavailable; command was not run directly: %w", queueTransportError(e))
	}
	deadline := time.Now().Add(15 * time.Second)
	if !g.ControlDeadline.IsZero() && g.ControlDeadline.Before(deadline) {
		deadline = g.ControlDeadline
	}
	if !deadline.After(time.Now()) {
		return nil, errors.New("Queue control deadline expired")
	}
	dialer := net.Dialer{Deadline: deadline}
	conn, e := dialer.DialContext(ctx, "unix", queueSocket(g))
	if e != nil {
		return nil, fmt.Errorf("Queue daemon unavailable; command was not run directly: %w", queueTransportError(e))
	}
	defer conn.Close()
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancellation()
	if e = conn.SetDeadline(deadline); e != nil {
		return nil, e
	}
	if e = json.NewEncoder(conn).Encode(queueRequest{Version: 1, Operation: operation, Payload: payload}); e != nil {
		return nil, queueTransportError(e)
	}
	var response queueResponse
	decoder := json.NewDecoder(&capReader{reader: conn, left: 32*1024*1024 + 1})
	decoder.UseNumber()
	if e = decoder.Decode(&response); e != nil {
		return nil, fmt.Errorf("Invalid queue daemon response: %w", queueTransportError(e))
	}
	if response.Version != 1 {
		return nil, errors.New("Unsupported queue protocol")
	}
	if response.Error != "" {
		switch response.ErrorCode {
		case "handover-busy":
			return nil, errQueueHandoverBusy
		case "lane-full":
			return nil, queueCodedError(errQueueLaneFull, response.Error)
		case "lane-unknown":
			return nil, queueCodedError(errQueueUnknownLane, response.Error)
		case "lease-gone":
			return nil, queueCodedError(errQueueLeaseGone, response.Error)
		case "lease-owner":
			return nil, queueCodedError(errQueueLeaseOwner, response.Error)
		}
		return nil, errors.New(response.Error)
	}
	if response.Result == nil {
		return nil, errors.New("Empty queue daemon response")
	}
	return response.Result, nil
}

var errQueueResponseTooLarge = errors.New("Queue daemon response exceeds the size limit")

// capReader fails with a definite error once the limit is exceeded. io.LimitReader
// would end the stream early, which classifies as a lost connection and is retried.
type capReader struct {
	reader io.Reader
	left   int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errQueueResponseTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.reader.Read(p)
	c.left -= int64(n)
	return n, err
}

type queueDaemon struct {
	g            *Gate
	store        *queueStore
	mu           sync.Mutex
	children     map[int]*exec.Cmd
	errors       map[int]string
	cleaning     map[int]bool
	cleanup      sync.WaitGroup
	handover     *queueHandoverReceipt
	handoverStop chan struct{}
	plugins      pluginRegistry
	retired      map[string]queueLane
	hooks        *hookRunner
	ownerAlive   map[int]bool
	finished     map[int]bool
	boot         string
	now          func() time.Time
	reported     map[int]bool
	logOutput    io.Writer
}

// queueCodedError keeps the daemon's message while restoring the sentinel the
// caller branches on; the transport only carries strings.
func queueCodedError(sentinel error, message string) error {
	if detail, ok := strings.CutPrefix(message, sentinel.Error()); ok {
		return fmt.Errorf("%w%s", sentinel, detail)
	}
	return fmt.Errorf("%w: %s", sentinel, message)
}

func queueErrorCode(err error) string {
	switch {
	case errors.Is(err, errQueueHandoverBusy):
		return "handover-busy"
	case errors.Is(err, errQueueLaneFull):
		return "lane-full"
	case errors.Is(err, errQueueUnknownLane):
		return "lane-unknown"
	case errors.Is(err, errQueueLeaseGone):
		return "lease-gone"
	case errors.Is(err, errQueueLeaseOwner):
		return "lease-owner"
	}
	return ""
}

func newQueueDaemon(g *Gate, store *queueStore) *queueDaemon {
	d := &queueDaemon{g: g, store: store, children: map[int]*exec.Cmd{}, errors: map[int]string{}, cleaning: map[int]bool{}, handoverStop: make(chan struct{}, 1), retired: map[string]queueLane{}, hooks: newHookRunner(g), ownerAlive: map[int]bool{}, finished: map[int]bool{}}
	d.plugins = loadPluginRegistry(g)
	return d
}

func ServeQueue(g *Gate) error {
	if e := g.Prepare(); e != nil {
		return e
	}
	unlock, e := LockPrivate(g, "queue-daemon.lock")
	if e != nil {
		return e
	}
	defer unlock()
	store, e := openQueueStore(g.State, true)
	if e != nil {
		return e
	}
	defer store.close()
	if e = store.importLegacy(); e != nil {
		return e
	}
	settings, e := store.settings()
	if e != nil {
		return e
	}
	adoption, e := queueAdoptHandover(g, store)
	if e != nil {
		return e
	}
	dirty, _ := integer(settings["dirty"])
	if dirty != 0 && adoption == nil {
		if _, e = store.db.Exec("UPDATE queue_settings SET status='Paused' WHERE id=1"); e != nil {
			return e
		}
	}
	if _, e = store.db.Exec("UPDATE queue_settings SET dirty=1 WHERE id=1"); e != nil {
		return e
	}
	path := queueSocket(g)
	if _, e = os.Lstat(path); e == nil {
		if e = safeQueueSocket(path); e != nil {
			return e
		}
		probe, e := net.DialTimeout("unix", path, 100*time.Millisecond)
		if e == nil {
			probe.Close()
			return errors.New("Queue socket already has a listener")
		}
		if !errors.Is(e, syscall.ECONNREFUSED) {
			return fmt.Errorf("Cannot safely replace queue socket: %w", e)
		}
		if e = os.Remove(path); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if e != nil {
		return e
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	if e = os.Chmod(path, 0600); e != nil {
		return e
	}
	defer os.Remove(path)
	d := newQueueDaemon(g, store)
	defer d.hooks.wait()
	if e = d.recover(); e != nil {
		return e
	}
	if adoption != nil {
		if e = consumeQueueHandover(g, adoption); e != nil {
			return e
		}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var servers sync.WaitGroup
	servers.Add(1)
	go func() {
		defer servers.Done()
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			servers.Add(1)
			go func() {
				defer servers.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(15 * time.Second))
				var req queueRequest
				dec := json.NewDecoder(io.LimitReader(conn, 32*1024*1024+1))
				dec.UseNumber()
				dec.DisallowUnknownFields()
				e := dec.Decode(&req)
				var result Object
				if e == nil && req.Version != 1 {
					e = errors.New("Unsupported queue protocol")
				}
				if e == nil {
					d.mu.Lock()
					result, e = d.control(req.Operation, req.Payload)
					d.mu.Unlock()
				}
				response := queueResponse{Version: 1, Result: result}
				if e != nil {
					response.Error = e.Error()
					response.ErrorCode = queueErrorCode(e)
				}
				json.NewEncoder(conn).Encode(response)
			}()
		}
	}()
	servers.Add(1)
	go func() {
		defer servers.Done()
		owners := time.NewTicker(leaseOwnerCheckInterval)
		defer owners.Stop()
		reported := time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-owners.C:
				if e := d.checkLeaseOwners(); e != nil && time.Since(reported) > time.Minute {
					fmt.Fprintln(os.Stderr, "agent-heavy daemon:", e)
					reported = time.Now()
				}
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-d.handoverStop:
			cancel()
			listener.Close()
			servers.Wait()
			d.cleanup.Wait()
			d.mu.Lock()
			e = d.releaseHandover()
			d.mu.Unlock()
			return e
		case <-stop:
			cancel()
			listener.Close()
			servers.Wait()
			d.cleanup.Wait()
			d.mu.Lock()
			jobs, e := store.unfinishedJobs()
			active := false
			for _, j := range jobs {
				// A held lease has no process that a restart could orphan.
				active = active || !taskIsLease(j.task) && (j.phase == "starting" || j.phase == "running" || j.phase == "outcome")
			}
			if e == nil && !active {
				_, e = store.db.Exec("UPDATE queue_settings SET dirty=0 WHERE id=1")
			}
			d.mu.Unlock()
			return e
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.mu.Lock()
			e = d.tick()
			d.mu.Unlock()
			if e != nil {
				fmt.Fprintln(os.Stderr, "agent-heavy daemon:", e)
			}
		}
	}
}
func (d *queueDaemon) recover() error {
	jobs, e := d.store.unfinishedJobs()
	if e != nil {
		return e
	}
	for _, j := range jobs {
		if j.phase == "starting" {
			if e = d.finish(j, 125); e != nil && !errors.Is(e, errQueueChanged) {
				return e
			}
		}
	}
	return nil
}

// queueRowRefused reports a store refusal the daemon settles instead of failing.
// Stale errors are not refusals.
func queueRowRefused(e error) bool {
	return errors.Is(e, errQueueStateDisagreement) || errors.Is(e, errQueueIllegalTransition)
}

// queuePublishedCode is the exit status a corrupted outcome row already holds: its
// column, then a result its status can still give. Only then is it unknown.
func queuePublishedCode(j queueJob) int {
	if j.outcome.Valid {
		return int(j.outcome.Int64)
	}
	if name, details, e := TaskStatus(j.task); e == nil && name == "Done" {
		if code, e := ResultCode(details["result"]); e == nil {
			return code
		}
	}
	if boolean(j.task["cancelled"]) {
		return 130
	}
	return 125
}

// reportRow writes one line per row per daemon run. daemon.log is bounded only at
// spawn, and tick repeats every 100 ms. The first report for a row stands.
func (d *queueDaemon) reportRow(j queueJob) {
	if d.reported[j.id] {
		return
	}
	if d.reported == nil {
		d.reported = map[int]bool{}
	}
	d.reported[j.id] = true
	status := "unreadable"
	if name, _, e := TaskStatus(j.task); e == nil {
		status = name
	}
	var output io.Writer = os.Stderr
	if d.logOutput != nil {
		output = d.logOutput
	}
	fmt.Fprintf(output, "agent-heavy daemon: corrupted job %d: phase %s, status %s; settling it once no owned process remains\n", j.id, j.phase, status)
}

// finish records a command's result. A row the store refuses is settled and logged.
func (d *queueDaemon) finish(j queueJob, code int) error {
	e := d.store.recordOutcome(j, code)
	if !queueRowRefused(e) {
		return e
	}
	d.reportRow(j)
	return d.store.overwriteOutcome(j, code)
}

func (d *queueDaemon) markCancelled(j queueJob) error {
	e := d.store.markCancelled(j)
	if !errors.Is(e, errQueueStateDisagreement) {
		return e
	}
	d.reportRow(j)
	return d.store.overwriteCancelled(j)
}

func (d *queueDaemon) control(op string, p Object) (Object, error) {
	for range 8 {
		value, e := d.controlOnce(op, p)
		if !errors.Is(e, errQueueChanged) {
			return value, e
		}
	}
	return nil, errQueueChanged
}
func (d *queueDaemon) controlOnce(op string, p Object) (Object, error) {
	if e := queuePrivateFiles(d.store.path); e != nil {
		return nil, e
	}
	if op == "prepare-handover" || op == "commit-handover" || op == "cancel-handover" {
		return d.handoverControl(op, p)
	}
	if op == "handover-status" {
		// Retired: an older installer reads this refusal as "use the drained path".
		return nil, errors.New("Unknown queue operation")
	}
	if d.handover != nil && op != "snapshot" && op != "lease-status" {
		if op == "submit" || op == "submit-lane" || op == "lease-acquire" {
			value := text(p["token"])
			if !validToken(value) {
				return nil, errors.New("Invalid submission token")
			}
			id, err := d.store.jobIDByToken(value)
			if err == nil {
				if op == "lease-acquire" {
					return d.leaseView(id)
				}
				return Object{"id": id}, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		return nil, errQueueHandoverBusy
	}
	switch op {
	case "snapshot":
		return d.snapshot()
	case "submit":
		return d.submit(p)
	case "submit-lane":
		lane := text(p["lane"])
		if _, ok := d.plugins.lanes[lane]; !ok {
			return nil, fmt.Errorf("%w %q", errQueueUnknownLane, lane)
		}
		return d.submitLane(p, lane)
	case "lease-acquire":
		return d.acquireLease(p)
	case "lease-renew":
		return d.renewLease(p)
	case "lease-release":
		return d.releaseLease(p)
	case "lease-status":
		id, ok := integer(p["id"])
		if !ok || id < 0 {
			return nil, errors.New("Invalid lease ID")
		}
		return d.leaseView(id)
	case "plugins-reload":
		if err := d.reloadPlugins(); err != nil {
			return nil, err
		}
		return d.snapshot()
	}
	if op == "limit" {
		limit, ok := integer(p["limit"])
		if !ok {
			return nil, errors.New("Invalid queue limit")
		}
		if e := ValidateLimit(limit); e != nil {
			return nil, e
		}
		if _, e := d.store.db.Exec("UPDATE queue_settings SET parallel_limit=? WHERE id=1", limit); e != nil {
			return nil, e
		}
		return d.store.settings()
	}
	if op == "pause" || op == "resume" {
		status := "Paused"
		if op == "resume" {
			status = "Running"
		}
		_, e := d.store.db.Exec("UPDATE queue_settings SET status=? WHERE id=1", status)
		if e != nil {
			return nil, e
		}
		return d.store.settings()
	}
	id, ok := integer(p["id"])
	if !ok || id < 0 {
		return nil, errors.New("Invalid queue job ID")
	}
	j, e := d.store.job(id)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, errors.New("Queue job does not exist")
	}
	if e != nil {
		return nil, e
	}
	name, _, e := TaskStatus(j.task)
	if e != nil {
		return nil, e
	}
	switch op {
	case "start":
		if name != "Queued" && name != "Stashed" {
			return nil, errors.New("Only queued jobs can be started")
		}
		if taskIsLease(j.task) {
			e = d.grantLease(j)
		} else {
			e = d.launch(j)
		}
		if e != nil {
			return nil, e
		}
		return d.snapshot()
	case "hold", "enqueue":
		// Holding or enqueueing anything that is not waiting has always done nothing.
		if e = d.store.setWaiting(j, op == "hold"); queueRowRefused(e) {
			e = nil
		}
	case "priority", "next", "last":
		if name != "Queued" && name != "Stashed" {
			return nil, errors.New("Only queued jobs can be reordered")
		}
		priority, valid := integer(p["priority"])
		if op != "priority" {
			jobs, e := d.store.jobs()
			if e != nil {
				return nil, e
			}
			values := []int{}
			for _, candidate := range jobs {
				state, _, e := TaskStatus(candidate.task)
				if e != nil {
					return nil, e
				}
				// Reordering stays within the item's own lane.
				if (state == "Queued" || state == "Stashed") && taskLane(candidate.task) == taskLane(j.task) {
					v, _ := integer(candidate.task["priority"])
					values = append(values, v)
				}
			}
			sort.Ints(values)
			priority = values[0] - 1
			if op == "next" {
				priority = values[len(values)-1] + 1
			}
			valid = true
		}
		if !valid {
			return nil, errors.New("Invalid priority")
		}
		if e = ValidatePriority(priority); e != nil {
			return nil, e
		}
		j.task["priority"] = priority
		e = d.store.saveTask(j)
		if e == nil {
			return Object{"priority": priority}, nil
		}
	case "cancel", "kill":
		if taskIsLease(j.task) {
			// Cancelling a queued lease drops it; cancelling a held lease
			// releases it. There is no process to signal.
			if name != "Done" {
				e = d.endLease(j, "cancelled")
			}
			break
		}
		if op == "kill" {
			if name != "Done" {
				if e = d.markCancelled(j); e == nil {
					e = d.signal(j, syscall.SIGKILL)
				}
			}
			break
		}
		if name == "Done" {
			return j.task, nil
		}
		if e = d.markCancelled(j); e != nil {
			return nil, e
		}
		if j.phase == "queued" || j.phase == "starting" {
			e = d.finish(j, 130)
		} else {
			e = d.signal(j, syscall.SIGINT)
			if name == "Paused" && e == nil {
				e = d.signal(j, syscall.SIGCONT)
			}
		}
	case "pause-job", "resume-job":
		if taskIsLease(j.task) {
			return nil, errors.New("A lease has no process to pause or resume")
		}
		if name != "Running" && name != "Paused" {
			return nil, errors.New("Only active jobs can be paused or resumed")
		}
		// A refusal must send nothing: a stopped process with no record stays stopped.
		if e = checkQueuePausable(j); e != nil {
			return nil, e
		}
		sig := syscall.SIGSTOP
		if op == "resume-job" {
			sig = syscall.SIGCONT
		}
		if e = d.signal(j, sig); e == nil {
			e = d.store.setPaused(j, op == "pause-job")
		}
	default:
		return nil, errors.New("Unknown queue operation")
	}
	if e != nil {
		return nil, e
	}
	return d.snapshot()
}
func (d *queueDaemon) submit(p Object) (Object, error) {
	return d.submitLane(p, defaultLaneName)
}
func (d *queueDaemon) submitLane(p Object, lane string) (Object, error) {
	value := text(p["token"])
	if !validToken(value) {
		return nil, errors.New("Invalid submission token")
	}
	existing, e := d.store.jobIDByToken(value)
	if e == nil {
		return Object{"id": existing}, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	settings, e := d.store.settings()
	if e != nil {
		return nil, e
	}
	if settings["status"] != "Running" {
		return nil, errors.New("Dedicated default queue must be Running; no command was submitted")
	}
	path, _ := d.g.PayloadPath(value)
	payload, e := ReadJobPayload(path)
	if e != nil {
		return nil, e
	}
	if payload["token"] != value {
		return nil, errors.New("Invalid private submission identity")
	}
	protocol, supported := integer(payload["client_handover_protocol"])
	var caller *queueHandoverDaemon
	if supported && protocol == queueHandoverProtocol {
		caller, e = decodeQueueHandoverIdentity(payload["client_handover_caller"])
		if e != nil {
			return nil, e
		}
		alive, identityErr := queueHandoverDaemonAlive(*caller)
		if identityErr != nil {
			return nil, identityErr
		}
		if !alive {
			return nil, errors.New("Original queue caller exited before submission")
		}
	}
	if d.handover != nil && (!supported || protocol != queueHandoverProtocol) {
		return nil, errors.New("A legacy caller cannot submit while a live queue handover is prepared")
	}
	argv, e := stringsFrom(payload["argv"])
	if e != nil || len(argv) == 0 {
		return nil, errors.New("Invalid private command")
	}
	configuration, e := SchedulingSettings(d.g)
	if e != nil {
		return nil, e
	}
	managed := configuration["mode"] == "adaptive"
	state := "Queued"
	if managed {
		state = "Stashed"
	}
	next, validNext := integer(settings["next_id"])
	if !validNext || next < 0 || next == math.MaxInt {
		return nil, errors.New("Queue job ID space exhausted")
	}
	task := Object{"token": value, "path": payload["cwd"], "label": payload["label"], "priority": 0, "group": "default", "created_at": queueNow(), "status": Object{state: Object{}}, "logPath": filepath.Join(d.g.State, "logs", strconv.Itoa(next)+".log")}
	if lane != defaultLaneName {
		task["lane"] = lane
	}
	if supported && protocol == queueHandoverProtocol {
		task["handoverProtocol"] = queueHandoverProtocol
		task["handoverCaller"] = caller
	}
	e = d.store.enqueue(next, value, task, func() error {
		payload["task_id"] = next
		payload["resource_managed"] = managed
		if e := WriteJobPayload(path, payload); e != nil {
			return e
		}
		return WritePrivate(d.g.ReceiptPath(next), Object{"task_id": next, "token": value})
	})
	if e != nil {
		return nil, e
	}
	if lane != defaultLaneName {
		d.emitTask("job.queued", next, task, nil)
		return Object{"id": next, "lane": lane}, nil
	}
	return Object{"id": next}, nil
}
func (d *queueDaemon) launch(j queueJob) error {
	if d.handover != nil {
		return errors.New("Queue dispatch is frozen for live handover")
	}
	if e := d.laneRoom(taskLane(j.task)); e != nil {
		return e
	}
	j, e := d.store.claimAttempt(j)
	if queueRowRefused(e) {
		return errQueueLaunchChanged // The result main gives its guard on such a row.
	}
	if e != nil {
		return e
	}
	path := text(j.task["logPath"])
	fd, e := syscall.Open(path, syscall.O_CREAT|syscall.O_APPEND|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return d.finish(j, 125)
	}
	log := os.NewFile(uintptr(fd), path)
	defer log.Close()
	info, e := log.Stat()
	if e != nil {
		return d.finish(j, 125)
	}
	logStat, validLog := info.Sys().(*syscall.Stat_t)
	if !validLog || !fileOwned(info, true) || logStat.Nlink != 1 {
		return d.finish(j, 125)
	}
	executable, e := os.Executable()
	if e != nil {
		return d.finish(j, 125)
	}
	cmd := exec.Command(executable, "execute-attempt", strconv.Itoa(j.id), j.token, j.attempt)
	cmd.Env = envList(d.g.ControlEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		return d.finish(j, 125)
	}
	d.children[j.id] = cmd
	d.emitTask("job.started", j.id, j.task, nil)
	go func() {
		e := cmd.Wait()
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.children, j.id)
		current, readError := d.store.job(j.id)
		if readError == nil && current.attempt == j.attempt && (current.phase == "starting" || current.phase == "running") {
			code := 125
			if boolean(current.task["cancelled"]) {
				code = 130
			}
			if e != nil {
				if exit, ok := e.(*exec.ExitError); ok && exit.ExitCode() > 0 {
					code = exit.ExitCode()
				}
			}
			_ = d.finish(current, code)
		}
	}()
	return nil
}
func (d *queueDaemon) signal(j queueJob, sig syscall.Signal) error {
	if j.worker == nil {
		return nil
	}
	owned, e := VerifyGroupIdentity(j.worker)
	if e != nil {
		return e
	}
	if !owned {
		return nil
	}
	pgid, _ := integer(j.worker["pgid"])
	e = syscall.Kill(-pgid, sig)
	if errors.Is(e, syscall.ESRCH) {
		return nil
	}
	return e
}
func (d *queueDaemon) tick() error {
	if e := d.expireHandover(); e != nil {
		return e
	}
	jobs, e := d.store.unfinishedJobs()
	if e != nil {
		return e
	}
	usage := map[string]*laneCount{}
	occupy := func(j queueJob) {
		lane := taskLane(j.task)
		if usage[lane] == nil {
			usage[lane] = &laneCount{}
		}
		if taskIsLease(j.task) {
			usage[lane].held++
		} else {
			usage[lane].running++
		}
	}
	waiting := []queueJob{}
	now := d.clock()
	for _, j := range jobs {
		if taskIsLease(j.task) {
			if leaseExpired(j.task, now) {
				if e = d.endLease(j, "ttl"); e != nil && !errors.Is(e, errQueueChanged) {
					return e
				}
				continue
			}
			name, _, unreadable := TaskStatus(j.task)
			if unreadable != nil {
				if e = d.endLease(j, "corrupted"); e != nil && !errors.Is(e, errQueueChanged) {
					return e
				}
				continue
			}
			if j.phase == "running" {
				occupy(j)
			} else if name == "Queued" {
				waiting = append(waiting, j)
			}
			continue
		}
		switch j.phase {
		case "starting":
			// A live child settles the row itself when it exits.
			if _, _, unreadable := TaskStatus(j.task); unreadable != nil && d.children[j.id] == nil {
				if e = d.finish(j, 125); e != nil && !errors.Is(e, errQueueChanged) {
					return e
				}
				continue
			}
			occupy(j)
		case "running":
			if d.children[j.id] != nil {
				occupy(j)
				continue
			}
			// A corrupted row with no worker recorded has nothing to verify. One whose
			// worker cannot be verified is neither settled nor allowed to stop the tick.
			corrupted := queueJobAgreement(j) != nil
			owned, unverified := false, error(nil)
			if !corrupted || j.worker != nil {
				owned, unverified = VerifyGroupIdentity(j.worker)
			}
			if unverified != nil && !corrupted {
				return unverified
			}
			if unverified != nil {
				d.reportRow(j)
			}
			if owned || unverified != nil {
				occupy(j)
				continue
			}
			code := 125
			if boolean(j.task["cancelled"]) {
				code = 130
			}
			if e = d.finish(j, code); e != nil && !errors.Is(e, errQueueChanged) {
				return e
			}
		case "outcome":
			occupy(j)
			// Its exit status stands if it has one. The next tick retires it as usual.
			if queueJobAgreement(j) != nil {
				if e = d.finish(j, queuePublishedCode(j)); e != nil && !errors.Is(e, errQueueChanged) {
					return e
				}
				continue
			}
			if !d.finished[j.id] {
				if d.finished == nil {
					d.finished = map[int]bool{}
				}
				d.finished[j.id] = true
				extra := Object{}
				if j.outcome.Valid {
					extra["exitStatus"] = j.outcome.Int64
				}
				d.emitTask("job.finished", j.id, j.task, extra)
			}
			if !d.cleaning[j.id] {
				d.cleaning[j.id] = true
				d.cleanup.Add(1)
				go func(job queueJob) {
					defer d.cleanup.Done()
					cleanupError := Cleanup(d.g, job.id)
					d.mu.Lock()
					defer d.mu.Unlock()
					delete(d.cleaning, job.id)
					if cleanupError != nil {
						d.errors[job.id] = cleanupError.Error()
						return
					}
					// A row that already moved on is not a cleanup failure.
					if cleanupError = d.store.retire(job); cleanupError != nil && !errors.Is(cleanupError, errQueueChanged) {
						d.errors[job.id] = cleanupError.Error()
					} else {
						delete(d.errors, job.id)
						delete(d.finished, job.id)
					}
				}(j)
			}
		case "queued":
			name, _, e := TaskStatus(j.task)
			if e != nil {
				if e = d.finish(j, 125); e != nil && !errors.Is(e, errQueueChanged) {
					return e
				}
				continue
			}
			if name == "Queued" {
				waiting = append(waiting, j)
			}
		}
	}
	settings, e := d.store.settings()
	if e != nil {
		return e
	}
	if d.handover != nil {
		return nil
	}
	limit, _ := integer(settings["parallel_tasks"])
	if settings["status"] != "Running" {
		return nil
	}
	count := func(name string) *laneCount {
		if usage[name] == nil {
			usage[name] = &laneCount{}
		}
		return usage[name]
	}
	sharing := 0
	for name, c := range usage {
		if lane, _ := d.lane(name, limit); lane.SharesLimit {
			sharing += c.occupied()
		}
	}
	// With only the default lane this is exactly the former "active >= limit"
	// early return: every candidate shares the global limit.
	startable := false
	for _, j := range waiting {
		lane, _ := d.lane(taskLane(j.task), limit)
		startable = startable || laneAdmissible(lane, count(lane.Name), sharing, limit)
	}
	if !startable {
		return nil
	}
	configuration, e := SchedulingSettings(d.g)
	if e != nil {
		return e
	}
	if configuration["mode"] == "adaptive" {
		return nil
	}
	sort.Slice(waiting, func(i, j int) bool {
		laneA, _ := d.lane(taskLane(waiting[i].task), limit)
		laneB, _ := d.lane(taskLane(waiting[j].task), limit)
		if laneA.Priority != laneB.Priority {
			return laneA.Priority > laneB.Priority
		}
		a, _ := integer(waiting[i].task["priority"])
		b, _ := integer(waiting[j].task["priority"])
		if a != b {
			return a > b
		}
		return waiting[i].id < waiting[j].id
	})
	for _, j := range waiting {
		lane, _ := d.lane(taskLane(j.task), limit)
		if !laneAdmissible(lane, count(lane.Name), sharing, limit) {
			continue
		}
		if taskIsLease(j.task) {
			e = d.grantLease(j)
		} else {
			e = d.launch(j)
		}
		if errors.Is(e, errQueueLaneFull) || errors.Is(e, errQueueChanged) {
			continue
		}
		if e != nil {
			return e
		}
		occupy(j)
		if lane.SharesLimit {
			sharing++
		}
	}
	return nil
}

// ExecuteAttempt durably fences a worker before any command side effect. The
// worker writes its outcome itself, even when the queue daemon has died.
func ExecuteAttempt(g *Gate, id int, value, attempt string) (code int, err error) {
	if id < 0 || !validToken(value) || !validToken(attempt) {
		return 125, errors.New("Invalid execution attempt")
	}
	s, e := openQueueStore(g.State, false)
	if e != nil {
		return 125, e
	}
	defer s.close()
	pgid := syscall.Getpgrp()
	if pgid != os.Getpid() || pgid <= 1 {
		return 125, errors.New("Worker requires its own process group")
	}
	boot, e := BootIdentity()
	if e != nil {
		return 125, e
	}
	members, e := ProcessGroupMembers(pgid)
	if e != nil {
		return 125, e
	}
	seen := false
	for _, m := range members {
		pid, _ := integer(m["pid"])
		seen = seen || pid == os.Getpid()
	}
	if !seen {
		return 125, errors.New("Cannot establish worker birth identity")
	}
	worker := Object{"pgid": pgid, "pid": os.Getpid(), "boot_uuid": boot, "members": members}
	if e = s.claimWorker(id, value, attempt, worker); e != nil {
		return 125, e
	}
	code = 125
	defer func() {
		j, e := s.job(id)
		if e != nil {
			err = errors.Join(err, e)
			return
		}
		if j.token != value || j.attempt != attempt || j.phase != "running" {
			err = errors.Join(err, errors.New("Execution attempt identity changed"))
			return
		}
		// A row settled since the read above keeps the outcome it already has.
		if e = s.recordOutcome(j, code); !errors.Is(e, errQueueChanged) {
			err = errors.Join(err, e)
		}
	}()
	return executeWithAttempt(g, value, attempt)
}
