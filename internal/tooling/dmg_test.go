package tooling

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskImageVerificationCleansUpWithoutDiscardingBusyMount(t *testing.T) {
	script, err := filepath.Abs("../../scripts/test-dmg.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                   string
		attach, verify, detach string
		wantCode               int
		retain                 bool
	}{
		{"success", "0", "0", "0", 0, false},
		{"verification fails", "0", "17", "0", 17, false},
		{"busy mount", "0", "0", "16", 1, true},
		{"partial attach stays busy", "16", "0", "16", 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			record := filepath.Join(root, "mountpoint")
			testTool(t, root, "hdiutil", `case "$1" in
verify) exit 0;;
attach)
  while [ "$1" != '-mountpoint' ]; do shift; done
  shift
  printf '%s' "$1" > "$DMG_TEST_RECORD"
  for file in 'Contents/MacOS/Installer' \
    'Contents/Resources/payload/Agent Queue.app/Contents/MacOS/AgentQueue' \
    'Contents/Resources/payload/source/bin/agent-heavy' \
    'Contents/Resources/payload/source/bin/agent-remote'; do
    target="$1/Agent Job Queue Installer.app/$file"
    mkdir -p "${target%/*}"
    printf 'fixture' > "$target"
    chmod 700 "$target"
  done
  exit "$DMG_TEST_ATTACH";;
detach) exit "$DMG_TEST_DETACH";;
*) exit 99;;
esac
`)
			testTool(t, root, "go", `exit "$DMG_TEST_VERIFY"`)
			cmd := exec.Command("/bin/bash", script, "fixture.dmg")
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
				"DMG_TEST_RECORD="+record, "DMG_TEST_ATTACH="+test.attach,
				"DMG_TEST_VERIFY="+test.verify, "DMG_TEST_DETACH="+test.detach)
			output, runErr := cmd.CombinedOutput()
			code := 0
			if runErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) {
					t.Fatal(runErr)
				}
				code = exitErr.ExitCode()
			}
			mount, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err, string(output))
			}
			work := filepath.Dir(string(mount))
			t.Cleanup(func() { _ = os.RemoveAll(work) }) // Only a simulated mount.
			_, statErr := os.Stat(work)
			if code != test.wantCode || test.retain && statErr != nil || !test.retain && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("exit=%d, work error=%v: %s", code, statErr, output)
			}
			if test.retain && !strings.Contains(string(output), "retaining "+work) {
				t.Fatal("busy mount lacked recovery location", string(output))
			}
		})
	}
}
