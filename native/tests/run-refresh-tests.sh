#!/bin/zsh
set -eu
source_dir=${0:A:h}
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
bash "$source_dir/build-test.sh" "$temporary/RefreshTests" "$source_dir/RefreshTests.swift"
"$temporary/RefreshTests"
