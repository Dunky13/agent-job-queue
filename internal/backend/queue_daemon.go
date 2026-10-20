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
	Version int    `json:"version"`
	Result  Object `json:"result"`
	Error   string `json:"error,omitempty"`
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
	if g.queueControl != nil {
		return g.queueControl(operation, payload)
	}
	if e := safeQueueSocket(queueSocket(g)); e != nil {
		return nil, fmt.Errorf("Queue daemon unavailable; command was not run directly: %w", e)
	}
	deadline := time.Now().Add(15 * time.Second)
	if !g.ControlDeadline.IsZero() && g.ControlDeadline.Before(deadline) {
		deadline = g.ControlDeadline
	}
	if !deadline.After(time.Now()) {
		return nil, errors.New("Queue control deadline expired")
	}
	conn, e := net.DialTimeout("unix", queueSocket(g), time.Until(deadline))
	if e != nil {
		return nil, errors.New("Queue daemon unavailable; command was not run directly")
	}
	defer conn.Close()
	if e = conn.SetDeadline(deadline); e != nil {
		return nil, e
	}
	if e = json.NewEncoder(conn).Encode(queueRequest{Version: 1, Operation: operation, Payload: payload}); e != nil {
		return nil, e
	}
	var response queueResponse
	decoder := json.NewDecoder(io.LimitReader(conn, 32*1024*1024+1))
	decoder.UseNumber()
	if e = decoder.Decode(&response); e != nil {
		return nil, fmt.Errorf("Invalid queue daemon response: %w", e)
	}
	if response.Version != 1 {
		return nil, errors.New("Unsupported queue protocol")
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	if response.Result == nil {
		return nil, errors.New("Empty queue daemon response")
	}
	return response.Result, nil
}

