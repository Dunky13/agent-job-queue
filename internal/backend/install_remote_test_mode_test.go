package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// agent-remote refuses service control on this signal (internal/remote), so the
// installer's real command runner must deliver it from its own test mode.
func TestInstallerTestModeReachesRemoteHelper(t *testing.T) {
	i := managedTestFixture(t).installer
	i.command = nil
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_MODE", "")
	helper := filepath.Join(i.app, "Contents/MacOS/agent-remote")
	integrationTestWrite(t, helper, []byte("#!/bin/sh\nprintf '%s:%s' \"$1\" \"$AGENT_QUEUE_INSTALL_TEST_MODE\"\n"))
	if err := os.Chmod(helper, 0700); err != nil {
		t.Fatal(err)
	}
	for _, testing := range []bool{true, false} {
		i.testing = testing
		for _, action := range []string{"disable", "migrate"} {
			want := action + ":"
			if testing {
				want += "1"
			}
			if output, err := i.run(context.Background(), helper, action); err != nil || string(output) != want {
				t.Errorf("testing=%v: helper received %q, want %q: %v", testing, output, want, err)
			}
		}
	}
}
