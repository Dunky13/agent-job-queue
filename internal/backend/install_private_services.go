package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"golang.org/x/sys/unix"
)

var errInstallNativeRefused = errors.New("Managed installer tests require the private native lifecycle fixture; real service management is disabled in test mode")

// A private HOME does not isolate Service Management's login domain. Artifact
// acceptance must explicitly opt into a private parent-owned lifecycle tool;
// otherwise test-mode native commands fail before reaching macOS registration.
// Signature checks and actual packaged daemon/job execution remain unchanged.
type installPrivateController struct{ install *installer }

func (adapter installPrivateController) ServiceManagement(ctx context.Context, app, action, service string) ([]byte, error) {
	native, err := appservices.Arguments(app, action, service)
	if err != nil {
		return nil, err
	}
	if app != adapter.install.app {
		return nil, errInstallNativeRefused
	}
	return adapter.run(ctx, native, action, service)
}

func (adapter installPrivateController) PrepareAppReplacement(ctx context.Context, app string) ([]byte, error) {
	if app != adapter.install.app || app != filepath.Clean(app) {
		return nil, errInstallNativeRefused
	}
	return adapter.run(ctx, installReplacementArguments(app), "prepare-app-replacement", app)
}

// run executes the private tool in place of native, keeping native's isolated
// environment. It bypasses installer.command on purpose: only tests set that seam,
// and the private tool must run for real.
func (adapter installPrivateController) run(ctx context.Context, native []string, toolArgs ...string) ([]byte, error) {
	tool, err := adapter.tool()
	if err != nil {
		return nil, err
	}
	return adapter.install.runAs(ctx, native, append([]string{tool}, toolArgs...))
}

// tool rechecks the fixture's custody before every use.
func (adapter installPrivateController) tool() (string, error) {
	install := adapter.install
	if !install.testing || os.Getenv("AGENT_QUEUE_INSTALL_TEST_MANAGED") != "private-native-lifecycle" {
		return "", errInstallNativeRefused
	}
	tools := os.Getenv("AGENT_QUEUE_INSTALL_TEST_TOOLS")
	if !installPrivateTestLayout(install.home, tools) {
		return "", errInstallNativeRefused
	}
	root := filepath.Dir(install.home)
	tool := filepath.Join(tools, "service-management")
	var executable, socket unix.Stat_t
	if unix.Lstat(tool, &executable) != nil || executable.Mode&unix.S_IFMT != unix.S_IFREG || executable.Mode&0077 != 0 || executable.Mode&0100 == 0 || executable.Uid != uint32(os.Getuid()) || executable.Nlink != 1 {
		return "", errInstallNativeRefused
	}
	if unix.Lstat(filepath.Join(root, "native.sock"), &socket) != nil || socket.Mode&unix.S_IFMT != unix.S_IFSOCK || socket.Mode&0077 != 0 || socket.Uid != uint32(os.Getuid()) {
		return "", errInstallNativeRefused
	}
	return tool, nil
}

// installPrivateTestLayout reports whether home and tools sit in a private test
// root. Installer test mode starts only there, so a real home is never its target.
func installPrivateTestLayout(home, tools string) bool {
	root := filepath.Dir(home)
	// A list separator in root would split the tools folder out of the test PATH.
	if filepath.Base(home) != "User With Spaces" || !strings.HasPrefix(filepath.Base(root), "agent-queue-install-test-") || strings.ContainsRune(root, os.PathListSeparator) || tools != filepath.Join(root, "tools") {
		return false
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return false
	}
	for _, directory := range []string{root, home, tools} {
		var info unix.Stat_t
		if unix.Lstat(directory, &info) != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Mode&0777 != 0700 || info.Uid != uint32(os.Getuid()) {
			return false
		}
	}
	return true
}
