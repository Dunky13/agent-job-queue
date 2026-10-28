#!/bin/sh
# Invoke only after the owner approves merging the reviewed PR head.
set -eu

if [ "$#" -ne 3 ]; then
    echo 'Usage: sh scripts/merge-pr.sh PR_NUMBER REVIEWED_HEAD SQUASH_BODY_FILE' >&2
    exit 2
fi
pr=$1
head=$2
case "$pr" in
    ''|0|*[!0-9]*) echo 'Expected a positive PR number.' >&2; exit 2 ;;
esac
case "$head" in
    ''|*[!0-9a-f]*) echo 'Expected a full reviewed commit SHA.' >&2; exit 2 ;;
esac
if [ "${#head}" -ne 40 ]; then
    echo 'Expected a full reviewed commit SHA.' >&2
    exit 2
fi

scratch=$(mktemp -d "${TMPDIR:-/tmp}/agent-queue-merge.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
trap 'exit 1' HUP INT TERM
cat < "$3" > "$scratch/body"
valid_certification() {
    awk '
    tolower($0) ~ /^signed-off-by:[[:space:]]*marc went[[:space:]]*</ {
        count++
        if ($0 != "Signed-off-by: Marc Went <marc@went.io>") invalid = 1
    }
    END { exit !(count == 1 && !invalid) }
    '
}
if ! valid_certification < "$scratch/body"; then
    echo 'Squash body must have exactly one maintainer sign-off: Signed-off-by: Marc Went <marc@went.io>' >&2
    exit 1
fi

metadata=$(gh pr view "$pr" --repo Dunky13/agent-job-queue \
    --json author,baseRefName,headRefOid \
    --jq '.author.login + "\n" + .author.name + "\n" + .baseRefName + "\n" + .headRefOid')
expected=$(printf 'Dunky13\nMarc Went\nmain\n%s' "$head")
if [ "$metadata" != "$expected" ]; then
    echo 'Expected a maintainer-owned PR targeting main at the reviewed head; merge refused.' >&2
    exit 1
fi

gh pr merge "$pr" --repo Dunky13/agent-job-queue --squash \
    --author-email marc@went.io --match-head-commit "$head" \
    --body-file "$scratch/body"

# A successful CLI exit can also mean a queued merge. Inspect the actual commit
# before reporting success; never rewrite main if GitHub returns bad metadata.
merged=$(gh pr view "$pr" --repo Dunky13/agent-job-queue \
    --json state,mergeCommit \
    --jq 'if .state == "MERGED" then .mergeCommit.oid else empty end')
case "$merged" in
    ''|*[!0-9a-f]*) echo 'Merge completion is unverified; inspect the PR state.' >&2; exit 1 ;;
esac
if [ "${#merged}" -ne 40 ]; then
    echo 'Merge completion is unverified; inspect the PR state.' >&2
    exit 1
fi
verification=$(gh api "repos/Dunky13/agent-job-queue/commits/$merged" \
    --jq '.commit | [.author.name, .author.email, (.verification.verified | tostring), .message] | join("\n")')
identity=$(printf '%s\n' "$verification" | sed -n '1,3p')
expected_identity=$(printf 'Marc Went\nmarc@went.io\ntrue')
if [ "$identity" != "$expected_identity" ] || \
    ! printf '%s\n' "$verification" | sed '1,3d' | valid_certification; then
    echo "Merge $merged occurred but failed author, DCO, or signature verification; recovery is required." >&2
    exit 1
fi
printf 'Verified merge %s: primary maintainer author, single DCO certification, and GitHub signature.\n' "$merged"
