package backend

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	drainWait     = "wait"
	drainFail     = "fail"
	drainDirect   = "direct"
	drainDefault  = 4 * time.Hour
	drainMaximum  = 24 * time.Hour
	drainExitFail = 75
)

// errQueueDraining is the daemon's refusal of new work while intake is closed.
var errQueueDraining = errors.New("Queue is draining; no new work is accepted")

// drainNoticePeriod spaces the progress notices of a waiting caller; tests shorten it.
var drainNoticePeriod = 15 * time.Second

var errDrainPolicy = errors.New("--on-drain must be wait, fail or direct")

func drainPath(g *Gate) string { return filepath.Join(g.State, "drain.json") }

// drainExpiry rounds up to the second the file stores, so a drain lasts at least d.
func drainExpiry(now time.Time, d time.Duration) time.Time {
	return now.UTC().Add(d + time.Second - 1).Truncate(time.Second)
}

func drainClock(t time.Time) string { return t.Local().Format("15:04") }

func signalExit(sig os.Signal) int {
	number, _ := sig.(syscall.Signal)
	return 128 + int(number)
}

// drainUntil reports whether intake is closed. A missing file or a past expiry
// is no drain; an unsafe or invalid file is an error so that callers fail
// closed. The queue daemon calls it too, so it takes no lock.
func drainUntil(g *Gate) (time.Time, bool, error) {
	value, err := ReadPrivate(drainPath(g))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("Cannot read the drain file: %w", err)
	}
	raw, ok := value["until"].(string)
	until, parseErr := time.Parse(time.RFC3339, raw)
	if len(value) != 1 || !ok || parseErr != nil {
		return time.Time{}, false, errors.New("Invalid drain file; run agent-heavy undrain to remove it")
	}
	return until, until.After(time.Now()), nil
}

// drainHolds is true only for a valid active drain; callers use it to keep
// waiting through a daemon gap, so a bad file must not extend any wait.
func drainHolds(g *Gate) bool {
	_, draining, err := drainUntil(g)
	return err == nil && draining
}

// refuseDraining is the daemon-side gate for new submissions and leases.
func (d *queueDaemon) refuseDraining() error {
	until, draining, err := drainUntil(d.g)
	if err != nil {
		// Refused like a drain, so a caller re-reads the file and fails closed.
		return fmt.Errorf("%w: %v", errQueueDraining, err)
	}
	if draining {
		return fmt.Errorf("%w until %s", errQueueDraining, drainClock(until))
	}
	return nil
}

// applyDrainPolicy handles a caller that meets an active drain. done is false
// only when a wait ended and the submission should go ahead. signals must
// already be registered with signal.Notify.
func applyDrainPolicy(g *Gate, policy string, argv []string, lane string, until time.Time, signals <-chan os.Signal) (code int, done bool, err error) {
	switch policy {
	case drainFail:
		fmt.Fprintf(os.Stderr, "agent-heavy: queue draining until %s; no job was created (--on-drain fail)\n", drainClock(until))
		return drainExitFail, true, nil
	case drainDirect:
		code, err = runDirect(argv, lane, until, signals)
		return code, true, err
	}
	return waitForDrain(g, until, signals)
}

// waitForDrain blocks until the drain ends.
func waitForDrain(g *Gate, until time.Time, signals <-chan os.Signal) (int, bool, error) {
	notice := time.Time{}
	for {
		if time.Since(notice) >= drainNoticePeriod {
			fmt.Fprintf(os.Stderr, "agent-heavy: queue draining until %s; waiting to submit, no job created yet (use --on-drain fail or direct to change this; agent-heavy undrain ends a drain that is no longer wanted)\n", drainClock(until))
			notice = time.Now()
		}
		select {
		case sig := <-signals:
			return signalExit(sig), true, nil
		case <-time.After(500 * time.Millisecond):
		}
		var draining bool
		var err error
		if until, draining, err = drainUntil(g); err != nil {
			return 125, true, err
		}
		if !draining {
			return 0, false, nil
		}
	}
}

// runDirect runs the command in the caller, outside the queue, with the
// environment and exit statuses a queued worker would give it. The child leads
// its own session and process group, which receives every forwarded signal, so
// descendants stop with it and a terminal Ctrl-C arrives once. Having no
// controlling terminal, it cannot be stopped by a background tty read or write.
func runDirect(argv []string, lane string, until time.Time, signals <-chan os.Signal) (int, error) {
	if lane != "" {
		fmt.Fprintf(os.Stderr, "agent-heavy: --on-drain direct cannot use lane %q because lanes protect exclusive resources; no command was run\n", lane)
		return laneExitUsage, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 125, err
	}
	environment := queueCommandEnvironment(forwardedEnvironment(), token())
	fmt.Fprintf(os.Stderr, "agent-heavy: queue draining until %s; running this command outside the queue (--on-drain direct)\n", drainClock(until))
	// SIGQUIT is forwarded for this path only; elsewhere it keeps Go's default.
	quits := make(chan os.Signal, 2)
	signal.Notify(quits, syscall.SIGQUIT)
	defer signal.Stop(quits)
	stop := make(chan struct{})
	defer close(stop)
	err = runExecutableObserved(argv, cwd, environment, os.Stdout, os.Stderr, &executableHooks{
		prepare: func(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} },
		started: func(cmd *exec.Cmd) {
			go func() {
				for {
					select {
					case sig := <-signals:
						if number, ok := sig.(syscall.Signal); ok {
							_ = syscall.Kill(-cmd.Process.Pid, number)
						}
					case <-quits:
						_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGQUIT)
					case <-stop:
						return
					}
				}
			}()
		},
	})
	code, err := commandExitCode(err)
	if err != nil {
		return 125, err
	}
	return code, nil
}

