# Main and Homebrew CI recovery

## Main commit policy

[Main CI run 40938301171](https://github.com/Dunky13/agent-job-queue/actions/runs/40938301171), at `743daa26d568e863b205498562aeb143cecfff7c`, failed only Commit policy and its required aggregate. All build, vulnerability, lint, runtime, tooling, browser and native jobs passed. GitHub reports the squash commit's cryptographic signature as verified.

PR 54's source commits used `Marc Went <marc@went.io>`. The resulting squash commit instead used `Marc Went <Dunky13@users.noreply.github.com>`, while its DCO trailers certify `marc@went.io`. The author-matching check correctly rejects it. Local reproduction:

```sh
go run -p 2 ./cmd/queue-tool check-commits \
  --base 2755c958f12c8c07a85c435887eb9617a41de9b1 \
  --head 743daa26d568e863b205498562aeb143cecfff7c \
  --github-repository Dunky13/agent-job-queue
```

Expected result: `missing author-matching DCO sign-off`. Rerunning this historical run cannot repair immutable commit metadata. Do not rewrite main, synthesize an alias certification or relax the policy. This documentation repair supplies a new signed, primary-identity candidate. After owner approval, merge it with the existing guarded procedure in CONTRIBUTING.md, using one canonical maintainer sign-off. Check the actual merged author, DCO, signature and reviewed tree, then verify that new main head's CI separately. The old run remains failed.

## Homebrew refresh

[Tap refresh run 40956511962](https://github.com/Dunky13/homebrew-agent-job-queue/actions/runs/40956511962), at `b45ee373129beb41bc4658daf5232c90e05994a9`, failed with `Unexpected installer asset`. Tap still used installer-era trusted tooling, while the latest public release `build-156-2755c95` contains `Agent-Queue-0.1.0-universal.zip`.

The companion tap repair copies all five canonical trusted files from this source revision, with no source helper changes: `packaging/homebrew/refresh.go`, `refresh_test.go`, `test-cask.rb`, `.github/workflows/refresh.yml`, and the root `LICENSE`. It preserves checksum and publication checks, validates the legacy predecessor without executing its cask, and renders only the new plain app cask. Generated cask metadata is left to the trusted refresh workflow. Merge the tap repair before completing source delivery verification.

The actual published build-156 manifest fails with the old helper and passes with the canonical helper. Standalone race tests, vet, formatting, workflow lint and Ruby syntax checks pass. Source parity and Homebrew lifecycle checks use private fixtures:

```sh
go test -race -p 2 -parallel 2 ./packaging/homebrew
go test -race -p 2 -parallel 2 ./internal/tooling \
  -run '^(TestHomebrew.*|TestManagedCask.*|TestRenderedCask.*)$' -count=1
```

## Remaining gates

Final-head PR CI, independent review and owner approval precede merge. Main recovery requires successful CI on the resulting valid merge. Homebrew recovery requires a successful scheduled or authorized manual refresh and verification of its resulting signed tap metadata. These are separate outcomes. This repair does not authorize an app release, Apple submission, explicit cask publication or local installation. Retain exact existing public artifact bytes and identities.
