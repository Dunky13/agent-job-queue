# Dependency batch: PRs 32, 34, 35, 36, and 37

## Change

Scheduled version updates for GitHub Actions, npm, and Go modules now share
the weekly `dependencies` multi-ecosystem group. Each ecosystem uses `*`, so
all eligible dependency versions join the same PR, including major updates.
Security updates retain Dependabot's separate security-update behavior.

The batch also includes all five requested Go upgrades:

| Original PR | Module | Previous | Updated |
| --- | --- | --- | --- |
| #32 | `github.com/aws/aws-sdk-go-v2` | 1.47.3 | 1.47.4 |
| #34 | `github.com/aws/aws-sdk-go-v2/service/s3` | 1.114.4 | 1.114.5 |
| #35 | `github.com/go-jose/go-jose/v4` | 4.1.5 | 4.1.6 |
| #36 | `github.com/aws/aws-sdk-go-v2/credentials` | 1.20.4 | 1.20.10 |
| #37 | `modernc.org/sqlite` | 1.60.1 | 1.61.0 |

Related indirect versions and checksums were resolved together with `go get`
and `go mod tidy`. The Go toolchain pin remains unchanged.

## Commit-policy recovery

The original Dependabot commits have verified signatures, but their author
email is the bot's GitHub noreply address while their DCO sign-off uses
`support@github.com`. Hosted Commit policy fails with
`missing author-matching DCO sign-off`.

As required by CONTRIBUTING.md, these changes use a maintainer-owned
replacement PR with signed commits and both established maintainer DCO
trailers. Do not synthesize a bot certification or relax the policy. Close
the five superseded PRs after the replacement merges. PR #33 is independent
and excluded from this batch.

## Validation and completion

Before merging, require the complete finite validation batch documented in
CONTRIBUTING.md, independent diff review, and the replacement PR's hosted CI.
The local batch additionally checks the YAML grouping invariants,
`go mod tidy -diff`, and `go mod verify`.

At document creation validation and review are in progress. Current check
results and the eventual merge state belong to the replacement PR. After
merge, verify the GitHub signature, author-matching DCO trailers, and merged
tree, then close the superseded PRs. Publication, Apple notarization, Homebrew
delivery, and installed runtime qualification are separate from this task.

The next scheduled Dependabot run confirms GitHub's service-level grouping;
local YAML checks cannot prove that future run. See
[GitHub's multi-ecosystem configuration guide](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/secure-your-dependencies/configuring-multi-ecosystem-updates).
