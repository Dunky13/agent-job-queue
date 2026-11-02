# Status snapshot contract

## Result

The status snapshot now has one typed definition, `internal/wire`. `StatusData` and the simulated dashboard in `internal/preview` both produce it. The real producer generates `native/tests/fixtures/queue.json` and `native/tests/fixtures/status-variants.json`, and `TestStatusContractGolden` fails when either is stale. The Swift decoder tests, the Zod tests and the browser tests read those files instead of keeping their own copies.

Regenerate after an intended contract change:

```sh
go test -p 2 ./internal/backend -run '^TestStatusContractGolden$' -update
```

The seven `groups.default` reads use the daemon's private state, which has `tasks`, `groups`, `lanes` and `plugins` and is a different shape from the wire snapshot. They now share a small `defaultGroup` accessor. The two shapes stay separate.

## Design notes

- Struct fields are declared in JSON key order, and `Job.MarshalJSON` merges the embedded lease keys back into sorted order, so output stays byte-identical to the old map encoding.
- Controller metrics are `json.Number`, so their literals pass through unchanged.
- A lease has every lease key, with explicit nulls, and a command has none. The embedded `*wire.Lease` models that.
- `familySamples` is on the wire and Zod removes it. The golden keeps it, and the Zod test states the difference.

## Intended differences

Consumers see the same JSON for every state a real daemon and controller produce. These inputs now behave differently, and no current writer produces them:

- A pass-through field of the wrong JSON type, such as a non-integer limit, now fails `status` with `Queue returned invalid status data`. Before, it was published as is, and the Swift and Zod consumers then rejected it.
- A nullable text field holding a non-string value now publishes `null`.
- A required pass-through field missing from `resource-status.json` or a lane, such as the scheduler `reason`, publishes its zero value instead of `null`.
- Keys the wire types do not declare are no longer published. None exist today.

The simulated dashboard gained a `snapshot` fixture state for the new browser test. It rewrites every job's `logPath` into the preview's temporary directory, because the test-mode log endpoint trusts that path. An automated security review flagged the first version, which let a fixture choose the file.

## Evidence

Before deleting the old construction, a temporary harness captured the old code's output and compared it with the new code byte for byte:

- 10 daemon scenarios, each as encoded JSON, `status --json` and human `status`: the empty queue, running, waiting, lanes, plugins, leases in every state, adaptive and shadow scheduling, a missing default group and missing optional fields. All 30 files matched.
- 10 simulated dashboard states. All matched.
- One private real daemon with the resource controller, plugins, a held and a queued lease, and running, queued and failed jobs in adaptive mode, read by binaries built from the old and new code. JSON and human output were byte-identical, and the JSON passed `snapshotSchema`.

The stale check was confirmed by editing the golden file and by dropping a wire field. Both made `TestStatusContractGolden` fail.

See the pull request for the validation runs and their results.

## Remaining work

- The Swift and Zod types remain hand-written by design. A contract change still updates them by hand, and the shared fixtures are what catch a mismatch.
- `learning.spec.mjs` keeps an inline resources variant for a job with a learned profile. Strict fixture decoding checks its fields against the wire types.

## Next steps

1. Run independent Standards and Spec review of the draft pull request.
2. Confirm the hosted checks on the reviewed head.
