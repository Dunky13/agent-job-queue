#!/bin/bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
repository=${1:?Usage: configure-repository.sh OWNER/REPO}
[[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { echo 'Invalid repository.' >&2; exit 1; }
gh api --method PATCH "repos/$repository" \
  -F allow_merge_commit=false -F allow_rebase_merge=false -F allow_squash_merge=true \
  -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=COMMIT_MESSAGES \
  -F delete_branch_on_merge=true -F web_commit_signoff_required=true >/dev/null
gh api --method PUT "repos/$repository/branches/main/protection" \
  --input "$root/repository/main-protection.json" >/dev/null
gh api --method POST "repos/$repository/branches/main/protection/required_signatures" >/dev/null
gh api "repos/$repository/branches/main/protection" --jq '
  if .required_status_checks.strict == true
    and .required_status_checks.checks == [{"context":"ci-required","app_id":15368}]
    and .enforce_admins.enabled == true
    and .required_pull_request_reviews != null
    and .required_signatures.enabled == true
    and .required_conversation_resolution.enabled == true
    and .allow_force_pushes.enabled == false
    and .allow_deletions.enabled == false
  then "Verified main: required up-to-date CI, pull requests, signatures, conversations; no force/deletion/admin bypass."
  else error("Repository protection verification failed") end'
