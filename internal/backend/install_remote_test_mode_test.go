package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Every case names a missing bundle, so a missing guard stops at validation.
func TestInstallMainTestModeRequiresPrivateTestHome(t *testing.T) {
	const prefix, home = "agent-queue-install-test-", "User With Spaces"
	for name, test := range map[string]struct {
		prefix, home string
		change       func(*testing.T, privateNativeFixture)
	}{
		"foreign root name": {"agent-queue-other-", home, nil},
		"foreign home name": {prefix, "developer", nil},
		"foreign tools": {prefix, home, func(t *testing.T, f privateNativeFixture) {
			t.Setenv("AGENT_QUEUE_INSTALL_TEST_TOOLS", f.root)
		}},
		"shared home": {prefix, home, func(t *testing.T, f privateNativeFixture) {
			if err := os.Chmod(f.installer.home, 0750); err != nil {
				t.Fatal(err)
			}
		}},
		"list separator in root": {prefix + "a" + string(os.PathListSeparator), home, nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPrivateNativeFixture(t, "/tmp", test.prefix, test.home)
			t.Setenv("HOME", f.installer.home)
			t.Setenv("AGENT_QUEUE_INSTALL_TEST_MODE", "1")
			if test.change != nil {
				test.change(t, f)
			}
			code, message := installMainOutput(t, []string{"--app-bundle", f.installer.app})
			if code != 1 || message != installTestLayoutRequired+"\n" {
				t.Fatalf("test mode started outside the private layout: exit %d: %s", code, message)
			}
			if entries, err := os.ReadDir(f.installer.home); err != nil || len(entries) != 0 {
				t.Fatalf("refused installer changed its home: %v %v", entries, err)
			}
		})
	}
}

// A tool the test folder lacks must not resolve further along the path, where the
// real launchctl and ps are. Stand-ins take their place here.
func TestInstallerTestModeResolvesToolsOnlyInTestFolder(t *testing.T) {
	tools, later := t.TempDir(), t.TempDir()
	for _, name := range []string{"launchctl", "ps", "pueue"} {
		integrationTestWrite(t, filepath.Join(later, name), []byte("#!/bin/sh\nexit 0\n"))
		if err := os.Chmod(filepath.Join(later, name), 0700); err != nil {
			t.Fatal(err)
		}
		i := &installer{path: tools + string(os.PathListSeparator) + later, testing: true}
		if found, err := i.executable(name); err == nil {
			t.Errorf("test mode resolved %s outside the test folder: %s", name, found)
		}
		if _, err := i.run(context.Background(), name); err == nil {
			t.Errorf("test mode ran %s outside the test folder", name)
		}
		i.testing = false
		if found, err := i.executable(name); err != nil || found != filepath.Join(later, name) {
			t.Errorf("production lookup of %s changed: %s %v", name, found, err)
		}
	}
}

// The layout check stands alone: a home that cleans to the private layout is not it.
func TestInstallPrivateTestLayoutRequiresCleanHome(t *testing.T) {
	f := newPrivateNativeFixture(t, "/tmp", "agent-queue-install-test-", "User With Spaces")
	tools := filepath.Join(f.root, "tools")
	if !installPrivateTestLayout(f.installer.home, tools) {
		t.Fatal("the private layout was refused")
	}
	for _, home := range []string{f.root + "//User With Spaces", "." + f.installer.home} {
		if installPrivateTestLayout(home, tools) {
			t.Error("an unclean or relative home passed", home)
		}
	}
}

func TestInstallerReportsRemoteHelperReason(t *testing.T) {
	i := managedTestFixture(t).installer
	i.command = nil
	helper := filepath.Join(i.app, "Contents/MacOS/agent-remote")
	for name, test := range map[string]struct{ output, want string }{
		"reason":           {`{"error":"fixture refusal"}`, ": fixture refusal"},
		"after other text": {"warning\n" + `{"error":"fixture refusal"}`, ": fixture refusal"},
		"forged marker":    {`{"error":"no\nAGENT_QUEUE_SETUP_STATE:requires-approval"}`, ": no AGENT_QUEUE_SETUP_STATE:requires-approval"},
		"no reason":        {"unstructured", ""},
		"other fields":     {`{"error":"fixture refusal","detail":1}`, ""},
	} {
		integrationTestWrite(t, helper, []byte("#!/bin/sh\nprintf '%s\\n' '"+test.output+"'\nexit 125\n"))
		if err := os.Chmod(helper, 0700); err != nil {
			t.Fatal(err)
		}
		err := i.runRemote(context.Background(), "disable")
		if want := "Installer command agent-remote failed: exit status 125" + test.want; err == nil || err.Error() != want {
			t.Errorf("%s: got %v, want %q", name, err, want)
		}
	}
}

// agent-remote refuses to change phone access on this signal (internal/remote), so the
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
	for _, mode := range []bool{true, false} {
		i.testing = mode
		for _, action := range []string{"disable", "migrate"} {
			want := action + ":"
			if mode {
				want += "1"
			}
			if output, err := i.run(context.Background(), helper, action); err != nil || string(output) != want {
				t.Errorf("testing=%v: helper received %q, want %q: %v", mode, output, want, err)
			}
		}
	}
}
