# Notarization stage record owned by Go

Base: `7b4b27c`. Branch: `arch/notarization-record`. Endpoint: draft PR for
independent review. Merge, release and distribution runs need separate
authorization.

## Result

`internal/notaryapi/record.go` now parses and validates the stage record and owns
its phase transitions. `submit` uses the same module. Scripts reach it through a
credential-free seam:

| Command | Purpose |
| --- | --- |
| `queue-tool notarize record get --record R --field a,b [--stage S]` | Validate, then print fields joined by commas |
| `queue-tool notarize record validate --record R --stage S` | Validate only |
| `queue-tool notarize record create --record R --stage S --run N --attempt N --sha256 H` | Write a new prepared record, never replacing one |
| `queue-tool notarize record advance --record R --to P [--id U] [--completed-sha256 H]` | Take one legal step |

The only legal steps are prepared, uploading, submitted, accepted, finished. Every
failure exits 1 with a fixed code and no path, so `set -e`, `pipefail` and the
`|| fail` messages behave as before. Shell-driven writes now get the directory
lock, fsync and atomic rename that only the Go path had, where the `jq` writers only
renamed.

Output matches the last shell writers (the expressions at `7b4b27c`) byte for byte,
but this is not byte compatibility with every existing record. Real production
records hold `prepared_attempt` before `prepared_run_id`, and a rewrite reverses
those two keys. That is harmless because every consumer ignores key order, and
`TestRecordRewriteOfAttemptFirstRecordChangesOnlyKeyOrder` proves nothing else changes.

## Where each step gets the tool

| Step | Runner | Source of the tool | Change |
| --- | --- | --- | --- |
| `notarize`: resume, receipts, stage action | `macos-15` | Existing build step before any record use, from `automation_sha` | None |
| `publish`: `distribution-publish.sh` | `ubuntu-24.04` | New one-step build from `automation_sha`; setup-go was already there | One workflow step |
| `recover`: `distribution-recover.sh` | `ubuntu-24.04` | Not available | Not changed, see below |
| `source`, `notarized-tests`, `homebrew-sync` | Mixed | Read no stage records | None |

The tool is never downloaded, so no job depends on another job's architecture.

## Reported rather than worked around

`distribution-recovery.yml` has no Go toolchain. It runs every 15 minutes with
`actions: write` and a 10 minute budget, so adding setup-go and a build there is a
meaningful workflow change. It validates `recovery.json`, a separate 20-key
document, and cross-checks retained stage files by equality. Its `jq` therefore
still restates the phase list, the empty-ID rule, the SHA-256 format and the UUID
format. `TestRecoveryProjectionMatchesRecordRules` runs those rules through real `jq`
and the module's `valid()` over the same corpus and fails on any disagreement it
exposes, from either side. It only sees differences that corpus exposes, and it leaves
out trailing-newline values, where the two differ on purpose. `afd42e1` overstated this:
four format widenings passed its corpus until non-hex and prefixed values were added.

Other remaining record-adjacent `jq` binds documents to each other and is not
record validation: `verify_publication`, the log and asset bindings in
`distribution-publish.sh`, and the receipt envelope checks.

## What the wiring guard covers

`TestStageRecordToolIsBuiltBeforeEveryConsumer` reads the workflow text and checks path
strings and step order. It does not interpret the workflow.

- **It catches:** a missing, commented-out, conditional or cross-compiled build, a
  build allowed to fail, a missing `$GITHUB_ENV` export, a consumer given a different
  path, a later `$GITHUB_ENV` rewrite, and a later `cp`, `mv`, redirect or
  `download-artifact` onto the path.
- **It misses, confirmed by trying them:** a write through another variable, `rm` of the
  tool, and a write inside a called script. A missing tool fails closed at the first
  record read. A replaced binary that still behaves would go unnoticed.

## Behaviour differences

- `macos-resume.sh` now requires `QUEUE_NOTARY_TOOL` for every operation on
  existing state, including local Keychain-profile use. CI always sets it.
  One-shot `macos-signing.sh package` is unaffected.
- The Go parser is stricter than `jq`. Independent review compared them over 1,477
  unique records: `jq` accepted 108 that Go rejected (trailing newline 38, duplicate
  keys 20, over 16 KiB 20, multiple documents 20, BOM 10), and Go accepted nothing
  `jq` rejected. No writer produces any of these. My earlier claim that every
  disagreement was a trailing newline was wrong: my 835-record corpus did not
  contain the other shapes.
- `submit` previously accepted malformed-but-parseable records and then refused
  them. It now returns `invalid_record` for those. Both fail before any provider call.
- `publish` now rejects unknown or missing keys in embedded stages. Its test
  fixture gained the two keys real records carry; no assertion was removed.
