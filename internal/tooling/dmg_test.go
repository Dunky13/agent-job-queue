package tooling

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The mount stubs place the committed Finder layout where a built image carries it.
var finderLayout, _ = filepath.Abs("../../packaging/dmg/DS_Store")

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
  app="$1/Agent Queue.app"
  for file in 'Contents/MacOS/AgentQueue' 'Contents/MacOS/agent-heavy' 'Contents/MacOS/agent-remote'; do
    target="$app/$file"
    mkdir -p "${target%/*}"
    printf 'fixture' > "$target"
    chmod 700 "$target"
  done
  ln -s /Applications "$1/Applications"
  cp "$DMG_TEST_FINDER_LAYOUT" "$1/.DS_Store"
  exit "$DMG_TEST_ATTACH";;
detach) exit "$DMG_TEST_DETACH";;
*) exit 99;;
esac
`)
			testTool(t, root, "go", `exit "$DMG_TEST_VERIFY"`)
			cmd := exec.Command("/bin/bash", script, "fixture.dmg")
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "DMG_TEST_FINDER_LAYOUT="+finderLayout,
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

func TestDiskImageVerificationSelectsOnlyCompleteDistributionLayouts(t *testing.T) {
	script, err := filepath.Abs("../../scripts/test-dmg.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, layout := range []string{
		"drag",
		"volume metadata",
		"installer layout",
		"mixed",
		"missing apps",
		"missing link",
		"wrong link",
		"relative link",
		"newline link",
		"link directory",
		"symlink app",
		"dangling installer",
		"extra app",
		"hidden app",
		"uppercase app",
		"extra file",
		"missing helper",
		"non executable helper",
		"missing finder layout",
		"changed finder layout",
		"older image",
	} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			events := filepath.Join(root, "events")
			record := filepath.Join(root, "mountpoint")
			testTool(t, root, "hdiutil", `printf 'hdiutil|%s\n' "$*" >> "$DMG_TEST_EVENTS"
case "$1" in
verify) exit 0;;
attach)
  case "$*" in *'-readonly -nobrowse -noautoopen -mountpoint '*) ;; *) exit 91;; esac
  while [ "$1" != '-mountpoint' ]; do shift; done
  shift
  mountpoint=$1
  printf '%s' "$mountpoint" > "$DMG_TEST_RECORD"
  [ "$DMG_TEST_LAYOUT" != 'missing apps' ] || exit 0
  if [ "$DMG_TEST_LAYOUT" = 'installer layout' ] || [ "$DMG_TEST_LAYOUT" = mixed ]; then
    target="$mountpoint/Agent Job Queue Installer.app/Contents/MacOS/Installer"
    mkdir -p "${target%/*}"
    printf 'fixture' > "$target"
    chmod 700 "$target"
    [ "$DMG_TEST_LAYOUT" != 'installer layout' ] || exit 0
  fi
  app="$mountpoint/Agent Queue.app"
  for file in 'Contents/MacOS/AgentQueue' 'Contents/MacOS/agent-heavy' 'Contents/MacOS/agent-remote'; do
    target="$app/$file"
    mkdir -p "${target%/*}"
    printf 'fixture' > "$target"
    chmod 700 "$target"
  done
  case "$DMG_TEST_LAYOUT" in
    'missing link' ) ;;
    'wrong link') ln -s /tmp "$mountpoint/Applications";;
    'relative link') ln -s Applications "$mountpoint/Applications";;
    'newline link') target=$(printf '/Applications\n.'); ln -s "${target%.}" "$mountpoint/Applications";;
    'link directory') mkdir "$mountpoint/Applications";;
    *) ln -s /Applications "$mountpoint/Applications";;
  esac
  cp "$DMG_TEST_FINDER_LAYOUT" "$mountpoint/.DS_Store"
  case "$DMG_TEST_LAYOUT" in
    'symlink app') mv "$app" "$mountpoint/app-target"; ln -s app-target "$app";;
    'dangling installer') ln -s missing "$mountpoint/Agent Job Queue Installer.app";;
    'extra app') mkdir "$mountpoint/Other.app";;
    'hidden app') mkdir "$mountpoint/.Other.app";;
    'uppercase app') mkdir "$mountpoint/Other.APP";;
    'extra file') printf 'extra' > "$mountpoint/README.txt";;
    'volume metadata') mkdir "$mountpoint/.fseventsd" "$mountpoint/.Trashes";;
    'missing finder layout'|'older image') rm "$mountpoint/.DS_Store";;
    'changed finder layout') printf 'x' >> "$mountpoint/.DS_Store";;
    'missing helper') rm "$app/Contents/MacOS/agent-remote";;
    'non executable helper') chmod 600 "$app/Contents/MacOS/agent-heavy";;
  esac
  exit 0;;
detach) exit 0;;
*) exit 99;;
esac
`)
			testTool(t, root, "go", `printf 'go' >> "$DMG_TEST_EVENTS"
for argument in "$@"; do printf '|%s' "$argument" >> "$DMG_TEST_EVENTS"; done
printf '\n' >> "$DMG_TEST_EVENTS"
`)
			// Fresh builds require the committed layout; re-verifying an image built before it must not.
			arguments := []string{script, "fixture.dmg", "--finder-layout"}
			if layout == "older image" {
				arguments = arguments[:2]
			}
			cmd := exec.Command("/bin/bash", arguments...)
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "DMG_TEST_EVENTS="+events, "DMG_TEST_RECORD="+record, "DMG_TEST_LAYOUT="+layout, "DMG_TEST_FINDER_LAYOUT="+finderLayout)
			output, runErr := cmd.CombinedOutput()
			mount, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err, string(output))
			}
			work := filepath.Dir(string(mount))
			t.Cleanup(func() { _ = os.RemoveAll(work) }) // Only a simulated mount.
			recorded, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			log := string(recorded)
			if !strings.Contains(log, "hdiutil|detach "+string(mount)+"\n") {
				t.Fatalf("verification did not detach: %s", log)
			}
			if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("detached fixture retained temporary state: %v", err)
			}
			if strings.HasSuffix(layout, "finder layout") && !strings.Contains(string(output), "Disk image lacks the committed Finder layout.") {
				t.Fatalf("layout failure did not name the Finder layout: %s", output)
			}
			if layout != "drag" && layout != "volume metadata" && layout != "older image" {
				if runErr == nil || strings.Contains(log, "go|") {
					t.Fatalf("malformed layout reached verification: %v: %s\n%s", runErr, output, log)
				}
			} else if runErr != nil || !strings.Contains(log, "go|run|-p|2|./cmd/queue-tool|verify-app|--app|"+filepath.Join(string(mount), "Agent Queue.app")+"\n") {
				t.Fatalf("wrong layout verification command: %v: %s\n%s", runErr, output, log)
			}
		})
	}
}
