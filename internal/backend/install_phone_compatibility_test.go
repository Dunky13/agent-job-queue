package backend

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInstallerPhoneAccessRecoveryLegacyRemoteVirtualenv(t *testing.T) {
	for _, kind := range []string{"canonical", "foreign-interpreter", "foreign-script", "extra-argument", "foreign-state", "program", "test-mode", "loaded-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			f.legacy(t)
			install := f.installer
			arguments := []string{filepath.Join(install.root, "remote/.venv/bin/python"), filepath.Join(install.root, "remote/control.py"), "serve"}
			definition := Object{"Label": "io.went.agent-remote", "ProgramArguments": arguments}
			switch kind {
			case "foreign-interpreter":
				arguments[0] = filepath.Join(install.home, "other/.venv/bin/python")
			case "foreign-script":
				arguments[1] = filepath.Join(install.home, "other/control.py")
			case "extra-argument":
				definition["ProgramArguments"] = append(arguments, "--custom")
			case "foreign-state":
				definition["EnvironmentVariables"] = Object{"AGENT_REMOTE_STATE": filepath.Join(install.home, "other/state")}
			case "program":
				definition["Program"] = "/usr/bin/unrelated"
			case "test-mode":
				definition["EnvironmentVariables"] = Object{"AGENT_REMOTE_TEST_MODE": "1"}
			}
			if err := install.writePlist(filepath.Join(install.launch, "io.went.agent-remote.plist"), definition); err != nil {
				t.Fatal(err)
			}
			f.loaded["io.went.agent-remote"] = true
			originalCommand := install.command
			install.command = func(ctx context.Context, args []string) ([]byte, error) {
				if kind == "loaded-mismatch" && args[0] == "launchctl" && args[1] == "print" && filepath.Base(args[2]) == "io.went.agent-remote" {
					return []byte("program = /usr/bin/unrelated\narguments = {\n/usr/bin/unrelated\nserve\n}\n"), nil
				}
				if args[0] == filepath.Join(install.payload, "source/bin/agent-remote") {
					delete(f.loaded, "io.went.agent-remote")
					return nil, nil
				}
				return originalCommand(ctx, args)
			}
			before := installTreeWitness(t, install.home)
			err := install.validatePhoneAccessRecovery(context.Background())
			if kind != "canonical" {
				if err == nil {
					t.Fatal("unowned legacy remote service accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("shipped legacy remote virtualenv rejected: %v", err)
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, install.home)) {
				t.Fatal("ownership inspection changed installation files")
			}
			report := install.preflight(context.Background())
			if !preflightIDs(report)["legacy-phone-access"] {
				t.Fatalf("legacy phone access recovery unavailable: %+v", report)
			}
			install.disablePhoneAccess = true
			if err := f.run(t); err != nil {
				t.Fatalf("legacy remote virtualenv recovery failed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(install.root, "install-in-progress.json")); !os.IsNotExist(err) {
				t.Fatalf("installation did not finish: %v", err)
			}
		})
	}
}
