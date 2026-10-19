# Contributing

Use Python 3.12+, Node.js 24+, a macOS 15+ development environment, and Xcode command-line tools. The installer itself does not need Xcode, but building it does.

```sh
brew install pueue caddy uv actionlint shellcheck
uv venv --python 3.12
uv pip install --require-hashes -r remote/requirements.lock
uv pip install -r requirements-dev.txt
npm ci
git config core.hooksPath .githooks
```

Activate the virtual environment for these commands:

```sh
source .venv/bin/activate
bash scripts/lint.sh
python -m unittest discover -s tests -p test_agent_heavy.py -v
python -m unittest discover -s tests -p 'test_resource*.py' -v
python -m unittest discover -s tests -p test_job_profiles.py -v
python -m unittest discover -s tests -p test_agent_integration.py -v
python -m unittest discover -s tests -p test_integration.py -v
python -m unittest discover -s remote/tests -p test_remote.py -v
npm run test:web
npx playwright install chromium
npm run test:browser
bash scripts/test-native.sh
scripts/build-installer.sh
scripts/test-installer.sh "dist/Agent-Job-Queue-Installer-$(cat VERSION)-universal.zip"
```

Apply your shared queue policy to resource-intensive local batches. Hosted CI has its own isolated runners. Do not submit the queue wrapper inside an already queued batch.

Core and HTTPS acceptance tests launch their own temporary daemons, state directories, child processes, ports, and test-only credentials. Browser tests use simulated jobs. Installer and agent-integration tests use isolated user directories. They must never operate on your active queue or real agent instructions. Native tests are headless and check the actual store and navigation handlers, including opt-in integration, duplicate-save protection, partial failures, and onboarding state.

Use `AGENT_QUEUE_TEST_PYTHON` to select a Python interpreter with the remote dependencies for browser tests. Playwright runs one Chromium worker and does not retry failures.

## README screenshots

Use artificial jobs when capturing documentation. Never capture your real queue, logs, passwords, or certificate settings.

```sh
scripts/capture-native-screenshots.sh
python scripts/preview-screenshots.py
```

The first command builds the native app for the current Mac, captures its queue and start confirmation, and verifies confirmation navigation using an inert action recorder. It needs a logged-in graphical macOS session. The fixture is [native-fixture.json](docs/screenshots/native-fixture.json), and timestamps are shifted at capture time to keep elapsed labels readable.

The second command serves the actual dashboard with simulated jobs at the printed ephemeral URL. Pass `--bind` with your current private LAN IPv4 address to preview it on another device. Resize the browser to 390 by 844 CSS pixels and capture the viewport as `docs/screenshots/phone-queue.png`. Stop the preview with Ctrl+C afterward. This preview cannot call the installed queue or load its settings.

When changing runtime Python dependencies, update the exact constraints in `remote/requirements.txt`, then regenerate the hash lock:

```sh
uv pip compile remote/requirements.txt --python-version 3.12 --generate-hashes --output-file remote/requirements.lock --no-header
```

Keep all existing lifecycle, cancellation, ownership, crash-recovery, authentication, and UI regression coverage. Fix lint failures rather than suppressing them. Vendored code is excluded from style lint and retains its license.

Commit with configured cryptographic signing enabled and a DCO sign-off:

```sh
git commit -s -m "fix(queue): describe the change"
```

The pre-push hook checks the full PR range from `origin/main` to the pushed head, including previously pushed PR commits. Fetch `origin/main` before pushing to keep that baseline current. A fresh repository without that reference checks the initial history or commits since the existing remote branch. GitHub checks DCO and its verified-signature record, and protects `main` with the aggregate required check. Open a pull request; direct pushes, force pushes, and admin bypasses are not part of the delivery process. Preserve sign-offs in the final squash commit message.

## Squash author and sign-off

A squash merge creates a new commit. Its author name and email must have an exactly matching `Signed-off-by` trailer, even when all original PR commits already passed. GitHub email privacy can select a noreply address for that new author while retaining trailers containing another email address.

In GitHub's squash confirmation, select the verified author email that matches the sign-off, or add a sign-off matching the selected author. Compulsory web sign-offs do not replace checking the final identity. Do not change privacy settings or weaken the DCO check to resolve a mismatch.

From this repository's checkout, the CLI can set both fields consistently after reviewing the PR and confirming its required checks passed:

```sh
merge_pr=2 # Replace with the reviewed pull request number.
merge_author_name=$(git config user.name)
merge_author_email=$(git config user.email)
reviewed_head=$(gh pr view "$merge_pr" --json headRefOid --jq .headRefOid)
gh pr merge "$merge_pr" --squash --match-head-commit "$reviewed_head" \
  --author-email "$merge_author_email" \
  --body "Signed-off-by: $merge_author_name <$merge_author_email>"
```

Check that both configured identity values are nonempty before running the merge. This supplies the final squash sign-off and pins the reviewed head; GitHub still enforces branch protection. Afterward, verify the resulting commit's author, trailer, and Verified signature, then check the `main` workflow and release publication.

Build artifacts in `dist/`, private runtime data, local logs, credentials, certificates, and personal checkout paths must stay out of commits. The public-source check enforces common boundary mistakes.
