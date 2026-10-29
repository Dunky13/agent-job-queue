#!/bin/bash
# Sourced by macos-checkpoint.sh after its strict archive helpers are defined.
# Every matching execution is inspected, including executions with no artifacts.
checkpoint_distribution_candidates() {
  local temporary=$1 run id attempt sha title minimum selected
  distribution_context
  for id in BUILD_RUN_ID BUILD_RUN_NUMBER BUILD_RUN_ATTEMPT BUILD_ARTIFACT_ID; do
    distribution_number "${!id:-}" || checkpoint_error "Invalid $id."
  done
  [[ ${BUILD_SHA:-} =~ ^[a-f0-9]{40}$ && ${BUILD_ARTIFACT_DIGEST:-} =~ ^sha256:[a-f0-9]{64}$ ]] || checkpoint_error 'Invalid original source artifact identity.'
  distribution_source_run "$BUILD_RUN_ID" "$temporary/source.json"
  jq -e --arg sha "$BUILD_SHA" --arg number "$BUILD_RUN_NUMBER" '
    .head_sha == $sha and (.run_number|tostring) == $number
  ' "$temporary/source.json" >/dev/null || checkpoint_error 'Source build provenance changed.'
  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/workflows/distribution.yml/runs?branch=main" workflow_runs "$temporary/runs"
  title="Distribute build $BUILD_RUN_ID"
  jq -s -e --arg title "$title" --arg current "$GITHUB_RUN_ID" '
    any(.[]; (.id|tostring) == $current and .display_title == $title)
  ' "$temporary/runs" >/dev/null || checkpoint_error 'Current distribution execution is missing from the trusted inventory.'
  jq -s -c --arg title "$title" '[.[] | select(.display_title == $title)] | sort_by(.id)[]' "$temporary/runs" > "$temporary/matching"
  : > "$temporary/candidates"
  # Legacy CI checkpoints are part of the same recovery chain. Do not allow
  # migration to turn an uncertain legacy upload into a fresh submission.
  checkpoint_distribution_inventory "$temporary" "$BUILD_RUN_ID" legacy "$BUILD_SHA" "$(jq -r '.run_attempt' "$temporary/source.json")" 0
  while IFS= read -r run; do
    id=$(jq -r '.id' <<< "$run")
    gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id" > "$temporary/execution.json"
    jq -e --arg repo "$GITHUB_REPOSITORY" --arg id "$id" --arg title "$title" '
      (.id|tostring) == $id and .path == ".github/workflows/distribution.yml" and .head_branch == "main" and
      (.event == "workflow_dispatch" or .event == "workflow_run") and .display_title == $title and
      (.head_sha|test("^[a-f0-9]{40}$")) and (.run_attempt|tostring|test("^[1-9][0-9]{0,14}$")) and
      .repository.full_name == $repo and .head_repository.full_name == $repo
    ' "$temporary/execution.json" >/dev/null || checkpoint_error 'Distribution execution provenance mismatch.'
    sha=$(jq -r '.head_sha' "$temporary/execution.json")
    attempt=$(jq -r '.run_attempt' "$temporary/execution.json")
    minimum=$attempt
    if [[ $id == "$GITHUB_RUN_ID" ]]; then
      [[ $attempt == "$GITHUB_RUN_ATTEMPT" ]] || checkpoint_error 'Distribution attempt changed during recovery.'
      minimum=$((attempt - 1))
    fi
    checkpoint_distribution_inventory "$temporary" "$id" distribution "$sha" "$attempt" "$minimum"
  done < "$temporary/matching"
  # Artifact IDs and run creation order do not order checkpoint production:
  # an older execution can be rerun after a newer one. Each execution has
  # already selected its numeric attempt/slot; compare those states by the
  # authoritative artifact creation time, without an arbitrary tie breaker.
  jq -s -e '
    all(.[]; (.created_at|type) == "string" and
      (.created_at|test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
      (try ((.created_at|fromdateiso8601|todateiso8601) == .created_at) catch false)) and
    ([.[].created_at]|length) == ([.[].created_at]|unique|length)
  ' "$temporary/candidates" >/dev/null || checkpoint_error 'Missing, invalid or ambiguous checkpoint creation time.'
  selected=$(jq -s -c 'sort_by(.created_at)|last // empty' "$temporary/candidates")
  printf '%s' "$selected"
}

checkpoint_distribution_inventory() {
  local temporary=$1 id=$2 kind=$3 sha=$4 attempt=$5 minimum=$6 prefix selected observed
  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$id/artifacts" artifacts "$temporary/inventory-$id"
  if [[ $kind == legacy ]]; then prefix="Apple-notarization-$id";
  else prefix="Apple-notarization-b$BUILD_RUN_ID-r$id"; fi
  jq -s -e --arg prefix "$prefix" --arg id "$id" --arg sha "$sha" --argjson attempt "$attempt" '
    [.[]|select(.name|startswith("Apple-notarization-"))] |
    ([.[].name]|length) == ([.[].name]|unique|length) and all(.[];
      (.name|test("^" + $prefix + "-a[1-9][0-9]{0,14}-s(0[0-9]|10)$")) and
      (.name|capture("-a(?<attempt>[0-9]+)-s").attempt|tonumber) <= $attempt and
      (.workflow_run.id|tostring) == $id and .workflow_run.head_branch == "main" and .workflow_run.head_sha == $sha)
  ' "$temporary/inventory-$id" >/dev/null || checkpoint_error 'Mixed or malformed checkpoint provenance.'
  # The jobs API repeats successful jobs in partial reruns with new IDs and
  # run_attempt values. Preserve their original execution custody only when the
  # complete successful job and stage-step timestamps/evidence are identical.
  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$id/jobs?filter=all" jobs "$temporary/jobs-$id.raw"
  jq -s -e --arg id "$id" --arg sha "$sha" --argjson attempt "$attempt" '
    all(.[]; (.run_id|tostring) == $id and .head_sha == $sha and
      (.run_attempt|type) == "number" and .run_attempt >= 1 and .run_attempt <= $attempt and
      .run_attempt == (.run_attempt|floor)) and
    ([.[]|select(.name == "Sign and notarize main installer")|.run_attempt]|length) ==
    ([.[]|select(.name == "Sign and notarize main installer")|.run_attempt]|unique|length)
  ' "$temporary/jobs-$id.raw" >/dev/null || checkpoint_error 'Malformed signing job provenance or attempt.'
  jq -s -c '
    def signing: .name == "Sign and notarize main installer";
    def completed_execution:
      .status == "completed" and .conclusion == "success" and
      (.started_at|type) == "string" and (.completed_at|type) == "string" and
      (.started_at|test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
      (.completed_at|test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
      .completed_at >= .started_at and (.steps|type) == "array" and
      any(.steps[]; (.name == "Notarize the app" or .name == "Notarize the distribution disk image") and
        .status == "completed" and .conclusion == "success" and
        (.started_at|type) == "string" and (.completed_at|type) == "string") and
      all(.steps[]; (.number|type) == "number" and (.name|type) == "string" and
        .status == "completed" and has("started_at") and has("completed_at"));
    . as $jobs | .[] | . as $job |
    . + {custody_attempt: (if signing and completed_execution then
      [$jobs[] | select(signing and completed_execution and .run_attempt <= $job.run_attempt and
        .started_at == $job.started_at and .completed_at == $job.completed_at and .steps == $job.steps) |
        .run_attempt] | min
      else .run_attempt end)}
  ' "$temporary/jobs-$id.raw" > "$temporary/jobs-$id"
  if [[ $kind == legacy ]]; then
    minimum=$attempt
  fi
  # minimum initially bounds completed/prior attempts. The current restore job
  # has not signed yet. Every earlier distribution attempt must have affirmative
  # evidence: skipped/never-started signing, an actual signing execution, or a
  # downstream-only retry retaining an earlier successful signing execution.
  if [[ $kind == distribution && $minimum -gt 0 ]]; then
    if [[ ! -s $temporary/jobs-$id ]] && ((attempt == 1 && minimum == 1)) &&
      jq -e '((.status == "completed" and .conclusion == "cancelled") or
        .status == "queued" or .status == "pending" or .status == "waiting") and
        has("run_started_at") and .run_started_at == null' "$temporary/execution.json" >/dev/null; then
      minimum=0
    else
      jq -s -e --argjson through "$minimum" '
        def signing: .name == "Sign and notarize main installer";
        [.[]|select(.run_attempt <= $through)] as $jobs |
        ([$jobs[].run_attempt]|unique|length) == $through and
        ($jobs|group_by(.run_attempt)|all(.[];
          .[0].run_attempt as $attempt |
          any(.[]; signing) or
          (all(.[]; .name == "Notarized distribution (macos-15)" or
            .name == "Notarized distribution (macos-15-intel)" or
            .name == "Publish main installer" or .name == "Verify Homebrew delivery") and
           any($jobs[]; signing and .custody_attempt < $attempt and .status == "completed" and .conclusion == "success"))))
      ' "$temporary/jobs-$id" >/dev/null || checkpoint_error 'Unknown signing execution; checkpoint custody cannot be established.'
    fi
  fi
  selected=$minimum
  minimum=$(jq -s -r --argjson through "$selected" '
    def notarization_not_entered:
      . as $job | .status == "completed" and (.steps|type) == "array" and
      all(.steps[]; (.name|type) == "string" and
        (.number|type) == "number" and .number > 0 and .number == (.number|floor) and
        .status == "completed" and (.conclusion|type) == "string" and .conclusion != "") and
      ([.steps[].number]|length) == ([.steps[].number]|unique|length) and
      (["Notarize the app", "Notarize the distribution disk image"] |
        all(.[]; . as $name | [$job.steps[]|select(.name == $name)] |
          length == 1 and .[0].conclusion == "skipped"));
    [.[] | select(.name == "Sign and notarize main installer" and .run_attempt <= $through) |
      select((.status == "completed" and .conclusion == "skipped" and
        all(.steps[]?; .conclusion == "skipped")) or
        (.status == "completed" and .conclusion == "cancelled" and has("started_at") and .started_at == null and
          all(.steps[]?; .conclusion == "skipped")) or notarization_not_entered) | .id] as $never_started |
    [.[] | select(.name == "Sign and notarize main installer" and .run_attempt <= $through) |
      select(.id as $id | $never_started | index($id) | not) | .custody_attempt] | max // 0
  ' "$temporary/jobs-$id")
  selected=$(jq -s -c --argjson maximum "$selected" --arg kind "$kind" '
    [.[]|select(.name|startswith("Apple-notarization-"))|
      . + (.name|capture("-a(?<attempt>[0-9]+)-s(?<slot>[0-9]+)$"))|
      .attempt |= tonumber|.slot |= tonumber|select($kind == "legacy" or .attempt <= $maximum)]|
      sort_by(.attempt,.slot)|last // empty
  ' "$temporary/inventory-$id")
  if ((minimum > 0)); then
    [[ -n $selected ]] || checkpoint_error 'Matching execution has no surviving checkpoint; refusing a fresh submission.'
    observed=$(jq -r '.attempt' <<< "$selected")
    ((observed >= minimum)) || checkpoint_error 'Newest execution attempt has no surviving checkpoint.'
  fi
  [[ -n $selected ]] || return 0
  [[ $(jq -r '.expired' <<< "$selected") == false ]] || checkpoint_error 'Newest checkpoint expired; refusing an older state.'
  if [[ -f $temporary/jobs-$id ]]; then
    # Composite internals are not separate jobs-API steps. Their outer stage
    # names still prove a stage was entered or completed. A lost trailing
    # checkpoint must never turn completed/uncertain work into fresh signing.
    observed=$(jq -s -r --argjson attempt "$(jq -r '.attempt' <<< "$selected")" '
      [.[] | select(.name == "Sign and notarize main installer" and .custody_attempt == $attempt) |
        .steps[]? | select((.status == "completed" or .status == "in_progress") and .conclusion != "skipped") |
        if .name == "Notarize the app" then (if .conclusion == "success" then 3 else 1 end)
        elif .name == "Notarize the distribution disk image" then (if .conclusion == "success" then 9 else 7 end)
        else 0 end] | max // 0
    ' "$temporary/jobs-$id")
    (( $(jq -r '.slot' <<< "$selected") >= observed )) || checkpoint_error 'Newest stage checkpoint is missing; refusing an older state.'
  fi
  printf '%s\n' "$selected" >> "$temporary/candidates"
}
