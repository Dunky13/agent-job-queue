package backend

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// A submitted job lives in the daemon, not in the client. When the daemon is
// slow or briefly gone (a supervisor restart waits 10 seconds, a handover
// custody window lasts minutes), the job is still queued or running and only
// this client lost sight of it. Waiting commands therefore ride out transport
// failures for a bounded time instead of ending with a status that looks like a
// job failure; a daemon gone longer ends visibly.
//
// The window is chosen from how a stopped daemon comes back: the menu bar app
// restarts it after at most a 10 second throttle (AgentSupervisor.swift), and
// startup adds the database open and recovery. Two minutes covers several
// restart attempts with room for a slow, swapping host.
const queueOutageWindow = 2 * time.Minute

// queueOutageBound is a variable only so tests can shorten it.
var queueOutageBound = queueOutageWindow

// queueOutageBackoff is the pause before the next attempt after n consecutive
// transport failures: short first, then capped so recovery is noticed promptly.
var queueOutageBackoff = func(n int) time.Duration {
	return min(400*time.Millisecond<<min(n, 3), 3*time.Second)
}

// queueOutage tracks one continuous loss of the queue daemon for a waiting
// command. Its only definition of recovery is an iteration without a transport
// failure anywhere. Notices are rate limited so a flapping daemon stays quiet.
type queueOutage struct {
	id        int
	noun      string
	out       io.Writer
	since     time.Time
	noticed   time.Time
	failures  int
	announced bool
	lostOnce  bool
}

// queueOutageNoticeGap is the quiet period after a notice pair.
const queueOutageNoticeGap = 30 * time.Second

func newQueueOutage(id int, out io.Writer) *queueOutage {
	return &queueOutage{id: id, noun: "job", out: out}
}

// lose records a transport failure and reports whether the caller may keep
// waiting, that is whether the outage is still within the bound.
func (o *queueOutage) lose(cause error) bool {
	if o.since.IsZero() {
		o.since = time.Now()
	}
	o.lostOnce = true
	if !o.announced && (o.noticed.IsZero() || time.Since(o.noticed) >= queueOutageNoticeGap) {
		o.announced, o.noticed = true, time.Now()
		o.say(cause, "")
	}
	o.failures++
	return time.Since(o.since) <= queueOutageBound
}

// say prints the loss notice; extra qualifies what is lost, such as a drain.
func (o *queueOutage) say(cause error, extra string) {
	what := "queue daemon not responding"
	if errors.Is(cause, errQueueHandoverBusy) {
		what = "queue handover in progress"
	}
	fmt.Fprintf(o.out, "agent-heavy: %s%s; still waiting for %s %d\n", what, extra, o.noun, o.id)
}

// hold records a loss that a drain explains. It speaks like lose, repeating the
// notice every drainNoticePeriod because a drain can last hours, but it never
// starts the bound: that begins with the first loss after the drain is over.
func (o *queueOutage) hold(cause error) {
	o.lostOnce = true
	if !o.announced || time.Since(o.noticed) >= drainNoticePeriod {
		o.announced, o.noticed = true, time.Now()
		o.say(cause, " (drain active)")
	}
	o.since = time.Time{}
	o.failures++
}

// delay is the pause before the next attempt.
func (o *queueOutage) delay() time.Duration { return queueOutageBackoff(o.failures - 1) }

// elapsed is how long the daemon has been unreachable.
func (o *queueOutage) elapsed() time.Duration { return time.Since(o.since).Round(time.Second) }

// recover ends the outage and reports whether one was in progress, including a
// loss that reset explained but the daemon has not yet answered.
func (o *queueOutage) recover() bool {
	was := !o.since.IsZero() || o.announced
	o.reset()
	if o.announced {
		o.announced, o.noticed = false, time.Now()
		fmt.Fprintf(o.out, "agent-heavy: queue answering again (%s %d)\n", o.noun, o.id)
	}
	return was
}

// reset restarts the bound without a notice, for a loss that an authenticated
// handover explains. The loss notice stays pending until the daemon answers.
func (o *queueOutage) reset() {
	o.since = time.Time{}
	o.failures = 0
}

// queueStoredOutcome reads the job's terminal outcome straight from the queue
// store, read-only, the way handover inspection does: no migration, no write
// and no lock the daemon holds. It reports false unless the stored job carries
// exactly the submitted token and is terminal, so any doubt keeps the caller's
// "may still be running" report.
func queueStoredOutcome(g *Gate, id int, token string) (int, bool) {
	if !validToken(token) {
		return 0, false
	}
	store, err := queueReadHandoverStore(g)
	if err != nil {
		return 0, false
	}
	defer store.close()
	job, err := store.job(id)
	if err != nil || job.token != token {
		return 0, false
	}
	switch job.phase {
	case "done":
		name, details, err := TaskStatus(job.task)
		if err != nil || name != "Done" {
			return 0, false
		}
		code, err := ResultCode(details["result"])
		return code, err == nil
	case "outcome":
		if job.outcome.Valid && job.outcome.Int64 >= 0 && job.outcome.Int64 <= 255 {
			return int(job.outcome.Int64), true
		}
	}
	return 0, false
}
