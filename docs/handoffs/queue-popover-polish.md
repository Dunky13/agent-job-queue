# Queue popover polish handoff (PR #86)

The operator reported that the menu-bar popover felt cramped and jumped, skipped or closed without user action. Layout A, "compact inline", was chosen from three mockups. The popover also stays open when another app takes focus.

## Result

- **Header:** the limit and scheduling mode are menu pickers that apply on selection. Each shows the requested value until the queue reports it, and both are disabled while an action runs or in preview mode. Help text lives in tooltips. Host load is one line with drawn memory and CPU bars; the scheduler summary and reason are in the tooltip and the accessibility label.
- **Rows:** each row shows the name, then one meta line (context, id, state, elapsed), then one resource line (`QueueJob.resourceLine`). Run next, Move to end and Start now are icon buttons with per-job accessibility labels. Start now and Cancel still use the confirmation panel.
- **Jumps removed:**
  - The status-item title is held while the popover is shown and applied on close.
  - Job details and confirmations overlay the list instead of replacing it, so its scroll position is kept.
  - Notices replace the footer status line and clear after five seconds.
  - The list is a `ScrollView`/`LazyVStack` with no translucent pinned headers.
- **Closing:** the popover no longer closes on `didResignActive`. Outside clicks (local and global monitors), Escape and the status item still dismiss it.

## Evidence

- **Native suite:** `scripts/test-native.sh` passes, including the new row-summary checks in `BehaviorTests`.
- **Menu-bar UI suite:** `native/tests/run-menu-bar-ui-tests.sh` passes, including the inverted focus check and the held-title checks. Its eight system-click checks skip on hosts without event-posting permission.
- **Resume UI fixture:** the list is no longer a table, so `ResumeUITests` clicks the paused banner's Resume button relative to the list's scroll content and checks that the content shrinks after resume. `native/tests/run-resume-ui-tests.sh` passes all ten checks locally; hosted CI first failed on "Paused fixture list missing".
- **Screenshots:** `scripts/capture-native-screenshots.sh` regenerated `mac-queue.png` and `mac-start-now.png`.
- **Reviews:** independent Standards and Spec reviews ran on the change, and their findings were fixed before delivery.

## Remaining

1. In a live session, confirm that returning from job details keeps the scroll position and that a notice clears after five seconds. No automated test covers either yet.
2. Hosted CI and merge approval are pending on PR #86.
