package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func missingServiceFixture(t *testing.T) (*installFixture, map[string]string) {
	t.Helper()
	f := installTestFixture(t)
	f.legacy(t)
	outputs := map[string]string{}
	for _, label := range []string{"io.went.agent-heavy", "io.went.agent-resources"} {
		p := filepath.Join(f.installer.launch, label+".plist")
		definition, err := updatePlist(p)
		if err != nil {
			t.Fatal(err)
		}
		args, err := stringsFrom(definition["ProgramArguments"])
		if err != nil {
			t.Fatal(err)
		}
		if label == "io.went.agent-heavy" {
			args[0] = "/opt/homebrew/bin/pueued"
		}
		outputs[label] = fmt.Sprintf("%s/%s = {\n\tpath = %s\n\ttype = LaunchAgent\n\tprogram = %s\n\targuments = {\n\t\t%s\n\t}\n\tenvironment = {\n\t\tHOME => %s\n\t\tPATH => /opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin\n\t\tXPC_SERVICE_NAME => %s\n\t}\n}\n", f.installer.domain(), label, p, args[0], strings.Join(args, "\n\t\t"), f.installer.home, label)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	original := f.installer.command
	f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" {
			label := filepath.Base(args[2])
			if output, ok := outputs[label]; ok && f.loaded[label] {
				return []byte(output), nil
			}
		}
		return original(ctx, args)
	}
	return f, outputs
}
func offersServiceRestore(report installPreflightReport) bool {
	for _, issue := range report.Issues {
		for _, action := range issue.Actions {
			if action.ID == "restore-service-definitions" {
				return action.Destructive
			}
		}
	}
	return false
}
func TestInstallerMissingServiceDefinitionsRecovery(t *testing.T) {
	f, outputs := missingServiceFixture(t)
	before := installTreeWitness(t, f.installer.home)
	report := f.installer.preflight(context.Background())
	if !offersServiceRestore(report) {
		t.Fatalf("missing running definitions have no confirmed recovery: %+v", report)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.home)) {
		t.Fatal("preflight mutated files")
	}
	if err := f.installer.recover(context.Background(), "restore-service-definitions", nil, ""); err != nil {
		t.Fatal(err)
	}
	for label := range outputs {
		p := filepath.Join(f.installer.launch, label+".plist")
		definition, err := updatePlist(p)
		if err != nil {
			t.Fatal(err)
		}
		if definition["Label"] != label {
			t.Fatal("wrong identity")
		}
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("not private", err)
		}
	}
	for _, call := range f.calls {
		if len(call) > 1 && call[0] == "launchctl" && call[1] != "print" {
			t.Fatalf("recovery changed live service: %v", call)
		}
	}
	if offersServiceRestore(f.installer.preflight(context.Background())) {
		t.Fatal("restoration offered after completion")
	}
	if err := f.run(t); err != nil {
		t.Fatal(err)
	}
}
func TestInstallerMissingServiceDefinitionsRejectUnowned(t *testing.T) {
	for _, kind := range []string{"path", "program", "arguments", "home", "override", "nested", "duplicate", "truncated", "unknown-environment", "no-home"} {
		t.Run(kind, func(t *testing.T) {
			f, outputs := missingServiceFixture(t)
			label := "io.went.agent-resources"
			output := outputs[label]
			switch kind {
			case "path":
				output = strings.Replace(output, "path = "+f.installer.launch, "path = /other", 1)
			case "program":
				output = strings.Replace(output, "program = ", "program = /other", 1)
			case "arguments":
				output = strings.Replace(output, "\n\t\tscheduler\n", "\n\t\tcustom\n", 1)
			case "home":
				output = strings.Replace(output, "HOME => "+f.installer.home, "HOME => /other", 1)
			case "override":
				output = strings.Replace(output, "\tenvironment = {", "\tenvironment = {\n\t\tAGENT_HEAVY_STATE => /other", 1)
			case "nested":
				output = strings.Replace(output, "\tprogram = ", "\tunknown = {\n\tprogram = ", 1) + "}\n"
			case "duplicate":
				output = strings.Replace(output, "\tprogram = ", "\tprogram = /other\n\tprogram = ", 1)
			case "truncated":
				output = strings.TrimSuffix(output, "}\n")
			case "unknown-environment":
				output = strings.Replace(output, "\tenvironment = {", "\tenvironment = {\n\t\tDYLD_INSERT_LIBRARIES => /other", 1)
			case "no-home":
				output = strings.Replace(output, "\t\tHOME => "+f.installer.home+"\n", "", 1)
			}
			outputs[label] = output
			before := installTreeWitness(t, f.installer.home)
			if offersServiceRestore(f.installer.preflight(context.Background())) {
				t.Fatal("unsafe restoration offered")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.home)) {
				t.Fatal("rejected preflight changed files")
			}
			if err := f.installer.recover(context.Background(), "restore-service-definitions", nil, ""); err == nil {
				t.Fatal("unsafe restoration accepted")
			}
			// Recovery may create private coordination locks, but no service definition.
			for label := range outputs {
				if _, err := os.Lstat(filepath.Join(f.installer.launch, label+".plist")); !os.IsNotExist(err) {
					t.Fatal("unsafe recovery published definition", err)
				}
			}
		})
	}
}

