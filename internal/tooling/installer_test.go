package tooling

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const verificationBuildInfo = `{"schemaVersion":1,"repository":"Dunky13/agent-job-queue","version":"1.2.3","commit":"0123456789012345678901234567890123456789","build":42}`

type verificationFixture struct {
	root, app, source, events string
}

func verificationWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func verificationSource(t *testing.T, source string) {
	t.Helper()
	for _, name := range []string{"README.md", "LICENSE", "THIRD-PARTY-NOTICES.md", "licenses/Go-LICENSE"} {
		verificationWrite(t, filepath.Join(source, name), "Fixture notice\n", 0600)
	}
	verificationWrite(t, filepath.Join(source, "VERSION"), "1.2.3\n", 0600)
	verificationWrite(t, filepath.Join(source, "build-info.json"), verificationBuildInfo, 0600)
}

func verificationBundle(t *testing.T, app, name, identifier string) {
	t.Helper()
	info := fmt.Sprintf(`<?xml version="1.0"?><plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>%s</string>
<key>CFBundleIdentifier</key><string>%s</string>
<key>CFBundleShortVersionString</key><string>1.2.3</string>
<key>CFBundleVersion</key><string>42</string>
<key>LSMinimumSystemVersion</key><string>15.0</string>
<key>AgentQueueEmbeddedPayload</key><true/>
<key>AgentQueueServiceManagement</key><true/>
</dict></plist>`, name, identifier)
	verificationWrite(t, filepath.Join(app, "Contents/Info.plist"), info, 0600)
	verificationWrite(t, filepath.Join(app, "Contents/Resources/build-info.json"), verificationBuildInfo, 0600)
	verificationWrite(t, filepath.Join(app, "Contents/MacOS", name), "Fixture native executable", 0700)
}

func newVerificationFixture(t *testing.T) verificationFixture {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "Agent Queue.app")
	source := filepath.Join(app, "Contents/Resources/payload/source")
	verificationBundle(t, app, "AgentQueue", "io.went.agent-queue")
	verificationSource(t, source)
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		// Executing any bundled helper is an error, even with harmless flags.
		verificationWrite(t, filepath.Join(app, "Contents/MacOS", name), "#!/bin/sh\nprintf 'helper executed' >> \"$VERIFY_TEST_EVENTS\"\nexit 99\n", 0700)
	}
	for _, name := range []string{"heavy", "resources", "remote"} {
		filename := "io.went.agent-" + name + ".plist"
		raw, err := os.ReadFile(filepath.Join("../../native/LaunchAgents", filename))
		if err != nil {
			t.Fatal(err)
		}
		verificationWrite(t, filepath.Join(app, "Contents/Library/LaunchAgents", filename), string(raw), 0600)
	}
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	testTool(t, tools, "codesign", `printf 'codesign|%s\n' "$*" >> "$VERIFY_TEST_EVENTS"
[ "$1" = --verify ] && [ "$2" = --strict ] && [ "$3" = --deep ] || exit 98
[ -z "$VERIFY_TEST_BAD_SIGNATURE" ] || [ "$4" != "$VERIFY_TEST_BAD_SIGNATURE" ] || exit 17
`)
	testTool(t, tools, "xcrun", `printf 'xcrun|%s\n' "$*" >> "$VERIFY_TEST_EVENTS"
[ "$1" = lipo ] && [ "$2" = -archs ] || exit 98
if [ "$3" = "$VERIFY_TEST_BAD_ARCH" ]; then printf 'arm64\n'; else printf 'x86_64 arm64\n'; fi
`)
	events := filepath.Join(root, "events")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VERIFY_TEST_EVENTS", events)
	t.Setenv("VERIFY_TEST_BAD_SIGNATURE", "")
	t.Setenv("VERIFY_TEST_BAD_ARCH", "")
	return verificationFixture{root: root, app: app, source: source, events: events}
}

