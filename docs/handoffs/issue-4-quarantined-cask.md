# Issue 4: quarantined Homebrew installer

The cask clears only `com.apple.quarantine` from its verified staged download before executing the installer or exposing CLI wrappers. Ad hoc signatures, unrelated extended attributes, the queue daemon, existing jobs, credentials, and system-wide security settings remain intact. Apple Developer Program membership and notarization are outside this change.

## Cause and implementation

Issue #4 reports build 13's quarantined installer script terminated by SIGKILL before any user directories were created. The original cask had no preflight. Calling an interpreter explicitly would leave the copied app and executable wrappers quarantined.

The trusted renderer now selects supported `preflight_steps` on current Homebrew and legacy `preflight` on older versions. Both execute `/usr/bin/xattr -dr com.apple.quarantine` against only the cask's staged path and require success. Homebrew 7.0.9 rejects the issue's literal legacy block in developer mode; the compatibility branch preserves developer validation instead of suppressing it.

The real Homebrew DSL helper requires exactly one preflight artifact, verifies its order before the installer and both binaries, and executes it before the private fixture install. The optional quarantine fixture marks every staged entry and a separate retained test attribute. Manual ZIP/DMG installation continues to use Apple's per-app approval flow; its script is unchanged.

## Verification

- The new renderer regression failed against the original template because no preflight existed.
- All 15 Homebrew tests passed, including actual modern execution, simulated older DSL registration using the real legacy artifact, missing-preflight rejection, failed-command abort before installer execution, default unquarantined installs, and quarantined payloads with both CLIs executing afterward.
- All 12 installer tests passed, including actual isolated daemon execution, concurrent-installer rejection, and upgrade preservation.
- The configured lint suite passed: public-source boundary, Ruff, web lint, shell checks, actionlint, and Ruby syntax. No Python type checker is configured.
- The actual published build-13-68a78e6 payload installed with the new cask into a private fixture: all 39 staged quarantine attributes were removed and all 39 unrelated test attributes survived. The installed app passed strict deep code-signature verification; both installed CLI help commands succeeded.
- The published app also launched from that fixture in inert screenshot mode and produced a queue image with exit 0. A sandbox-extension diagnostic appeared on stderr, without preventing rendering. No live queue or service was changed.
- The exact reported SIGKILL was not reproduced locally: a bare quarantined script returned its normal usage error, while executing the script inside the quarantined signed bundle stalled in OS evaluation until the bounded probe terminated it. The fix is verified through the actual quarantine attribute and artifact execution path, not a claim that the original OS failure was reproduced exactly.
- The AST knowledge graph was refreshed. Generated graph outputs remain untracked.

## Distribution and remaining gates

The tap refresh regenerates casks using its own trusted renderer, not the Ruby file attached to a release. A companion tap pull request synchronizes `render_cask.py` and `test-cask.rb` from this source, updates its trust guidance, and leaves generated `Casks/` files untouched.

After human approval, merge the tap tooling first, then the app pull request. App main CI publishes a new installer build. Run the tap refresh workflow after publication; its existing monotonic build check requires that newer release before updating the generated cask. Verify the generated preflight and checksum, then retry a clean default Homebrew installation. Do not hand-edit generated metadata or overwrite release assets.

Hosted CI, merge, new release publication, generated tap publication, and a complete installation in a clean macOS user account remain separate gates. No live host installation or uninstall was performed. Protected main checks and the human merge gate remain required.

## References

- App issue: https://github.com/Dunky13/agent-job-queue/issues/4
- App source: `packaging/homebrew/render_cask.py`, `packaging/homebrew/test-cask.rb`, `tests/test_homebrew.py`
- Tap: https://github.com/Dunky13/homebrew-agent-job-queue
- Apple manual approval: https://support.apple.com/102445
