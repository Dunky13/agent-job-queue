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

Core and HTTPS acceptance tests launch their own temporary daemons, state directories, child processes, ports, and test-only credentials. Browser tests use simulated jobs. Installer tests use stub tools and an isolated user directory. They must never operate on your active queue. Native tests are headless and check the actual store and navigation handlers.

Use `AGENT_QUEUE_TEST_PYTHON` to select a Python interpreter with the remote dependencies for browser tests. Playwright runs one Chromium worker and does not retry failures.

When changing runtime Python dependencies, update the exact constraints in `remote/requirements.txt`, then regenerate the hash lock:

```sh
uv pip compile remote/requirements.txt --python-version 3.12 --generate-hashes --output-file remote/requirements.lock --no-header
```

Keep all existing lifecycle, cancellation, ownership, crash-recovery, authentication, and UI regression coverage. Fix lint failures rather than suppressing them. Vendored code is excluded from style lint and retains its license.

Commit with configured cryptographic signing enabled and a DCO sign-off:

```sh
git commit -s -m "fix(queue): describe the change"
```

The pre-push hook checks every commit being introduced. GitHub checks DCO and its verified-signature record, and protects `main` with the aggregate required check. Open a pull request; direct pushes, force pushes, and admin bypasses are not part of the delivery process. Preserve sign-offs in the final squash commit message.

Build artifacts in `dist/`, private runtime data, local logs, credentials, certificates, and personal checkout paths must stay out of commits. The public-source check enforces common boundary mistakes.
