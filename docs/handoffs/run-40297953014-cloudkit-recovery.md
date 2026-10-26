# Distribution 40297953014: CloudKit ticket recovery

## Scope and evidence

Strengthen CI against the repeated Apple ticket-service failures in
[distribution run 40297953014](https://github.com/Dunky13/agent-job-queue/actions/runs/40297953014).
The original installer comes from CI build `40295870502`, at source commit
`097ca2775e43f237a957f467bfeda2f4907d0100`.

[Attempt 1](https://github.com/Dunky13/agent-job-queue/actions/runs/40297953014/attempts/1)
failed while stapling the enclosing installer. The command returned exit 68,
`NSURLErrorDomain Code=-1005`, and Apple's CloudKit ticket lookup URL. Stapling
had no retry protection.

[Attempt 2](https://github.com/Dunky13/agent-job-queue/actions/runs/40297953014/attempts/2)
recovered the installer stage but failed during DMG preparation. Revalidation
of the payload ticket encountered the same CloudKit connection loss three
times, exhausting the existing two- and four-second delays.

## Change and safety boundary

The shared signing script now applies the same bounded retry helper to both
`stapler staple` and `stapler validate`. An operation gets six attempts with
5-, 10-, 20-, 40- and 80-second delays. This allows 155 seconds of backoff,
plus tool execution time, within the existing 45-minute signing job budget.
Logs identify the operation, next attempt, delay and retry exhaustion.

Only exit 68 combined with the observed outer NSError domain/code and exact
CloudKit lookup endpoint qualifies. Invalid, revoked, missing and unrelated
ticket failures still stop immediately. A different failure after a transient
one also stops immediately and retains its exit status. Successful stapling
still requires separate successful ticket validation.

Retries operate on the same working copy and never sign or submit again.
Immutable submitted archives, accepted receipts, checkpoint verification,
credential custody, architecture acceptance and publication gates remain in
place. Exhaustion retains the accepted stage for checkpoint recovery.

## Verification

Private fixture regressions reproduced both failure paths before the repair:
the payload staple failed on its first CloudKit disconnect, and DMG preparation
failed after the third validation attempt. Both scenarios passed after the
repair, including recovery on attempt six.

The fixture matrix covers payload, installer and DMG stapling; retry exhaustion;
invalid tickets; a terminal error after a transient failure; preserved exit
statuses and submitted bytes; and recovery from accepted state without signing,
waiting or submitting again. Existing validation cases cover malformed,
unrelated, nested and quoted error fields to keep retry eligibility narrow.

The targeted validation commands are:

```sh
agent-heavy run --label 'Notarization recovery tests' -- go test -p 2 -parallel 2 ./internal/tooling -run 'TestMacOS(TicketValidation|Resume)' -count=1 -timeout 5m
agent-heavy run --label 'Signing and workflow regression tests' -- go test -p 2 -parallel 2 ./internal/tooling -run 'TestMacOSSigning|TestNotarization|TestWorkflow|TestDistributionWorkflow' -count=1 -timeout 5m
shellcheck -x scripts/macos-signing.sh
bash -n scripts/macos-signing.sh
zsh -n scripts/macos-signing.sh
actionlint .github/workflows/ci.yml .github/workflows/distribution.yml
git diff --check
```

Both targeted Go suites passed, as did ShellCheck, Bash/Zsh syntax checks,
Actionlint and the whitespace check. These local fixture results do not measure
the live Apple service's failure rate or certify a released artifact.

## Delivery boundary and recovery

This is a local repair. Hosted CI, live Apple ticket-service recovery, signing,
both architecture acceptance jobs, publication and Homebrew delivery require
separate verification after the change reaches trusted main automation.

Rerunning the old distribution run uses its old workflow revision and does not
adopt this local repair. Once merged, inspect existing distribution executions
for the original build before starting any recovery. If recovery is still
needed and none is active, use the current main distribution workflow with
the original CI build ID, following [Apple signing](../apple-signing.md).
Reuse retained checkpoints and submissions; do not rebuild or submit duplicates.

The Markdown handoff belongs in source control with the repair. Its standalone
HTML chat deliverable is generated locally and must remain outside version
control.
