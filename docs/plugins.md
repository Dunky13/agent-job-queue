# Plugins and lanes

Status: v1. Sections marked **Contract** are binding for the daemon, CLI, app, dashboard and any plugin. The queue daemon and CLI implement them; the notes after each Contract section describe details of that implementation. App and dashboard lane views follow separately.

Plugins tweak the queue without running code inside it. A plugin is a declarative manifest that adds **lanes** (named queues with their own concurrency and priority) and optional **hooks** (separate, time-limited processes the daemon starts when queue events happen). The motivating plugin is a Teifi QA lane: QA must run one session at a time, while builds and tests keep using the ordinary queue.

## Concepts

- **Lane**: a named queue. Every job and lease belongs to exactly one lane. The built-in lane `default` is today's queue and behaves exactly as before. Plugins add more lanes.
- **Job**: a wrapped command (`agent-heavy run ... -- cmd`), unchanged except for its lane.
- **Lease**: a new job kind that holds a lane slot without a process. An agent acquires a lease, does multi-step work (for example driving an emulator over many tool calls), then releases it. Leases are for work that is not one command.
- **Shared budget**: all lanes draw from the same laptop. By default every running job and every held lease counts against the global parallel limit (`agent-heavy limit`) as well as its lane's concurrency.
- **Priority**: when capacity frees, waiting work in higher-priority lanes is considered first.

## Plugin location and manifest (Contract)

Plugins live in `~/.config/agent-heavy/plugins/<name>/plugin.json`. `<name>` must equal the manifest `name`, match `^[a-z][a-z0-9-]{0,31}$`, and is unique.

Safety requirements, enforced on every load, with the repository's existing fail-closed file checks (no symlinks anywhere in the path below `~/.config/agent-heavy`, owned by the user, not group or world writable, regular files, bounded size of 64 KiB per manifest). A plugin that fails any check is not loaded; the reason is reported in status and by `agent-heavy plugins list`. A bad plugin never stops the daemon, the default lane or other plugins.

```json
{
  "schemaVersion": 1,
  "name": "teifi-qa",
  "description": "One QA session at a time on the emulator and dev app",
  "lanes": [
    {
      "name": "qa",
      "label": "QA",
      "concurrency": 1,
      "priority": 50,
      "sharesLimit": true,
      "reservation": { "memoryMiB": 4096, "cpuPercent": 100 },
      "lease": { "ttlSeconds": 0 }
    }
  ],
  "hooks": {
    "lease.granted": ["hooks/notify-granted"],
    "lease.released": ["hooks/stop-dev-server"],
    "lease.expired": ["hooks/stop-dev-server"]
  }
}
```

Fields:

