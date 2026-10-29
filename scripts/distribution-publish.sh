#!/bin/bash
# Publish only the exact accepted candidate, with a private draft as the boundary.
set -euo pipefail
umask 077
export LC_ALL=C
fail() { printf 'distribution-publish: %s\n' "$*" >&2; exit 1; }
api() { gh api --hostname github.com "$@" || fail 'GitHub request failed; publication is unverified.'; }
hash() { shasum -a 256 "$1" | awk '{print $1}'; }
[[ $# == 1 && -d $1 && ! -L $1 ]] || fail 'Usage: distribution-publish.sh ASSETS_DIR'
[[ ${GITHUB_REPOSITORY:-} =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ &&
  ${BUILD_SHA:-} =~ ^[a-f0-9]{40}$ && ${BUILD_RUN_NUMBER:-} =~ ^[1-9][0-9]{0,8}$ &&
  ${BUILD_TAG:-} == "build-$BUILD_RUN_NUMBER-${BUILD_SHA:0:7}" ]] || fail 'Invalid release source identity.'
[[ -n ${GH_TOKEN:-} ]] || fail 'GH_TOKEN is required.'
assets=$(cd -- "$1" && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
mkdir "$temporary/candidate" "$temporary/readback"
# Freeze the accepted inputs before the first provider operation. Include hidden
# entries so a stale or private file cannot silently become a release asset.
shopt -s nullglob dotglob
for asset in "$assets"/*; do
  name=${asset##*/}
  [[ -f $asset && ! -L $asset && $name =~ ^[A-Za-z0-9][A-Za-z0-9.+_-]*$ ]] || fail 'Assets must be regular files with portable names.'
  cp "$asset" "$temporary/candidate/$name"
done
candidate="$temporary/candidate"
manifest="$candidate/agent-job-queue.json"
for document in "$manifest" "$candidate/apple-notarization.json"; do
  jq -s -e 'length == 1 and (.[0] | type == "object")' "$document" >/dev/null ||
    fail 'Candidate metadata must contain exactly one JSON object.'
done
jq -S . "$candidate/apple-notarization.json" > "$temporary/canonical-apple-evidence.json"
cmp -s "$candidate/apple-notarization.json" "$temporary/canonical-apple-evidence.json" ||
  fail 'Public Apple evidence must retain its canonical serialization without duplicate keys.'
jq -e --arg repository "$GITHUB_REPOSITORY" --arg sha "$BUILD_SHA" --arg tag "$BUILD_TAG" --arg build "$BUILD_RUN_NUMBER" '
  keys == ["asset", "build", "commit", "repository", "schemaVersion", "sha256", "tag", "version"] and
  .schemaVersion == 1 and .repository == $repository and .commit == $sha and .tag == $tag and
  (.build|type) == "number" and (.build|tostring) == $build and (.version|type) == "string" and
  (.version|test("^[0-9]+\\.[0-9]+\\.[0-9]+([+-][A-Za-z0-9.+-]+)?$")) and
  .asset == ("Agent-Queue-" + .version + "-universal.zip") and
  (.sha256|test("^[a-f0-9]{64}$"))
' "$manifest" >/dev/null || fail 'Candidate manifest does not match the release source.'
version=$(jq -r '.version' "$manifest")
base="Agent-Queue-$version-universal"
names=("$base.dmg" "$base.dmg.sha256" "$base.zip" "$base.zip.sha256" agent-job-queue.json agent-job-queue.rb apple-notarization.json)
entries=("$candidate"/*)
[[ ${#entries[@]} == "${#names[@]}" ]] || fail 'Unexpected candidate asset inventory.'
: > "$temporary/checksums"
for name in "${names[@]}"; do
  [[ -f $candidate/$name ]] || fail 'Missing candidate asset.'
  checksum=$(hash "$candidate/$name")
  printf '%s  %s\n' "$checksum" "$name" >> "$temporary/checksums"
done
for extension in zip dmg; do
  checksum=$(hash "$candidate/$base.$extension")
  printf '%s  %s\n' "$checksum" "$base.$extension" > "$temporary/expected-checksum"
  cmp -s "$temporary/expected-checksum" "$candidate/$base.$extension.sha256" || fail 'Candidate checksum file differs.'
done
archive_hash=$(hash "$candidate/$base.zip")
[[ $archive_hash == "$(jq -r '.sha256' "$manifest")" ]] || fail 'Candidate archive does not match its manifest.'
jq -e --arg repo "$GITHUB_REPOSITORY" --arg sha "$BUILD_SHA" --arg version "$version" \
  --arg run "${BUILD_RUN_ID:?}" --arg number "$BUILD_RUN_NUMBER" --arg attempt "${BUILD_RUN_ATTEMPT:?}" \
  --arg artifact "${BUILD_ARTIFACT_ID:?}" --arg digest "${BUILD_ARTIFACT_DIGEST:?}" \
  --arg zip "$archive_hash" --arg dmg "$(hash "$candidate/$base.dmg")" '
  . as $evidence |
  .schema == 1 and .metadata.repository == $repo and .metadata.source_sha == $sha and
  .metadata.run_id == $run and .metadata.run_number == $number and .metadata.version == $version and
  (.metadata.team | test("^[A-Z0-9]{10}$")) and
  (.metadata.schema == 1 or (.metadata.schema == 2 and .metadata.build_attempt == $attempt and
    .metadata.artifact_id == $artifact and .metadata.artifact_digest == $digest)) and
  ([.stages[].stage] == ["app", "dmg"]) and (.logs | length) == 2 and
  all(range(0;2); . as $i |
    $evidence.stages[$i].phase == "finished" and $evidence.logs[$i].status == "Accepted" and
    ($evidence.stages[$i].submission_id | test("^[A-Fa-f0-9]{8}(-[A-Fa-f0-9]{4}){3}-[A-Fa-f0-9]{12}$")) and
    ($evidence.stages[$i].archive_sha256 | test("^[a-f0-9]{64}$")) and
    $evidence.logs[$i].jobId == $evidence.stages[$i].submission_id and
    $evidence.logs[$i].sha256 == $evidence.stages[$i].archive_sha256) and
  .stages[0].completed_sha256 == $zip and .stages[1].completed_sha256 == $dmg
' "$candidate/apple-notarization.json" >/dev/null || fail 'Public Apple evidence does not bind Accepted stages to this source and final candidate.'
{
  printf '%s\n\n%s\n\n' \
    "Automated app from validated main commit ${BUILD_SHA}. Includes Apple Silicon and Intel executables plus checksum-pinned Homebrew metadata." \
    'Signed with Apple Developer ID and notarized by Apple. The menu app and DMG carry stapled tickets; drag Agent Queue to Applications and open it to finish setup. Homebrew preserves quarantine. Standalone CLI first launch can require access to Apple notarization services.'
  printf 'Immutable candidate SHA-256 inventory:\n'
  cat "$temporary/checksums"
} > "$temporary/notes"
repository="repos/$GITHUB_REPOSITORY"

# A successful, paginated list establishes absence. Never interpret a failed
# request (including a 404, timeout or denied access) as permission to create.
inventory() {
  api "$1?per_page=100" --paginate --slurp > "$temporary/pages"
  jq -e 'type == "array" and length > 0 and all(.[]; type == "array" and length <= 100)' \
    "$temporary/pages" >/dev/null || fail 'Malformed provider inventory.'
  jq 'add' "$temporary/pages" > "$2"
  jq -e 'all(.[]; (.id|type) == "number" and .id > 0 and .id < 1e15 and .id == (.id|floor)) and
    ([.[].id]|length) == ([.[].id]|unique|length)' "$2" >/dev/null || fail 'Duplicate or malformed provider identity.'
}

# Resolve both lightweight and annotated tags, without ever creating or moving
# a ref. Prefix matches from matching-refs are deliberately filtered exactly.
tag_identity() {
  local object type sha depth=0 identity
  api "$repository/git/matching-refs/tags/$BUILD_TAG" > "$temporary/refs"
  jq -e 'type == "array"' "$temporary/refs" >/dev/null || fail 'Malformed tag response.'
  object=$(jq -c --arg ref "refs/tags/$BUILD_TAG" '[.[] | select(.ref == $ref)]' "$temporary/refs")
  [[ $(jq 'length' <<< "$object") == 0 ]] && return 0
  [[ $(jq 'length' <<< "$object") == 1 ]] || fail 'Duplicate release tag.'
  identity=$(jq -c '.[0].object | {type,sha}' <<< "$object")
  object=$identity
  while :; do
    jq -e '(.sha|test("^[a-f0-9]{40}$")) and (.type == "commit" or .type == "tag")' <<< "$object" >/dev/null || fail 'Malformed release tag object.'
    sha=$(jq -r '.sha' <<< "$object") type=$(jq -r '.type' <<< "$object")
    if [[ $type == commit ]]; then
      [[ $sha == "$BUILD_SHA" ]] || fail 'Release tag points to a different source commit.'
      printf '%s' "$identity"
      return
    fi
    ((depth < 8)) || fail 'Release tag nesting exceeds limit.'
    api "$repository/git/tags/$sha" > "$temporary/tag"
    jq -e --arg sha "$sha" '.sha == $sha' "$temporary/tag" >/dev/null || fail 'Annotated tag identity changed.'
    object=$(jq -c '.object' "$temporary/tag")
    depth=$((depth + 1))
  done
}

validate_release() {
  jq -e --arg sha "$BUILD_SHA" --arg tag "$BUILD_TAG" --arg number "$BUILD_RUN_NUMBER" '
    (.id|type) == "number" and .id > 0 and .id < 1e15 and .id == (.id|floor) and
    .target_commitish == $sha and .tag_name == $tag and .prerelease == true and
    .name == ("Main build " + $number) and (.draft|type) == "boolean"
  ' "$temporary/release" >/dev/null || fail 'Release target, tag or channel conflicts with the candidate.'
  if [[ $(jq -r '.draft' "$temporary/release") == true ]]; then
    jq -e --rawfile notes "$temporary/notes" '.body == $notes' "$temporary/release" >/dev/null || fail 'Draft belongs to a different immutable candidate.'
  fi
}

read_release() {
  api "$repository/releases/$release_id" > "$temporary/release"
  validate_release
  [[ $(jq -r '.id' "$temporary/release") == "$release_id" ]] || fail 'Release identity changed.'
  jq -e '.draft == true or .immutable == true' "$temporary/release" >/dev/null || fail 'Published release is not protected by GitHub immutability.'
}

remote_inventory() {
  inventory "$repository/releases/$release_id/assets" "$temporary/remote"
  jq -e 'all(.[]; .state == "uploaded" and (.name|type) == "string" and
      (.name|test("^[A-Za-z0-9][A-Za-z0-9.+_-]*$")) and
      (.size|type) == "number" and .size >= 0 and .size == (.size|floor)) and
    ([.[].name]|length) == ([.[].name]|unique|length)' "$temporary/remote" >/dev/null || fail 'Incomplete or duplicate remote assets.'
  while IFS= read -r remote_name; do
    found=false
    for name in "${names[@]}"; do [[ $remote_name != "$name" ]] || found=true; done
    [[ $found == true ]] || fail 'Unexpected remote release asset.'
  done < <(jq -r '.[].name' "$temporary/remote")
}

verify_remote_bytes() {
  local row name id size
  while IFS= read -r row; do
    name=$(jq -r '.name' <<< "$row") id=$(jq -r '.id' <<< "$row") size=$(jq -r '.size' <<< "$row")
    api "$repository/releases/assets/$id" -H 'Accept: application/octet-stream' > "$temporary/readback/$name"
    [[ $(wc -c < "$temporary/readback/$name" | tr -d ' ') == "$size" ]] || fail 'Remote asset size differs.'
    cmp -s "$candidate/$name" "$temporary/readback/$name" || fail 'Remote release bytes differ from the accepted candidate.'
  done < <(jq -c '.[]' "$temporary/remote")
}

original_tag=$(tag_identity)
inventory "$repository/releases" "$temporary/releases"
jq -e --arg tag "$BUILD_TAG" '[.[] | select(.tag_name == $tag)] | length <= 1' "$temporary/releases" >/dev/null || fail 'Duplicate release tag inventory.'
jq --arg tag "$BUILD_TAG" '.[] | select(.tag_name == $tag)' "$temporary/releases" > "$temporary/release"
if [[ ! -s $temporary/release ]]; then
  jq -n --arg tag "$BUILD_TAG" --arg sha "$BUILD_SHA" --arg number "$BUILD_RUN_NUMBER" --rawfile notes "$temporary/notes" \
    '{tag_name:$tag,target_commitish:$sha,name:("Main build " + $number),body:$notes,draft:true,prerelease:true}' > "$temporary/create"
  api "$repository/releases" --method POST --input "$temporary/create" > "$temporary/release"
fi
validate_release
release_id=$(jq -r '.id' "$temporary/release")
draft=$(jq -r '.draft' "$temporary/release")
[[ $draft == true || -n $original_tag ]] || fail 'Public release tag is missing.'
read_release
[[ $(jq -r '.draft' "$temporary/release") == "$draft" ]] || fail 'Release visibility changed before verification.'
remote_inventory
verify_remote_bytes
if [[ $draft == true ]]; then
  for name in "${names[@]}"; do
    if ! jq -e --arg name "$name" 'any(.[]; .name == $name)' "$temporary/remote" >/dev/null; then
      # No clobber, delete, PATCH or replacement: an ambiguous upload stops this
      # invocation. A rerun inventories and verifies any bytes already retained.
      encoded_name=$(jq -nr --arg name "$name" '$name|@uri')
      api "https://uploads.github.com/$repository/releases/$release_id/assets?name=$encoded_name" \
        --method POST -H 'Content-Type: application/octet-stream' --input "$candidate/$name" > "$temporary/upload"
      jq -e --arg name "$name" --arg size "$(wc -c < "$candidate/$name" | tr -d ' ')" '
        (.id|type) == "number" and .id > 0 and .id < 1e15 and .id == (.id|floor) and
        .name == $name and .state == "uploaded" and (.size|tostring) == $size
      ' "$temporary/upload" >/dev/null || fail 'Malformed upload response; retained bytes require reconciliation.'
    fi
  done
fi
# Re-download everything after upload, then require stable IDs, sizes and names.
remote_inventory
[[ $(jq 'length' "$temporary/remote") == "${#names[@]}" ]] || fail 'Release asset inventory is incomplete.'
cp "$temporary/remote" "$temporary/verified-inventory"
verify_remote_bytes
read_release
[[ $(jq -r '.draft' "$temporary/release") == "$draft" ]] || fail 'Release visibility changed during verification.'
current_tag=$(tag_identity)
[[ $current_tag == "$original_tag" ]] || fail 'Release tag changed during verification.'
remote_inventory
jq -S 'sort_by(.id) | map({id,name,size,state})' "$temporary/remote" > "$temporary/current-inventory"
jq -S 'sort_by(.id) | map({id,name,size,state})' "$temporary/verified-inventory" > "$temporary/previous-inventory"
cmp -s "$temporary/current-inventory" "$temporary/previous-inventory" || fail 'Release assets changed during verification.'
if [[ $draft == true ]]; then
  api "$repository/releases/$release_id" --method PATCH -F draft=false -F prerelease=true -f make_latest=false > "$temporary/published"
fi
# The publication request can have succeeded despite a lost response. Fail now
# on any ambiguity; the next invocation verifies a public candidate read-only.
read_release
[[ $(jq -r '.draft' "$temporary/release") == false ]] || fail 'Release is still a draft.'
published_tag=$(tag_identity)
[[ -n $published_tag && ( -z $original_tag || $published_tag == "$original_tag" ) ]] || fail 'Published release tag is missing or changed.'
remote_inventory
jq -S 'sort_by(.id) | map({id,name,size,state})' "$temporary/remote" > "$temporary/current-inventory"
cmp -s "$temporary/current-inventory" "$temporary/previous-inventory" || fail 'Published release inventory changed.'
verify_remote_bytes
printf 'Verified immutable prerelease %s at %s.\n' "$BUILD_TAG" "$BUILD_SHA"
