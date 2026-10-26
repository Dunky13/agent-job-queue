# Homebrew managed-app upgrade repair

## Problem and scope

The script-installer cask executes setup directly from the shared Caskroom. Payload validation introduced with drag-to-Applications installation rejects group-writable ancestors, including Homebrew's Caskroom and some installation prefixes. The old cask also deletes the installed app before attempting replacement, while its rollback metadata does not describe an app artifact.

This repair changes the source and standalone tap cask renderers, their trusted validation helper, and focused regression coverage. It does not change payload permissions policy, process ownership verification, private queue state, existing release bytes, or generated cask metadata. Merge and public release remain separate approval and validation steps.

## Resulting installation flow

1. Homebrew verifies and extracts the release archive.
2. Its native `app` artifact moves the embedded complete menu app into the account's Applications directory.
3. Homebrew links the existing stable command wrappers.
4. The user opens Agent Queue. Its existing first-launch flow runs the installed app's signed embedded controller with `install --app-bundle` and shows Retry if setup fails. Command-line tools are ready after this setup completes.

The cask has no executable installation hooks. It never executes setup through the Caskroom payload path or changes the permissions of shared Homebrew ancestors. App and helper signatures and quarantine remain authoritative. Launch-agent removal uses a bounded, non-root system command during uninstall; the app artifact owns app removal.

## Recovery boundaries

Upgrades between managed-app casks use Homebrew's app backup and restoration if later artifact installation fails. Backend setup occurs after cask installation. A first-launch setup failure retains the newer app for its Retry action; an incomplete backend installation marker remains authoritative. This is not a transactionally reverted backend migration.

The first transition from a legacy script-installer cask still executes the old installed cask's deletion rules. Its metadata cannot acquire app rollback support retroactively. The new cask does not guarantee restoration of the old app on that one transition.

Private queue data, credentials, the independent daemon, and active jobs are retained. Tests use isolated homes, private fixtures, and inert app-opening commands.

## Delivery order

1. Merge the companion tap helper PR after its required checks and review. Leave generated `Casks/` files to the refresh workflow.
2. Merge the source PR after its required checks and review.
3. Let main CI and distribution publish a new numbered build with Developer ID signing, notarization, Apple Silicon and Intel acceptance, and verified tap delivery.
4. Users run the documented normal Homebrew upgrade command. No local permission workaround is part of this fix.

Existing archives, release manifests, and published casks remain immutable. Tests against retained release bytes prove compatibility of the new cask flow with that artifact; they do not establish publication or runtime qualification of a new release.

## Validation

The source cask contract regression was observed failing against the prior templates before the repair. Backend coverage stages the app beneath group-writable Apple Silicon, Intel, and custom prefix layouts, confirms direct classic payload reads still fail closed, then verifies setup and retry after app relocation without changing shared directory modes or the app bundle.

An actual notarized build 77 test rejected an intermediate design that ran setup through Homebrew's declarative postflight sandbox: the private daemon did not become ready. The installed Homebrew sandbox explicitly denies outbound Unix sockets, app launching, and relevant Mach services. That failed witness is the reason setup remains in the app's existing first-launch flow. Neither sandbox permissions nor readiness checks were disabled.

Local validation passed:

- `bash scripts/lint.sh`, including source portability, Go vet, formatting, web lint, shell and workflow lint, and Ruby syntax.
- Go race tests for tooling, delivery, standalone tap tooling, and the installer regression scope.
- Real Homebrew App/Binary lifecycle tests for three prefix layouts, spaced account home, and a custom app directory setting. A failure in the last replacement Binary artifact exercises partial link cleanup, app rollback, and restoration of links to the preceding cask's exact sources. Full uninstall preserves private state and credentials.
- Headless native regressions, including automatic first-launch setup, failed setup, Retry, and readiness verification.
- Standalone companion tap tests, Go vet, Ruby syntax, and exact canonical helper synchronization.
- Retained `build-77-b5dac59` notarized app relocation and signed first-launch setup against a private daemon. All 387 checked entries retained quarantine and the fixture attribute; the daemon stayed healthy. The archive SHA-256 was `8390abdf36b55830f2678dc0e20d3018aad10edbab466282638bc5fcba60ce08`.

Independent source review has no unresolved findings. It caught a fixture-environment omission when setup moved outside Ruby; the corrected request explicitly supplies private test mode and launchctl tools, and the actual signed-artifact test passes with that isolation. The operator's installed app and queue were not updated by this repair.

Hosted CI, merge, new artifact publication, and public tap delivery remain separate gates. The first-launch request was exercised directly with the signed helper; no production app was opened in the operator's graphical session. The operating contract is in [Homebrew delivery](../homebrew-delivery.md).
