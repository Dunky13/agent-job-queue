# GitHub Pages delivery handoff

## Result

Homebrew is dropped. The Sparkle appcast and `latest.json` are deployed to GitHub Pages at `https://dunky13.github.io/agent-job-queue/` by the distribution workflow, together with the homepage files from `site/`. Nothing is committed to `main` for delivery. This replaces the in-repository tap work (ADR 0002) under ADR 0003 and is package B of `docs/specs/sparkle-updates.md`. Package A (Sparkle inside the app, with the feed URL above) and the homepage are separate; the first delivery needs both `packaging/sparkle/public-ed-key.txt` and a `site/` directory on `main`.

## What changed

- Removed: `Casks/`, `tooling.RenderCask` and its callers, `packaging/homebrew/`, the Homebrew DSL checks, the notarized Homebrew install test and the cask-only tests, the `agent-job-queue.rb` release asset (`agent-job-queue.json`, ZIP and DMG stay), the GitHub App token step, `createCommitOnBranch`, compare-and-swap and readback, and the CI `paths-ignore`. Runtime Homebrew handling in `internal/backend` and `internal/remote` is unchanged.
- `internal/pages` and `cmd/queue-pages` (renamed from `internal/tap` and `cmd/queue-tap`): `prepare` verifies and binds the release, reads the live site, signs and assembles `site/` plus `appcast.xml` and `latest.json`; `verify-live` reads the deployed site back. `queue-tool render-cask` is now `render-manifest`.
- `.github/workflows/distribution.yml`: `tap-delivery` became `delivery` (**Publish Sparkle feed and homepage**, `macos-15`, `github-pages` environment, permissions `contents: read`, `pages: write`, `id-token: write`). It uses SHA-pinned `actions/upload-pages-artifact` v5.0.0 and `actions/deploy-pages` v5.0.1. The Sparkle key reaches only the signing step.
- A run superseded by a newer public or live build, or whose feed content is already live (signatures ignored), deploys nothing and succeeds. Only an HTTP 404 means no site yet; every other live-read failure fails the job.
- `scripts/distribution-recover.sh` and `scripts/distribution-checkpoints.sh` classify the new job as delivery and still accept both older job names.
- Docs: `docs/pages-delivery.md` and `.html` replace `docs/homebrew-delivery.md` and `.html`; README, CONTRIBUTING, `docs/apple-signing` and `docs/upgrade-recovery` no longer describe Homebrew delivery. Historical handoffs and ADR 0002 are untouched.

## Human steps still required

1. Done 2026-11-25: the EdDSA key was generated in the owner's login keychain under the account `agent-queue` (`generate_keys --account agent-queue`), the private key is the Actions secret `SPARKLE_ED_PRIVATE_KEY`, and the public key is committed in `packaging/sparkle/public-ed-key.txt`.
2. Done 2026-11-25: Pages is enabled with the GitHub Actions source, and the `github-pages` environment allows only `main`.
3. Merge the homepage so `site/` exists on `main`; delivery fails clearly without it.
4. Optional cleanup: delete the `HOMEBREW_SYNC_APP_CLIENT_ID` variable and `HOMEBREW_SYNC_APP_PRIVATE_KEY` secret, uninstall the Homebrew-sync App from this repository, remove its bypass entries on the `main` and `main-ci` rulesets, and archive `homebrew-agent-job-queue` with a pointer to the homepage.
5. Acceptance gate: once two Sparkle builds are published, update from build N to N+1 on the SentinelOne Mac.

Existing Homebrew users need to do nothing. `brew untap dunky13/agent-job-queue` is optional, and `brew uninstall` must never be suggested because it deletes the app.

## Not verified

- No GitHub write API, workflow dispatch, Pages deployment or live read-back ran. `actions/upload-pages-artifact`, `actions/deploy-pages`, the `github-pages` environment rules and the propagation delay (the read-back allows 18 attempts, 10 seconds apart) need the first real run.
- The `site/` homepage was not available here; assembler behaviour is tested against fixtures in `internal/pages/testdata`.
- The real Sparkle `sign_update` was checked locally through the opt-in `SPARKLE_SIGN_UPDATE` test with a throwaway key only; the production key and public key file were not exercised.
- Whether `actions/upload-pages-artifact` keeps `.nojekyll` (it skips hidden files) was not confirmed; Pages artifact deployments do not run Jekyll, so this should not matter.
