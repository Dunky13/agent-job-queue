package backend

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type privateNativeFixture struct {
	installer                 *installer
	root, tools, tool, socket string
}

// newPrivateNativeFixture builds a custody the private adapter accepts: parent
// holds the root, named prefix plus a random suffix, with homeName and tools in it.
func newPrivateNativeFixture(t *testing.T, parent, prefix, homeName string) privateNativeFixture {
	t.Helper()
	// Unix-domain socket paths on macOS cannot fit Go's long test-name roots.
	root, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if parent == "/tmp" {
		if root, err = filepath.EvalSymlinks(root); err != nil {
			t.Fatal(err)
		}
	}
	home, tools := filepath.Join(root, homeName), filepath.Join(root, "tools")
	for _, directory := range []string{home, tools} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f := privateNativeFixture{root: root, tools: tools, tool: filepath.Join(tools, "service-management"), socket: filepath.Join(root, "native.sock"),
		installer: &installer{home: home, app: filepath.Join(home, "Applications/Agent Queue.app"), state: filepath.Join(home, ".local/state/agent-heavy"), config: filepath.Join(home, ".config/agent-heavy"), path: tools, testing: true}}
	if err := os.WriteFile(f.tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\" \"$HOME\" \"$AGENT_HEAVY_STATE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(f.socket, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_MANAGED", "private-native-lifecycle")
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_TOOLS", tools)
	f.installer.selectTestLifecycle()
	return f
}

// The guards are checked on the adapter itself: the installer would not choose it
// outside test mode.
func (f privateNativeFixture) status() ([]byte, error) {
	return installPrivateController{f.installer}.ServiceManagement(context.Background(), f.installer.app, "status", "queue")
}

// requireRefused expects both operations refused with the isolation error.
func (f privateNativeFixture) requireRefused(t *testing.T) {
	t.Helper()
	_, status := f.status()
	_, replacement := installPrivateController{f.installer}.PrepareAppReplacement(context.Background(), f.installer.app)
	for _, err := range []error{status, replacement} {
		if err == nil || err.Error() != errInstallNativeRefused.Error() {
			t.Fatal("accepted unsafe fixture", err)
		}
	}
}

