# PR 54: generic QA plugin examples

[Pull request](https://github.com/Dunky13/agent-job-queue/pull/54)

## Change

The plugin guide and Go, JavaScript, and Swift fixtures use `qa-plugin` as a generic example. The guide describes a QA lane without a project-specific name. Runtime behavior, schemas, assertions, and coverage are preserved. The implementation is commit `644d30472e7f3b6037c57b41e7b7bae69a0bb09c`.

## Local evidence

The following checks passed on the implementation:

```sh
go test -race -p 2 -parallel 2 ./internal/backend -run '^(TestPluginManifestLoadsDefaultsAndHooks|TestRealDaemonLaneConcurrencyUnderConcurrentAcquireRunAndStartNow|TestRealDaemonLeaseCLIWaitsOwnersAndStatus)$' -count=1
npm run lint:web
npm run test:web
bash scripts/test-native.sh
git diff --check
```

All eight web schema tests passed. A case-insensitive tracked-content and filename search found no remaining references to the removed project name. Independent Standards and Spec reviews found no issues in the five-file implementation diff, whose SHA256 is `3d44497492796c24f06f863f9986ec822d780bfb0a8af0f0abce8b7de68466c4`.

## Delivery boundary

The requested endpoint is a signed commit, push, and PR. Hosted checks on the final PR head and owner approval remain required before merge. Use the guarded maintainer merge procedure in CONTRIBUTING.md after approval. Release and deployment are outside this task.

Private test fixtures completed their cleanup. The dependencies installed for web validation were removed afterward. A self-contained HTML copy of this handoff is retained as a local review artifact outside version control.
