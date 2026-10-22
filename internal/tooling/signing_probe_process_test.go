package tooling

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestKeychainSigningProbeStopsOwnedDescendants(t *testing.T) {
	for _, mode := range []string{"success", "cancel", "ignore-term"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			script := filepath.Join(root, "probe.sh")
			body := `#!/bin/bash
set -eu
root=$1
mode=$2
printf '%s' "$$" > "$root/parent"
trap 'printf restored > "$root/restored"' EXIT
if [[ $mode == ignore-term ]]; then trap '' TERM; else trap 'exit 143' TERM; fi
if [[ $mode == success ]]; then exec 3>/dev/null; else exec 3>&1; fi
/bin/bash -c 'trap "" TERM; printf "%s" "$$" > "$1"; while :; do sleep 1; done' probe "$root/child" >&3 2>&3 &
child=$!
exec 3>&-
while [[ ! -s $root/child ]]; do sleep 0.01; done
if [[ $mode == success ]]; then exit 0; fi
wait "$child"
`
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Only these owned test processes are signalled. No Keychain tool,
			// production credential or system setting is used by this fixture.
			t.Cleanup(func() {
				cancel()
				if data, err := os.ReadFile(filepath.Join(root, "parent")); err == nil {
					if pid, err := strconv.Atoi(string(data)); err == nil && pid > 1 {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
					}
				}
			})
			done := make(chan error, 1)
			go func() {
				_, err := runKeychainSigningProbe(ctx, os.Environ(), "/bin/bash", script, root, mode)
				done <- err
			}()
			deadline := time.Now().Add(5 * time.Second)
			var child int
			for time.Now().Before(deadline) {
				if data, err := os.ReadFile(filepath.Join(root, "child")); err == nil {
					child, err = strconv.Atoi(string(data))
					if err == nil && child > 1 {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if child <= 1 {
				t.Fatal("owned signing descendant did not start")
			}
			if mode != "success" {
				cancel()
			}
			select {
			case err := <-done:
				if (mode == "success") != (err == nil) {
					t.Fatalf("unexpected signing probe status: %v", err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("signing probe exceeded its bounded cancellation wait")
			}
			// A killed orphan can briefly remain a zombie pending OS reaping;
			// neither absent nor zombie processes can retain executable work.
			deadline = time.Now().Add(2 * time.Second)
			for {
				state, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(child)).Output()
				if err != nil || strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("owned signing descendant survived probe teardown")
				}
				time.Sleep(10 * time.Millisecond)
			}
			_, restoreErr := os.Stat(filepath.Join(root, "restored"))
			if mode == "ignore-term" {
				if !os.IsNotExist(restoreErr) {
					t.Fatal("SIGKILL fixture unexpectedly ran its EXIT trap")
				}
			} else if restoreErr != nil {
				t.Fatal("graceful fixture did not run its restoration trap")
			}
		})
	}
}
