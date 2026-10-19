# PR 8: shutdown recovery and verified Homebrew delivery

[PR 8](https://github.com/Dunky13/agent-job-queue/pull/8) repairs the post-PR-7 shutdown race and implements the subsequently approved release-driven Homebrew synchronization. The complete setup and recovery procedure is in [Verified Homebrew delivery](../homebrew-delivery.md), with a standalone [HTML guide](../homebrew-delivery.html).

## Scope and diagnosis

[Main CI run 22](https://github.com/Dunky13/agent-job-queue/actions/runs/39271222753) failed after [PR 7](https://github.com/Dunky13/agent-job-queue/pull/7) merged as `086f0c3fa3c13b411f68fb6702696c91219623d7`. On macOS ARM, the pre-acknowledgement gateway test observed `disable` exiting 125. The next test could not become ready. Installer publication was skipped, so the version and learning-controls release did not publish.

An unacknowledged child can exit after its supervisor dies, between the controller's birth check and process-group lookup. Supervisors can likewise depart between their identity check and signal delivery. Four deterministic regressions against the original main source reproduce `ProcessLookupError` or a shutdown error containing `[Errno 3] No such process`. The CI log omitted the original disable error text: this establishes a matching failure mechanism rather than proving its exact historical stderr.

## Repair and safety

`remote/control.py` treats `ProcessLookupError` as an already departed process at the owned child group lookup, supervisor TERM/KILL signals, and test-mode service stop. Child cleanup retains its final birth check before removing the record. Permission errors and mismatched identities remain failures. Boot, birth, self-process, generation, acknowledgement, ownership and escalation checks remain intact.

`remote/tests/test_remote.py` adds nine deterministic real-process regressions with private temporary state. Four departure cases fail against the original source and pass with the repair. Ownership and permission controls pass against both. The existing real HTTPS scenario now includes CLI stdout/stderr in its assertions. No tests, deadlines or checks were weakened.

## Validation and delivery boundary

- All 32 remote/API/real-Caddy HTTPS tests passed after the final changes, including the original pre-acknowledgement scenario. The complete run used the shared heavy-command queue.
- The added delivery work passed 33 source/configuration tests, 45 tap reconciliation tests and 16 existing Homebrew DSL/installer-fixture tests in a queued final batch. All 45 reconciliation tests also passed from the standalone companion tap checkout. No test published to GitHub or installed the operator app.
- Repository-wide Python Ruff, the public-source boundary check and diff whitespace checks passed. There is no configured Python type checker.
- Independent Standards and Spec/security source reviews reported no findings in the executable diff. Cross-provider review was unavailable and skipped.
- Adversarial review caught two template-transition failures. Both were fixed and re-reviewed: historical casks are compared as bounded data, remain unchanged across successive tooling revisions, and advance with the next numbered app release. The prepared credential wizard passed Bash syntax and ShellCheck; the human-only browser procedure was not executed by the agent.
- Hosted CI on the new PR, merge, main installer publication, Homebrew publication and operator installation are separate steps. Their live state belongs to GitHub, rather than this local validation record.

Reproduce the applicable suite in the configured development environment using the operator's shared queue policy:

```sh
python -m unittest discover -s remote/tests -p test_remote.py -v
python -m ruff check .
python scripts/check-public-source.py
```

## Homebrew observations and synchronization

Read-only inspection on 2026-10-19 found one installed user app and one running Agent Queue UI, both from Homebrew build 15. The Caskroom installer and its nested payload are staging copies. Both CLI entry points use the same per-user backend. No duplicate running queue was established, and no installed files or live services were changed.

The remote tap also still pointed to build 15, although app build 18 was published. Its refresh workflow was active, but GitHub reported zero scheduled runs and only manual refreshes. Build 18 predates PR 7's version window. Installing it alone would not supply that window.

Each successfully published immutable main build now dispatches the tap workflow through a GitHub App limited to Actions write access on the tap. The tap validates the exact requested release, published source ancestry and actual installer digest, then publishes its signed/DCO cask commit with its own token and expected-head guard. The source waits for the exact run ID and independently verifies the public release, signed tap head, strict manifest and canonical cask. A newer validated published descendant can explicitly supersede an older request without rollback. Missing credentials, failed workflows, drift and deadline expiry fail visibly.

The source also requires byte parity for six trusted tap files. They are copied to [companion tap PR 2](https://github.com/Dunky13/homebrew-agent-job-queue/pull/2), which must merge before this source PR. The scheduled refresh remains recovery and revalidates no-op state. Current casks from a previous reviewed template are checked against their own historical release artifact as data. Template changes await a newer numbered app release, preserving provenance across repeated tooling updates; downloaded Ruby is never evaluated. PR validation has separate concurrency and no publication credentials.

The source repository still needs its owner to register/install the restricted GitHub App and configure the Client ID variable and private-key secret before merge. The prepared setup procedure sends the PEM directly to GitHub. Live cross-repository dispatch, main release publication, Homebrew publication and operator installation remain unperformed until those gates are satisfied.

The target is the latest successfully published main build. Failed CI must leave the prior Homebrew build available. Once the repaired main release and tap are verified, use the Homebrew upgrade path for this installation. The version appears in the app gear menu under **About and updates** after the new app is installed and running.

This handoff also has a complete standalone [HTML version](pr-7-ci-22.html).
