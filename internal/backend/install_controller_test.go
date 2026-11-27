package backend

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

// memoryController is the installer's controller port for tests: statuses states
// what Service Management reports per service. Operations are recorded as
// {"controller", action, service, app} and
// {"controller", "prepare-app-replacement", app}.
type memoryController struct {
	statuses map[string]string
	// manage answers ServiceManagement; a test wraps it to act or fail then.
	manage func(ctx context.Context, app, action, service string) ([]byte, error)
	// prepare answers PrepareAppReplacement; a test wraps it likewise.
	prepare func(ctx context.Context, app string) ([]byte, error)
	record  func(call []string) error
}

func newMemoryController(record func([]string) error) *memoryController {
	controller := &memoryController{record: record,
		statuses: map[string]string{"queue": "notRegistered", "resources": "notRegistered", "remote": "notRegistered", "login": "notRegistered"}}
	controller.manage = func(_ context.Context, _, action, service string) ([]byte, error) {
		switch action {
		case "status":
			return json.Marshal(appservices.Status{SchemaVersion: 1, Service: service, Status: controller.statuses[service]})
		case "register":
			controller.statuses[service] = "enabled"
		case "unregister":
			controller.statuses[service] = "notRegistered"
		}
		return nil, nil
	}
	controller.prepare = func(_ context.Context, app string) ([]byte, error) {
		return json.Marshal(Object{"schemaVersion": 1, "app": app, "status": "stopped"})
	}
	return controller
}

func (controller *memoryController) ServiceManagement(ctx context.Context, app, action, service string) ([]byte, error) {
	// The app executable is only ever asked for a valid operation.
	if _, err := appservices.Arguments(app, action, service); err != nil {
		return nil, err
	}
	if err := controller.record([]string{"controller", action, service, app}); err != nil {
		return nil, err
	}
	return controller.manage(ctx, app, action, service)
}

func (controller *memoryController) PrepareAppReplacement(ctx context.Context, app string) ([]byte, error) {
	if err := controller.record([]string{"controller", "prepare-app-replacement", app}); err != nil {
		return nil, err
	}
	return controller.prepare(ctx, app)
}

// controllerCall reports a recorded Service Management operation on service.
func controllerCall(call []string, action, service string) bool {
	return len(call) == 4 && call[0] == "controller" && call[1] == action && call[2] == service
}

// controllerChange reports a recorded registration or unregistration.
func controllerChange(call []string) bool {
	return len(call) == 4 && call[0] == "controller" && call[1] != "status"
}

// The app executable adapter's own seam is the installer's command runner.
func TestInstallAppController(t *testing.T) {
	var calls [][]string
	var failure error
	install := &installer{app: "/Applications/Agent Queue.app", command: func(_ context.Context, args []string) ([]byte, error) {
		calls = append(calls, slices.Clone(args))
		return []byte("answer"), failure
	}}
	controller, ctx := install.appController(), context.Background()

	previous := "/Users/someone/Applications/Previous.app"
	for _, operation := range [][]string{{install.app, "status", "queue"}, {previous, "register", "login"}, {install.app, "unregister", "remote"}} {
		if output, err := controller.ServiceManagement(ctx, operation[0], operation[1], operation[2]); err != nil || string(output) != "answer" {
			t.Fatalf("service management: %q %v", output, err)
		}
	}
	if output, err := controller.PrepareAppReplacement(ctx, install.app); err != nil || string(output) != "answer" {
		t.Fatalf("prepare replacement: %q %v", output, err)
	}
	for _, invalid := range [][]string{{"relative.app", "status", "queue"}, {install.app, "enable", "queue"}, {install.app, "status", "daemon"}} {
		if _, err := controller.ServiceManagement(ctx, invalid[0], invalid[1], invalid[2]); err == nil || err.Error() != "invalid service management command" {
			t.Fatalf("invalid operation %v: %v", invalid, err)
		}
	}
	failure = errors.New("exit status 2")
	if _, err := controller.ServiceManagement(ctx, install.app, "status", "queue"); !errors.Is(err, failure) {
		t.Fatal("service management hid its failure", err)
	}
	if _, err := controller.PrepareAppReplacement(ctx, install.app); !errors.Is(err, failure) {
		t.Fatal("replacement preparation hid its failure", err)
	}

	executable := install.app + "/Contents/MacOS/AgentQueue"
	status := []string{executable, "--service-management", "status", "queue"}
	replacement := []string{executable, "--prepare-app-replacement", install.app}
	want := [][]string{
		status,
		{previous + "/Contents/MacOS/AgentQueue", "--service-management", "register", "login"},
		{executable, "--service-management", "unregister", "remote"},
		replacement,
		status,
		replacement,
	}
	if !slices.EqualFunc(calls, want, slices.Equal[[]string]) {
		t.Fatalf("app executable argv: %v", calls)
	}
}

