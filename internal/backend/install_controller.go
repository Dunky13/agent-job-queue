package backend

import (
	"context"
	"os"
	"path/filepath"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

// installController is the installer's port to the menu bar app's own
// executable, which reaches Service Management for an app bundle and quits the
// running copy of the installer's app before it is replaced.
type installController interface {
	ServiceManagement(ctx context.Context, app, action, service string) ([]byte, error)
	PrepareAppReplacement(ctx context.Context) ([]byte, error)
}

func (install *installer) appController() installController {
	if install.controller != nil {
		return install.controller
	}
	// A test-mode installer with no chosen adapter must not reach macOS.
	if install.testing {
		return installRefusingController{}
	}
	return installAppExecutable{install}
}

// installTestController chooses test mode's adapter when the installer is built.
func installTestController(install *installer) installController {
	if os.Getenv("AGENT_QUEUE_INSTALL_TEST_MANAGED") == "private-native-lifecycle" {
		return installPrivateController{install}
	}
	return installRefusingController{}
}

type installAppExecutable struct{ install *installer }

func (adapter installAppExecutable) ServiceManagement(ctx context.Context, app, action, service string) ([]byte, error) {
	args, err := appservices.Arguments(app, action, service)
	if err != nil {
		return nil, err
	}
	return adapter.install.run(ctx, args...)
}

func (adapter installAppExecutable) PrepareAppReplacement(ctx context.Context) ([]byte, error) {
	return adapter.install.run(ctx, adapter.install.replacementArguments()...)
}

func (install *installer) replacementArguments() []string {
	return []string{filepath.Join(install.app, "Contents/MacOS/AgentQueue"), "--prepare-app-replacement", install.app}
}

type installRefusingController struct{}

func (installRefusingController) ServiceManagement(context.Context, string, string, string) ([]byte, error) {
	return nil, errInstallNativeRefused
}

func (installRefusingController) PrepareAppReplacement(context.Context) ([]byte, error) {
	return nil, errInstallNativeRefused
}
