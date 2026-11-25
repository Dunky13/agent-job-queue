---
status: accepted
---

# The Homebrew tap and Sparkle feed live on this repository's main branch

Every main build is published as a GitHub prerelease. GitHub's latest-release download URL excludes prereleases, so it cannot serve a stable Sparkle feed. Rather than add GitHub Pages or keep a separate tap repository with its own cross-repository tooling sync, the owner chose to fold the tap into this repository. After each successful publication, the distribution workflow commits `Casks/agent-job-queue.rb`, `Casks/agent-job-queue.json` and `appcast.xml` to `main`. Sparkle reads the feed from `https://raw.githubusercontent.com/Dunky13/agent-job-queue/main/appcast.xml`.

## Consequences

- Only the Homebrew-sync GitHub App writes these commits. It uses GitHub-signed API commits with a DCO trailer and an expected-head compare-and-swap. The App is a bypass actor on the `main` and `main-ci` rulesets. This is a deliberate owner-approved exception to the rule against bypassing branch protection, and is limited to that App. Human changes still go through pull requests.
- CI ignores pushes that change only `Casks/**` and `appcast.xml`, so a delivery commit cannot start another release.
- Brew clones the default branch, so users tap with the explicit URL: `brew tap dunky13/agent-job-queue https://github.com/Dunky13/agent-job-queue`. Existing users run `brew tap --custom-remote dunky13/agent-job-queue https://github.com/Dunky13/agent-job-queue` once. The old `homebrew-agent-job-queue` repository has its scheduled refresh disabled and is archived with a pointer to this repository.
- The cask declares `auto_updates true` because Sparkle updates the app.
- The cross-repository tooling parity check and the tap dispatch are removed.

## Considered options

- GitHub Pages from this repository. It needs no commits to `main` and no bypass, but it was rejected in favor of a single home for the cask and the feed.
- An orphan feed branch. It avoids the bypass but leaves the tap in a separate repository.
