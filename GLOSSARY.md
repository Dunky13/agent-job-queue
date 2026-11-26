# Agent Queue

Agent Queue runs heavy agent commands one at a time, or a few at a time, on one Mac so builds and test suites do not overload it. This glossary fixes the words used for its queue, its setup and its app.

## Queue

**Job**:
A unit of queued work, either a command submitted through `agent-heavy run` or a lease, owned by the queue daemon until it finishes.
_Avoid_: Task, process

**Lease**:
A job without a process, acquired with `agent-heavy lane acquire`, that holds a lane slot while an agent does multi-step work.
_Avoid_: Lock, reservation

**Lane**:
A named queue; every job and lease belongs to exactly one. The built-in lane is `default`.
_Avoid_: Channel, pool

**Parallel limit**:
The number of jobs that may run at once across the lanes that share it. Start now may exceed it.
_Avoid_: Concurrency, slots

**Workload**:
A recurring kind of job that the queue learns resource use for, grouped from similar commands.
_Avoid_: Profile, job type

**Queue action**:
An operator decision on the queue or one job, such as Run next, Start now, Cancel, Resume, or changing the parallel limit or scheduling mode.
_Avoid_: Command, button, operation

## Upgrades and setup

**Service**:
A background program the installer registers with the user's login session, such as the queue, the resource sampler or phone access. It is either loaded or not loaded.
_Avoid_: Daemon (for every service), agent (unqualified)

**Drain**:
An operator-declared window, started with `agent-heavy drain`, during which new runs wait so a restart cannot strand their callers.
_Avoid_: Pause, freeze

**Drained queue**:
A queue with no unfinished jobs and no pending cleanup.
_Avoid_: Empty queue, idle queue

**Handover**:
Moving custody of running and queued work, and their callers, from the daemon of one build to another.
_Avoid_: Migration, takeover

**Preflight**:
The read-only check setup runs before installing, reporting each issue that blocks it and the recoveries it offers.
_Avoid_: Diagnostics, health check

**Recovery**:
One operator-chosen fix for a single preflight issue, such as disabling phone access or replacing a custom callback.
_Avoid_: Repair (unqualified), fix

**Approved repair**:
The automatic sequence of recoveries setup may run without asking per step, once the operator has approved continuing.
_Avoid_: Auto-fix, self-heal

## App

**Surface**:
Any window or popover that shows live queue data.
_Avoid_: View, screen, panel