type drainOptions struct {
	duration                          time.Duration
	forGiven                          bool
	wait, cancelQueued, cancelRunning bool
}

const drainUsage = "Usage: agent-heavy drain [--for DURATION] [--wait] [--cancel-queued] [--cancel-running]"

func parseDrainArguments(args []string) (drainOptions, error) {
	options := drainOptions{duration: drainDefault}
	for i := 0; i < len(args); i++ {
		switch value, found := strings.CutPrefix(args[i], "--for="); {
		case args[i] == "--wait":
			options.wait = true
		case args[i] == "--cancel-queued":
			options.cancelQueued = true
		case args[i] == "--cancel-running":
			options.cancelRunning = true
		case found || args[i] == "--for":
			if !found {
				if i++; i == len(args) {
					return options, errors.New("--for requires a duration")
				}
				value = args[i]
			}
			duration, err := time.ParseDuration(value)
			if err != nil || duration < time.Second || duration > drainMaximum {
				return options, errors.New("--for must be a duration of at least 1s and at most 24h, such as 4h or 90m")
			}
			options.duration, options.forGiven = duration, true
		default:
			return options, errors.New(drainUsage)
		}
	}
	return options, nil
}

// closedNote tells an operator whose drain command failed that intake is still
// closed, and how to open it again.
func closedNote(g *Gate, err error) error {
	until, draining, readErr := drainUntil(g)
	if readErr != nil || !draining {
		return err
	}
	return fmt.Errorf("%w; intake stays closed until %s, run agent-heavy undrain to reopen it", err, drainClock(until))
}

// DrainMain closes intake without stopping the queue. Only the flags that act
// on queued work need a daemon. Without --for, a drain that is already active
// keeps its expiry, so these flags can never shorten it.
func DrainMain(g *Gate, args []string) (int, error) {
	options, err := parseDrainArguments(args)
	if err != nil {
		return 125, err
	}
	if os.Getenv("AGENT_HEAVY_ACTIVE") != "" {
		if options.wait {
			return 125, errors.New("drain --wait cannot run inside a queued command: it would wait for its own job to finish; run it outside the queue")
		}
		if options.cancelRunning {
			return 125, errors.New("drain --cancel-running cannot run inside a queued command: it would cancel its own job partway through; run it outside the queue")
		}
	}
	if err = g.Prepare(); err != nil {
		return 125, err
	}
	until, active, readErr := drainUntil(g)
	if active && !options.forGiven && readErr == nil {
		fmt.Fprintf(os.Stderr, "agent-heavy: already draining until %s; the expiry is unchanged (pass --for to replace it)\n", drainClock(until))
	} else {
		until = drainExpiry(time.Now(), options.duration)
		if err = WritePrivate(drainPath(g), Object{"until": until.Format(time.RFC3339)}); err != nil {
			return 125, err
		}
		fmt.Fprintf(os.Stderr, "agent-heavy: draining until %s; new run calls wait, queued and running jobs continue\n", drainClock(until))
	}
	if options.cancelQueued || options.cancelRunning {
		if err = cancelForDrain(g, options); err != nil {
			return 125, closedNote(g, err)
		}
	}
	if options.wait {
		code, err := waitDrained(g)
		if err != nil {
			err = closedNote(g, err)
		}
		return code, err
	}
	return 0, nil
}

// cancelForDrain cancels what has not started in one daemon pass, then what
// is running one by one, so nothing queued starts in the place of a job that
// was just stopped.
func cancelForDrain(g *Gate, options drainOptions) error {
	queued, running := 0, 0
	if options.cancelQueued {
		result, err := g.QueueControl("cancel-queued", nil)
		if err != nil {
			return err
		}
		queued, _ = integer(result["cancelled"])
	}
	if options.cancelRunning {
		snapshot, err := g.Snapshot()
		if err != nil {
			return err
		}
		tasks := obj(snapshot["tasks"])
		ids := []int{}
		for key := range tasks {
			if id, err := strconv.Atoi(key); err == nil {
				ids = append(ids, id)
			}
		}
		sort.Ints(ids)
		for _, id := range ids {
			name, _, err := TaskStatus(obj(tasks[strconv.Itoa(id)]))
			if err != nil {
				return err
			}
			if name != "Running" && name != "Paused" {
				continue
			}
			if err = Cancel(g, id); err != nil {
				return err
			}
			running++
		}
	}
	fmt.Fprintf(os.Stderr, "agent-heavy: cancelled %d queued and %d running or held item(s)\n", queued, running)
	return nil
}

