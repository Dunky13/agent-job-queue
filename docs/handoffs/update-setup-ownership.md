# Update setup ownership recovery

## Reported failure

After an upgrade from the Python distribution to the Go distribution, setup displayed “Review loaded background service ownership” with a missing-file error and “Review loaded phone access ownership”. Read-only inspection confirmed the queue, scheduler, and phone services were still loaded, while all three corresponding LaunchAgent files were absent. The cause of deletion was not established. The historical cask inspected for this upgrade only removed the menu app and its login file.

The retained phone service also used the shipped `remote/.venv/bin/python` interpreter, which the installer rejected because its allowlist only included the main installation's `.venv/bin/python` interpreter.

A second screenshot showed “Completed jobs still need cleanup”. The same check also sees markers held by active work. Read-only inspection later found no receipts or retained lease markers, and this blocker cleared without deleting anything. The diagnostic now describes both queue work and cleanup rather than assuming completed jobs, and points to the actual blocking receipt directory or lease marker rather than always opening the receipts folder.

## Recovery contract

The requested final flow requires one approval rather than separate manual repairs. **Repair and continue** applies only the fixed, verified startup repairs, rechecks after each step, waits for active jobs, and completes installation and readback automatically. Cancellation or a failure revokes approval. Unknown ownership, custom callbacks and paused/stashed jobs remain explicit holds. The ZIP installer opts into the same policy through `--repair-and-continue`; its controller deadline remains finite.

Setup offers a confirmed **Restore verified service files** operation only for missing startup files whose loaded commands and installation paths can be verified. Recovery recreates standard startup definitions without stopping or reloading services. It preserves existing files and rejects ambiguous or foreign ownership evidence. The confirmation explains that lost custom startup settings cannot be recovered.

After restoration, a fresh preflight retains the ordinary migration gates: unfinished jobs and cleanup must finish, phone access requires explicit disablement, and custom callbacks or uncertain ownership still require review. The legacy phone interpreter allowlist includes the exact shipped remote virtual environment path without accepting arbitrary Python executables or scripts.

Backend tests use private installation and launch-service fixtures. Native tests use an injected controller, disposable homes, and simulated queue jobs. Production service inspection is read-only. Local validation, hosted CI, distribution, and installed runtime recovery are distinct evidence boundaries.

## Validation-discovered sidecar race

Full backend acceptance exposed a scheduler hold while SQLite retired a transaction journal. A focused real-SQL regression reproduced a regular, private journal owned by the current account with a zero link count during concurrent commit and unlink. The ownership check now rechecks only that retired sidecar case, with a fixed observation bound. Confirmed absence or a fresh fully safe file can pass; the main database, unsafe permissions or ownership, symlinks, hardlinks, unsafe replacements, and unresolved repeated retirement still fail closed. The original scheduler scenario and the existing unsafe-file tests remain required checks.

See [guided recovery](../upgrade-recovery.md) for the operator flow and controller commands.

## Bounded legacy history capture

The command output wrapper embedded `bytes.Buffer`, whose promoted `ReadFrom` method let a real child-process copy bypass the intended `Write` limit. Capture now enforces that limit through both copy paths. The exact owned Pueue status command uses the same 32 MB bound as saved legacy state; other installer commands retain their 4 MB bound. Real-process regressions cover retained history above 4 MB, oversized state rejection and ordinary command bounds. No private job contents are included in these fixtures.