func TestInstallerMissingServiceDefinitionsLoadedEnvironment(t *testing.T) {
	for _, kind := range []string{"launchd-socket", "launchd-run-socket", "foreign-socket", "nested-socket", "inherited-target", "nested-arguments", "foreign-type", "foreign-domain", "test-mode", "duplicate-env", "unverified-default-path", "argument-whitespace", "argument-leading-space", "argument-leading-tab", "argument-empty", "home-whitespace", "path-whitespace"} {
		t.Run(kind, func(t *testing.T) {
			f, outputs := missingServiceFixture(t)
			label := "io.went.agent-heavy"
			output := outputs[label]
			switch kind {
			case "launchd-socket", "launchd-run-socket":
				output = strings.Replace(output, "\tenvironment = {", "\tinherited environment = {\n\t\tSSH_AUTH_SOCK => /private/tmp/com.apple.launchd.fixture/Listeners\n\t}\n\tdefault environment = {\n\t\tPATH => /usr/bin:/bin:/usr/sbin:/sbin\n\t}\n\tenvironment = {\n\t\tOSLogRateLimit => 64", 1)
				if kind == "launchd-run-socket" {
					output = strings.Replace(output, "/private/tmp/com.apple.launchd.fixture/Listeners", "/var/run/com.apple.launchd.fixture/Listeners", 1)
				}
			case "nested-socket":
				output = strings.Replace(output, "\tenvironment = {", "\tinherited environment = {\n\t\tSSH_AUTH_SOCK => /private/tmp/com.apple.launchd.fixture/nested/Listeners\n\t}\n\tenvironment = {", 1)
			case "foreign-socket":
				output = strings.Replace(output, "\tenvironment = {", "\tinherited environment = {\n\t\tSSH_AUTH_SOCK => /other/socket\n\t}\n\tenvironment = {", 1)
			case "inherited-target":
				output = strings.Replace(output, "\tenvironment = {", "\tinherited environment = {\n\t\tAGENT_HEAVY_CONFIG => /other/config\n\t}\n\tenvironment = {", 1)
			case "nested-arguments":
				output = strings.Replace(output, "\targuments = {", "\targuments = {\n\t\tnested = {\n\t\t}", 1)
			case "foreign-type":
				output = strings.Replace(output, "type = LaunchAgent", "type = LaunchDaemon", 1)
			case "foreign-domain":
				output = strings.Replace(output, f.installer.domain(), "gui/99999", 1)
			case "test-mode":
				output = strings.Replace(output, "\tenvironment = {", "\tenvironment = {\n\t\tAGENT_REMOTE_TEST_MODE => 1", 1)
			case "duplicate-env":
				output = strings.Replace(output, "\tenvironment = {", "\tenvironment = {\n\t\tHOME => "+f.installer.home, 1)
			case "unverified-default-path":
				output = strings.Replace(output, "\t\tPATH => /opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin\n", "", 1)
				output = strings.Replace(output, "\tenvironment = {", "\tdefault environment = {\n\t\tPATH => /unreviewed/bin\n\t}\n\tenvironment = {", 1)
			case "argument-whitespace":
				output = strings.Replace(output, "\t\t--config\n", "\t\t--config \n", 1)
			case "argument-leading-space":
				output = strings.Replace(output, "\t\t--config\n", "\t\t --config\n", 1)
			case "argument-leading-tab":
				output = strings.Replace(output, "\t\t--config\n", "\t\t\t--config\n", 1)
			case "argument-empty":
				output = strings.Replace(output, "\targuments = {", "\targuments = {\n\t\t", 1)
			case "home-whitespace":
				output = strings.Replace(output, "HOME => "+f.installer.home+"\n", "HOME => "+f.installer.home+" \n", 1)
			case "path-whitespace":
				output = strings.Replace(output, "path = "+f.installer.launch+"/"+label+".plist\n", "path = "+f.installer.launch+"/"+label+".plist \n", 1)
			}
			outputs[label] = output
			offered := offersServiceRestore(f.installer.preflight(context.Background()))
			if offered != (kind == "launchd-socket" || kind == "launchd-run-socket") {
				t.Fatalf("unexpected offer %v for %s", offered, kind)
			}
			if offered {
				if err := f.installer.recover(context.Background(), installRestoreServices, nil, ""); err != nil {
					t.Fatal(err)
				}
				value, err := updatePlist(filepath.Join(f.installer.launch, label+".plist"))
				if err != nil {
					t.Fatal(err)
				}
				env := obj(value["EnvironmentVariables"])
				for _, key := range []string{"SSH_AUTH_SOCK", "XPC_SERVICE_NAME", "OSLogRateLimit"} {
					if _, exists := env[key]; exists {
						t.Fatalf("runtime inherited metadata copied: %s", key)
					}
				}
			}
		})
	}
}