// Without the seam, the runner gives the app executable the installed target's
// environment. The executable here is a private script.
func TestInstallAppControllerIsolatesEnvironment(t *testing.T) {
	home := t.TempDir()
	install := &installer{home: home, app: filepath.Join(home, "Agent Queue.app"), state: filepath.Join(home, "state")}
	executable := filepath.Join(install.app, "Contents/MacOS/AgentQueue")
	integrationTestWrite(t, executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$HOME\" \"$AGENT_HEAVY_STATE\"\n"))
	if err := os.Chmod(executable, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_HEAVY_STATE", "/foreign/queue")
	want := home + "\n" + install.state + "\n"
	status, err := install.appController().ServiceManagement(context.Background(), install.app, "status", "queue")
	if err != nil || string(status) != want {
		t.Fatalf("service management environment: %q %v", status, err)
	}
	replacement, err := install.appController().PrepareAppReplacement(context.Background(), install.app)
	if err != nil || string(replacement) != want {
		t.Fatalf("replacement environment: %q %v", replacement, err)
	}
}

func TestInstallTestModeWithoutFixtureRefusesNativeOperations(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Agent Queue.app")
	executed := filepath.Join(t.TempDir(), "executed")
	integrationTestWrite(t, filepath.Join(app, "Contents/MacOS/AgentQueue"), []byte("#!/bin/sh\n: > "+installQuote(executed)+"\n"))
	if err := os.Chmod(filepath.Join(app, "Contents/MacOS/AgentQueue"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, managed := range []string{"", "1", "private-native-lifecycle "} {
		t.Setenv("AGENT_QUEUE_INSTALL_TEST_MANAGED", managed)
		// A test-mode installer that never made the choice refuses too.
		unchosen, chosen := &installer{app: app, testing: true}, &installer{app: app, testing: true}
		chosen.selectTestLifecycle()
		for _, install := range []*installer{unchosen, chosen} {
			controller := install.appController()
			if _, refusing := controller.(installRefusingController); !refusing {
				t.Fatalf("managed %q chose %T", managed, controller)
			}
			_, status := controller.ServiceManagement(context.Background(), app, "status", "queue")
			_, replacement := controller.PrepareAppReplacement(context.Background(), app)
			for _, err := range []error{status, replacement} {
				if err == nil || err.Error() != "Managed installer tests require the private native lifecycle fixture; real service management is disabled in test mode" {
					t.Fatalf("managed %q: %v", managed, err)
				}
			}
			// A command that is invalid anywhere keeps saying so.
			if _, err := controller.ServiceManagement(context.Background(), "relative.app", "status", "queue"); err == nil || err.Error() != "invalid service management command" {
				t.Fatalf("managed %q: invalid command: %v", managed, err)
			}
		}
	}
	if _, err := os.Lstat(executed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("test mode ran the app executable", err)
	}
	t.Setenv("AGENT_QUEUE_INSTALL_TEST_MANAGED", "private-native-lifecycle")
	install := &installer{app: app, testing: true}
	install.selectTestLifecycle()
	if controller, private := install.appController().(installPrivateController); !private || controller.install != install {
		t.Fatalf("managed fixture chose %T", install.appController())
	}
}