func TestVerifyAppChecksPayloadInsideOutWithoutExecutingIt(t *testing.T) {
	f := newVerificationFixture(t)
	if err := VerifyApp(f.app); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(f.events)
	if err != nil {
		t.Fatal(err)
	}
	record := string(events)
	previous := -1
	for _, path := range []string{filepath.Join(f.app, "Contents/MacOS/agent-heavy"), filepath.Join(f.app, "Contents/MacOS/agent-remote"), f.app} {
		index := strings.Index(record, "codesign|--verify --strict --deep "+path+"\n")
		if index <= previous {
			t.Fatalf("missing inside-out signature verification: %s", record)
		}
		previous = index
	}
	if strings.Contains(record, "helper executed") {
		t.Fatal("verification executed a bundled helper")
	}
}

func TestVerifyAppRejectsIncompleteOrModifiedPayload(t *testing.T) {
	for _, missing := range []string{"build-info.json", "licenses/Go-LICENSE", "README.md", "LICENSE", "THIRD-PARTY-NOTICES.md", "VERSION"} {
		t.Run("missing "+missing, func(t *testing.T) {
			f := newVerificationFixture(t)
			if err := os.Remove(filepath.Join(f.source, missing)); err != nil {
				t.Fatal(err)
			}
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("incomplete payload accepted")
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, verificationFixture)
	}{
		{"missing payload", func(t *testing.T, f verificationFixture) {
			if err := os.RemoveAll(filepath.Dir(f.source)); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty payload", func(t *testing.T, f verificationFixture) {
			if err := os.RemoveAll(f.source); err != nil {
				t.Fatal(err)
			}
		}},
		{"modified source identity", func(t *testing.T, f verificationFixture) {
			verificationWrite(t, filepath.Join(f.source, "build-info.json"), strings.ReplaceAll(verificationBuildInfo, "1.2.3", "1.2.4"), 0600)
		}},
		{"missing app identity", func(t *testing.T, f verificationFixture) {
			if err := os.Remove(filepath.Join(f.app, "Contents/Resources/build-info.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"malformed source identity", func(t *testing.T, f verificationFixture) {
			verificationWrite(t, filepath.Join(f.source, "build-info.json"), "{}", 0600)
		}},
		{"modified app identity", func(t *testing.T, f verificationFixture) {
			verificationWrite(t, filepath.Join(f.app, "Contents/Resources/build-info.json"), strings.ReplaceAll(verificationBuildInfo, "42", "43"), 0600)
		}},
		{"modified native metadata", func(t *testing.T, f verificationFixture) {
			path := filepath.Join(f.app, "Contents/Info.plist")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			verificationWrite(t, path, strings.ReplaceAll(string(data), "io.went.agent-queue", "io.went.other"), 0600)
		}},
		{"modified version", func(t *testing.T, f verificationFixture) {
			verificationWrite(t, filepath.Join(f.source, "VERSION"), "1.2.4", 0600)
		}},
		{"recursive app", func(t *testing.T, f verificationFixture) {
			verificationWrite(t, filepath.Join(f.source, "nested.app/Contents/Info.plist"), "nested", 0600)
		}},
		{"symlink source", func(t *testing.T, f verificationFixture) {
			target := filepath.Join(f.root, "external-source")
			if err := os.Rename(f.source, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, f.source); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newVerificationFixture(t)
			test.mutate(t, f)
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("malformed app accepted")
			}
		})
	}
}

func TestVerifyAppChecksPermissionsArchitecturesAndSignatures(t *testing.T) {
	for _, relative := range []string{"Contents/MacOS/AgentQueue", "Contents/MacOS/agent-heavy", "Contents/MacOS/agent-remote"} {
		for _, failure := range []string{"permissions", "architectures", "signature"} {
			t.Run(relative+"/"+failure, func(t *testing.T) {
				f := newVerificationFixture(t)
				path := filepath.Join(f.app, relative)
				switch failure {
				case "permissions":
					if err := os.Chmod(path, 0600); err != nil {
						t.Fatal(err)
					}
				case "architectures":
					t.Setenv("VERIFY_TEST_BAD_ARCH", path)
				case "signature":
					if relative == "Contents/MacOS/AgentQueue" {
						path = f.app // Native code is verified through its app bundle.
					}
					t.Setenv("VERIFY_TEST_BAD_SIGNATURE", path)
				}
				if err := VerifyApp(f.app); err == nil {
					t.Fatal("invalid executable accepted")
				}
			})
		}
	}
}

func TestVerifyAppRejectsPrivateStateAcrossBundle(t *testing.T) {
	for _, relative := range []string{"Contents/Resources/state.json", "Contents/Resources/payload/source/remote.json", "Contents/Resources/payload/source/logs/job.log", "Contents/Resources/payload/source/root.key", "Contents/Resources/payload/source/queue.sqlite3", "Contents/Resources/payload/source/runtime.py", "Contents/Resources/payload/source/.git/config"} {
		t.Run(relative, func(t *testing.T) {
			f := newVerificationFixture(t)
			verificationWrite(t, filepath.Join(f.app, relative), "private", 0600)
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("private state accepted")
			}
		})
	}
}

func TestVerifyAppRejectsInstallerEraLayouts(t *testing.T) {
	for _, mutation := range []string{"missing service marker", "embedded helpers", "wrapper installer"} {
		t.Run(mutation, func(t *testing.T) {
			f := newVerificationFixture(t)
			switch mutation {
			case "missing service marker":
				info := filepath.Join(f.app, "Contents/Info.plist")
				raw, err := os.ReadFile(info)
				if err != nil {
					t.Fatal(err)
				}
				verificationWrite(t, info, strings.Replace(string(raw), "<key>AgentQueueServiceManagement</key><true/>", "", 1), 0600)
			case "embedded helpers":
				verificationWrite(t, filepath.Join(f.source, "bin/agent-heavy"), "duplicate", 0700)
			case "wrapper installer":
				verificationWrite(t, filepath.Join(f.app, "Contents/Resources/payload/Agent Queue.app/Contents/Info.plist"), "nested", 0600)
			}
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("installer-era layout accepted")
			}
		})
	}
}

