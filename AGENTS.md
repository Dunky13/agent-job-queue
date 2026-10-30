# Agent guide

Keep the app, CLI, installer, and dashboard portable across Apple Silicon and Intel Macs. Do not commit local state, certificates, credentials, private logs, or machine-specific checkout paths. The menu bar app owns the queue processes: it starts and supervises the queue daemon, resource scheduler and phone-access service as its own children and stops them when the app exits for any reason (quit, crash or force quit). Preserve fail-closed process ownership checks, including the daemon's own lifetime locks, which still refuse a duplicate instance. Tests use temporary private daemons and simulated dashboard jobs.

Run the applicable commands documented in CONTRIBUTING.md. Resource-intensive local validation follows the operator's shared queue policy. CI runs on separate hosted machines.

Commits require cryptographic signing and a DCO sign-off matching the author identity. Keep configured signing enabled. Use Conventional Commits. Install the repository's pre-push hook with `git config core.hooksPath .githooks`.

Do not bypass required checks, branch protection, lint rules, tests, signatures, or ownership verification. Do not use em dashes.
