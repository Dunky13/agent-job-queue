# Approved repair has one owner

The setup window's "Repair and continue" used a Swift copy of the approved-repair policy (which issue maps to which recovery, in what order, when to wait). Go owns it alone now.

## Result

- The window runs `agent-heavy install --app-bundle APP --repair-and-continue --keep-custom-callback` after one confirmation. The Swift step loop, `ApprovedRepairStep`, `approvedRepairStep` and the draining hold are deleted.
- `--keep-custom-callback` is valid only with `--repair-and-continue`. It removes `reset-callback` from every reported issue, so a requirement that needs a callback replacement stops for review. An existing managed installation still keeps its customized callback byte for byte and continues.
- An unsupported requirement raises a typed review error. `installReportFailure` prints `AGENT_QUEUE_SETUP_STATE:requires-review` before the message, and the app shows the review state, refreshes the listed requirements and keeps Go's line in the diagnostic output. If the refresh finds nothing, the failure state is shown.
- The window gets the final installation, legacy service backup and approval or relocation waits by construction, and issues no separate plain install.
- Progress lines name the repair by its human title. The window's headline follows the last progress line.
- The button is offered whenever requirements are listed and no action is running; the controller decides and reports.
- Ordinary launches no longer pre-check the mapping in Swift. Go does it instead: before changing anything, `--repair-and-continue` checks the plain preflight report, so customized legacy service settings, which automatic setup would back up and retire, still stop for review (owner decision, 25 November 2026). Definitions that Go itself restores are still retired automatically.

## Evidence

- `TestInstallerAutomaticSetupStopsForCustomizedLegacyServiceOptions` pins the review stop with no file changes.
- Suites: see PR

## Remaining work

- None beyond merge approval.

## Next steps

1. Merge after hosted CI passes and the operator approves.
2. Continue the plan with the queue action module, stacked on PR #86.