func TestManagedPrivateNativeCommandsCannotReachRealServices(t *testing.T) {
	f := newPrivateNativeFixture(t, "/tmp", "agent-queue-install-test-", "User With Spaces")
	i, ctx := f.installer, context.Background()
	t.Setenv("AGENT_HEAVY_STATE", "/foreign/queue")
	output, err := i.appController().ServiceManagement(ctx, i.app, "status", "queue")
	if want := strings.Join([]string{"status", "queue", i.home, i.state, ""}, "\n"); err != nil || string(output) != want {
		t.Fatalf("private lifecycle tool lost argument or environment isolation: %q: %v", output, err)
	}
	output, err = i.appController().PrepareAppReplacement(ctx, i.app)
	if want := strings.Join([]string{"prepare-app-replacement", i.app, i.home, i.state, ""}, "\n"); err != nil || string(output) != want {
		t.Fatalf("private UI shutdown tool lost argument or environment isolation: %q: %v", output, err)
	}
	other := filepath.Join(i.home, "Another.app")
	_, status := i.appController().ServiceManagement(ctx, other, "status", "queue")
	_, replacement := i.appController().PrepareAppReplacement(ctx, other)
	for _, err := range []error{status, replacement} {
		if err == nil || err.Error() != errInstallNativeRefused.Error() {
			t.Fatal("accepted another app's native command", err)
		}
	}
	for _, change := range [][]string{{i.app + "/", "status", "queue"}, {i.app, "unknown", "queue"}, {i.app, "status", "unknown"}} {
		if _, err := i.appController().ServiceManagement(ctx, change[0], change[1], change[2]); err == nil || err.Error() != "invalid service management command" {
			t.Fatal("accepted invalid native command", change, err)
		}
	}
	// Pending recovery works on a copy pointed at the retained app.
	retained := *i
	retained.app = other
	output, err = retained.appController().ServiceManagement(ctx, other, "status", "login")
	if want := strings.Join([]string{"status", "login", i.home, i.state, ""}, "\n"); err != nil || string(output) != want {
		t.Fatalf("copied installer lost its own app: %q: %v", output, err)
	}
	output, err = retained.appController().PrepareAppReplacement(ctx, other)
	if want := strings.Join([]string{"prepare-app-replacement", other, i.home, i.state, ""}, "\n"); err != nil || string(output) != want {
		t.Fatalf("copied installer prepared another app: %q: %v", output, err)
	}
	// A failing tool is reported as itself, not as the app executable.
	integrationTestWrite(t, f.tool, []byte("#!/bin/sh\nexit 3\n"))
	if err := os.Chmod(f.tool, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.status(); err == nil || err.Error() != "Installer command service-management failed: exit status 3" {
		t.Fatal("private tool failure", err)
	}
}

// Every guard refuses on its own: each case breaks one in an accepted fixture.
// Foreign owners are not covered, since a test cannot give files away.
func TestInstallPrivateControllerGuards(t *testing.T) {
	chmod := func(t *testing.T, target string, mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(target, mode); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(t *testing.T, target string) {
		t.Helper()
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, f privateNativeFixture){
		"not testing": func(_ *testing.T, f privateNativeFixture) { f.installer.testing = false },
		"managed unset": func(t *testing.T, _ privateNativeFixture) {
			t.Setenv("AGENT_QUEUE_INSTALL_TEST_MANAGED", "")
		},
		"foreign tools": func(t *testing.T, f privateNativeFixture) {
			t.Setenv("AGENT_QUEUE_INSTALL_TEST_TOOLS", f.root)
		},
		"shared tool":  func(t *testing.T, f privateNativeFixture) { chmod(t, f.tool, 0710) },
		"inert tool":   func(t *testing.T, f privateNativeFixture) { chmod(t, f.tool, 0600) },
		"missing tool": func(t *testing.T, f privateNativeFixture) { remove(t, f.tool) },
		"linked tool": func(t *testing.T, f privateNativeFixture) {
			if err := os.Link(f.tool, f.tool+"-second"); err != nil {
				t.Fatal(err)
			}
		},
		"symlink tool": func(t *testing.T, f privateNativeFixture) {
			if err := os.Rename(f.tool, f.tool+"-target"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.tool+"-target", f.tool); err != nil {
				t.Fatal(err)
			}
		},
		"shared socket":  func(t *testing.T, f privateNativeFixture) { chmod(t, f.socket, 0620) },
		"missing socket": func(t *testing.T, f privateNativeFixture) { remove(t, f.socket) },
		"file as socket": func(t *testing.T, f privateNativeFixture) {
			remove(t, f.socket)
			integrationTestWrite(t, f.socket, nil)
			chmod(t, f.socket, 0600)
		},
		"symlink tools": func(t *testing.T, f privateNativeFixture) {
			if err := os.Rename(f.tools, f.tools+"-target"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.tools+"-target", f.tools); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, directory := range map[string]func(privateNativeFixture) string{
		"root":  func(f privateNativeFixture) string { return f.root },
		"home":  func(f privateNativeFixture) string { return f.installer.home },
		"tools": func(f privateNativeFixture) string { return f.tools },
	} {
		for kind, mode := range map[string]os.FileMode{"shared": 0720, "narrowed": 0500} {
			cases[kind+" "+name] = func(t *testing.T, f privateNativeFixture) {
				target := directory(f)
				chmod(t, target, mode)
				t.Cleanup(func() { _ = os.Chmod(target, 0700) })
			}
		}
	}
	for name, breakGuard := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPrivateNativeFixture(t, "/tmp", "agent-queue-install-test-", "User With Spaces")
			if _, err := f.status(); err != nil {
				t.Fatal("intact fixture refused", err)
			}
			breakGuard(t, f)
			f.requireRefused(t)
		})
	}
	// An unclean app is an invalid service command anywhere, so only replacement
	// preparation needs the adapter's own guard.
	t.Run("unclean app", func(t *testing.T) {
		f := newPrivateNativeFixture(t, "/tmp", "agent-queue-install-test-", "User With Spaces")
		f.installer.app += "/"
		if _, err := f.status(); err == nil || err.Error() != "invalid service management command" {
			t.Fatal("accepted unclean service command", err)
		}
		if _, err := (installPrivateController{f.installer}).PrepareAppReplacement(context.Background(), f.installer.app); err == nil || err.Error() != errInstallNativeRefused.Error() {
			t.Fatal("prepared an unclean app", err)
		}
	})
	t.Run("foreign home name", func(t *testing.T) {
		newPrivateNativeFixture(t, "/tmp", "agent-queue-install-test-", "User").requireRefused(t)
	})
	t.Run("foreign root name", func(t *testing.T) {
		newPrivateNativeFixture(t, "/tmp", "agent-queue-other-", "User With Spaces").requireRefused(t)
	})
	t.Run("root through a symlink", func(t *testing.T) {
		base := newPrivateNativeFixture(t, "/tmp", "agent-queue-link-", "User With Spaces").root
		parent := base + "/parent"
		if err := os.Symlink(base, parent); err != nil {
			t.Fatal(err)
		}
		// The root itself is a real private directory; only its path is not canonical.
		newPrivateNativeFixture(t, parent, "agent-queue-install-test-", "User With Spaces").requireRefused(t)
	})
}
