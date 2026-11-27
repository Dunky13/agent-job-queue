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
	PrepareAppReplacement(ctx context.Context, app string) ([]byte, error)
}

// The adapter binds to the installer that asks, because pending recovery copies
// the installer and points the copy at the retained app.
func (install *installer) appController() installController {
	switch {
	case install.controller != nil:
		return install.controller
	case !install.testing:
		return installAppExecutable{install}
	case install.privateLifecycle:
		return installPrivateController{install}
	}
	// Test mode without the private fixture must not reach macOS.
	return installRefusingController{}
}

// selectTestLifecycle records, when the installer is built, whether test mode
// uses the private lifecycle adapter.
func (install *installer) selectTestLifecycle() {
	install.privateLifecycle = os.Getenv("AGENT_QUEUE_INSTALL_TEST_MANAGED") == "private-native-lifecycle"
}

type installAppExecutable struct{ install *installer }

func (adapter installAppExecutable) ServiceManagement(ctx context.Context, app, action, service string) ([]byte, error) {
	args, err := appservices.Arguments(app, action, service)
	if err != nil {
		return nil, err
	}
	return adapter.install.run(ctx, args...)
}

func (adapter installAppExecutable) PrepareAppReplacement(ctx context.Context, app string) ([]byte, error) {
	return adapter.install.run(ctx, installReplacementArguments(app)...)
}

func installReplacementArguments(app string) []string {
	return []string{filepath.Join(app, "Contents/MacOS/AgentQueue"), "--prepare-app-replacement", app}
}

type installRefusingController struct{}

func (installRefusingController) ServiceManagement(_ context.Context, app, action, service string) ([]byte, error) {
	if _, err := appservices.Arguments(app, action, service); err != nil {
		return nil, err
	}
	return nil, errInstallNativeRefused
}

func (installRefusingController) PrepareAppReplacement(context.Context, string) ([]byte, error) {
	return nil, errInstallNativeRefused
}