func TestInstallerMissingServiceDefinitionsRequiresLaunchDirectory(t *testing.T) {
	f, _ := missingServiceFixture(t)
	if err := os.Remove(f.installer.launch); err != nil {
		t.Fatal(err)
	}
	before := installTreeWitness(t, f.installer.home)
	if offersServiceRestore(f.installer.preflight(context.Background())) {
		t.Fatal("offered recovery without its required launch directory")
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.home)) {
		t.Fatal("preflight created the missing launch directory")
	}
	if err := f.installer.recover(context.Background(), installRestoreServices, nil, ""); err == nil {
		t.Fatal("recovery accepted a missing launch directory")
	}
	if _, err := os.Lstat(f.installer.launch); !os.IsNotExist(err) {
		t.Fatal("rejected recovery created the launch directory", err)
	}
}

func TestInstallerMissingServiceDefinitionsRemoteAndExisting(t *testing.T) {
	for _, kind := range []string{"all-three", "missing-remote-state", "existing-valid", "existing-custom", "existing-symlink"} {
		t.Run(kind, func(t *testing.T) {
			f, outputs := missingServiceFixture(t)
			if kind == "all-three" || kind == "missing-remote-state" {
				label := "io.went.agent-remote"
				args := []string{filepath.Join(f.installer.root, "remote/.venv/bin/python"), filepath.Join(f.installer.root, "remote/control.py"), "serve"}
				outputs[label] = fmt.Sprintf("%s/%s = {\n\tpath = %s\n\ttype = LaunchAgent\n\tprogram = %s\n\targuments = {\n\t\t%s\n\t}\n\tenvironment = {\n\t\tHOME => %s\n\t\tPATH => /opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin\n\t}\n}\n", f.installer.domain(), label, filepath.Join(f.installer.launch, label+".plist"), args[0], strings.Join(args, "\n\t\t"), f.installer.home)
				f.loaded[label] = true
				if err := os.Mkdir(filepath.Join(f.installer.state, "remote"), 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "missing-remote-state" {
					if err := os.Remove(filepath.Join(f.installer.state, "remote")); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				filename := filepath.Join(f.installer.launch, "io.went.agent-resources.plist")
				value := Object{"Label": "io.went.agent-resources", "ProgramArguments": []string{filepath.Join(f.installer.root, ".venv/bin/python"), filepath.Join(f.installer.root, "agent_heavy.py"), "scheduler"}}
				if kind == "existing-custom" {
					value["ProgramArguments"] = []string{"/custom", "scheduler"}
				}
				if kind == "existing-symlink" {
					if err := os.Symlink(filepath.Join(f.installer.home, "other.plist"), filename); err != nil {
						t.Fatal(err)
					}
				} else if err := f.installer.writePlist(filename, value); err != nil {
					t.Fatal(err)
				}
			}
			before := installTreeWitness(t, f.installer.launch)
			report := f.installer.preflight(context.Background())
			eligible := kind == "all-three" || kind == "existing-valid"
			if offersServiceRestore(report) != eligible {
				t.Fatalf("unexpected eligibility %+v", report)
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.launch)) {
				t.Fatal("preflight changed plists")
			}
			err := f.installer.recover(context.Background(), installRestoreServices, nil, "")
			if (err == nil) != eligible {
				t.Fatalf("unexpected recovery result %v", err)
			}
			if !eligible {
				if !reflect.DeepEqual(before, installTreeWitness(t, f.installer.launch)) {
					t.Fatal("rejected recovery changed plists")
				}
				return
			}
			if err := f.installer.validateLoadedServices(context.Background()); err != nil {
				t.Fatal(err)
			}
			if kind == "all-three" {
				if err := f.installer.validatePhoneAccessRecovery(context.Background()); err != nil {
					t.Fatal(err)
				}
				after := f.installer.preflight(context.Background())
				if offersServiceRestore(after) || !preflightIDs(after)["legacy-phone-access"] {
					t.Fatalf("did not advance to phone migration choice: %+v", after)
				}
			}
		})
	}
}

func TestInstallerMissingServiceDefinitionsRechecksAndPreservesRaceWinner(t *testing.T) {
	for _, kind := range []string{"stopped", "changed", "race-file", "race-symlink"} {
		t.Run(kind, func(t *testing.T) {
			f, outputs := missingServiceFixture(t)
			if !offersServiceRestore(f.installer.preflight(context.Background())) {
				t.Fatal("setup not eligible")
			}
			original := f.installer.command
			queries := 0
			filename := filepath.Join(f.installer.launch, "io.went.agent-resources.plist")
			f.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
				if len(args) == 3 && args[0] == "launchctl" && args[1] == "print" && filepath.Base(args[2]) == "io.went.agent-resources" {
					queries++
					if queries == 2 {
						switch kind {
						case "stopped":
							return nil, errInstallServiceNotLoaded
						case "changed":
							return []byte(strings.Replace(outputs["io.went.agent-resources"], "scheduler", "custom", 1)), nil
						case "race-file":
							integrationTestWrite(t, filename, []byte("race winner"))
						case "race-symlink":
							if err := os.Symlink(filepath.Join(f.installer.home, "race-target"), filename); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				return original(ctx, args)
			}
			if err := f.installer.recover(context.Background(), installRestoreServices, nil, ""); err == nil {
				t.Fatal("stale service or concurrent file accepted")
			}
			if _, err := os.Lstat(filepath.Join(f.installer.launch, "io.went.agent-heavy.plist")); !os.IsNotExist(err) {
				t.Fatal("partial recovery was not rolled back", err)
			}
			if kind == "race-file" {
				raw, err := os.ReadFile(filename)
				if err != nil || string(raw) != "race winner" {
					t.Fatal("race winner overwritten or removed", err)
				}
			} else if kind == "race-symlink" {
				target, err := os.Readlink(filename)
				if err != nil || target != filepath.Join(f.installer.home, "race-target") {
					t.Fatal("race symlink overwritten or removed", err)
				}
			} else if _, err := os.Lstat(filename); !os.IsNotExist(err) {
				t.Fatal("stale service definition created", err)
			}
		})
	}
}

func TestInstallerMissingServiceDefinitionsRestrictiveUmask(t *testing.T) {
	f, outputs := missingServiceFixture(t)
	for _, name := range []string{"install.lock", "scheduling.lock"} {
		lock, err := installLock(f.installer.state, name)
		if err != nil {
			t.Fatal(err)
		}
		lock.Close()
	}
	previous := unix.Umask(0777)
	func() {
		defer unix.Umask(previous)
		if err := f.installer.recover(context.Background(), installRestoreServices, nil, ""); err != nil {
			t.Fatal(err)
		}
	}()
	for label := range outputs {
		info, err := os.Stat(filepath.Join(f.installer.launch, label+".plist"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("restored definition did not retain private readable mode", err)
		}
	}
}
