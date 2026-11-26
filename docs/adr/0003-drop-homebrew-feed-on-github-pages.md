---
status: accepted
---

# Drop Homebrew; publish the Sparkle feed and homepage on GitHub Pages

This supersedes [ADR 0002](0002-homebrew-tap-in-source-repository.md). Sparkle updates the app in place ([ADR 0001](0001-sparkle-updates-in-place.md)), so Homebrew would only matter for first installs. For that it needs a tap, generated casks, a GitHub App and either a second repository or a bypass of `main`'s ruleset, so the owner dropped Homebrew distribution. The DMG from the homepage or the release page is the only install path. After each successful publication, the distribution workflow deploys the Sparkle feed and the project homepage to GitHub Pages at `https://dunky13.github.io/agent-job-queue/`. Sparkle reads `https://dunky13.github.io/agent-job-queue/appcast.xml`.

## Consequences

- No delivery commits reach `main`. The GitHub App, its ruleset bypass and the CI path filter are not needed.
- Releases no longer publish `agent-job-queue.rb`. `agent-job-queue.json` and the ZIP stay unchanged, because installed pre-Sparkle builds read them for their last manual update.
- Runtime support for apps already installed through Homebrew stays. Sparkle updates them in place, and their stale Homebrew receipt is harmless.
- The `Dunky13/homebrew-agent-job-queue` tap is frozen: its refresh workflow is disabled, a README notice points to the homepage, and it is archived.
- A Pages deployment replaces the whole site, so the feed, `latest.json` and the homepage are always published together from one verified build. A run for an older build never replaces a newer live site.
