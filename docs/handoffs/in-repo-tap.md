# In-repository tap and Sparkle feed handoff

## Result

The Homebrew cask and the Sparkle appcast are now committed to this repository's `main` by the distribution workflow, replacing the separate `homebrew-agent-job-queue` tap and its dispatch. This is package B of `docs/specs/sparkle-updates.md`; decisions are in ADR 0002. Package A (Sparkle inside the app) is separate, and B must merge first or builds would ship a feed that returns 404.

## What changed

- `Casks/agent-job-queue.rb` and `Casks/agent-job-queue.json`: byte-for-byte the content of the old tap's `tap` branch at build 284, so the first run starts from a verified state.
- `internal/tap` and `cmd/queue-tap`: the refresh logic moved into a normal package. It verifies the public release and ZIP checksum, selects the highest build without regressing, renders the cask (through `tooling.RenderCask`, now with `auto_updates true`), checks it in the real Homebrew DSL, signs the ZIP and feed with `sign_update --ed-key-file -` (key on stdin only), and makes one GitHub-signed `createCommitOnBranch` on `main` with a DCO trailer and `expectedHeadOid`, retrying a bounded number of times when `main` moved. It reads the commit back by SHA.
- `.github/workflows/distribution.yml`: the `homebrew-sync` job became `tap-delivery` (**Publish Homebrew tap and Sparkle feed**, `macos-15`). `.github/workflows/ci.yml`: push to `main` ignores `Casks/**` and `appcast.xml`.
- Removed: `internal/delivery`, `cmd/queue-delivery`, `packaging/homebrew/refresh*.go`, `SEED.txt`, the tap's `refresh.yml` and the cask template parity test. `packaging/homebrew/test-cask.rb` stays.
- `scripts/distribution-recover.sh` and `scripts/distribution-checkpoints.sh` classify the renamed job as delivery and still accept the old name for historical runs.
- Docs: `docs/homebrew-delivery.md` and `.html`, README and CONTRIBUTING.

## Human steps still required

1. Generate the EdDSA key and store it as the Actions secret without pasting it anywhere: run `"$dir/bin/generate_keys"`, then `"$dir/bin/generate_keys" -x sparkle-private-key`, then `gh secret set SPARKLE_ED_PRIVATE_KEY --repo Dunky13/agent-job-queue < sparkle-private-key`, then `rm sparkle-private-key` (`dir` is the output of `packaging/sparkle/fetch.sh "$TMPDIR/sparkle"`). Commit the public key (one base64 line) to `packaging/sparkle/public-ed-key.txt` (package A). It must exist before the first delivery: the job verifies both Sparkle signatures against it and fails if the file is missing, malformed or does not match the secret.
2. Install the Homebrew-sync App on this repository with **Contents: Read and write**, and add it as a bypass actor on the `main` and `main-ci` rulesets. Keep the existing `HOMEBREW_SYNC_APP_CLIENT_ID` variable and `HOMEBREW_SYNC_APP_PRIVATE_KEY` secret.
3. After the first successful delivery: disable the old tap's refresh workflow, then archive `homebrew-agent-job-queue` with a pointer here.
4. Acceptance gate: once two Sparkle builds are published, update from build N to N+1 on the SentinelOne Mac.

## Behaviour worth knowing

- The first run after merge commits the new template (`auto_updates true`) and `appcast.xml` for the build already on `main`, because the seed predates both. Later runs with identical content create no commit.
- A delivery for build N when build N+1 is already public is superseded: it delivers nothing, after verifying release ancestry, and the N+1 run publishes N+1. The job downloads this run's notarized artifact and refuses to sign unless it matches the public manifest and ZIP exactly.
- The delivery tool is built before secrets exist, reads the Sparkle key once and unsets it; only `gh` gets the App token, `brew` and `sign_update` run with a minimal environment.
- The publish step sets `HOMEBREW_NO_INSTALL_FROM_API=1`, mirroring the cask test harness environment; this could not be confirmed locally, see below.
- The commit author is expected to be `<app-slug>[bot]` with email `<bot id>+<app-slug>[bot]@users.noreply.github.com`, computed in the job; the readback fails visibly if GitHub attributes the commit differently.

## Not verified

- The live GitHub-signed commit, bypass behaviour, the App-bot author attribution and the real Sparkle signer on a hosted runner were not exercised locally; they need the first real run.
- The real Homebrew DSL check could not run on the development machine (Homebrew 7.0.16 refuses path-loaded casks, which also fails the existing `TestHomebrew*` tests there); it runs in the delivery job on `macos-15`.
