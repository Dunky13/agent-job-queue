# Queue snapshot scopes

Branch: `dunky13/agent-heavy-daemon-high-cpu`. Authorized endpoint: PR. Merge and release are separate steps.

## Result

The daemon no longer reads all job history for every poll. The `snapshot` operation takes an optional scope in its payload:

| Payload | Rows | Used by |
|---|---|---|
| none | all history | install and recovery paths, and the scheduler fallback below |
| `{"id": N}` | one job | `Gate.Task`, so every waiting `run` client |
| `{"recentDone": 0}` | active jobs | submission checks, lease capacity, permits, drain, lanes, Start |
| `{"recentDone": 30}` | active jobs and the 30 most recently finished | `status` |
| `{"recentDone": 30, "commandsOnly": true}` | active jobs and the 30 most recently finished commands | scheduler tick |

Replies to queue mutations list active jobs only. No caller read history from them.

## Decisions

- **The scope lives in the existing operation.** A daemon that predates it ignores the payload and answers with all history, a superset every caller already filters. An old client sends no scope and still gets all history. A live upgrade needs no protocol change.
- **The scheduler window leaves out leases.** The tick looks at the last 30 commands, so finished leases must not fill its window.
- **A sampled run outside the window falls back to a full read only if it is not finalized.** It still owes its final record. This should be rare: more than 30 commands must finish after it before a tick sees it.
- **A finished row with no end time stays listed by the recent scopes.** Its status may disagree with its phase, and `status` still fails on or lists such a row however old it is. A readable `Done` status with no end, which only a legacy import can write, is still cut after the 30 most recent.

## Known limits

- The recent scopes sort every finished row in SQLite, so `status` and scheduler ticks still grow with history, far more slowly than before. An indexed end time column is the next step if they show up in a profile.
- Unscoped snapshots still grow with history, and clients cap a response at 32 MiB. At about 1.8 KB per job that is roughly 18,000 retained jobs. Only install paths and the scheduler fallback still read them. Nothing prunes finished jobs.
- `ActiveSnapshot` hides a corrupted row whose phase is `done` but whose status is not. `waitDrained` can then report drained, though the installer's unscoped check still refuses. `LeaseCapacity`, `heldLeases`, Start and `drain --cancel-running` also stop seeing it. The daemon's own tick and lane counts already ignore such rows.
- A `run` client started before an upgrade keeps polling with full snapshots until it exits, so CPU only falls once those clients finish.
