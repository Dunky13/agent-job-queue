# Agent integration setup

## Result

The native menu-bar app offers optional Agent integration on its first launch after this update, including Homebrew installations. The queue gear menu can reopen the same settings. Codex and Claude Code are initially unselected unless this app has already installed their managed instructions. Saving configures only selected runners; deselecting a configured runner removes only its managed section. The CLI exposes the same status/install/remove operations through agent-heavy agents.

## File preservation

The backend appends a delimited section while preserving every unrelated byte and the existing file mode. It writes private adjacent backups before atomic replacement, is idempotent, and refuses unsafe ownership, symlinks, hard links, duplicate or edited sections, concurrent changes and invalid profile paths. Status inspection does not create files or queue state. Existing custom guidance or imports are never claimed as this app's configuration.

Codex targets its actual global instruction file, honoring CODEX_HOME and the first non-whitespace AGENTS.override.md. Whitespace matching follows Codex's lossy UTF-8 decoding and Rust Unicode White_Space behavior. Empty overrides stay untouched so existing AGENTS.md rules remain active. Claude Code honors CLAUDE_CONFIG_DIR. Codex additions above the default 32 KiB global instruction budget are refused. The app displays paths visible in its own environment; shell-only profiles can use the CLI from that shell.

## Validation and review

Local validation passed: lint and source-boundary checks; 59 core tests; 29 instruction integration tests; 22 real-daemon acceptance tests; 18 HTTPS/API tests; native refresh, navigation and integration-store regressions; 12 installer tests; and 10 Homebrew metadata/installer-artifact tests. The universal arm64/x86_64 installer was built, strict signatures and private-state exclusion verified, and its packaged integration module compared with source. Native preview uses inert sample data and cannot change real instructions. The setup screenshot was visually inspected.

Independent source review caught and resolved Codex whitespace-override precedence and a missing local-validation entry. A Homebrew fixture initially timed out downloading a package index into its empty temporary account. Its existing local-cask/stub-dependency path now disables API package resolution, and the legacy compatibility shim explicitly rejects package-index downloads. All ten checks pass with real Homebrew DSL and installer artifacts; no timeout or production boundary was relaxed.

Hosted checks, merge and public release must be checked on the pull request's actual head. This handoff records local evidence, not remote publication. Cross-provider review was unavailable and was not treated as passed.

## User flow

Open Agent Queue, select Agent integration, choose runners, review Preview queue instructions, then Save instructions. Confirm the selected runner says Instructions installed and start a new agent session. Not now closes only the integration window. Refresh rechecks current files. Per-runner file errors remain visible; a partial save reconciles actual status and permits a later retry. The app does not automatically save selections or silently retry failed writes.

## Limits

Instructions guide agent behavior; they do not intercept shell commands or enforce admission. Existing sessions need restarting. Other runners require manual instructions. Hand-edited managed sections and unsupported file layouts require manual resolution. No real provider sessions were launched, and the installed live app, queue, jobs, credentials and instruction files were left unchanged. This feature becomes publicly installable after its PR merges and validated main publication completes.

## Operator commands

```sh
agent-heavy agents status
agent-heavy agents install --runner codex
agent-heavy agents install --runner claude
agent-heavy agents remove --runner codex
```

## Installer verification follow-up

The published build-10-ca5ff4b ZIP matched its checksum and passed strict deep code-signature integrity verification, while spctl rejected it for execution. It is ad hoc signed with no Developer ID or notarization. This does not attribute the reported generic popup to Agent Queue: local logs around its timestamp did not identify its source. The README now explains Apple's per-app Privacy & Security approval flow for the named Agent Queue installer or menu app. Future release notes state the same limitation prominently. No Gatekeeper or quarantine setting was changed, no signing boundary was bypassed, and no paid-account signing was configured.

## Source references

- [Codex instruction discovery](https://learn.chatgpt.com/docs/agent-configuration/agents-md)
- [Codex upstream file-selection implementation](https://github.com/openai/codex/blob/main/codex-rs/codex-home/src/instructions/mod.rs)
- [Claude Code user instructions](https://code.claude.com/docs/en/memory)
- [Claude Code environment variables](https://code.claude.com/docs/en/env-vars)

## Native setup preview

![Agent integration with Codex and Claude Code unselected](../screenshots/agent-integration.png)
