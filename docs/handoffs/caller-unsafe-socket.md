# Caller exits 125 on a socket that is still being published

Branch: `fix/caller-unsafe-socket`. Authorized endpoint: PR against `main`. Review and merge are separate steps.

## Result

An attached `agent-heavy run` caller could exit 125 with `Unsafe queue socket; work is held` while its job kept running. CI hit it once: run 41656379541, job 125987678248 (`macOS tests (macos-15-intel, installer)`, `TestInstallerManagedLiveResumeRetainsRunningAttempts/queued`).

Cause: `ServeQueue` bound the socket at its final path and chmodded it to 0600 afterwards. The daemon runs with umask 077, so between `ListenUnix` and `Chmod` the socket exists with mode 0700. `safeQueueSocket` requires exactly 0600 and returns a plain error, which `queueTransportError` does not classify as retryable, so the follow loop from #64 treats it as final. A restart or live resume always opens this window, and the polling callers in that test make a hit likely.

Fix: `listenQueueSocket` binds a private name (`q.new`, short because macOS limits socket paths to 104 bytes), chmods it, then renames it onto `queue.sock`. The socket is never visible at the final path before it is private, and a stale socket is replaced atomically, so the old remove-then-listen gap is gone too. `safeQueueSocket` is unchanged: a socket that stays unsafe still fails visibly.

## Evidence

- `TestQueueSocketIsNeverVisibleBeforeItIsPrivate` runs `listenQueueSocket` under umask 077 and checks the path from inside the bind window. It fails without the fix with the CI message (`Unsafe queue socket; work is held`) and passes with it. It also checks that the renamed socket passes `safeQueueSocket` and accepts connections.
- Not reproduced: the CI failure itself. The window is microseconds wide, so no end-to-end repro was attempted.

## Not changed or not checked

- The transient and permanent unsafe states are indistinguishable to a caller, so the fix removes the transient state instead of retrying on it.
- `internal/remote/control.go` binds its own socket under a process umask. It does not use `safeQueueSocket`, so it was not touched.
- Hang on PR #71's arm shard (20-minute limit): no hang path tied to this cause was found. In `testInstallerManagedLiveResume`, `defer finishCallers()` runs before the `t.Cleanup` joins, so `<-blockerDone` and `<-finished` do not wait on a live caller. The `Wait` calls on `exec.CommandContext` have no `WaitDelay`; a descendant holding the output pipe would block them. This is unproven and unchanged.
- `install_managed.go` is untouched, so there is no conflict with #71 or #65.