func TestVerifyAppRejectsTamperedServices(t *testing.T) {
	for _, mutation := range []string{"marker type", "marker false", "helper missing", "writable helper", "hardlinked helper", "missing plist", "plist symlink", "argument", "run policy", "home environment", "log path", "extra login plist"} {
		t.Run(mutation, func(t *testing.T) {
			f := newVerificationFixture(t)
			agent := filepath.Join(f.app, "Contents/Library/LaunchAgents/io.went.agent-heavy.plist")
			helper := filepath.Join(f.app, "Contents/MacOS/agent-heavy")
			switch mutation {
			case "marker type", "marker false":
				info := filepath.Join(f.app, "Contents/Info.plist")
				raw, _ := os.ReadFile(info)
				value := "<string>true</string>"
				if mutation == "marker false" {
					value = "<false/>"
				}
				verificationWrite(t, info, strings.Replace(string(raw), "<key>AgentQueueServiceManagement</key><true/>", "<key>AgentQueueServiceManagement</key>"+value, 1), 0600)
			case "helper missing":
				if err := os.Remove(helper); err != nil {
					t.Fatal(err)
				}
			case "writable helper":
				if err := os.Chmod(helper, 0777); err != nil {
					t.Fatal(err)
				}
			case "hardlinked helper":
				if err := os.Link(helper, filepath.Join(f.root, "extra-helper")); err != nil {
					t.Fatal(err)
				}
			case "missing plist":
				if err := os.Remove(agent); err != nil {
					t.Fatal(err)
				}
			case "plist symlink":
				target := filepath.Join(f.root, "external.plist")
				if err := os.Rename(agent, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, agent); err != nil {
					t.Fatal(err)
				}
			case "extra login plist":
				verificationWrite(t, filepath.Join(filepath.Dir(agent), "io.went.agent-queue.plist"), "login", 0600)
			default:
				raw, _ := os.ReadFile(agent)
				value := string(raw)
				switch mutation {
				case "argument":
					value = strings.Replace(value, "<string>daemon</string>", "<string>scheduler</string>", 1)
				case "run policy":
					value = strings.Replace(value, "<key>SuccessfulExit</key><false/>", "<key>SuccessfulExit</key><true/>", 1)
				case "home environment":
					value = strings.Replace(value, "</dict></plist>", "<key>EnvironmentVariables</key><dict><key>HOME</key><string>/private/var/empty</string></dict></dict></plist>", 1)
				case "log path":
					value = strings.Replace(value, "</dict></plist>", "<key>StandardOutPath</key><string>/tmp/private.log</string></dict></plist>", 1)
				}
				verificationWrite(t, agent, value, 0600)
			}
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("managed tampering accepted")
			}
		})
	}
}