// waitDrained returns once every task is Done and its cleanup receipt is gone.
// The snapshot does not expose the outcome phase, so receipts decide it.
func waitDrained(g *Gate) (int, error) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	notice := time.Time{}
	for {
		snapshot, err := g.Snapshot()
		if err != nil {
			return 125, err
		}
		tasks := obj(snapshot["tasks"])
		receipts, err := os.ReadDir(g.Receipts)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 125, err
		}
		var pending *installDrainPendingError
		if err = installRequireDrained(tasks); err != nil && !errors.As(err, &pending) {
			return 125, err
		}
		if pending == nil && len(receipts) == 0 {
			fmt.Fprintln(os.Stderr, "agent-heavy: queue drained; every job and its cleanup is finished")
			return 0, nil
		}
		if time.Since(notice) >= drainNoticePeriod {
			unfinished, held := 0, []string{}
			for key, raw := range tasks {
				name, _, err := TaskStatus(obj(raw))
				if err != nil {
					return 125, err
				}
				if name != "Done" {
					unfinished++
					if name == "Running" && taskIsLease(obj(raw)) {
						held = append(held, fmt.Sprintf("lease %s (lane %s)", key, taskLane(obj(raw))))
					}
				}
			}
			sort.Strings(held)
			message := fmt.Sprintf("agent-heavy: waiting for the queue to empty: %d unfinished, %d cleanup receipt(s) pending", unfinished, len(receipts))
			if len(held) > 0 {
				message += "; held " + strings.Join(held, ", ") + " blocks it"
			}
			if obj(obj(snapshot["groups"])["default"])["status"] == "Paused" {
				message += "; the queue is paused, so queued jobs will not start until agent-heavy resume or agent-heavy undrain"
			}
			fmt.Fprintln(os.Stderr, message)
			notice = time.Now()
		}
		if _, draining, err := drainUntil(g); err != nil {
			return 125, err
		} else if !draining {
			fmt.Fprintln(os.Stderr, "agent-heavy: drain ended before the queue emptied; intake has reopened")
			return 1, nil
		}
		select {
		case sig := <-signals:
			fmt.Fprintln(os.Stderr, "agent-heavy: stopped waiting; the drain stays in place until it expires or agent-heavy undrain runs")
			return signalExit(sig), nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

const undrainUsage = "Usage: agent-heavy undrain [--force]"

// UndrainMain reopens intake. With an active drain it resumes the queue first
// and removes the drain only afterwards, so waiting callers are never released
// into a queue that cannot take them. Without an active drain it changes
// nothing: a pause after an unclean restart needs explicit operator recovery.
func UndrainMain(g *Gate, args []string) (int, error) {
	force := len(args) == 1 && args[0] == "--force"
	if len(args) > 1 || len(args) == 1 && !force {
		return 125, errors.New(undrainUsage)
	}
	until, draining, readErr := drainUntil(g)
	if readErr != nil || !draining {
		// An expired or invalid file is stale or unsafe; remove it and stop there.
		if err := os.Remove(drainPath(g)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 125, err
		}
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "agent-heavy: removed an invalid drain file; the queue was not changed")
		} else {
			fmt.Fprintln(os.Stderr, "agent-heavy: no drain is active; the queue was not changed")
		}
		return 0, nil
	}
	snapshot, err := g.Snapshot()
	if errors.Is(err, errQueueTransportUnavailable) {
		if !force {
			fmt.Fprintf(os.Stderr, "agent-heavy: the queue daemon is not running, so the drain (until %s) stays and waiting callers stay attached; open Agent Queue, wait until the queue is up, then run agent-heavy undrain again, or pass --force to drop the drain anyway (waiting callers then exit 125)\n", drainClock(until))
			return 1, nil
		}
		if err = os.Remove(drainPath(g)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 125, err
		}
		fmt.Fprintln(os.Stderr, "agent-heavy: drain removed without a daemon; nothing was resumed, and if the queue restarts paused, run agent-heavy resume")
		return 0, nil
	}
	if err != nil {
		return 125, err
	}
	wasPaused := obj(obj(snapshot["groups"])["default"])["status"] == "Paused"
	if _, err = g.QueueControl("resume", nil); err != nil {
		return 125, fmt.Errorf("%w; the drain stays in place", err)
	}
	if err = os.Remove(drainPath(g)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 125, err
	}
	fmt.Fprintln(os.Stderr, "agent-heavy: drain removed; new run calls are accepted")
	if wasPaused {
		fmt.Fprintln(os.Stderr, "agent-heavy: the queue was paused and is resumed; this also ends a pause set with agent-heavy pause")
	}
	return 0, nil
}
