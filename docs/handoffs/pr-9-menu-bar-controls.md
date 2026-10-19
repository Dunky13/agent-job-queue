# PR 9: menu-bar shortcuts

[PR 9](https://github.com/Dunky13/agent-job-queue/pull/9) implements the request: "Let's add a right click settings menu on the menubar icon. Include open window as option, and add double click to open window as well".

## Behavior

- A primary click toggles the existing queue popover.
- Right-click or Control-click opens a native menu with **Open window**, **Learned workloads**, **About and updates**, **Agent integration**, **Remote access settings**, and **Quit Agent Queue**. The existing explanation that quitting leaves jobs running remains visible.
- A primary double-click opens or focuses the existing main queue window and closes the popover.
- The gear menu and the icon menu use the same application actions and settings labels.
- Keyboard and accessibility activation opens the popover directly. Mouse-only event properties are never read for other event types.

## Implementation

The previous icon was owned by SwiftUI's `MenuBarExtra`, with no right-click or double-click routing. Public AppKit status-item and popover APIs now own those interactions; the popover keeps the existing SwiftUI queue panel and stores. The queue badge, error state, window reuse, onboarding and independent daemon remain part of the existing application behavior.

A primary click opens the popover immediately. A second click switches to the main window. Dismissal uses an application-local event monitor and application focus notifications, so icon clicks remain available while the popover is open. Menu tracking keeps the footer and job menus usable.

The application retains standard editing, window and Quit shortcuts through owned AppKit menus. Closing its last window leaves the menu-bar app running.

## Validation

- The graphical fixture executed 32 successful checks against the real AppKit status button, popover, menus and production window handlers. It covers immediate single clicks, double-click sequences from closed and open popovers, singleton window reopening and restoration, both menu gestures while open, child-menu selection, Escape, outside-click dismissal, badges and accessibility, and monitor teardown. Events are synthetic AppKit events; fixture animations are disabled.
- Three global keyboard-dispatch checks (Select All, Undo, and Close Window) were explicitly skipped because macOS declined foreground activation of the fixture. Standard menu selectors/key equivalents and the owned editor's first responder were checked. The report records each skip, and `--require-foreground` makes unavailable foreground checks fail.
- Fixture testing reproduced two intermediate implementation regressions: a transient popover consumed Control-click, and a retained hosting view left the hidden popover counted as visible. Owned event dismissal fixes the first; explicit popover lifecycle accounting fixes the second. Repeated measured visibility cycles now return from zero to one to zero, and closed popovers retain no input monitor.
- Final queued validation passed all four native suites, the warnings-as-errors universal build for `arm64` and `x86_64`, ShellCheck, Bash syntax, public-source boundaries and diff checks. The actual application entry point rendered the fixture queue window and popover-size queue. Four confirmation/navigation scenarios preserved the visible surface, dispatched exactly the confirmed inert actions, and rejected stale or duplicate requests. Native screenshots were inspected locally.
- Standards/Spec review and exact-head hosted CI are recorded on the pull request separately from local verification. Cross-provider review is unavailable under the current operator policy.

Run the documented native suite and portable build in the configured development environment, through the operator's shared heavy-command queue:

```sh
bash scripts/test-native.sh
native/build.sh build/AgentQueue
bash native/tests/run-menu-bar-ui-tests.sh build/menu-bar-verification.json
```

The graphical runner requires a logged-in macOS session. Add `--require-foreground` to require the three foreground dispatch checks.

Tests and previews use artificial jobs and private fixture state. They must not dispatch work, alter real credentials or agent instructions, replace the installed app, or stop the live queue.

## Delivery boundary

The target is a reviewed, signed and DCO-signed-off pull request against `main`. Human merge approval, main installer publication, Homebrew delivery and operator installation are separate steps. Native compatibility remains macOS 15 or later on Apple Silicon and Intel.

This handoff also has a complete standalone [HTML version](pr-9-menu-bar-controls.html).
