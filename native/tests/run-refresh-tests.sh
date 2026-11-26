#!/bin/zsh
set -eu
source_dir=${0:A:h}
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
bash "$source_dir/build-test.sh" "$temporary/RefreshTests" "$source_dir/RefreshTests.swift"
# The isolation check proves a live controller call is observable: give it a private home.
mkdir "$temporary/home"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" AGENT_QUEUE_TRIPWIRE_HOME="$temporary/home" \
  "$temporary/RefreshTests"