type queueDaemon struct {
	g        *Gate
	store    *queueStore
	mu       sync.Mutex
	children map[int]*exec.Cmd
	errors   map[int]string
	cleaning map[int]bool
	cleanup  sync.WaitGroup
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
	dirty, _ := integer(settings["dirty"])
	if dirty != 0 {
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
	d := &queueDaemon{g: g, store: store, children: map[int]*exec.Cmd{}, errors: map[int]string{}, cleaning: map[int]bool{}}
	if e = d.recover(); e != nil {
		return e
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
				}
				json.NewEncoder(conn).Encode(response)
			}()
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			cancel()
			listener.Close()
			servers.Wait()
			d.cleanup.Wait()
			d.mu.Lock()
			jobs, e := store.unfinishedJobs()
			active := false
			for _, j := range jobs {
				active = active || j.phase == "starting" || j.phase == "running" || j.phase == "outcome"
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
	if op == "snapshot" {
		return d.store.snapshot()
	}
	if op == "submit" {
		return d.submit(p)
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
	name, details, e := TaskStatus(j.task)
	if e != nil {
		return nil, e
	}
	switch op {
	case "start":
		if name != "Queued" && name != "Stashed" {
			return nil, errors.New("Only queued jobs can be started")
		}
		if e = d.launch(j); e != nil {
			return nil, e
		}
		return d.store.snapshot()
	case "hold", "enqueue":
		if name == "Queued" || name == "Stashed" {
			state := "Queued"
			if op == "hold" {
				state = "Stashed"
			}
			j.task["status"] = Object{state: Object{}}
			e = d.store.saveTask(j)
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
				if state == "Queued" || state == "Stashed" {
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
	case "cancel":
		if name == "Done" {
			return j.task, nil
		}
		j.task["cancelled"] = true
		if e = d.store.saveTask(j); e != nil {
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
	case "kill":
		if name != "Done" {
			j.task["cancelled"] = true
			if e = d.store.saveTask(j); e == nil {
				e = d.signal(j, syscall.SIGKILL)
			}
		}
	case "pause-job", "resume-job":
		if name != "Running" && name != "Paused" {
			return nil, errors.New("Only active jobs can be paused or resumed")
		}
		sig := syscall.SIGSTOP
		state := "Paused"
		if op == "resume-job" {
			sig = syscall.SIGCONT
			state = "Running"
		}
		if e = d.signal(j, sig); e == nil {
			j.task["status"] = Object{state: details}
			e = d.store.saveTask(j)
		}
	default:
		return nil, errors.New("Unknown queue operation")
	}
	if e != nil {
		return nil, e
	}
	return d.store.snapshot()
}
func (d *queueDaemon) submit(p Object) (Object, error) {
	value := text(p["token"])
	if !validToken(value) {
		return nil, errors.New("Invalid submission token")
	}
	var existing int
	e := d.store.db.QueryRow("SELECT id FROM queue_jobs WHERE token=?", value).Scan(&existing)
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
	data, e := queueJSON(task)
	if e != nil {
		return nil, e
	}
	tx, e := d.store.db.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(?,?,?,'queued')", next, value, data); e != nil {
		return nil, e
	}
	if _, e = tx.Exec("UPDATE queue_settings SET next_id=? WHERE id=1", next+1); e != nil {
		return nil, e
	}
	payload["task_id"] = next
	payload["resource_managed"] = managed
	if e = WriteJobPayload(path, payload); e != nil {
		return nil, e
	}
	if e = WritePrivate(d.g.ReceiptPath(next), Object{"task_id": next, "token": value}); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return Object{"id": next}, nil
}
func (d *queueDaemon) launch(j queueJob) error {
	j.attempt = token()
	j.task["status"] = Object{"Running": Object{"start": queueNow()}}
	data, e := queueJSON(j.task)
	if e != nil {
		return e
	}
	res, e := d.store.db.Exec("UPDATE queue_jobs SET attempt=?,phase='starting',task=? WHERE id=? AND phase='queued'", j.attempt, data, j.id)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return errors.New("Job changed before launch")
	}
	j.phase = "starting"
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
func (d *queueDaemon) finish(j queueJob, code int) error {
	_, details, e := TaskStatus(j.task)
	if e != nil {
		return e
	}
	details["end"] = queueNow()
	details["result"] = queueResult(code)
	j.task["status"] = Object{"Done": details}
	data, e := queueJSON(j.task)
	if e != nil {
		return e
	}
	result, e := d.store.db.Exec("UPDATE queue_jobs SET phase='outcome',task=?,outcome=? WHERE id=? AND token=? AND attempt=? AND phase=?", data, code, j.id, j.token, j.attempt, j.phase)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e == nil && n != 1 {
		return errQueueChanged
	}
	return e
}
func (d *queueDaemon) tick() error {
	jobs, e := d.store.unfinishedJobs()
	if e != nil {
		return e
	}
	active := 0
	waiting := []queueJob{}
	for _, j := range jobs {
		switch j.phase {
		case "starting":
			active++
		case "running":
			if d.children[j.id] != nil {
				active++
				continue
			}
			owned, e := VerifyGroupIdentity(j.worker)
			if e != nil {
				return e
			}
			if owned {
				active++
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
			active++
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
					_, cleanupError = d.store.db.Exec("UPDATE queue_jobs SET phase='done' WHERE id=? AND token=? AND attempt=? AND phase='outcome'", job.id, job.token, job.attempt)
					if cleanupError != nil {
						d.errors[job.id] = cleanupError.Error()
					} else {
						delete(d.errors, job.id)
					}
				}(j)
			}
		case "queued":
			name, _, e := TaskStatus(j.task)
			if e != nil {
				return e
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
	limit, _ := integer(settings["parallel_tasks"])
	if settings["status"] != "Running" || active >= limit {
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
		a, _ := integer(waiting[i].task["priority"])
		b, _ := integer(waiting[j].task["priority"])
		if a != b {
			return a > b
		}
		return waiting[i].id < waiting[j].id
	})
	for _, j := range waiting {
		if active >= limit {
			break
		}
		if e = d.launch(j); e != nil {
			return e
		}
		active++
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
	workerJSON, e := queueJSON(worker)
	if e != nil {
		return 125, e
	}
	result, e := s.db.Exec("UPDATE queue_jobs SET phase='running',worker=? WHERE id=? AND token=? AND attempt=? AND phase='starting'", workerJSON, id, value, attempt)
	if e != nil {
		return 125, e
	}
	count, e := result.RowsAffected()
	if e != nil || count != 1 {
		return 125, errors.New("Execution attempt is no longer authorized")
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
		_, details, e := TaskStatus(j.task)
		if e != nil {
			err = errors.Join(err, e)
			return
		}
		details["end"] = queueNow()
		details["result"] = queueResult(code)
		j.task["status"] = Object{"Done": details}
		data, e := queueJSON(j.task)
		if e == nil {
			_, e = s.db.Exec("UPDATE queue_jobs SET phase='outcome',task=?,outcome=? WHERE id=? AND token=? AND attempt=? AND phase='running'", data, code, id, value, attempt)
		}
		err = errors.Join(err, e)
	}()
	return Execute(g, value)
}
