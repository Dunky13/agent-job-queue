package tooling

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testTool(t *testing.T, directory, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}
func TestPrePushPreservesFullRangeAndFailsClosed(t *testing.T) {
	hook, err := filepath.Abs("../../.githooks/pre-push")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, local, remote, reference, lookup, resolve, check, base string
		code                                                         int
	}{{"new branch", strings.Repeat("1", 40), strings.Repeat("0", 40), "refs/heads/feature", "0", "0", "0", strings.Repeat("2", 40), 0}, {"existing branch", strings.Repeat("1", 40), strings.Repeat("3", 40), "refs/heads/feature", "0", "0", "0", strings.Repeat("2", 40), 0}, {"bootstrap", strings.Repeat("1", 40), strings.Repeat("0", 40), "refs/heads/feature", "1", "0", "0", strings.Repeat("0", 40), 0}, {"lookup failure", strings.Repeat("1", 40), strings.Repeat("3", 40), "refs/heads/feature", "128", "0", "0", "", 128}, {"resolve failure", strings.Repeat("1", 40), strings.Repeat("3", 40), "refs/heads/feature", "0", "128", "0", "", 128}, {"checker failure", strings.Repeat("1", 40), strings.Repeat("3", 40), "refs/heads/feature", "0", "0", "17", strings.Repeat("2", 40), 17}, {"deletion", strings.Repeat("0", 40), strings.Repeat("3", 40), "refs/heads/feature", "0", "0", "0", "", 0}, {"tag", strings.Repeat("1", 40), strings.Repeat("3", 40), "refs/tags/version", "0", "0", "0", "", 0}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			calls := filepath.Join(root, "calls")
			testTool(t, root, "git", `case "$1" in
show-ref) exit "$HOOK_LOOKUP";;
rev-parse) printf '%s\n' "$HOOK_MAIN"; exit "$HOOK_RESOLVE";;
*) exit 99;;
esac
`)
			testTool(t, root, "go", `printf '%s\n' "$*" > "$HOOK_CALLS"
exit "$HOOK_CHECK"
`)
			cmd := exec.Command("/bin/sh", hook, "origin", "unused")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "HOOK_CALLS="+calls, "HOOK_MAIN="+strings.Repeat("2", 40), "HOOK_LOOKUP="+test.lookup, "HOOK_RESOLVE="+test.resolve, "HOOK_CHECK="+test.check)
			cmd.Stdin = strings.NewReader("refs/heads/feature " + test.local + " " + test.reference + " " + test.remote + "\n")
			output, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				code = cmd.ProcessState.ExitCode()
			}
			if code != test.code {
				t.Fatalf("code%d want%d: %s", code, test.code, output)
			}
			body, err := os.ReadFile(calls)
			if test.base == "" {
				if !os.IsNotExist(err) {
					t.Fatal("checker invoked after refusal or irrelevant ref")
				}
			} else {
				expected := "run -p 2 ./cmd/queue-tool check-commits --base " + test.base + " --head " + test.local + " --verify-local"
				if err != nil || strings.TrimSpace(string(body)) != expected {
					t.Fatalf("range mismatch %q %v", body, err)
				}
			}
		})
	}
}
func TestHomebrewManifestAndCaskContracts(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "Agent-Job-Queue-Installer-0.1.0-universal.zip")
	if err := os.WriteFile(archive, []byte("immutable archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	manifest, err := CreateManifest(archive, "0.1.0", "build-123-aaaaaaa", commit, 123)
	if err != nil {
		t.Fatal(err)
	}
	body, err := RenderCask(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{manifest.SHA256, "source/links/agent-heavy", "source/links/agent-remote", `app "Agent Job Queue Installer.app/Contents/Resources/payload/Agent Queue.app", target: application`, `args: ["-f", "--", "#{Dir.home}/Library/LaunchAgents/io.went.agent-queue.plist"]`, "Open Agent Queue to finish setup", "Command-line tools are ready after setup completes", "Developer ID signed and notarized", "Quarantine is retained", "sudo: false"} {
		if !strings.Contains(body, expected) {
			t.Errorf("missing %s", expected)
		}
	}
	for _, obsolete := range []string{"depends_on formula:", "sudo: true", "preflight", "/usr/bin/xattr", "com.apple.quarantine", "source/scripts/install-user.sh", "installer script:", "postflight", "system_command", `"--payload"`, `delete: [`, `"#{Dir.home}/Applications/Agent Queue.app",`} {
		if strings.Contains(body, obsolete) {
			t.Errorf("obsolete dependency %s", obsolete)
		}
	}
	manifest.Tag = "bad\"; system('unsafe')"
	if _, err = RenderCask(manifest); err == nil {
		t.Fatal("accepted injected release tag")
	}
}
func TestHomebrewManifestRejectsUntrustedInputs(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "unexpected.zip")
	os.WriteFile(archive, nil, 0o600)
	if _, err := CreateManifest(archive, "0.1.0", "build-1-aaaaaaa", strings.Repeat("a", 40), 1); err == nil {
		t.Fatal("accepted wrong asset name")
	}
}
func TestSourceBoundaryFindsPrivatePathsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
	file := filepath.Join(root, "public.txt")
	os.WriteFile(file, []byte("portable source"), 0o600)
	if err := CheckSource(root); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("/"+"Users/fictional/checkout"), 0o600)
	if err := CheckSource(root); err == nil {
		t.Fatal("accepted a machine home path")
	}
	os.Remove(file)
	os.Symlink("missing", file)
	if err := CheckSource(root); err == nil {
		t.Fatal("accepted source symlink")
	}
}
