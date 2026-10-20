package backend

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func Drain(path string, offset int64) (int64, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return offset, nil
	}
	if err != nil {
		return offset, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return offset, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !fileOwned(info, false) || st.Nlink != 1 {
		return offset, errors.New("Unsafe job output log")
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	n, err := io.Copy(os.Stdout, file)
	return offset + n, err
}
func WaitCleanup(g *Gate, id int) error {
	deadline := time.Now().Add(12 * time.Second)
	for {
		receipt, err := g.Receipt(id)
		if err != nil {
			return err
		}
		if receipt == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Job %d finished but cleanup did not complete; shared slot retained", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
func Follow(g *Gate, id int, signals <-chan os.Signal) (int, error) {
	offset := int64(0)
	notice := time.Now()
	path := g.LogPath(id)
	for {
		var err error
		offset, err = Drain(path, offset)
		if err != nil {
			return 125, err
		}
		task, err := g.Task(id)
		if err != nil {
			return 125, err
		}
		name, details, err := TaskStatus(task)
		if err != nil {
			return 125, err
		}
		if name == "Missing" {
			cancelled, err := ReadPrivate(filepath.Join(g.Cancelled, strconv.Itoa(id)+".json"))
			if err != nil {
				return 125, fmt.Errorf("Job %d disappeared before its result could be read", id)
			}
			if err = WaitCleanup(g, id); err != nil {
				return 125, err
			}
			_, err = Drain(path, offset)
			if err != nil {
				return 125, err
			}
			code, ok := integer(cancelled["exit_code"])
			if !ok || code < 0 || code > 255 {
				return 125, errors.New("Invalid cancellation result")
			}
			return code, nil
		}
		if name == "Done" {
			offset, err = Drain(path, offset)
			if err != nil {
				return 125, err
			}
			code, err := ResultCode(details["result"])
			if err != nil {
				return 125, err
			}
			if err = WaitCleanup(g, id); err != nil {
				return 125, err
			}
			_, err = Drain(path, offset)
			return code, err
		}
		if time.Since(notice) >= 15*time.Second {
			receipt, err := g.Receipt(id)
			if err != nil {
				return 125, err
			}
			phase := strings.ToLower(name)
			if name == "Running" && receipt != nil && receipt["pgid"] == nil {
				phase = "waiting for shared validation slot"
			}
			fmt.Fprintf(os.Stderr, "agent-heavy: job %d %s\n", id, phase)
			notice = time.Now()
		}
		select {
		case sig := <-signals:
			if err := Cancel(g, id); err != nil {
				return 125, err
			}
			return 128 + int(sig.(syscall.Signal)), nil
		case <-time.After(400 * time.Millisecond):
		}
	}
}
func Cancel(g *Gate, id int) error {
	if id < 0 {
		return errors.New("Task ID must be nonnegative")
	}
	if err := g.Prepare(); err != nil {
		return err
	}
	path := filepath.Join(g.Cancelled, strconv.Itoa(id)+".json")
	if err := WritePrivate(path, Object{"task_id": id, "exit_code": 130}); err != nil {
		return err
	}
	deadline := time.Now().Add(12 * time.Second)
	var graceful time.Time
	for time.Now().Before(deadline) {
		task, err := g.Task(id)
		if err != nil {
			return err
		}
		name, details, err := TaskStatus(task)
		if err != nil {
			return err
		}
		switch name {
		case "Done":
			code, err := ResultCode(details["result"])
			if err != nil {
				return err
			}
			if err = WritePrivate(path, Object{"task_id": id, "exit_code": code}); err != nil {
				return err
			}
			return WaitCleanup(g, id)
		case "Missing":
			receipt, err := g.Receipt(id)
			if err != nil {
				return err
			}
			if receipt != nil {
				payload, err := g.PayloadPath(text(receipt["token"]))
				if err != nil {
					return err
				}
				if err = removeMissing(payload); err != nil {
					return err
				}
				if receipt["pgid"] == nil {
					if err = removeMissing(g.ReceiptPath(id)); err != nil {
						return err
					}
				}
			}
			return WaitCleanup(g, id)
		case "Queued", "Stashed", "Running", "Paused":
			op := "cancel"
			if graceful.IsZero() {
				graceful = time.Now().Add(3 * time.Second)
			} else if time.Now().After(graceful) {
				op = "kill"
			}
			_, err = g.QueueControl(op, Object{"id": id})
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("Job %d has unexpected state %s; inspect the queue before recovery", id, name)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("Cancellation of job %d did not finish; its exclusion lock is retained", id)
}
func RejectPipedInput() error {
	fd := int(os.Stdin.Fd())
	events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	ready, err := unix.Poll(events, 0)
	if errors.Is(err, syscall.EINTR) {
		return nil
	}
	if err != nil {
		return err
	}
	if ready == 0 || events[0].Revents&unix.POLLIN == 0 {
		return nil
	}
	// A terminal remains available for ordinary shell use. Queued workers always
	// receive EOF; a readable pipe with data is rejected without blocking.
	if term.IsTerminal(fd) {
		return nil
	}
	flags, err := unix.FcntlInt(os.Stdin.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		return err
	}
	defer unix.FcntlInt(os.Stdin.Fd(), unix.F_SETFL, flags)
	var one [1]byte
	n, err := unix.Read(fd, one[:])
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
		return nil
	}
	if err != nil {
		return err
	}
	if n > 0 {
		return errors.New("Piped/heredoc input is unsupported; use argv or an explicit input file. Queued commands receive stdin EOF")
	}
	return nil
}
func Run(g *Gate, argv []string, label string) (code int, err error) {
	if os.Getenv("AGENT_HEAVY_ACTIVE") != "" {
		return 125, errors.New("Nested agent-heavy would deadlock; wrap the outer command once")
	}
	if len(argv) == 0 {
		return 125, errors.New("Specify a command after --")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return 125, errors.New("Invalid command argv")
		}
	}
	if err = RejectPipedInput(); err != nil {
		return 125, err
	}
	if err = g.Prepare(); err != nil {
		return 125, err
	}
	snapshot, err := g.Snapshot()
	if err != nil {
		return 125, err
	}
	group := obj(obj(snapshot["groups"])["default"])
	limit, ok := integer(group["parallel_tasks"])
	if !ok {
		return 125, errors.New("Invalid parallel limit")
	}
	if err = ValidateLimit(limit); err != nil {
		return 125, err
	}
	if group["status"] != "Running" {
		return 125, errors.New("Dedicated default queue must be Running; no command was submitted")
	}
	jobToken := token()
	path, _ := g.PayloadPath(jobToken)
	cwd, err := os.Getwd()
	if err != nil {
		return 125, err
	}
	description, err := MakeDescription(g, argv, cwd, label)
	if err != nil {
		return 125, err
	}
	if err = CacheDescription(g, jobToken, description); err != nil {
		return 125, err
	}
	identity, err := IdentifyWorkload(argv, cwd, envMap())
	if err != nil {
		return 125, err
	}
	if err = WritePrivate(filepath.Join(g.State, "workloads", jobToken+".json"), identity); err != nil {
		return 125, err
	}
	forwarded := map[string]string{}
	for k, v := range envMap() {
		if !strings.HasPrefix(k, "AGENT_HEAVY_") && !strings.HasPrefix(k, "PUEUE_") {
			forwarded[k] = v
		}
	}
	content := Object{"argv": argv, "cwd": cwd, "env": forwarded, "token": jobToken, "label": description["title"], "explicit_label": label != ""}
	if err = WriteJobPayload(path, content); err != nil {
		return 125, err
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	id := -1
	submitted := false
	defer func() {
		if id < 0 && !submitted {
			_ = removeMissing(path)
		}
	}()
	unlock, err := LockPrivate(g, "scheduling.lock")
	if err != nil {
		return 125, err
	}
	_, err = SchedulingSettings(g)
	if err != nil {
		unlock()
		return 125, err
	}
	submitted = true
	added, err := g.QueueControl("submit", Object{"token": jobToken})
	// A lost response does not establish whether submission committed. Reconcile
	// using the same immutable token; the daemon returns the original identity.
	for retry := 0; err != nil && retry < 2; retry++ {
		added, err = g.QueueControl("submit", Object{"token": jobToken})
	}
	if err != nil {
		unlock()
		return 125, err
	}
	var acknowledged bool
	id, acknowledged = integer(added["id"])
	if !acknowledged || id < 0 {
		unlock()
		id = -1
		return 125, errors.New("Queue did not acknowledge a numeric task ID")
	}
	unlock()
	fmt.Fprintf(os.Stderr, "agent-heavy: job %d queued\n", id)
	return Follow(g, id, signals)
}
func Execute(g *Gate, value string) (int, error) {
	if err := g.Prepare(); err != nil {
		return 125, err
	}
	path, err := g.PayloadPath(value)
	if err != nil {
		return 125, err
	}
	deadline := time.Now().Add(30 * time.Second)
	var content Object
	id := -1
	for {
		content, err = ReadJobPayload(path)
		if err != nil {
			return 125, err
		}
		if content["token"] != value {
			return 125, errors.New("Invalid job identity")
		}
		if raw, present := content["task_id"]; present {
			valid := false
			id, valid = integer(raw)
			if !valid || id < 0 {
				return 125, errors.New("Invalid job identity")
			}
			receipt, err := g.Receipt(id)
			if err != nil {
				return 125, err
			}
			if jsonEqual(receipt, Object{"task_id": id, "token": value}) {
				break
			}
			if receipt != nil {
				return 125, errors.New("Job receipt does not match its private payload")
			}
		}
		if time.Now().After(deadline) {
			_ = removeMissing(path)
			return 125, errors.New("Caller did not publish an acknowledged job identity; command was not started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	argv, cwd, forwarded, err := executionPayloadFields(content)
	if err != nil {
		return 125, err
	}
	if _, err = os.Lstat(filepath.Join(g.Metadata, value+".json")); errors.Is(err, os.ErrNotExist) {
		description, err := MakeDescription(g, argv, cwd, payloadLabel(content))
		if err != nil {
			return 125, err
		}
		if err = CacheDescription(g, value, description); err != nil {
			return 125, err
		}
	} else if err != nil {
		return 125, err
	}
	if err = os.Remove(path); err != nil {
		return 125, err
	}
	pgid := syscall.Getpgrp()
	if pgid <= 1 {
		return 125, errors.New("Refusing an unsafe process group")
	}
	members, err := ProcessGroupMembers(pgid)
	if err != nil {
		return 125, err
	}
	initial := []Object{}
	seen := false
	for _, member := range members {
		pid, _ := integer(member["pid"])
		if pid == pgid || pid == os.Getpid() {
			initial = append(initial, member)
		}
		seen = seen || pid == os.Getpid()
	}
	if !seen {
		return 125, errors.New("Cannot establish the helper process birth identity")
	}
	boot, err := BootIdentity()
	if err != nil {
		return 125, err
	}
	marker := Object{"task_id": id, "token": value, "pgid": pgid, "pid": os.Getpid(), "boot_uuid": boot, "initial_members": initial, "members": initial}
	if boolean(content["resource_managed"]) {
		marker["resourceManaged"] = true
	}
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, syscall.SIGINT)
	defer signal.Stop(interrupts)
	for {
		select {
		case <-interrupts:
			return 130, nil
		default:
		}
		capacity, err := LeaseCapacity(g)
		if err != nil {
			return 125, err
		}
		acquired, err := AcquireLease(g, marker, capacity)
		if err != nil {
			return 125, err
		}
		if acquired {
			break
		}
		select {
		case <-interrupts:
			return 130, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	select {
	case <-interrupts:
		return 130, nil
	default:
	}
	forwarded["AGENT_HEAVY_ACTIVE"] = value
	for k, v := range map[string]string{"GOMAXPROCS": "2", "GOMEMLIMIT": "512MiB", "GOGC": "50"} {
		if _, ok := forwarded[k]; !ok {
			forwarded[k] = v
		}
	}
	err = runExecutableForEnvironment(argv, cwd, forwarded, os.Stdout, os.Stderr)
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				code = 128 + int(status.Signal())
			} else {
				code = exit.ExitCode()
			}
		} else if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "agent-heavy: command executable or working directory does not exist")
			code = 127
		} else if errors.Is(err, os.ErrPermission) {
			fmt.Fprintln(os.Stderr, "agent-heavy: command executable or working directory is inaccessible")
			code = 126
		} else {
			return 125, err
		}
	}
	members, err = ProcessGroupMembers(pgid)
	if err != nil {
		return 125, err
	}
	known := map[string]bool{}
	all := list(marker["members"])
	for _, raw := range all {
		member := obj(raw)
		pid, _ := integer(member["pid"])
		known[strconv.Itoa(pid)+":"+text(member["birth"])] = true
	}
	for _, member := range members {
		pid, _ := integer(member["pid"])
		key := strconv.Itoa(pid) + ":" + text(member["birth"])
		if !known[key] {
			all = append(all, member)
			known[key] = true
		}
	}
	marker["members"] = all
	unlock, err := LockPrivate(g, "")
	if err != nil {
		return 125, err
	}
	defer unlock()
	if err = WritePrivate(JobMarkerPath(g, marker), marker); err != nil {
		return 125, err
	}
	return code, nil
}
