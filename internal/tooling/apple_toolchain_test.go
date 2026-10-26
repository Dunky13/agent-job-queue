package tooling

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppleToolchainFailsBeforeCredentialsForUnqualifiedTools(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"qualified", "wrong version", "wrong build", "missing stapler", "SDK error", "empty SDK", "Swift error", "Go error", "notary error", "OS error", "architecture error"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			testTool(t, root, "xcodebuild", `printf 'Xcode %s\nBuild version %s\n' "$TOOLCHAIN_TEST_VERSION" "$TOOLCHAIN_TEST_BUILD"`)
			testTool(t, root, "xcrun", `case "$*" in
--find\ stapler) [ "$TOOLCHAIN_TEST_VARIANT" != 'missing stapler' ] || exit 9; printf '/Applications/Xcode.app/stapler\n';;
--find\ *) printf '/Applications/Xcode.app/tool\n';;
--sdk\ macosx\ --show-sdk-version) [ "$TOOLCHAIN_TEST_VARIANT" != 'SDK error' ] || exit 42; [ "$TOOLCHAIN_TEST_VARIANT" != 'empty SDK' ] || exit 0; printf '15.5\n';;
swift\ --version) [ "$TOOLCHAIN_TEST_VARIANT" != 'Swift error' ] || exit 42; printf 'Apple Swift fixture\n';;
notarytool\ --version) [ "$TOOLCHAIN_TEST_VARIANT" != 'notary error' ] || exit 42; printf '1.1.0 fixture\n';;
*) exit 99;; esac`)
			testTool(t, root, "go", `[ "$TOOLCHAIN_TEST_VARIANT" != 'Go error' ] || exit 42; printf 'go version fixture\n'`)
			testTool(t, root, "sw_vers", `[ "$TOOLCHAIN_TEST_VARIANT" != 'OS error' ] || exit 42; printf '15.7.9\n'`)
			testTool(t, root, "uname", `[ "$TOOLCHAIN_TEST_VARIANT" != 'architecture error' ] || exit 42; printf 'arm64\n'`)
			version, build := "16.4", "16F6"
			if variant == "wrong version" {
				version = "27.1"
			}
			if variant == "wrong build" {
				build = "other"
			}
			script, _ := filepath.Abs("../../scripts/apple-toolchain.sh")
			output := filepath.Join(root, "toolchain.json")
			cmd := exec.Command("/bin/bash", script, output)
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
				"EXPECTED_XCODE_VERSION=16.4", "EXPECTED_XCODE_BUILD=16F6", "TOOLCHAIN_TEST_VERSION="+version,
				"TOOLCHAIN_TEST_BUILD="+build, "TOOLCHAIN_TEST_VARIANT="+variant)
			result, err := cmd.CombinedOutput()
			if variant != "qualified" {
				if err == nil {
					t.Fatalf("unqualified tools passed: %s", result)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatalf("unqualified tools created a receipt: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("qualified tools: %v: %s", err, result)
			}
			body, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				Xcode string
				Build string `json:"xcode_build"`
				SDK   string `json:"macos_sdk"`
			}
			if err := json.Unmarshal(body, &record); err != nil {
				t.Fatal(err)
			}
			if record.Xcode != "16.4" || record.Build != "16F6" || record.SDK != "15.5" || strings.Contains(string(body), root) {
				t.Fatalf("incorrect toolchain receipt: %s", body)
			}
		})
	}
}