- `schemaVersion`: must be `1`. Unknown top-level or lane fields are errors (strict decoding, duplicate keys rejected), so typos do not silently change scheduling.
- `lanes[].name`: `^[a-z][a-z0-9-]{0,31}$`, unique across all loaded plugins; `default` is reserved. A lane name collision disables both plugins' conflicting lanes and is reported.
- `lanes[].label`: display text, at most 40 characters, no control characters.
- `lanes[].concurrency`: integer 1 through 8. **Hard invariant**: running jobs plus held leases in a lane never exceed it, including through **Start now** (see Scheduling).
- `lanes[].priority`: integer -100 through 100. `default` has priority 0. Higher runs first.
- `lanes[].sharesLimit`: default `true`. When `true`, the lane's running jobs and held leases count against the global parallel limit. When `false`, they count only against the lane's own concurrency (use for work that does not compete for the laptop's CPU and memory, for example a lease that only serializes access to an external device). Resource reservations still apply in adaptive mode either way.
- `lanes[].reservation`: optional `memoryMiB` (0 through 65536) and `cpuPercent` (0 through 800). Adaptive scheduling subtracts the reservation of every held lease from headroom. Leases have no measured workers, so this is how a QA session's emulator and dev server are accounted for.
- `lanes[].lease.ttlSeconds`: 0 (default, no TTL) or 60 through 86400. With a TTL, a held lease that is not renewed in time expires.
- `hooks`: map from event name to a list of executable paths **relative to the plugin directory**, each resolving inside it. See Hooks.

Plugins are loaded when the daemon starts and when `agent-heavy plugins reload` runs. Removing a lane while it has queued or held work keeps that work visible and lets it finish or be released; no new work is accepted into a lane that no longer exists.

### Loading details

- The plugins directory is `plugins` next to the queue configuration file, so it follows `AGENT_HEAVY_CONFIG` in private test setups. Entries starting with `.` are ignored. At most 32 plugins load; each declares 1 to 8 lanes and at most 4 hooks per event.
- `description` is optional (at most 200 characters, no control characters). `label` defaults to the lane name. `null` is never accepted as a value, and field names match exactly.
- Manifests and hooks must have a single link; a hard link is refused like a symlink. Hooks must also be executable by you.
- A hook that fails its checks is skipped and reported in the plugin's `warning`; the plugin's lanes still load. A plugin that fails to load reports `loaded: false` and an `error`. A lane collision keeps both plugins loaded without that lane and reports it as an `error`. An unsafe plugins directory is reported as one entry named `plugins`.
- A removed lane keeps its last definition while it has queued or held work, and is listed with `removed: true`. If the daemon restarts after the plugin was removed, that work runs with concurrency 1, priority 0 and the shared limit until it finishes.
- `agent-heavy plugins validate DIRECTORY` runs the same checks on a directory anywhere on disk and exits 1 when the manifest or any hook fails them.

## Scheduling (Contract)

A waiting job or lease may start only when all hold:

1. The queue is running (not paused).
2. Its lane's running jobs plus held leases are below the lane's `concurrency`.
3. If the lane `sharesLimit`, total running jobs plus held leases of all limit-sharing lanes are below the global parallel limit.
4. Adaptive mode (when enabled): ordinary jobs keep today's resource admission. A lease is admitted by rules 1 through 3 plus its lane `reservation` fitting the current headroom; leases are never treated as "unfamiliar workloads that must run alone". Held lease reservations reduce headroom for every later admission.

Ordering: among candidates that may start, the highest lane priority wins; within a lane, queue order applies. `agent-heavy next <id>` and `last <id>` reorder within that item's lane only. Lanes of equal priority interleave by queue order (job ID order after reordering).

**Start now** overrides the global parallel limit and resource admission exactly as today, but it **never** exceeds the target lane's concurrency. If the lane is full, Start now is refused with an explanation (CLI exit status, app and dashboard message).

The default lane with no plugins loaded must behave byte-for-byte as before: same ordering, same limits, same status output apart from new additive fields. Existing tests must pass unmodified.

### Scheduling details

- The daemon recounts a lane from its durable state immediately before every start or grant, whichever path asked (dispatch, **Start now**, the resource controller or `lane acquire`). A full lane refuses the transition.
- A lane's occupancy includes jobs that have finished but whose process group is still being cleaned up, as the default lane's count always has.
- A lane at its concurrency never holds back other lanes. In adaptive mode, lanes that share the limit keep the existing head-of-line fairness among themselves; a lane with `sharesLimit: false` only waits behind work in its own lane.
- In adaptive mode the resource controller admits leases as it admits jobs, one admission per pass. A lease with a zero reservation does not wait for resource samples, and is still admitted while host sampling fails. Lease decisions appear as the lease's `phase` in status.
- The default lane has no plugin and never starts hooks. Leases always belong to a plugin lane.

## Leases (Contract)

Leases are a job kind (`kind: "lease"` in the task JSON; ordinary jobs are `kind: "command"`, the default when absent). A lease has no process group. Every consumer that assumes a process (execution, worker sampling, cancellation, offline drain, handover, history and learning) must handle the kind explicitly: no worker sampling and no learning records for leases; cancelling a queued lease removes it; "cancelling" a held lease releases it; a held lease is unfinished work for drain purposes, so a drained upgrade waits for held leases (correct: never yank an active QA session).

### Ownership and liveness

The CLI process exits between an agent's tool calls, so a lease belongs to an explicit owner process:

- `--owner-pid <pid>` (required unless `--ttl` is given). The daemon records the PID together with its process start time and boot identity (reuse the identity helpers in `internal/backend/ownership.go`) so PID reuse cannot keep a lease alive.
- When the owner process exits: a held lease is released (event `lease.expired`, reason `owner-exited`), a queued lease is dropped (event `lease.dropped`).
- Optional TTL: `--ttl <seconds>` (or the lane default). `agent-heavy lane renew <id>` extends it. Expiry releases or drops the lease (event `lease.expired`, reason `ttl`).
- The daemon has no knowledge of agents, T3 threads, idle detection or takeover policy. Plugins implement those using lease status and hooks.

### Metadata

`--label <text>` (at most 120 characters) and up to 8 `--meta key=value` pairs (key `^[a-z][a-z0-9_]{0,31}$`, value at most 512 bytes, no NUL or newline). Metadata is shown in status and passed to hooks; the daemon never interprets it. Example: `--meta worktree=/path --meta thread=abc --meta ticket=WM-123`.

### CLI (Contract)

All commands accept `--json`; JSON output is a single object with `schemaVersion: 1`.

```sh
agent-heavy lane list
agent-heavy lane acquire <lane> --owner-pid <pid> [--ttl S] [--label T] [--meta k=v]... [--no-wait] [--timeout S]
agent-heavy lane wait <lease-id> [--timeout S]
agent-heavy lane status <lease-id>
agent-heavy lane renew <lease-id>
agent-heavy lane release <lease-id> [--force]
agent-heavy run --lane <lane> [--label T] -- <command> [args...]
agent-heavy plugins list
agent-heavy plugins validate <directory>
agent-heavy plugins reload
```

- `acquire` without `--no-wait` blocks until granted. With `--timeout S` it returns exit status 75 (EX_TEMPFAIL) when the time runs out, **keeping the lease queued** so its position is preserved; the JSON includes the lease ID. `--no-wait` enqueues and returns immediately (exit 0, state `queued` or `held`).
- `wait <id> --timeout S` blocks until the lease is held (exit 0), the timeout passes (exit 75, still queued), or the lease is gone (exit 1 with the reason: dropped, expired, released, cancelled). It can be called repeatedly without losing queue position. This lets agents wait in bounded tool calls.
- `release` is accepted from any process of the same user; `--force` is required when the caller's `--owner-pid`/environment does not match the recorded owner (plugins use it for an approved takeover). Releasing a queued lease drops it.
- `status <id>` reports: `id`, `lane`, `state` (`queued`, `held`, `released`, `expired`, `dropped`), `position` (when queued), `label`, `meta`, `ownerPid`, `ownerAlive`, `queuedAt`, `grantedAt`, `renewedAt`, `expiresAt`.
- Exit statuses: 0 success, 1 lease gone or refused, 2 usage error, 75 timed out while still queued.
- `run --lane` behaves like `run` today (waits, streams output, returns the command's exit status) in the given lane. Unknown lane: exit 2 with the list of lanes. Without `--lane`, behaviour is unchanged.

### Lease details

- How a lease ends sets its `state` and `reason`:

  | Ended by | Held lease | Queued lease | Event |
  | --- | --- | --- | --- |
  | `lane release` | `released`, reason `released` | `dropped`, reason `released` | `lease.released` or `lease.dropped` |
  | `cancel` (CLI, app, dashboard) | `released`, reason `cancelled` | `dropped`, reason `cancelled` | `lease.released` or `lease.dropped` |
  | Owner exited | `expired`, reason `owner-exited` | `dropped`, reason `owner-exited` | `lease.expired` or `lease.dropped` |
  | TTL | `expired`, reason `ttl` | `expired`, reason `ttl` | `lease.expired` |

- The owner must be a running process of your user when the lease is acquired. The daemon checks owners about once per second. A missing process, a zombie, a different start time or a different boot ends the lease; if the process table cannot be read, leases stay as they are.
- `--owner-pid` defaults to `AGENT_QUEUE_OWNER_PID` when that is set, for both `acquire` and `release`. A lease acquired with only a TTL has no recorded owner and can be released without `--force`.
- A TTL runs from acquisition, restarts when the lease is granted, and restarts on every `renew`. Queued leases expire too, so renew while waiting. `renew` on a lease without a TTL succeeds and changes nothing.
- `--timeout 0` checks once. Interrupting a blocking `acquire` (Ctrl-C, SIGTERM or SIGHUP) withdraws the lease it created and exits 130; `wait` never changes the lease.
- `lane status` exits 0 for any lease that exists, including ended ones, and 1 for an unknown ID.
- An older daemon makes `lane` and `plugins` commands exit 1, and `run --lane` exit 2, with "This queue does not support lanes; update Agent Queue". Lane submissions use their own daemon operation, so an older daemon rejects them rather than running them in the default lane.
- Held leases do not mark the queue as having active processes, so restarting the daemon does not pause the queue. Held and queued leases survive a daemon restart.

### Status JSON (Contract)

Additive only. `agent-heavy status --json` gains:

- per job: `lane` (string, `"default"` when absent) and `kind` (`"command"` or `"lease"`); leases also carry the lease fields above (no `outcome`/log fields).
- top level: `lanes`: array of `{name, label, plugin, concurrency, priority, sharesLimit, running, held, waiting}` including `default`, in priority order; `plugins`: array of `{name, loaded, error, lanes}`.

Every consumer is updated together: Swift decoders and fixtures (`native/tests/fixtures/queue.json`), the dashboard schema (`remote/static/schema.mjs` and its tests), and the live handover payload.

### Status details

- Lease entries keep every field existing clients require: `title` is the label (or `Lease`), `project`, `branch`, `directory`, `executable` and `logPath` are empty and `exitCode` is `null`. `state` uses the lease vocabulary. `position` is the lease's position within its lane; `queuePosition` stays the position across all lanes.
- `phase` describes the lease: `Held`, `Waiting for lane capacity` (or the adaptive decision), or how it ended.
- Held leases count in `queue.running` and queued leases in `queue.waiting`.
- `lanes` entries add `removed: true` for a removed lane that still has work. `plugins` entries add `warning`, which reports skipped hooks and dropped hook events.
- Status from a daemon without lane support has no `lanes` or `plugins`; `agent-heavy status --json` then reports the default lane alone.
- The live handover payload is unchanged (protocol 1). Instead, unfinished lane work or any queued or held lease makes live upgrade ineligible, so setup waits for that work to finish or be released, as a drained upgrade does.

## Hooks (Contract)

Hooks are code execution from a configuration directory, so they follow the repository's safety posture:

- Each hook path is relative to the plugin directory, resolves inside it with no symlinks, is a regular executable file owned by the user and not group or world writable. Checked at load and again immediately before each run; a failing hook is skipped and reported.
- The daemon starts hooks off the scheduling path, fire-and-forget: a hook can never delay, block or change admission, ownership or queue state. At most 4 hook processes run at once; extra events are dropped with a counted, reported warning.
- Process: no arguments; the event as one JSON object on stdin (at most 16 KiB); environment limited to `HOME`, `PATH=/usr/bin:/bin:/usr/sbin:/sbin`, `AGENT_QUEUE_EVENT=<event name>`, `AGENT_QUEUE_PLUGIN=<name>`; working directory is the plugin directory; stdout and stderr captured, bounded to 64 KiB, kept in a private per-plugin log under `~/.local/state/agent-heavy/plugins/<name>/hooks.log` with rotation; 10 second timeout, then the hook's process group is terminated.
- Events: `job.queued`, `job.started`, `job.finished` (with exit status), `lease.queued`, `lease.granted`, `lease.released`, `lease.expired` (with reason), `lease.dropped`. Event JSON: `{"schemaVersion":1,"event":...,"time":...,"lane":...,"id":...,"kind":...,"label":...,"meta":{...},"ownerPid":...,"reason":...,"exitStatus":...}` with absent fields omitted. Hooks receive events only for their own plugin's lanes.

### Hook details

- Job events are sent for commands run with `run --lane`. `job.finished` carries `exitStatus` and is sent when the daemon first sees the result, so a daemon restart during cleanup can send it again. `lease.dropped` carries `reason` too.
- Anything a hook leaves running in its process group is terminated at the 10 second deadline, even when the hook itself exited earlier.
- Each log entry records the time, event, hook path and result (exit status, timeout or refusal) followed by the output. At 1 MiB the log is renamed to `hooks.log.1`, replacing the previous generation.
- Dropped events, including any event whose JSON exceeds 16 KiB, are counted in the plugin's `warning` until the daemon restarts.

## App and dashboard

- The menu-bar popover and window group work by lane when more than one lane exists: a section per lane (label, running/held, waiting) in priority order; the default lane looks as today when no plugins are loaded.
- Leases show label, holder metadata, held or waiting time and position. **Release** on a held lease and **Remove** on a queued lease use the existing confirmation pattern. Start now respects lane concurrency and explains a refusal.
- Settings (gear menu) lists plugins and their load errors (read-only).
- The phone dashboard shows the lane on each item and the lanes summary; its existing controls apply with the same lane rules.

## Compatibility

- No queue database schema change is required: lane, kind and lease fields live in the existing task JSON. If a change to the live handover protocol is unavoidable, bump it, keep old callers working through the existing drained-upgrade fallback, and document it in `docs/upgrade-recovery.md`.
- A daemon without plugin support ignores nothing: callers passing `--lane` to an older daemon must get a clear "this queue does not support lanes; update Agent Queue" error, never a silent run in the default lane.
