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
	return waitCleanupWithToken(g, id, text(receipt["token"]), nil)
}

// queueCleanupWindow is how long a finished job may wait for the daemon to
// release its receipt before the daemon is probed.
var queueCleanupWindow = 12 * time.Second

// queueCleanupUnconfirmed says a finished job's cleanup is not confirmed. The
// job's own result is known, so followers report it with this as a note.
type queueCleanupUnconfirmed struct {
	message string
	cause   error
}

func (e *queueCleanupUnconfirmed) Error() string {
	if e.cause == nil {
		return e.message
	}
	return e.message + ": " + e.cause.Error()
}
func (e *queueCleanupUnconfirmed) Unwrap() error { return e.cause }

// waitCleanupWithToken waits for a finished job's receipt to go. While a drain
// holds, a caller that passes signals keeps waiting through a daemon outage, as
// it does in the follow loop, and a signal ends the wait with a note that the
// job finished. Without signals (Cancel, WaitCleanup) the wait stays bounded,
// because nothing else could end it during a long drain.
func waitCleanupWithToken(g *Gate, id int, expectedToken string, signals <-chan os.Signal) error {
	deadline := time.Now().Add(queueCleanupWindow)
	waitingForHandover := false
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
				if allowed || signals != nil && drainHolds(g) {
					waitingForHandover = true
					deadline = time.Now().Add(400 * time.Millisecond)
					continue
				}
				// The job's result is already known to the caller, which decides
				// what to report; this only says why cleanup is unconfirmed.
				return &queueCleanupUnconfirmed{fmt.Sprintf("Job %d finished but its cleanup could not be confirmed, so its shared slot may stay retained until the queue daemon answers (inspect the job with: agent-heavy logs %d)", id, id), rpcError}
			} else if rpcError == nil && waitingForHandover {
				waitingForHandover = false
				deadline = time.Now().Add(queueCleanupWindow)
				continue
			}
			if rpcError == nil {
				// The daemon answers and keeps retrying cleanup, and the job holds its
				// slot until it finishes. The job's result is known, so the caller reports
				// it; a 125 here would add no safety.
				return &queueCleanupUnconfirmed{fmt.Sprintf("Job %d finished but its cleanup is not confirmed: the queue daemon answers and still holds its receipt, so its shared slot stays retained until the daemon completes cleanup (inspect the job with: agent-heavy logs %d)", id, id), nil}
			}
			return fmt.Errorf("Job %d finished but cleanup did not complete; shared slot retained%s", id, causeSuffix(rpcError))
		}
		// A drain lets a signal end the wait; elsewhere it stays for the caller.
		var interrupts <-chan os.Signal
		if signals != nil && drainHolds(g) {
			interrupts = signals
		}
		select {
		case <-interrupts:
			return &queueCleanupUnconfirmed{fmt.Sprintf("Detaching during a drain: job %d finished, and its cleanup completes once the queue daemon confirms it", id), nil}
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func Follow(g *Gate, id int, signals <-chan os.Signal) (int, error) {
	return followWithToken(g, id, signals, "")
}

// followWithToken is the only place that rides out a queue outage. One outage
// budget covers every step of a continuous outage: reading the job, delivering a
// held interrupt and the cancellation it triggers. The budget is per continuous
// outage, not per run: an iteration without a transport failure anywhere is the
// only recovery, so alternating failures cannot restart the clock. Its clock
// starts when the first timed-out call returns, and calls already in flight (the
// read, the cancellation's read and mutation, and a cleanup probe, up to 15
// seconds each) finish after it passes, so a hung daemon ends the run in under
// three minutes.
func followWithToken(g *Gate, id int, signals <-chan os.Signal, expectedToken string) (int, error) {
	offset := int64(0)
	notice := time.Now()
	path := g.LogPath(id)
	var pendingSignal os.Signal
	pauseShown, pauseCheckLost := false, false
	outage := newQueueOutage(id, os.Stderr)
	// cancelSentAt is the first cancel the daemon acknowledged in this run. After
	// one, a held interrupt reports the interrupt status as it always has, so a
	// network blip cannot decide the exit status. With none acknowledged, a job seen
	// finished reports its own result.
	var cancelSentAt time.Time
	cancelTried := false // a cancel was sent, whether or not the daemon acknowledged it
	finished := func(code int) (int, error) {
		switch {
		case pendingSignal == nil:
			return code, nil
		case !cancelSentAt.IsZero():
			fmt.Fprintf(os.Stderr, "agent-heavy: job %d finished with exit status %d after its cancellation was acknowledged; reporting the interrupt status\n", id, code)
			return 128 + int(pendingSignal.(syscall.Signal)), nil
		}
		fmt.Fprintf(os.Stderr, "agent-heavy: job %d finished with exit status %d; no cancellation was confirmed\n", id, code)
		return code, nil
	}
	// cleanupNote reports a cleanup the daemon has not confirmed without hiding
	// the job's known result; any other cleanup error is a failure.
	cleanupNote := func(err error, code int) error {
		var unconfirmed *queueCleanupUnconfirmed
		if !errors.As(err, &unconfirmed) {
			return err
		}
		fmt.Fprintf(os.Stderr, "agent-heavy: %v; the job's own exit status is %d\n", err, code)
		return nil
	}
	// stored reports a job whose result the queue store already holds, which a
	// caller that cannot reach the daemon must not lose.
	stored := func() (int, error, bool) {
		code, ok := queueStoredOutcome(g, id, expectedToken)
		if !ok {
			return 0, nil, false
		}
		if _, err := Drain(path, offset); err != nil {
			return 125, err, true
		}
		fmt.Fprintf(os.Stderr, "agent-heavy: queue daemon not answering; job %d finished with exit status %d (read from the queue store); its cleanup completes when the daemon answers\n", id, code)
		code, err := finished(code)
		return code, err, true
	}
	// detach ends an interrupted run during a drain gap without cancelling
	// anything. A result the store already holds is reported instead of the
	// interrupt; otherwise the job is left as it is.
	detach := func(sig os.Signal) (int, error) {
		if code, err, ok := stored(); ok {
			return code, err
		}
		announceDetach(id, cancelTried)
		return signalExit(sig), nil
	}
	// end reports an outage that cannot be waited out. The store may still prove
	// the job finished; otherwise the job may still be running.
	end := func(prefix string, cause error) (int, error) {
		if code, err, ok := stored(); ok {
			return code, err
		}
		message := fmt.Sprintf("%s; job %d may still be running; inspect it with: agent-heavy logs %d", prefix, id, id)
		if cause == nil {
			return 125, errors.New(message)
		}
		return 125, fmt.Errorf("%s: %w", message, cause)
	}
	interruptedAgain := func(context string) string {
		if cancelSentAt.IsZero() {
			return "Interrupted again" + context + "; the cancellation could not be confirmed"
		}
		return "Interrupted again" + context + "; the cancellation was acknowledged but the job's exit could not be confirmed"
	}
	// wait spends one backoff step of the outage budget, or ends the run.
	wait := func(cause error) (ended bool, code int, err error) {
		var unconfirmed *cancelUnconfirmedError
		if errors.As(cause, &unconfirmed) {
			cause = unconfirmed.cause // the end message says it once
		}
		if drainHolds(g) {
			// A drain lets the app restart, so the outage neither spends the budget
			// nor ends the run; the ordinary budget starts when the drain is over.
			// An interrupt leaves the job queued instead of cancelling it, unless a
			// cancellation was already acknowledged, which only the daemon can finish.
			if pendingSignal != nil && cancelSentAt.IsZero() {
				code, err = detach(pendingSignal)
				return true, code, err
			}
			outage.hold(cause)
			select {
			case sig := <-signals:
				if pendingSignal != nil {
					code, err = end(interruptedAgain(" while the queue daemon was not answering"), cause)
					return true, code, err
				}
				code, err = detach(sig)
				return true, code, err
			case <-time.After(outage.delay()):
			}
			return false, 0, nil
		}
		if !outage.lose(cause) {
			prefix := fmt.Sprintf("Lost contact with the queue daemon for %s", outage.elapsed())
			if pendingSignal != nil {
				prefix = fmt.Sprintf("Queue calls kept failing for %s and the cancellation requested by your interrupt could not be confirmed", outage.elapsed())
				if !cancelSentAt.IsZero() {
					prefix = fmt.Sprintf("Queue calls kept failing for %s after the daemon acknowledged the cancellation requested by your interrupt, and the job's exit could not be confirmed", outage.elapsed())
				}
			}
			code, err = end(prefix, cause)
			return true, code, err
		}
		select {
		case sig := <-signals:
			if pendingSignal != nil {
				code, err = end(interruptedAgain(" while the queue daemon was not answering"), cause)
				return true, code, err
			}
			pendingSignal = sig
			fmt.Fprintf(os.Stderr, "agent-heavy: interrupt received; cancelling job %d once the queue daemon answers (interrupt again to stop waiting)\n", id)
		case <-time.After(outage.delay()):
		}
		return false, 0, nil
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
				if ended, code, err := wait(err); ended {
					return code, err
				}
				continue
			}
		}
		if err != nil {
			return 125, err
		}
		if expectedToken == "" {
			expectedToken = text(task["token"])
		} else if got := text(task["token"]); task != nil && got != "" && got != expectedToken {
			// The queue may have been reset while it was unreachable; a different
			// job under this ID is not ours to report or cancel.
			return 125, fmt.Errorf("Job %d is no longer the command this client submitted; its result was not used", id)
		}
		name, details, err := TaskStatus(task)
		if err != nil {
			return 125, err
		}
		if name == "Done" {
			if rpcAvailable {
				outage.recover() // a finished job needs no mutation, so this read is recovery
			}
			offset, err = Drain(path, offset)
			if err != nil {
				return 125, err
			}
			code, err := ResultCode(details["result"])
			if err != nil {
				return 125, err
			}
			if err = cleanupNote(waitCleanupWithToken(g, id, expectedToken, signals), code); err != nil {
				return 125, err
			}
			_, err = Drain(path, offset)
			if err != nil {
				return 125, err
			}
			return finished(code)
		}
		if pendingSignal != nil && rpcAvailable {
			cancelTried = true
			seen, err := cancelJob(g, id, expectedToken, &cancelSentAt)
			switch {
			case seen != nil:
				if err = cleanupNote(err, *seen); err != nil {
					return 125, err
				}
				return finished(*seen)
			case err == nil:
				return 128 + int(pendingSignal.(syscall.Signal)), nil
			case errors.Is(err, errQueueTransportUnavailable), errors.Is(err, errQueueHandoverBusy):
				// A busy handover is bounded like an outage: alternating it with lost
				// replies must not restart the clock.
				if ended, code, err := wait(err); ended {
					return code, err
				}
				continue
			default:
				return 125, err
			}
		}
		recovered := rpcAvailable && outage.recover()
		if name == "Missing" {
			cancelled, err := ReadPrivate(filepath.Join(g.Cancelled, strconv.Itoa(id)+".json"))
			if err != nil {
				return 125, fmt.Errorf("Job %d disappeared before its result could be read", id)
			}
			code, ok := integer(cancelled["exit_code"])
			if !ok || code < 0 || code > 255 {
				return 125, errors.New("Invalid cancellation result")
			}
			if err = cleanupNote(waitCleanupWithToken(g, id, expectedToken, signals), code); err != nil {
				return 125, err
			}
			_, err = Drain(path, offset)
			if err != nil {
				return 125, err
			}
			return code, nil
		}
		tick := time.Since(notice) >= 15*time.Second
		if name == "Queued" && !pauseShown && outage.lostOnce && (recovered || tick || pauseCheckLost) {
			var answered bool
			pauseShown, answered = announcePausedQueue(g, id)
			pauseCheckLost = !answered
		}
		if tick {
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
			if pendingSignal != nil {
				return end(interruptedAgain(""), nil)
			}
			// Delivered at the next poll, which also sees a result that arrived first.
			pendingSignal = sig
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// queueCancelGrace is how long a cancelled job may take to stop before it is killed.
var queueCancelGrace = 3 * time.Second

// announcePausedQueue says what is known when a job waits behind a paused
// queue, without guessing why it is paused. It reports whether it said so and
// whether the daemon answered; the caller retries a check that was lost.
func announcePausedQueue(g *Gate, id int) (shown, answered bool) {
	snapshot, err := g.Snapshot()
	if err != nil {
		return false, false
	}
	if obj(obj(snapshot["groups"])["default"])["status"] != "Paused" {
		return false, true
	}
	fmt.Fprintf(os.Stderr, "agent-heavy: the queue is paused; job %d will not start until the queue is resumed\n", id)
	return true, true
}

// announceDetach says what an interrupted caller leaves behind when a drain
// keeps it waiting for the daemon. Without a cancel attempt the job stays queued,
// and cancelling it is explicit.
func announceDetach(id int, cancelTried bool) {
	if cancelTried {
		// The cancel may have reached the daemon before its reply was lost.
		fmt.Fprintf(os.Stderr, "agent-heavy: detaching; job %d may still be running; check agent-heavy status\n", id)
		return
	}
	fmt.Fprintf(os.Stderr, "agent-heavy: detaching; job %d stays in the queue, use agent-heavy cancel %d (a running job keeps running)\n", id, id)
}

func causeSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// Cancel does not retry through a daemon outage: a standalone caller learns at
// once that the cancellation could not be confirmed. A waiting client retries it.
func Cancel(g *Gate, id int) error {
	_, err := cancelJob(g, id, "", nil)
	return err
}

// cancelUnconfirmedError names what is not known when the daemon went silent:
// the request may or may not have taken effect.
type cancelUnconfirmedError struct {
	id    int
	cause error
}

func (e *cancelUnconfirmedError) Error() string {
	return fmt.Sprintf("Cancellation of job %d could not be confirmed; the job may still be running; inspect it with: agent-heavy logs %d: %v", e.id, e.id, e.cause)
}
func (e *cancelUnconfirmedError) Unwrap() error { return e.cause }

func cancelUnconfirmed(id int, err error) error {
	if !errors.Is(err, errQueueTransportUnavailable) {
		return err
	}
	return &cancelUnconfirmedError{id, err}
}

// cancelJob cancels job id. A job whose token differs from the first one seen
// (or from expectedToken) is refused before anything is written or sent. sentAt
// carries the time of the first cancel the daemon acknowledged, across calls
// when the caller supplies it, so a kill follows an acknowledged cancel and its
// grace period even if reads were lost in between. When the job is seen
// finished, its own exit status is returned beside any cleanup error.
func cancelJob(g *Gate, id int, expectedToken string, sentAt *time.Time) (*int, error) {
	if id < 0 {
		return nil, errors.New("Task ID must be nonnegative")
	}
	if err := g.Prepare(); err != nil {
		return nil, err
	}
	if sentAt == nil {
		sentAt = new(time.Time)
	}
	path := filepath.Join(g.Cancelled, strconv.Itoa(id)+".json")
	deadline := time.Now().Add(12 * time.Second)
	marked := false
	for time.Now().Before(deadline) {
		task, err := g.Task(id)
		if err != nil {
			return nil, cancelUnconfirmed(id, err)
		}
		if got := text(task["token"]); task != nil && got != "" {
			if expectedToken == "" {
				expectedToken = got
			} else if got != expectedToken {
				return nil, fmt.Errorf("Job %d is no longer the job this cancellation targeted; nothing further was cancelled", id)
			}
		}
		// A row with no task has no token to compare, but its receipt does. Without
		// this, a stale receipt under a reused ID would lose its payload and marker.
		if task == nil && expectedToken != "" {
			receipt, err := g.Receipt(id)
			if err != nil {
				return nil, err
			}
			if receipt != nil && text(receipt["token"]) != expectedToken {
				return nil, fmt.Errorf("Job %d is no longer the job this cancellation targeted; nothing further was cancelled", id)
			}
		}
		// The local marker that stands for this cancellation is written only once
		// the job is known to be the targeted one.
		if !marked {
			if err = WritePrivate(path, Object{"task_id": id, "exit_code": 130}); err != nil {
				return nil, err
			}
			marked = true
		}
		name, details, err := TaskStatus(task)
		if err != nil {
			return nil, err
		}
		switch name {
		case "Done":
			code, err := ResultCode(details["result"])
			if err != nil {
				return nil, err
			}
			if err = WritePrivate(path, Object{"task_id": id, "exit_code": code}); err != nil {
				return nil, err
			}
			return &code, WaitCleanup(g, id)
		case "Missing":
			receipt, err := g.Receipt(id)
			if err != nil {
				return nil, err
			}
			if receipt != nil {
				payload, err := g.PayloadPath(text(receipt["token"]))
				if err != nil {
					return nil, err
				}
				if err = removeMissing(payload); err != nil {
					return nil, err
				}
				if receipt["pgid"] == nil {
					if err = removeMissing(g.ReceiptPath(id)); err != nil {
						return nil, err
					}
				}
			}
			return nil, WaitCleanup(g, id)
		case "Queued", "Stashed", "Running", "Paused":
			// SIGKILL only follows a cancel the daemon acknowledged and that outlived
			// the grace period; a lost reply is no evidence the job was told to stop.
			op := "cancel"
			if !sentAt.IsZero() && time.Since(*sentAt) > queueCancelGrace {
				op = "kill"
			}
			if _, err = g.QueueControl(op, Object{"id": id}); err != nil {
				return nil, cancelUnconfirmed(id, err)
			}
			if op == "cancel" && sentAt.IsZero() {
				*sentAt = time.Now()
			}
		default:
			return nil, fmt.Errorf("Job %d has unexpected state %s; inspect the queue before recovery", id, name)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("Cancellation of job %d did not finish; its exclusion lock is retained", id)
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
	return RunInLaneOnDrain(g, argv, label, lane, drainWait)
}

// RunInLaneOnDrain is RunInLane with the caller's choice (wait, fail or direct)
// for a submission that meets an active drain.
func RunInLaneOnDrain(g *Gate, argv []string, label, lane, onDrain string) (code int, err error) {
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
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	// Before the snapshot: a drain needs no daemon, and no job exists yet.
	until, draining, err := drainUntil(g)
	if err != nil {
		return 125, err
	}
	if draining {
		if code, done, err := applyDrainPolicy(g, onDrain, argv, lane, until, signals); done || err != nil {
			return code, err
		}
	}
	snapshot, code, err := submissionPrecheck(g, lane)
	if code != 0 || err != nil {
		return code, err
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
	forwarded := forwardedEnvironment()
	caller, err := queueHandoverIdentity()
	if err != nil {
		return 125, err
	}
	content := Object{"argv": argv, "cwd": cwd, "env": forwarded, "token": jobToken, "label": description["title"], "explicit_label": label != "", "client_handover_protocol": queueHandoverProtocol, "client_handover_caller": caller}
	if err = WriteJobPayload(path, content); err != nil {
		return 125, err
	}
	id := -1
	submitted := false
	discard := func() {
		_ = removeMissing(path)
		_ = removeMissing(filepath.Join(g.State, "workloads", jobToken+".json"))
		_ = removeMissing(filepath.Join(g.Metadata, jobToken+".json"))
	}
	defer func() {
		if id < 0 && !submitted {
			discard()
		}
	}()
	// Lane submissions use their own operation, which an older daemon rejects
	// outright instead of queueing the command in the default lane.
	operation, request := "submit", Object{"token": jobToken}
	if lane != "" {
		operation, request = "submit-lane", Object{"token": jobToken, "lane": lane}
	}
	// The signal handler has been registered since before the drain check, so a
	// signal that arrived during preparation must not turn into a job now.
	select {
	case sig := <-signals:
		return signalExit(sig), nil
	default:
	}
	var unlock func()
	var added Object
	lostNotice := time.Time{}
	// responseLost records that some attempt ended without a definite answer, so
	// that an earlier attempt may have committed. From then on the files stay,
	// whatever a later attempt is told.
	responseLost := false
	attempt := func() (Object, error) {
		result, e := g.QueueControl(operation, request)
		if e != nil && !queueDefiniteAnswer(e) {
			responseLost = true
		}
		return result, e
	}
	unknown := func(e error) error {
		if responseLost {
			return fmt.Errorf("%w; the outcome of the earlier submission is unknown, so a job may already exist, check agent-heavy status", e)
		}
		return e
	}
	for {
		if unlock, err = LockPrivate(g, "scheduling.lock"); err != nil {
			// While a drain holds, a wait caller has no urgency: a lock that another
			// stalled caller keeps is waited for, not a failure.
			if onDrain == drainWait && errors.Is(err, errQueueLockTimeout) && drainHolds(g) {
				select {
				case sig := <-signals:
					if responseLost {
						fmt.Fprintln(os.Stderr, "agent-heavy: stopped before the submission outcome was known; the job may still exist, check agent-heavy status")
					}
					return signalExit(sig), nil
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			return 125, unknown(err)
		}
		if _, err = SchedulingSettings(g); err != nil {
			unlock()
			return 125, unknown(err)
		}
		submitted = true
		added, err = attempt()
		// A lost response does not establish whether submission committed. Reconcile
		// using the same immutable token; the daemon returns the original identity.
		for retry := 0; err != nil && !queueCodedRefusal(err) && retry < 2; retry++ {
			// A wait caller retries a lost daemon below, without keeping the lock.
			if onDrain == drainWait && errors.Is(err, errQueueTransportUnavailable) && drainHolds(g) {
				break
			}
			added, err = attempt()
		}
		// A drain began after the check above: the daemon refused before it
		// recorded the token, so nothing is submitted and the token stays usable.
		refused := errors.Is(err, errQueueDraining)
		// A wait caller keeps retrying the same token while the daemon is away
		// during a drain: the token lookup runs before the drain refusal, so an id
		// means the earlier submit committed and a refusal means it did not.
		// Fail and direct never run on an unknown outcome.
		lost := err != nil && !refused && onDrain == drainWait && errors.Is(err, errQueueTransportUnavailable) && drainHolds(g)
		if !refused && !lost {
			break
		}
		unlock()
		if lost {
			if time.Since(lostNotice) >= drainNoticePeriod {
				until, _, _ := drainUntil(g)
				fmt.Fprintf(os.Stderr, "agent-heavy: queue draining until %s; submission outcome unknown; retrying the same request when the queue returns (a job may already exist, check agent-heavy status)\n", drainClock(until))
				lostNotice = time.Now()
			}
			select {
			case sig := <-signals:
				fmt.Fprintln(os.Stderr, "agent-heavy: stopped before the submission outcome was known; the job may still exist, check agent-heavy status")
				return signalExit(sig), nil
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		submitted = false
		// The token lookup precedes the refusal, so no earlier attempt committed.
		responseLost = false
		if until, draining, err = drainUntil(g); err != nil {
			return 125, err
		}
		if draining {
			if onDrain != drainWait {
				discard()
			}
			if c, done, e := applyDrainPolicy(g, onDrain, argv, lane, until, signals); done || e != nil {
				return c, e
			}
		}
		// The queue may have changed while we waited: check it again before submitting.
		if snapshot, code, err = submissionPrecheck(g, lane); code != 0 || err != nil {
			return code, err
		}
	}
	if err != nil {
		unlock()
		// A daemon that answered the first attempt with an error refused the
		// request, so nothing was created and the files can go. After any lost
		// response a later answer proves nothing about the earlier attempt.
		if queueCodedRefusal(err) || queueDefiniteAnswer(err) && !responseLost {
			submitted = false
		}
		if errors.Is(err, errQueueUnknownLane) {
			fmt.Fprintln(os.Stderr, "agent-heavy:", unknownLaneError(snapshot, lane))
			return laneExitUsage, nil
		}
		return 125, unknown(err)
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

// queueCodedRefusal is a refusal the daemon codes so that callers can branch on it.
func queueCodedRefusal(err error) bool {
	return errors.Is(err, errQueueHandoverBusy) || errors.Is(err, errQueueUnknownLane) || errors.Is(err, errQueueDraining)
}

// queueDefiniteAnswer is any error the daemon itself returned, as opposed to a
// lost or garbled reply, after which an earlier request's outcome is unknown.
func queueDefiniteAnswer(err error) bool {
	var answered *queueAnsweredError
	return queueCodedRefusal(err) || errors.As(err, &answered)
}

// forwardedEnvironment is the caller's environment without queue control variables.
func forwardedEnvironment() map[string]string {
	forwarded := map[string]string{}
	for k, v := range envMap() {
		if !strings.HasPrefix(k, "AGENT_HEAVY_") && !strings.HasPrefix(k, "PUEUE_") {
			forwarded[k] = v
		}
	}
	return forwarded
}

// queueCommandEnvironment marks the command as queued (so nesting is refused)
// and applies the Go resource defaults a queued command gets.
func queueCommandEnvironment(forwarded map[string]string, active string) map[string]string {
	forwarded["AGENT_HEAVY_ACTIVE"] = active
	for k, v := range map[string]string{"GOMAXPROCS": "2", "GOMEMLIMIT": "512MiB", "GOGC": "50"} {
		if _, ok := forwarded[k]; !ok {
			forwarded[k] = v
		}
	}
	return forwarded
}

// commandExitCode maps the result of running a command to its exit status. A
// non-nil error is a failure that is not the command's own.
func commandExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	if exit, ok := err.(*exec.ExitError); ok {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "agent-heavy: command executable or working directory does not exist")
		return 127, nil
	}
	if errors.Is(err, os.ErrPermission) {
		fmt.Fprintln(os.Stderr, "agent-heavy: command executable or working directory is inaccessible")
		return 126, nil
	}
	return 0, err
}

// submissionPrecheck refuses before anything is submitted. A non-zero code ends
// the call with that exit status.
func submissionPrecheck(g *Gate, lane string) (Object, int, error) {
	snapshot, err := g.Snapshot()
	if err != nil {
		return nil, 125, err
	}
	group := defaultGroup(snapshot)
	limit, ok := integer(group["parallel_tasks"])
	if !ok {
		return nil, 125, errors.New("Invalid parallel limit")
	}
	if err = ValidateLimit(limit); err != nil {
		return nil, 125, err
	}
	if group["status"] != "Running" {
		return nil, 125, errors.New("Dedicated default queue must be Running; no command was submitted")
	}
	if lane != "" {
		// An older daemon ignores unknown submission fields; refuse before it
		// could run the command in the default lane.
		if obj(snapshot["lanes"]) == nil {
			fmt.Fprintln(os.Stderr, "agent-heavy:", errLanesUnsupported)
			return snapshot, laneExitUsage, nil
		}
		if definition := snapshotLane(snapshot, lane); definition == nil || definition["removed"] == true {
			fmt.Fprintln(os.Stderr, "agent-heavy:", unknownLaneError(snapshot, lane))
			return snapshot, laneExitUsage, nil
		}
	}
	return snapshot, 0, nil
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
	// Never stopped: a queue worker publishes its outcome after this returns, and Cancel
	// resends SIGINT until the job is done, so a resend must not kill the worker then.
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, syscall.SIGINT)
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
			if !allowed && !drainHolds(g) {
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
	code, err := commandExitCode(runExecutableForEnvironment(argv, cwd, queueCommandEnvironment(forwarded, value), os.Stdout, os.Stderr))
	if err != nil {
		return 125, err
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
