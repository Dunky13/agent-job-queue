# PR 7 follow-up: main CI run 22 shutdown repair

## Scope and diagnosis

[Main CI run 22](https://github.com/Dunky13/agent-job-queue/actions/runs/39271222753) failed after [PR 7](https://github.com/Dunky13/agent-job-queue/pull/7) merged as `086f0c3fa3c13b411f68fb6702696c91219623d7`. On macOS ARM, the pre-acknowledgement gateway test observed `disable` exiting 125. The next test could not become ready. Installer publication was skipped, so the version and learning-controls release did not publish.

An unacknowledged child can exit after its supervisor dies, between the controller's birth check and process-group lookup. Supervisors can likewise depart between their identity check and signal delivery. Four deterministic regressions against the original main source reproduce `ProcessLookupError` or a shutdown error containing `[Errno 3] No such process`. The CI log omitted the original disable error text: this establishes a matching failure mechanism rather than proving its exact historical stderr.

## Repair and safety

`remote/control.py` treats `ProcessLookupError` as an already departed process at the owned child group lookup, supervisor TERM/KILL signals, and test-mode service stop. Child cleanup retains its final birth check before removing the record. Permission errors and mismatched identities remain failures. Boot, birth, self-process, generation, acknowledgement, ownership and escalation checks remain intact.

`remote/tests/test_remote.py` adds nine deterministic real-process regressions with private temporary state. Four departure cases fail against the original source and pass with the repair. Ownership and permission controls pass against both. The existing real HTTPS scenario now includes CLI stdout/stderr in its assertions. No tests, deadlines or checks were weakened.

## Validation and delivery boundary

- All 32 remote/API/real-Caddy HTTPS tests passed after the final changes, including the original pre-acknowledgement scenario. The complete run used the shared heavy-command queue.
- Repository-wide Python Ruff, the public-source boundary check and diff whitespace checks passed. There is no configured Python type checker.
- Independent Standards and Spec/security source reviews reported no findings in the executable diff. Cross-provider review was unavailable and skipped.
- Hosted CI on the new PR, merge, main installer publication, Homebrew publication and operator installation are separate steps. Their live state belongs to GitHub, rather than this local validation record.

Reproduce the applicable suite in the configured development environment using the operator's shared queue policy:

```sh
python -m unittest discover -s remote/tests -p test_remote.py -v
python -m ruff check .
python scripts/check-public-source.py
```

## Homebrew observations and proposed synchronization

Read-only inspection on 2026-10-19 found one installed user app and one running Agent Queue UI, both from Homebrew build 15. The Caskroom installer and its nested payload are staging copies. Both CLI entry points use the same per-user backend. No duplicate running queue was established, and no installed files or live services were changed.

The remote tap also still pointed to build 15, although app build 18 was published. Its refresh workflow was active, but GitHub reported zero scheduled runs and only manual refreshes. Build 18 predates PR 7's version window. Installing it alone would not supply that window.

Recommended follow-up, not implemented by this repair: make each successfully published immutable main build dispatch the tap workflow using a narrowly scoped GitHub App. The tap should validate the exact source commit, manifest and installer digest, publish its signed cask commit with its own token, and allow the source delivery job to verify the result. Serialize updates, reject downgrades, make retries idempotent, and retain scheduled reconciliation plus a visible drift failure as recovery mechanisms. Synchronize tap tooling and its manifest validator through a reviewed update from the same source revision so copied helpers cannot silently drift.

The target is the latest successfully published main build. Failed CI must leave the prior Homebrew build available. Once the repaired main release and tap are verified, use the Homebrew upgrade path for this installation. The version appears in the app gear menu under **About and updates** after the new app is installed and running.

This handoff also has a complete standalone [HTML version](pr-7-ci-22.html).
