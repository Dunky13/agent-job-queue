# Removal refused a healthy app as "unowned" (intermittent)

## Symptoms

Hosted Intel runners, backend shards:

- PR #103 at 27d9a2d, job 140357469400, `TestManagedRemovalOfRunningAppRefusesActiveJobs`: `An unowned queue process holds its lifetime lock; setup refused before service mutation: holder 7118 cannot be read: invalid argument`.
- main at f07c4cb, run 45851602633, `TestManagedRemovalOfRunningAppQuitsItOnceDrained`: the same error for holder 4634.

A user removing the app while it runs could get the same spurious refusal. Nothing was changed when it happened; running removal again would normally succeed.

## Root cause (product)

`managedHeldByApp` (`internal/backend/install_managed.go`) judges every process that `lsof -nP -t -- <lock>` names. That list is every process with the file open, not only the flock owner.

A helper that holds a lifetime lock also starts short-lived children. The resource controller runs `/bin/ps` about every two seconds (`resourceProcessListing` in `internal/backend/monitor.go`) and runs `sysctl` to read the boot identity (`BootIdentity` in `internal/backend/ownership.go`). Go starts a child with `fork` then `execve` (`syscall/exec_libc2.go`), so between those two calls the child has a copy of every descriptor, including the lock. The lock is already opened with `O_CLOEXEC` (`LockPrivate`, `internal/backend/common.go`), which closes it only at exec. There is no descriptor flag that closes it earlier, so there is no leak to plug.

When `lsof` catches a child in that window, the later reads see one of:

| Child state when read | Refusal |
| --- | --- |
| exiting, zombie or reaped | `holder N cannot be read: invalid argument` (`kern.procargs2`) |
| not yet exec'd | `parent N of holder M runs .../agent-heavy` |
| exec'd | `holder N runs /usr/sbin/sysctl` |

Evidence: a temporary probe listed the `resource-controller.lock` holders of a real running fixture while a delay was injected after `fork` in a scratch copy of the Go toolchain. 33 transient holders appeared in 163 rounds. Their parent, where still readable, was the resource controller (the steady holder of that lock). They showed `ps`, `sysctl` or the not yet exec'd `agent-heavy`. None of the 33 was in the listing taken right after. Without the delay, 343 rounds showed none, which matches a rare hosted flake.

The CI logs carry only a PID and do not name the lock, so the identity of PIDs 7118 and 4634 is inferred from this reproduction, not read from CI.

## Fix

The first holder that fails any ownership check makes the lock be listed once more, a single time per check. A failing holder is disregarded only when that second listing succeeds and no longer names it. It stays fail-closed:

- A holder that is still listed is refused with the original error, readable or not.
- A failed second listing refuses with the original error.
- A disregarded process never counts: with no verified holder the result is `no holder found`.
- Every PID that appears only in the second listing must pass the same ownership check, or the check refuses with its error. Without this, a stranger that forked a child sharing the lock and then exited between the listings would pass, and the app quit and service unregistration both run before `managedStopped` rechecks the locks.
- No ownership rule is widened. A helper's child is not accepted as owned. There is no sleep, retry loop or longer timeout.

Alternative rejected: stopping inheritance. `O_CLOEXEC` is already set, and closing the descriptor between fork and exec is not available through `os/exec`.

Remaining limit: a child still between fork and exec at the second listing is refused. That needs the window to outlast a whole `lsof` run.

The phone access lock uses the same function and gets the same behaviour.

## Reproduction

200 calls of `managedHeldByApp` against `resource-controller.lock` of the running fixture, with the injected post-fork delay (temporary, not committed):

| Tree | Refused |
| --- | --- |
| origin/main 9f57b7c | 17 of 200 |
| fixed | 0 of 200 |

The real removal tests, `-run '^TestManagedRemovalOfRunningApp'` (three tests), through the shared queue on Apple Silicon:

| Tree | With the injected delay, `-count=15` | Plain, `-count=30` |
| --- | --- | --- |
| origin/main 9f57b7c | 16 of 45 failed, all `holder N cannot be read: invalid argument` (9 `RefusesActiveJobs`, 7 `StopsItsPhoneService`) | 0 of 90 failed |
| fixed | 0 of 45 failed | 0 of 90 failed |

The flake did not reproduce without the injected delay.

Regression test: `TestManagedHeldByAppDisregardsOnlyAHolderThatLeftTheListing` covers the accepted transients (a dead child, a live stranger that left, the helper joining the second listing, two transients with one relisting) and the refusals (still listed, second listing fails, only a transient, a stranger joining the second listing). Its row in `scripts/backend-test-weights.tsv` is an estimate; replace it with a measured Intel race duration at the next refresh.

## Validation

On the fixed tree:

- `gofmt -l internal`: no output.
- `go vet ./internal/backend/...`: passed.
- `go test -p 2 -parallel 2 -count=1 -timeout 30m -skip '^TestInstallerOwnershipParsesRealPrivateLaunchctlService$' ./internal/backend/...`: passed (370 s).

- `go run ./cmd/queue-tool check-source`: passed.
- `go test -race -count=2` for the `managedHeldByApp`, running app removal and unowned holder tests: passed.

After rebasing onto origin/main ed2e75d (#103, no conflicts), on the rebased code:

- `gofmt -l internal` and `go vet ./internal/backend/...`: passed.
- The new test and the three running app removal tests, `-count=10`: passed.
- The full backend command above: passed (376 s).

Not run: the CONTRIBUTING validation batch, the whole suite with the race detector, Intel hardware.

## Not covered: the third failure

main at b077bba, run 45700661540, `TestManagedRemovalOfRunningAppRefusesActiveJobs`: `isolated queue condition did not become true before deadline` at line 300. That line waits 20 s for the fixture job to start, before removal is invoked, so it is not this lock check. The runner was very slow (sibling fixture tests took about 27 s and the package later hit its 12 minute limit). The log has no job or queue output, so the cause is not diagnosed. The wait was not changed. If it recurs, capture `agent-heavy status` and the daemon output at the point of failure.
