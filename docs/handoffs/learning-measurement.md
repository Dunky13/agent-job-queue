# Learning measurement handoff

## Result

Runs now reach learning history. Before this change most runs recorded as incomplete, so no workload variant earned the evidence that adaptive overlap and exact predictions need. CPU on Apple Silicon also read about 42 times too low.

## What changed

- `internal/backend/monitor.go`: a sample is complete when it read every process that lasted through it.
- `internal/backend/monitor_darwin.go`: CPU counters are converted from Mach ticks with the timebase. A process that has exited is classified apart from one that cannot be read.
- `internal/backend/scheduler_controller.go`: a failed sample counts only if the job is sampled again, and never advances the last observation. Samples of a job the queue reports as finished stay out of its evidence, but admission still reserves what it holds. Finalization tolerates a sample landing just after the recorded end, and duration is the recorded end minus the start.

## Known limits

- Existing CPU history is not migrated. It stays low until each variant runs again, because envelopes keep the maximum.
- Correct CPU figures mean Adaptive mode refuses more heavy jobs: a peak above about 64% of cores now predicts over budget.
- Workers that start and exit between samples are invisible, and counters of a worker that exits during a sample are dropped. Observed totals are lower bounds, and such runs can now become evidence.
- A job needs about three good samples to count, so jobs under roughly 4 to 6 seconds still record as incomplete. This predates the change.
- A peak seen after a job finished is not kept once its footprint drops, because a finished job cannot climb back to it.
- The `values[1] != 0` branch of the Darwin exit classification has no test that reaches it on current macOS.

## Next steps

The grouping work that this enables is separate: per-repository learning groups and classification, then the project and operation browse hierarchy. Re-measure group counts after it lands, on a copy of the history database.
