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
	receipt, err := g.Receipt(id)
	if err != nil {
		return err
	}
	return waitCleanupWithToken(g, id, text(receipt["token"]))
}

// queueCleanupWindow is how long a finished job may wait for the daemon to
// release its receipt before the daemon is probed.
var queueCleanupWindow = 12 * time.Second

func waitCleanupWithToken(g *Gate, id int, expectedToken string) error {
	deadline := time.Now().Add(queueCleanupWindow)
	waitingForHandover := false
	outage := newQueueOutage(id, os.Stderr)
	for {
		receipt, err := g.Receipt(id)
		if err != nil {
			return err
		}
		if receipt == nil {
			return nil
		}
		if time.Now().After(deadline) {
			_, rpcError := g.Task(id)
			if errors.Is(rpcError, errQueueTransportUnavailable) {
				_, allowed, handoverError := queueHandoverFollowTask(g, id, expectedToken)
				if handoverError != nil {
					return handoverError
				}
				if allowed {
					outage.reset()
					waitingForHandover = true
					deadline = time.Now().Add(400 * time.Millisecond)
					continue
				}
				// The job already finished; a slow daemon must not turn that into
				// a failed cleanup while the daemon can still complete it.
				if outage.lose() {
					deadline = time.Now().Add(outage.delay())
					continue
				}
				return fmt.Errorf("Job %d finished but cleanup did not complete; the queue daemon has not responded for %s, so its shared slot stays retained until the daemon returns (inspect the job with: agent-heavy logs %d): %w", id, outage.elapsed(), id, rpcError)
			} else if rpcError == nil && (outage.recover() || waitingForHandover) {
				waitingForHandover = false
				deadline = time.Now().Add(queueCleanupWindow)
				continue
			}
			// A daemon that answers yet leaves the receipt behind is an anomaly, not an
			// outage: the slot really is retained, so this stays a failure.
			return fmt.Errorf("Job %d finished but cleanup did not complete; shared slot retained%s", id, causeSuffix(rpcError))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
func Follow(g *Gate, id int, signals <-chan os.Signal) (int, error) {
	return followWithToken(g, id, signals, "")
}
func followWithToken(g *Gate, id int, signals <-chan os.Signal, expectedToken string) (int, error) {
	offset := int64(0)
	notice := time.Now()
	path := g.LogPath(id)
	var pendingSignal os.Signal
	outage := newQueueOutage(id, os.Stderr)
	// A job whose daemon is gone for good may still have finished; report that
	// result when the store proves it, and otherwise name how to inspect the job.
	endOutage := func(cause error) (int, error) {
		if code, ok := queueStoredOutcome(g, id, expectedToken); ok {
			if _, err := Drain(path, offset); err != nil {
				return 125, err
			}
			fmt.Fprintf(os.Stderr, "agent-heavy: queue daemon unreachable for %s; job %d finished with exit status %d (read from the queue store); its cleanup completes when the daemon returns\n", outage.elapsed(), id, code)
			return code, nil
		}
		detail := ""
		if pendingSignal != nil {
			detail = "; your interrupt was not delivered, so the job was not cancelled"
		}
		return 125, fmt.Errorf("Lost contact with the queue daemon for %s; job %d may still be running%s; inspect it with: agent-heavy logs %d: %w", outage.elapsed(), id, detail, id, cause)
	}
	for {
		var err error
		offset, err = Drain(path, offset)
		if err != nil {
			return 125, err
		}
		task, err := g.Task(id)
		rpcAvailable := err == nil
		if err != nil && errors.Is(err, errQueueTransportUnavailable) {
			retained, allowed, handoverError := queueHandoverFollowTask(g, id, expectedToken)
			if handoverError != nil {
				return 125, handoverError
			}
			if allowed {
				task, err = retained, nil
				outage.reset()
			} else {
				// The job was submitted and keeps its place in the daemon; only
				// this client lost sight of it. Keep following, within the bound.
				if !outage.lose() {
					return endOutage(err)
				}
				select {
				case sig := <-signals:
					// Nothing can be cancelled while the daemon is unreachable. The first
					// interrupt is held for when it returns; a second ends the wait.
					if pendingSignal != nil {
						return 125, fmt.Errorf("Interrupted while the queue daemon was unreachable; the cancellation was not delivered and job %d may still be running; inspect it with: agent-heavy logs %d: %w", id, id, err)
					}
					pendingSignal = sig
					fmt.Fprintf(os.Stderr, "agent-heavy: interrupt received; job %d will be cancelled when the queue daemon responds (interrupt again to stop waiting)\n", id)
				case <-time.After(outage.delay()):
				}
				continue
			}
		}
		if err != nil {
			return 125, err
		}
		recovered := rpcAvailable && outage.recover()
		if expectedToken == "" {
			expectedToken = text(task["token"])
		} else if got := text(task["token"]); task != nil && got != "" && got != expectedToken {
			// The queue may have been reset while it was unreachable; a different
			// job under this ID is not ours to report or cancel.
			return 125, fmt.Errorf("Job %d is no longer the command this client submitted; its result was not used", id)
		}
		if pendingSignal != nil && rpcAvailable {
			if err := cancelWithSignals(g, id, signals); err == nil {
				return 128 + int(pendingSignal.(syscall.Signal)), nil
			} else if !errors.Is(err, errQueueHandoverBusy) {
				return 125, err
			}
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
			code, ok := integer(cancelled["exit_code"])
			if !ok || code < 0 || code > 255 {
				return 125, errors.New("Invalid cancellation result")
			}
			if err = waitCleanupWithToken(g, id, expectedToken); err != nil {
				if !errors.Is(err, errQueueTransportUnavailable) {
					return 125, err
				}
				// Only the daemon's absence blocks cleanup; the result is known.
				fmt.Fprintln(os.Stderr, "agent-heavy:", err)
			}
			_, err = Drain(path, offset)
			if err != nil {
				return 125, err
			}
			return code, nil
		}
		if name == "Done" && pendingSignal == nil {
			offset, err = Drain(path, offset)
			if err != nil {
				return 125, err
			}
			code, err := ResultCode(details["result"])
			if err != nil {
				return 125, err
			}
			if err = waitCleanupWithToken(g, id, expectedToken); err != nil {
				if !errors.Is(err, errQueueTransportUnavailable) {
					return 125, err
				}
				// Only the daemon's absence blocks cleanup; the job's own result is
				// known and must not be replaced by a failure status.
				fmt.Fprintln(os.Stderr, "agent-heavy:", err)
			}
			_, err = Drain(path, offset)
			return code, err
		}
		if recovered && name == "Queued" {
			announcePausedQueue(g, id)
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
			if !rpcAvailable {
				pendingSignal = sig
				continue
			}
			if err := cancelWithSignals(g, id, signals); err != nil {
				if errors.Is(err, errQueueHandoverBusy) {
					pendingSignal = sig
					continue
				}
				return 125, err
			}
			return 128 + int(sig.(syscall.Signal)), nil
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// queueCancelGrace is how long a cancelled job may take to stop before it is killed.
var queueCancelGrace = 3 * time.Second

// announcePausedQueue explains a job that stays queued after the daemon came
// back: a restart after an unclean stop pauses the queue until an operator acts.
func announcePausedQueue(g *Gate, id int) {
	snapshot, err := g.Snapshot()
	if err == nil && obj(obj(snapshot["groups"])["default"])["status"] == "Paused" {
		fmt.Fprintf(os.Stderr, "agent-heavy: the queue is paused (a daemon restart after an unclean stop pauses it); job %d will not start until you run: agent-heavy resume\n", id)
	}
}

func Cancel(g *Gate, id int) error { return cancelWithSignals(g, id, nil) }

// cancelWithSignals also stops waiting out a daemon outage when the caller is
// interrupted; a nil channel never fires.
func cancelWithSignals(g *Gate, id int, signals <-chan os.Signal) error {
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
	var cancelSent time.Time
	outage := newQueueOutage(id, os.Stderr)
	// A transport failure does not say whether the cancellation took effect. Retry
	// within the outage bound, without charging the lost time to the deadline.
	transient := func(err error, started time.Time) error {
		if !errors.Is(err, errQueueTransportUnavailable) {
			return err
		}
		if !outage.lose() {
			return fmt.Errorf("Cancellation of job %d was not delivered: the queue daemon has been unreachable for %s; the job may still be running; inspect it with: agent-heavy logs %d: %w", id, outage.elapsed(), id, err)
		}
		select {
		case <-signals:
			return fmt.Errorf("Interrupted while the queue daemon was unreachable; the cancellation of job %d was not delivered and it may still be running; inspect it with: agent-heavy logs %d: %w", id, id, err)
		case <-time.After(outage.delay()):
		}
		deadline = deadline.Add(time.Since(started))
		return nil
	}
	for time.Now().Before(deadline) {
		started := time.Now()
		task, err := g.Task(id)
		if err != nil {
			if err = transient(err, started); err != nil {
				return err
			}
			continue
		}
		name, details, err := TaskStatus(task)
		if err != nil {
			return err
		}
		// A read alone does not end an outage: a daemon that answers snapshots but
		// loses mutations must still reach the bound. Only a delivered mutation, or
		// a terminal state that needs none, counts as recovery.
		switch name {
		case "Done":
			outage.recover()
			code, err := ResultCode(details["result"])
			if err != nil {
				return err
			}
			if err = WritePrivate(path, Object{"task_id": id, "exit_code": code}); err != nil {
				return err
			}
			return WaitCleanup(g, id)
		case "Missing":
			outage.recover()
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
			// SIGKILL only follows a delivered cancel that outlived the grace period;
			// a lost response is no evidence that the job was ever asked to stop.
			op := "cancel"
			if !cancelSent.IsZero() && time.Since(cancelSent) > queueCancelGrace {
				op = "kill"
			}
			_, err = g.QueueControl(op, Object{"id": id})
			if err != nil {
				if err = transient(err, started); err != nil {
					return err
				}
				continue
			}
			outage.recover()
			if op == "cancel" && cancelSent.IsZero() {
				cancelSent = time.Now()
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
	return RunInLane(g, argv, label, "")
}

// RunInLane is Run in a plugin lane. Without a lane it is exactly Run.
func RunInLane(g *Gate, argv []string, label, lane string) (code int, err error) {
	if lane == defaultLaneName {
		lane = ""
	}
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
	if lane != "" {
		// An older daemon ignores unknown submission fields; refuse before it
		// could run the command in the default lane.
		if obj(snapshot["lanes"]) == nil {
			fmt.Fprintln(os.Stderr, "agent-heavy:", errLanesUnsupported)
			return laneExitUsage, nil
		}
		if definition := snapshotLane(snapshot, lane); definition == nil || definition["removed"] == true {
			fmt.Fprintln(os.Stderr, "agent-heavy:", unknownLaneError(snapshot, lane))
			return laneExitUsage, nil
		}
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
	caller, err := queueHandoverIdentity()
	if err != nil {
		return 125, err
	}
	content := Object{"argv": argv, "cwd": cwd, "env": forwarded, "token": jobToken, "label": description["title"], "explicit_label": label != "", "client_handover_protocol": queueHandoverProtocol, "client_handover_caller": caller}
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
	// Lane submissions use their own operation, which an older daemon rejects
	// outright instead of queueing the command in the default lane.
	operation, request := "submit", Object{"token": jobToken}
	if lane != "" {
		operation, request = "submit-lane", Object{"token": jobToken, "lane": lane}
	}
	added, err := g.QueueControl(operation, request)
	// A lost response does not establish whether submission committed. Reconcile
	// using the same immutable token; the daemon returns the original identity.
	for retry := 0; err != nil && !errors.Is(err, errQueueHandoverBusy) && !errors.Is(err, errQueueUnknownLane) && retry < 2; retry++ {
		added, err = g.QueueControl(operation, request)
	}
	if err != nil {
		unlock()
		if errors.Is(err, errQueueHandoverBusy) || errors.Is(err, errQueueUnknownLane) {
			submitted = false
		}
		if errors.Is(err, errQueueUnknownLane) {
			fmt.Fprintln(os.Stderr, "agent-heavy:", unknownLaneError(snapshot, lane))
			return laneExitUsage, nil
		}
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
	return followWithToken(g, id, signals, jobToken)
}
func Execute(g *Gate, value string) (int, error) {
	return executeWithAttempt(g, value, "")
}
func executeWithAttempt(g *Gate, value, attempt string) (int, error) {
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
		if err == nil {
			var acquired bool
			acquired, err = AcquireLease(g, marker, capacity)
			if err == nil && acquired {
				break
			}
		}
		if err != nil {
			if !errors.Is(err, errQueueTransportUnavailable) || !validToken(attempt) {
				return 125, err
			}
			allowed, handoverErr := queueHandoverWorkerWait(g, id, value, attempt)
			if handoverErr != nil {
				return 125, handoverErr
			}
			if !allowed {
				return 125, err
			}
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

func causeSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}
