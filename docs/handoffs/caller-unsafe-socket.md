# Caller exits 125 on a socket that is still being published

Branch: `fix/caller-unsafe-socket`. Authorized endpoint: PR against `main`. Review and merge are separate steps.

## Result

An attached `agent-heavy run` caller could exit 125 with `Unsafe queue socket; work is held` while its job kept running. CI hit it once: run 41656379541, job 125987678248 (`macOS tests (macos-15-intel, installer)`, `TestInstallerManagedLiveResumeRetainsRunningAttempts/queued`).

Two restart states could produce that error. `safeQueueSocket` returns a plain error for both, which `queueTransportError` does not classify as retryable, so the follow loop from #64 treats it as final. The CI log has no mode or link count, so it cannot say which one it hit. The first is the likelier, not a proven cause.

- **Mode:** `ServeQueue` bound the socket at its final path and chmodded it to 0600 afterwards. The daemon runs with umask 077, so between `ListenUnix` and `Chmod` the socket has mode 0700. One local measurement, idle, put this window at 17 microseconds mean and 58 worst, against a 400 ms follow poll, so a hit is rare per restart. Load widens it.
- **Link count 0:** a caller's `Lstat` can race an unlink and see `Nlink` 0, on daemon exit or when a rename replaces a stale socket. Tight pollers hit this about 1,800 times less often than the mode window, on one Mac. `queuePrivateFilesInspect` already handles the same race for SQLite sidecars.

Fix: `listenQueueSocket` binds a private name (`queue.next`, the same length as `queue.sock`, so a path too long to connect still fails at bind), chmods it, then renames it onto `queue.sock`. The socket is never visible at the final path before it is private, and a stale socket is replaced atomically, so the old remove-then-listen gap is gone too. `safeQueueSocket` now reports link count 0 as not found, which is retryable. Every other unsafe state still fails visibly.

## Evidence

- `TestQueueSocketRestartNeverLooksUnsafe` polls `safeQueueSocket` from three goroutines across 300 restarts under umask 077, half of them replacing a stale socket. Binding at the final path, renaming before the chmod, and dropping the link count check each make it fail with the CI message (`Unsafe queue socket; work is held`). It also checks the renamed socket accepts connections.
- Not reproduced: the CI failure itself. The window is microseconds wide, so no end-to-end repro was attempted.

## Not changed or not checked

- A socket left at 0700 by an older build that crashed inside the window is still refused at daemon startup. That fails closed and predates this change.
- `internal/remote/control.go` binds its own socket under a process umask. It does not use `safeQueueSocket`, so it was not touched.
- Hang on PR #71's arm shard (20-minute limit): no hang path tied to this cause was found. In `testInstallerManagedLiveResume`, `defer finishCallers()` runs before the `t.Cleanup` joins, so `<-blockerDone` and `<-finished` do not wait on a live caller. The `Wait` calls on `exec.CommandContext` have no `WaitDelay`; a descendant holding the output pipe would block them. This is unproven and unchanged.
- `install_managed.go` is untouched, so there is no conflict with #71 or #65.
