package backend

import (
	"fmt"
	"io"
	"time"
)

// A submitted job lives in the daemon, not in the client. When the daemon is
// slow or briefly gone (a supervisor restart waits 10 seconds, a handover
// custody window lasts minutes), the job is still queued or running and only
// this client lost sight of it. Waiting commands therefore ride out transport
// failures for a bounded time instead of ending with a status that looks like a
// job failure. The bound covers a supervised restart several times over and
// matches the handover prepare window; a daemon gone longer ends visibly.
var queueOutageBound = queueHandoverPrepareLifetime

// queueOutageBackoff is the pause before the next attempt after n consecutive
// transport failures: short first, then capped so recovery is noticed promptly.
var queueOutageBackoff = func(n int) time.Duration {
	return min(400*time.Millisecond<<min(n, 3), 3*time.Second)
}

// queueOutage tracks one continuous loss of the queue daemon for a waiting
// command and prints one notice on first loss and one on recovery.
type queueOutage struct {
	id        int
	out       io.Writer
	since     time.Time
	failures  int
	announced bool
}

func newQueueOutage(id int, out io.Writer) *queueOutage { return &queueOutage{id: id, out: out} }

// lose records a transport failure and reports whether the caller may keep
// waiting, that is whether the outage is still within the bound.
func (o *queueOutage) lose() bool {
	if o.since.IsZero() {
		o.since = time.Now()
	}
	if !o.announced {
		o.announced = true
		fmt.Fprintf(o.out, "agent-heavy: queue daemon not responding; waiting for it to return (job %d is not affected)\n", o.id)
	}
	o.failures++
	return time.Since(o.since) <= queueOutageBound
}

// delay is the pause before the next attempt.
func (o *queueOutage) delay() time.Duration { return queueOutageBackoff(o.failures - 1) }

// elapsed is how long the daemon has been unreachable.
func (o *queueOutage) elapsed() time.Duration { return time.Since(o.since).Round(time.Second) }

// recover ends the outage and reports whether one was in progress.
func (o *queueOutage) recover() bool {
	if o.since.IsZero() {
		return false
	}
	o.reset()
	if o.announced {
		o.announced = false
		fmt.Fprintf(o.out, "agent-heavy: queue daemon responding again (job %d)\n", o.id)
	}
	return true
}

// reset restarts the bound without a notice, for a loss that an authenticated
// handover explains. The loss notice stays pending until the daemon answers.
func (o *queueOutage) reset() {
	o.since = time.Time{}
	o.failures = 0
}
