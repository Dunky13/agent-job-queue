#!/bin/bash
# Headless startup decisions and lifecycle tests with private bundles/homes.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
bash "$root/native/tests/build-test.sh" "$temporary/BootstrapTests" \
  "$root/native/tests/BootstrapFixtures.swift" "$root/native/tests/BootstrapTests.swift"
"$temporary/BootstrapTests"
