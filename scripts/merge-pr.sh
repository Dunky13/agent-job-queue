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
if ! awk '
    tolower($0) ~ /^signed-off-by:[[:space:]]*marc went[[:space:]]*</ {
        count++
        if ($0 != "Signed-off-by: Marc Went <marc@went.io>") invalid = 1
    }
    END { exit !(count == 1 && !invalid) }
' "$scratch/body"; then
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
