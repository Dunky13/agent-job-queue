# Recognizable notarization submission names

Apple notarization history previously showed generic `payload.zip` and
`installer.zip` names, making Agent Job Queue difficult to distinguish from
other projects using the same Apple account.

Submissions now use the following names in both direct packaging and resumable
native/REST submission flows:

- `Agent-Job-Queue-Payload-VERSION-universal.zip`
- `Agent-Job-Queue-Installer-VERSION-universal.zip`
- `Agent-Job-Queue-Installer-VERSION-universal.dmg`

Resumable checkpoints retain their existing archive filenames and state schemas.
Before upload, the script copies the immutable archive to its submission filename
and verifies the copy against the recorded SHA-256. Existing submission IDs are
reused, and already recorded uploads are not repeated. Direct packaging recreates
the final ZIP after stapling the installer. Existing Apple history is unchanged.

## Validation

Passed locally:

- `go test -p 2 -parallel 2 ./internal/tooling -run 'TestMacOS(Signing|Resume)'`
  through the shared heavy-job queue, including exact submission names for direct,
  resumable native, and resumable REST paths, archive integrity, and retry custody.
- `shellcheck -x scripts/macos-signing.sh scripts/macos-resume.sh`
- Bash and Zsh syntax checks for both scripts.
- Go formatting checks for both changed test files.
- `go run -p 2 ./cmd/queue-tool check-source`
- `git diff --check`

Hosted CI, a real Apple submission, release publication, and installation of a
new release are separate verification steps. This change does not rename past
Apple submissions or submit artifacts solely to update their display names.
