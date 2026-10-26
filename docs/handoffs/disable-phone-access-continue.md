# Disable phone access and continue

When first-launch setup encounters enabled or loaded phone access from the Python installation, it now offers **Disable phone access and continue**. The window explains that credentials and configuration are preserved and phone access can be enabled again in settings. Unrelated failures retain **Retry**.

The installer emits a dedicated recovery diagnostic only for this legacy blocker. An explicit recovery click reruns the validated bundled installer with the trailing `--disable-phone-access` option. The backend invokes the bundled remote controller's existing ownership-checked disable operation before acquiring the remote control lock, then repeats migration checks. It retains queue drain, process ownership, payload validation, credential preservation, and installation readback requirements. This option does not disable an enabled Go installation's phone access. A failed shutdown keeps setup gated and displays its diagnostic.

## Validation

- The new native regression failed against the original generic failure state before implementation.
- The complete native regression suite passed, including transport, bootstrap, queue behavior, updates, and learning.
- The production AppKit setup-window fixtures passed for both phone access recovery and generic Retry, including the default button action, termination guard, installation readback, and transition into the simulated queue. Rendered screenshots were inspected.
- Installer and relevant remote controller ownership/shutdown tests passed with Go race detection and two test workers.
- Source portability, Go vet for the backend, Go formatting, shellcheck, shell syntax, and `git diff --check` passed.

Tests used disposable homes, injected setup executors, and private fixtures. The installed queue and phone access were not changed. A real legacy installation migration remains unverified. Delivery is authorized through a signed commit, PR, protected merge, and the existing notarized release pipeline. Full local validation and independent standards/spec reviews are running; hosted CI and release gates remain pending.

## Recheck

Run resource-intensive checks through the shared heavy-job queue as documented in CONTRIBUTING.md. Relevant commands are:

```sh
bash scripts/test-native.sh
bash native/tests/run-bootstrap-ui-tests.sh build/bootstrap-recovery
go test -p 2 -parallel 2 -race ./internal/backend -run '^Test(Installer|InstallMain)'
go test -p 2 -parallel 2 -race ./internal/remote -run 'TestRemote(Dependency|Configure|Ownership|Shutdown|Supervisor)'
go run -p 2 ./cmd/queue-tool check-source
go vet -p 2 ./internal/backend
```

The UI runner writes phone access recovery evidence to `build/bootstrap-recovery` and generic Retry evidence to its `generic-retry` subdirectory. Each includes JSON results, screenshots, Markdown, and a standalone HTML view. These generated artifacts stay outside version control.