- On the local `notarytool` path, a failed `submitted` write now prints Apple's ID in
  the failure message, as the REST path does. Before this, the ID was on neither disk
  nor in the message. This was a defect at `7b4b27c` too. Only a whole UUID is echoed,
  the same gate as the REST path. `afd42e1` used a looser check that echoed values
  such as `deadbeef`, and that is fixed.
- `queue-tool` runs without `GH_TOKEN` in `distribution-publish.sh` and in
  `distribution-source.sh`. Neither call needs it.
- A killed write can now leave a `.notary-*` temporary file beside the record, not
  `<record>.new`. Neither is read back.
- REST-written records are now indented like shell-written ones. Compact records
  from earlier runs still parse.
- Each `queue-tool` exec costs about 19 ms against about 3 ms for `jq`. Batched reads
  hold the two slowest resume tests to about 15% slower, measured on one host
  (15.3 s to 17.6 s, including the one-time tool build). Intel tooling shards took 8
  to 10 minutes per job on main against a 12 minute test limit, so watch that margin.

## Evidence

All local results are from a host under sustained load with swap nearly exhausted.
"Latest repair" is the commit after `afd42e1`.

| Check | Candidate | Result |
| --- | --- | --- |
| `go test ./internal/notaryapi` (non-race) | Latest repair | PASS |
| Narrow non-race `internal/tooling` runs: the new and changed tests | Latest repair | PASS |
| `scripts/lint.sh` | Latest repair | PASS |
| 26 mutations tried on the real files, each checked to compile | Latest repair | 23 caught by their assertion, 3 survived, see below |
| Narrow non-race runs of `TestDistributionPublish*`, `TestWorkflow*` and the main resume, upload and origin tests | `afd42e1` | PASS |
| Seven earlier mutations on the real files, including the first review's four | `afd42e1` | All caught by their assertion |
| `go test -race ./internal/notaryapi` inside validate job 2777 | `c38735c` | PASS |
| Focused non-race runs of `internal/tooling`: 139 of 176 top-level tests, covering resume, receipts, publication, checkpoint, signing and workflow | `c38735c` | 137 PASS, 2 skipped (`TestNotarizedHomebrew*`, environment-gated) |
| Linux amd64 cross-build of `queue-tool` | `c38735c` | PASS |
| `scripts/validate.sh`, job 2777 | `c38735c` | FAIL, see below |
| Tooling shard 1, job 2758 | Before batched reads | FAIL, 12 minute test timeout with no test failure |
| Tooling shard 2, job 2759 | Before batched reads | Interrupted by my cancel |

No commit after `c38735c` reran `validate.sh` or either shard locally, by instruction,
because of host memory pressure. Hosted CI covers them.

Of the 26 mutations, the six the re-review named are all caught: `jq` ID class widened
to `a-zA-Z0-9` and to `.`, `jq` ID without `^`, `jq` hash class `a-z0-9`, and Go's
`uuidPattern` and `hashPattern` widened to non-hex. The Go two are caught by the
recovery test alone as well as by the validation table. The three survivors are the
guard limits listed above.

Job 2777 stopped at `go test`, so its later steps never ran: web, browser and native
tests, app build, installer and DMG checks. Its failures:

- `internal/tooling` reached the 12 minute limit with no test failure reported.
  This run did not complete the package.
- `internal/backend` reached the same limit. This branch does not touch it.
- `internal/remote` failed three Caddy tests on wall-clock limits (41.5 s against a
  30 s wait, and `signal: killed` at 35 s). It does not import `notaryapi`.

Attributing those limits to host load is an inference, not a measurement: no
baseline `validate.sh` ran under the same conditions. Local shards were stopped
at the coordinator's request because the host ran out of swap, and hosted CI runs
both shards on this PR, on both architectures. Treat that CI as the first complete
run of `internal/tooling` on this code.

## Remaining verification

Real notarization cannot run here. The `Distribution` workflow
(`.github/workflows/distribution.yml`, job `notarize`, then `publish`) first exercises
this against Apple, only from `main` after CI succeeds or by `workflow_dispatch`.

## Next steps

1. Check the hosted tooling shards on this PR. Intel is the open risk: the 12 minute
   limit applies to the test binary, recent main runs took 441 s and 451 s, and the
   worst test step in 12 recent runs was 9:51. This head adds a few more resume tests.
2. Independent reviews of `c38735c` and `afd42e1` found no blocking production defect,
   and their findings are fixed here, apart from one brittle text match that fails
   closed, which was left as it is by decision. Re-review the latest commit.
3. After merge, watch the first `Distribution` run through `notarize` and `publish`.
4. Decide separately whether `distribution-recovery.yml` should build the tool, which
   would remove the remaining restated rules.
5. Separately, GitHub inventory pagination is duplicated between
   `scripts/macos-checkpoint.sh` and `scripts/distribution-common.sh`. It was out of
   scope and is unchanged.
