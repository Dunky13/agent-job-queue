package backend

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"golang.org/x/sys/unix"
)

// A private HOME does not isolate Service Management's login domain. Artifact
// acceptance must explicitly opt into a private parent-owned lifecycle tool;
// otherwise test-mode native commands fail before reaching macOS registration.
// Signature checks and actual packaged daemon/job execution remain unchanged.
func (install *installer) privateManagedCommand(args []string) ([]string, error) {
	refused := errors.New("Managed installer tests require the private native lifecycle fixture; real service management is disabled in test mode")
	if !install.testing || os.Getenv("AGENT_QUEUE_INSTALL_TEST_MANAGED") != "private-native-lifecycle" || len(args) < 3 {
		return nil, refused
	}
	app := filepath.Dir(filepath.Dir(filepath.Dir(args[0])))
	if args[0] != filepath.Join(app, "Contents/MacOS/AgentQueue") || app != install.app && app != filepath.Join(install.payload, "Agent Queue.app") {
		return nil, refused
	}
	var toolArgs []string
	if args[1] == "--prepare-app-replacement" {
		if len(args) != 3 || args[2] != install.app {
			return nil, refused
		}
		toolArgs = []string{"prepare-app-replacement", args[2]}
	} else {
		if len(args) != 4 || args[1] != "--service-management" {
			return nil, refused
		}
		expected, err := appservices.Arguments(app, args[2], args[3])
		if err != nil || !slices.Equal(expected, args) {
			return nil, refused
		}
		toolArgs = args[2:]
	}
	root := filepath.Dir(install.home)
	tools := filepath.Join(root, "tools")
	if filepath.Base(install.home) != "User With Spaces" || !strings.HasPrefix(filepath.Base(root), "agent-queue-install-test-") || os.Getenv("AGENT_QUEUE_INSTALL_TEST_TOOLS") != tools {
		return nil, refused
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return nil, refused
	}
	for _, directory := range []string{root, install.home, tools} {
		var info unix.Stat_t
		if unix.Lstat(directory, &info) != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Mode&0777 != 0700 || info.Uid != uint32(os.Getuid()) {
			return nil, refused
		}
	}
	tool := filepath.Join(tools, "service-management")
	var executable, socket unix.Stat_t
	if unix.Lstat(tool, &executable) != nil || executable.Mode&unix.S_IFMT != unix.S_IFREG || executable.Mode&0077 != 0 || executable.Mode&0100 == 0 || executable.Uid != uint32(os.Getuid()) || executable.Nlink != 1 {
		return nil, refused
	}
	if unix.Lstat(filepath.Join(root, "native.sock"), &socket) != nil || socket.Mode&unix.S_IFMT != unix.S_IFSOCK || socket.Mode&0077 != 0 || socket.Uid != uint32(os.Getuid()) {
		return nil, refused
	}
	return append([]string{tool}, toolArgs...), nil
}
